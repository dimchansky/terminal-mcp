//go:build windows

package main

import "errors"

// The existing updater relies on POSIX process separation and atomic binary
// replacement; never claim Windows update safety until it has its own tests.
func updateCommand([]string) error {
	return errors.New("self-update on Windows is not yet supported; stop the server, replace the installed executable, and restart")
}
