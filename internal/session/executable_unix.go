//go:build linux || darwin

package session

import "os"

func executableFile(_ string, info os.FileInfo) bool { return info.Mode()&0111 != 0 }
func executableSuffixes(_ string) []string           { return nil }
