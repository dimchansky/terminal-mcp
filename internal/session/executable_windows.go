//go:build windows

package session

import (
	"os"
	"path/filepath"
	"strings"
)

func executableFile(path string, _ os.FileInfo) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".exe", ".com", ".bat", ".cmd":
		return true
	}
	return false
}

func executableSuffixes(name string) []string {
	if filepath.Ext(name) != "" {
		return nil
	}
	return []string{".exe", ".com", ".bat", ".cmd"}
}
