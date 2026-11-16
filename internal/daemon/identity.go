package daemon

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"

	"github.com/anesthetised/lotsman/internal/awgconf"
)

// Identity is the downstream device's key and the obfuscation parameters every
// client must share with it. It is generated on first start and kept in the
// state directory; changing it invalidates every issued client config. It
// holds what every AWG generation needs: the awg setting only selects from it
// (Shared), so changing the level never rewrites the file.
type Identity struct {
	PrivateKey awgconf.Key
	// Shared AWG 2 parameters: S1-S4 and H1-H4. Junk (Jc/Jmin/Jmax) and signature
	// packets (I1-I5) are client-side only and are added when rendering client configs.
	Params awgconf.Params
	// HeaderProtectionKey is the AWG 3 key every client shares; level 2 ignores it.
	HeaderProtectionKey awgconf.Key
}

// minPadding is the smallest S1-S4: AWG 3 header protection takes its 12-byte
// nonce from the padding, and the runtime refuses a key with less.
const minPadding = 12

// Shared returns the parameters the listener and every client must agree on
// at AWG generation awg. Level 2 is AWG 2.0 on the wire. Level 3 adds header
// protection and random trailers, which the runtime applies to the whole
// device, so every client gets the same values.
func (id Identity) Shared(awg int) awgconf.Params {
	p := id.Params
	if awg >= 3 {
		p.HeaderProtectionKey = id.HeaderProtectionKey
		p.RandomTrailers = true
	}
	return p
}

// identityFile is the on-disk JSON. Files written before the explicit tags
// have capitalized S1..H4 keys; encoding/json matches keys case-insensitively,
// so they still load. Files written before AWG 3 support lack
// header_protection_key and are upgraded on load.
type identityFile struct {
	PrivateKey          string `json:"private_key"`
	S1                  uint16 `json:"s1"`
	S2                  uint16 `json:"s2"`
	S3                  uint16 `json:"s3"`
	S4                  uint16 `json:"s4"`
	H1                  string `json:"h1"`
	H2                  string `json:"h2"`
	H3                  string `json:"h3"`
	H4                  string `json:"h4"`
	HeaderProtectionKey string `json:"header_protection_key"`
}

// LoadOrCreateIdentity returns the saved identity, creating it on first use
// and upgrading one written before AWG 3 support. Safe across concurrent
// callers and processes: exactly one creator publishes a complete file, the
// others load the winner's.
func LoadOrCreateIdentity(stateDir string) (Identity, error) {
	path := filepath.Join(stateDir, "identity.json")
	id, err := loadIdentity(path)
	if err == nil {
		return upgradeIdentity(path, id)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return Identity{}, err
	}
	if id, err = newIdentity(); err != nil {
		return Identity{}, err
	}
	if err := saveIdentity(path, id, os.Link); err != nil {
		if errors.Is(err, os.ErrExist) {
			return loadIdentity(path)
		}
		return Identity{}, err
	}
	return id, nil
}

func loadIdentity(path string) (Identity, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Identity{}, err
	}
	var f identityFile
	if err := json.Unmarshal(data, &f); err != nil {
		return Identity{}, fmt.Errorf("%s: %w", path, err)
	}
	id := Identity{Params: awgconf.Params{S1: f.S1, S2: f.S2, S3: f.S3, S4: f.S4}}
	if id.PrivateKey, err = awgconf.ParseKey(f.PrivateKey); err != nil {
		return Identity{}, fmt.Errorf("%s: %w", path, err)
	}
	var hs [4]awgconf.Range
	for i, h := range []string{f.H1, f.H2, f.H3, f.H4} {
		if hs[i], err = awgconf.ParseRange(h); err != nil {
			return Identity{}, fmt.Errorf("%s: H%d: %w", path, i+1, err)
		}
	}
	id.Params.H1, id.Params.H2, id.Params.H3, id.Params.H4 = hs[0], hs[1], hs[2], hs[3]
	if f.HeaderProtectionKey != "" {
		if id.HeaderProtectionKey, err = awgconf.ParseKey(f.HeaderProtectionKey); err != nil {
			return Identity{}, fmt.Errorf("%s: header_protection_key: %w", path, err)
		}
	}
	return id, nil
}

