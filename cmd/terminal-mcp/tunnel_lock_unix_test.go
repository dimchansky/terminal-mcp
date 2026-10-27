//go:build linux || darwin

package main

import "testing"

func TestTunnelLockExclusiveAndCrashSafeRelease(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	id := "tunnel_sdktestaaaaaaaaaaaaaaaaaaaaaaaaa"
	first, err := lockTunnelID(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockTunnelID(id); err == nil {
		t.Fatal("duplicate tunnel ID accepted")
	}
	first()
	second, err := lockTunnelID(id)
	if err != nil {
		t.Fatalf("tunnel lock did not release: %v", err)
	}
	second()
	different, err := lockTunnelID("tunnel_sdktestbbbbbbbbbbbbbbbbbbbbbbbbb")
	if err != nil {
		t.Fatalf("other tunnel ID was unnecessarily excluded: %v", err)
	}
	different()
}
