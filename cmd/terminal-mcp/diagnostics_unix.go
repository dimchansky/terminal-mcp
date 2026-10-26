//go:build linux || darwin

package main

import (
	"fmt"
	"os"
)

func checkDiagnosticPrivate(path string, info os.FileInfo) error {
	if info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("diagnostic log must be private (0600): %s", path)
	}
	return nil
}
