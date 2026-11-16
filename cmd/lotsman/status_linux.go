package main

import (
	"encoding/json"
	"os"
)

func writeStatus(stateDir string, s statusFile) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := statusPath(stateDir) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, statusPath(stateDir))
}