// upgradeIdentity completes an identity written before AWG 3 support: it
// derives the missing header protection key, raises S1-S4 to minPadding and
// replaces the file atomically. The result depends only on the old file, so
// processes upgrading it concurrently write the same content. A complete
// identity is returned as is.
func upgradeIdentity(path string, id Identity) (Identity, error) {
	old := id
	if id.HeaderProtectionKey.IsZero() {
		id.HeaderProtectionKey = deriveHeaderProtectionKey(id.PrivateKey)
	}
	p := &id.Params
	p.S1, p.S2 = max(p.S1, minPadding), max(p.S2, minPadding)
	p.S3, p.S4 = max(p.S3, minPadding), max(p.S4, minPadding)
	if id == old {
		return id, nil
	}
	if err := saveIdentity(path, id, os.Rename); err != nil {
		return Identity{}, err
	}
	return id, nil
}

// deriveHeaderProtectionKey is HMAC-SHA256 of a fixed label under the private
// key: as unpredictable as a random key to anyone without the private key.
func deriveHeaderProtectionKey(priv awgconf.Key) awgconf.Key {
	mac := hmac.New(sha256.New, priv[:])
	mac.Write([]byte("lotsman header protection key"))
	return awgconf.Key(mac.Sum(nil))
}

// saveIdentity publishes id at path atomically: it writes a private temp file
// in the same directory and moves it into place with publish, so readers never
// see a partial file. With os.Link it never replaces an existing file and
// returns an error matching os.ErrExist if path is taken; os.Rename replaces it.
func saveIdentity(path string, id Identity, publish func(oldpath, newpath string) error) error {
	f := identityFile{
		PrivateKey: id.PrivateKey.String(),
		S1:         id.Params.S1, S2: id.Params.S2, S3: id.Params.S3, S4: id.Params.S4,
		H1: id.Params.H1.String(), H2: id.Params.H2.String(),
		H3: id.Params.H3.String(), H4: id.Params.H4.String(),
		HeaderProtectionKey: id.HeaderProtectionKey.String(),
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(path, data, publish)
}

// writePrivate publishes data at path atomically as a 0600 file: it writes a
// temp file in the same directory, creating the directory 0700 if missing,
// and moves it into place with publish (os.Rename or os.Link).
func writePrivate(path string, data []byte, publish func(oldpath, newpath string) error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*.tmp") // mode 0600
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return publish(tmp.Name(), path)
}

// newIdentity draws parameters from the ranges the AmneziaWG documentation
// recommends, with S1-S4 of at least minPadding so that every level can use
// them. S4 pads every transport packet and costs MTU, so it stays small.
func newIdentity() (Identity, error) {
	key, err := awgconf.GeneratePrivateKey()
	if err != nil {
		return Identity{}, err
	}
	var hpk awgconf.Key
	if _, err := rand.Read(hpk[:]); err != nil {
		return Identity{}, err
	}
	p := awgconf.Params{
		S1: uint16(randInt(15, 100)),
		S2: uint16(randInt(15, 100)),
		S3: uint16(randInt(minPadding, 60)),
		S4: uint16(randInt(minPadding, 16)),
	}
	// Four non-overlapping header ranges, each 4096 wide, well above the
	// stock WireGuard message types 1-4.
	base := uint32(randInt(1<<16, 1<<30))
	var hs [4]awgconf.Range
	for i := range hs {
		lo := base + uint32(i)*(1<<20)
		hs[i] = awgconf.Range{Lo: lo, Hi: lo + 4095}
	}
	p.H1, p.H2, p.H3, p.H4 = hs[0], hs[1], hs[2], hs[3]
	return Identity{PrivateKey: key, Params: p, HeaderProtectionKey: hpk}, nil
}

// randInt returns a uniform random integer in [lo, hi].
func randInt(lo, hi int64) int64 {
	n, err := rand.Int(rand.Reader, big.NewInt(hi-lo+1))
	if err != nil {
		panic(err) // crypto/rand failing means the system is unusable
	}
	return lo + n.Int64()
}
