package daemon

import (
	"crypto/rand"
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
// state directory; changing it invalidates every issued client config.
type Identity struct {
	PrivateKey awgconf.Key
	// Shared parameters: S1-S4 and H1-H4. Junk (Jc/Jmin/Jmax) and signature
	// packets (I1-I5) are client-side only and are added when rendering client configs.
	Params awgconf.Params
}

type identityFile struct {
	PrivateKey string `json:"private_key"`
	S1, S2     uint16
	S3, S4     uint16
	H1, H2     string
	H3, H4     string
}

func LoadOrCreateIdentity(stateDir string) (Identity, error) {
	path := filepath.Join(stateDir, "identity.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		id, err := newIdentity()
		if err != nil {
			return Identity{}, err
		}
		return id, saveIdentity(path, id)
	}
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
	return id, nil
}

func saveIdentity(path string, id Identity) error {
	f := identityFile{
		PrivateKey: id.PrivateKey.String(),
		S1:         id.Params.S1, S2: id.Params.S2, S3: id.Params.S3, S4: id.Params.S4,
		H1: id.Params.H1.String(), H2: id.Params.H2.String(),
		H3: id.Params.H3.String(), H4: id.Params.H4.String(),
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// newIdentity draws parameters from the ranges the AmneziaWG documentation
// recommends. S4 pads every transport packet and costs MTU, so it stays small.
func newIdentity() (Identity, error) {
	key, err := awgconf.GeneratePrivateKey()
	if err != nil {
		return Identity{}, err
	}
	p := awgconf.Params{
		S1: uint16(randInt(15, 100)),
		S2: uint16(randInt(15, 100)),
		S3: uint16(randInt(10, 60)),
		S4: uint16(randInt(8, 16)),
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
	return Identity{PrivateKey: key, Params: p}, nil
}

// randInt returns a uniform random integer in [lo, hi].
func randInt(lo, hi int64) int64 {
	n, err := rand.Int(rand.Reader, big.NewInt(hi-lo+1))
	if err != nil {
		panic(err) // crypto/rand failing means the system is unusable
	}
	return lo + n.Int64()
}
