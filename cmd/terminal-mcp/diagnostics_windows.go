//go:build windows

package main

import (
	"os"

	"terminalmcp/internal/winsecure"
)

func checkDiagnosticPrivate(path string, _ os.FileInfo) error {
	return winsecure.CheckPrivate(path)
}
