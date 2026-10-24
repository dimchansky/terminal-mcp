//go:build linux || darwin

package main

import (
	"fmt"
	"os"
)

func prepareTokenDirectory(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("token directory must be private (0700): %s", dir)
	}
	return nil
}

func checkPrivateToken(path string, info os.FileInfo) error {
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("token must be a private regular file: %s", path)
	}
	return nil
}
