//go:build linux || darwin

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFileRootFromConfig(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.json")
	root, err := fileRootFromConfig("", file)
	if err != nil || root != "." {
		t.Fatalf("missing config: %q %v", root, err)
	}
	if err := os.WriteFile(file, []byte(`{"file_root":"/tmp/work","listen":"127.0.0.1:8090"}`), 0600); err != nil {
		t.Fatal(err)
	}
	root, err = fileRootFromConfig("", file)
	if err != nil || root != "/tmp/work" {
		t.Fatalf("configured root: %q %v", root, err)
	}
	root, err = fileRootFromConfig("/tmp/explicit", file)
	if err != nil || root != "/tmp/explicit" {
		t.Fatalf("flag did not override config: %q %v", root, err)
	}
	if err := os.WriteFile(file, []byte(`{"file_root":"relative"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := fileRootFromConfig("", file); err == nil {
		t.Fatal("accepted relative root")
	}
	if err := os.WriteFile(file, []byte(`not-json`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := fileRootFromConfig("", file); err == nil {
		t.Fatal("accepted invalid configuration")
	}
}
