//go:build linux || darwin

package httpguard

import (
	"fmt"
	"os"
)

func validateTokenFile(_ string, info os.FileInfo) error {
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("token file must be regular and unreadable by group/other users (for example chmod 600)")
	}
	return nil
}
