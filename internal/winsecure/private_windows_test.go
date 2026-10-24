//go:build windows

package winsecure

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsPrivateDirectoryAndFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "child")
	if err := EnsurePrivateDir(path); err != nil {
		t.Fatalf("create private tree: %v", err)
	}
	if err := CheckPrivate(path); err != nil {
		t.Fatalf("new directory is not private: %v", err)
	}
	file := filepath.Join(path, "token")
	if err := os.WriteFile(file, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivate(file); err != nil {
		t.Fatalf("file did not inherit private ACL: %v", err)
	}
}

func TestWindowsRejectsBroadDACL(t *testing.T) {
	private := filepath.Join(t.TempDir(), "private")
	if err := EnsurePrivateDir(private); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(private, "exposed")
	if err := os.WriteFile(file, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(file, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivate(file); err == nil {
		t.Fatal("world-readable token was accepted")
	}
}
