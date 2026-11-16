package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/anesthetised/lotsman/internal/proxy"
)

// The last good key of each dynamic upstream is kept in the state directory,
// so the daemon starts while the key's URL is unreachable. The file holds a
// working access key: it is written like the identity, 0600 and atomically.

// keyCachePath is where an upstream's last good dynamic key is kept.
func keyCachePath(stateDir, upstream string) string {
	return filepath.Join(stateDir, "ssconf", upstream+".json")
}

type cachedKey struct {
	Location string `json:"location"` // the URL's fingerprint: a cached key serves only the URL it came from
	Config   string `json:"config"`   // the response, as proxy.ParseConfig reads it
}

func saveCachedKey(stateDir, upstream string, loc *proxy.Location, k dynamicKey) error {
	data, err := json.Marshal(cachedKey{Location: loc.Fingerprint(), Config: k.text})
	if err != nil {
		return err
	}
	return writePrivate(keyCachePath(stateDir, upstream), data, os.Rename)
}

// loadCachedKey reads the key cached for loc. Errors never quote the file.
func loadCachedKey(stateDir, upstream string, loc *proxy.Location) (dynamicKey, error) {
	data, err := os.ReadFile(keyCachePath(stateDir, upstream))
	if errors.Is(err, os.ErrNotExist) {
		return dynamicKey{}, errors.New("no cached key")
	} else if err != nil {
		return dynamicKey{}, err
	}
	var c cachedKey
	if json.Unmarshal(data, &c) != nil {
		return dynamicKey{}, errors.New("cached key is corrupt")
	}
	if c.Location != loc.Fingerprint() {
		return dynamicKey{}, errors.New("no cached key for this URL")
	}
	key, err := proxy.ParseConfig(c.Config)
	if err != nil {
		return dynamicKey{}, errors.New("cached key does not parse")
	}
	return dynamicKey{key, c.Config}, nil
}

// removeCachedKey deletes a key once its upstream is gone or no longer
// dynamic, so no credential outlives its configuration.
func removeCachedKey(stateDir, upstream string) error {
	err := os.Remove(keyCachePath(stateDir, upstream))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
