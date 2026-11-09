package main

import (
	"errors"
	"os"
	"path/filepath"
)

// savedDashboardTarget returns the saved standalone listener and token file.
func savedDashboardTarget() (listen, tokenFile string, ok bool, err error) {
	dir, err := windowsStateDir()
	if err != nil {
		return "", "", false, err
	}
	var c winStandaloneConfig
	if err := readJSON(filepath.Join(dir, "config.json"), &c); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", "", false, nil
		}
		return "", "", false, err
	}
	if c.Listen == "" || c.TokenFile == "" {
		return "", "", false, errors.New("incomplete standalone config")
	}
	return c.Listen, c.TokenFile, true, nil
}
