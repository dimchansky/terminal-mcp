//go:build darwin

package main

// A terminated macOS child is reaped by its parent/launchd and becomes
// ESRCH. Linux procfs zombie detection is not meaningful on Darwin.
func processZombie(int) bool { return false }
