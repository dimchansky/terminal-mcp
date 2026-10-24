//go:build windows

package main

import (
	"fmt"
	"os"

	"terminalmcp/internal/winsecure"
)

func prepareTokenDirectory(dir string) error { return winsecure.EnsurePrivateDir(dir) }
func checkPrivateToken(path string, info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return fmt.Errorf("token must be a regular file: %s", path)
	}
	return winsecure.CheckPrivate(path)
}
