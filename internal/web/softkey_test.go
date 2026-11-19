package web

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"testing"

	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
)

// softKey is a software passkey with the browser around it: it answers
// the panel's ceremony options as a browser on origin would, with an
// ECDSA P-256 key scoped to rpID. Tests change origin or rpID to play a
// phishing page.
type softKey struct {
	origin, rpID string
	// skipUV leaves out user verification (a PIN or biometric).
	skipUV bool

	id     []byte
	key    *ecdsa.PrivateKey
	handle []byte
	count  uint32
}

func newSoftKey(t *testing.T, origin, rpID string) *softKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &softKey{origin: origin, rpID: rpID, id: randomBytes(16), key: key}
}

var b64 = base64.RawURLEncoding

const (
	flagUP = 1 << 0 // user present
	flagUV = 1 << 2 // user verified
	flagAT = 1 << 6 // attested credential data follows
)

// create answers a navigator.credentials.create() options body with a
// registration response body.
func (k *softKey) create(t *testing.T, options []byte) []byte {
	t.Helper()
	var o struct {
		PublicKey struct {
			Challenge string
			User      struct{ ID string }
		}
	}
	if err := json.Unmarshal(options, &o); err != nil {
		t.Fatalf("creation options %s: %v", options, err)
	}
	var err error
	if k.handle, err = b64.DecodeString(o.PublicKey.User.ID); err != nil {
		t.Fatal(err)
	}
	point, err := k.key.PublicKey.Bytes() // 0x04 || X || Y
	if err != nil {
		t.Fatal(err)
	}
	cose, err := webauthncbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: point[1:33], -3: point[33:]}) // EC2, ES256, P-256
	if err != nil {
		t.Fatal(err)
	}
	attested := append(make([]byte, 16), byte(len(k.id)>>8), byte(len(k.id))) // zero AAGUID, length
	attested = append(append(attested, k.id...), cose...)
	authData := append(k.authData(flagUP|flagUV|flagAT), attested...)
	attestation, err := webauthncbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": authData})
	if err != nil {
		t.Fatal(err)
	}
	return k.credential(map[string]any{
		"clientDataJSON":    b64.EncodeToString(k.clientData("webauthn.create", o.PublicKey.Challenge)),
		"attestationObject": b64.EncodeToString(attestation),
	})
}

// get answers a navigator.credentials.get() options body with an
// assertion response body.
func (k *softKey) get(t *testing.T, options []byte) []byte {
	t.Helper()
	var o struct{ PublicKey struct{ Challenge string } }
	if err := json.Unmarshal(options, &o); err != nil {
		t.Fatalf("request options %s: %v", options, err)
	}
	k.count++
	authData := k.authData(flagUP | flagUV)
	clientData := k.clientData("webauthn.get", o.PublicKey.Challenge)
	hash := sha256.Sum256(clientData)
	digest := sha256.Sum256(append(authData, hash[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, k.key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return k.credential(map[string]any{
		"clientDataJSON":    b64.EncodeToString(clientData),
		"authenticatorData": b64.EncodeToString(authData),
		"signature":         b64.EncodeToString(sig),
		"userHandle":        b64.EncodeToString(k.handle),
	})
}

func (k *softKey) authData(flags byte) []byte {
	if k.skipUV {
		flags &^= flagUV
	}
	rpIDHash := sha256.Sum256([]byte(k.rpID))
	return binary.BigEndian.AppendUint32(append(rpIDHash[:], flags), k.count)
}

func (k *softKey) clientData(typ, challenge string) []byte {
	b, _ := json.Marshal(map[string]any{"type": typ, "challenge": challenge, "origin": k.origin, "crossOrigin": false})
	return b
}

func (k *softKey) credential(response map[string]any) []byte {
	b, _ := json.Marshal(map[string]any{
		"id": b64.EncodeToString(k.id), "rawId": b64.EncodeToString(k.id), "type": "public-key",
		"response": response, "clientExtensionResults": map[string]any{},
	})
	return b
}
