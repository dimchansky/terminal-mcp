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

// Windows elevated accounts can create private files owned by
// BUILTIN\Administrators. Neither a broad user group nor an unrelated
// account can satisfy the owner check.
func TestTrustedPrivateOwner(t *testing.T) {
	user, err := currentUser()
	if err != nil {
		t.Fatal(err)
	}
	admins, err := windows.StringToSid("S-1-5-32-544")
	if err != nil {
		t.Fatal(err)
	}
	users, err := windows.StringToSid("S-1-5-32-545")
	if err != nil {
		t.Fatal(err)
	}
	everyone, err := windows.StringToSid("S-1-1-0")
	if err != nil {
		t.Fatal(err)
	}
	if !trustedOwner(user, user) {
		t.Fatal("own user SID rejected")
	}
	if !trustedOwner(admins, user) {
		t.Fatal("Administrators ownership rejected")
	}
	for _, sid := range []*windows.SID{nil, users, everyone} {
		if trustedOwner(sid, user) {
			t.Fatalf("untrusted owner accepted: %v", sid)
		}
	}
}
