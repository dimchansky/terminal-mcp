//go:build windows

package httpguard

import (
	"fmt"
	"os"

	"terminalmcp/internal/winsecure"
)

func validateTokenFile(path string, info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return fmt.Errorf("token file must be a regular file")
	}
	return winsecure.CheckPrivate(path)
}
