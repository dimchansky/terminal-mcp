//go:build linux || darwin

package main

import (
	"errors"
	"path/filepath"
)

// savedDashboardTarget returns the saved standalone listener and token file.
func savedDashboardTarget() (listen, tokenFile string, ok bool, err error) {
	dir, err := standaloneDirectory()
	if err != nil {
		return "", "", false, err
	}
	c, err := savedStandaloneConfig(filepath.Join(dir, "config.json"))
	if err != nil || c.Listen == "" {
		return "", "", false, err
	}
	if c.TokenFile == "" {
		return "", "", false, errors.New("stored standalone configuration is incomplete")
	}
	return c.Listen, c.TokenFile, true, nil
}
