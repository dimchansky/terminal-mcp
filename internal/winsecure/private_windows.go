//go:build windows

// Package winsecure handles private user-owned files on Windows. POSIX mode
// bits cannot be used to validate Windows permissions: DACLs are authoritative.
package winsecure

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

func currentUser() (*windows.SID, error) {
	token := windows.GetCurrentProcessToken()
	u, err := token.GetTokenUser()
	if err != nil {
		return nil, err
	}
	return u.User.Sid, nil
}

func privateDescriptor() (*windows.SECURITY_DESCRIPTOR, error) {
	sid, err := currentUser()
	if err != nil {
		return nil, err
	}
	// SYSTEM, Administrators and exactly the current user. Protect against
	// inherited broad Users/Everyone access; make child ACL inheritance private.
	return windows.SecurityDescriptorFromString(fmt.Sprintf(
		"D:P(A;OICI;FA;;;%s)(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)", sid.String()))
}

func EnsurePrivateDir(path string) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("private state path is not a real directory: %s", path)
		}
		return CheckPrivate(path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return fmt.Errorf("cannot create a private drive root")
	}
	// Only the final leaf is required to be private, but any intermediate
	// created directory is private from its creation.
	if _, err := os.Stat(parent); errors.Is(err, os.ErrNotExist) {
		if err := EnsurePrivateDir(parent); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	sd, err := privateDescriptor()
	if err != nil {
		return err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attrs := windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: sd,
	}
	if err := windows.CreateDirectory(name, &attrs); err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return err
	}
	return CheckPrivate(path)
}

func CheckPrivate(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !(info.IsDir() || info.Mode().IsRegular()) {
		return fmt.Errorf("private path must not be a link or special file: %s", path)
	}
	attrs, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	flags, err := windows.GetFileAttributes(attrs)
	if err != nil {
		return err
	}
	if flags&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("private path must not be a reparse point: %s", path)
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	if sd == nil {
		return fmt.Errorf("missing security descriptor: %s", path)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	user, err := currentUser()
	if err != nil {
		return err
	}
	if owner == nil || !owner.Equals(user) {
		return fmt.Errorf("private path must be owned by the service user: %s", path)
	}
	acl, _, err := sd.DACL()
	if err != nil || acl == nil {
		return fmt.Errorf("private path must have an explicit DACL: %s: %w", path, err)
	}
	system, err := windows.StringToSid("S-1-5-18")
	if err != nil {
		return err
	}
	admins, err := windows.StringToSid("S-1-5-32-544")
	if err != nil {
		return err
	}
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, i, &ace); err != nil {
			return err
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return fmt.Errorf("private path has an unexpected ACL entry: %s", path)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.Equals(user) && !sid.Equals(system) && !sid.Equals(admins) {
			return fmt.Errorf("private path grants access to another identity: %s", path)
		}
	}
	return nil
}

// SingleLinkOwned verifies a regular file belongs to the current identity and
// isn't hard-linked to another path. Used before transactional replacement.
func SingleLinkOwned(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("requires a regular file")
	}
	var identity windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &identity); err != nil {
		return err
	}
	if identity.NumberOfLinks != 1 {
		return fmt.Errorf("replacement requires a single-link file")
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(file.Fd()), windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	user, err := currentUser()
	if err != nil {
		return err
	}
	if owner == nil || !owner.Equals(user) {
		return errors.New("replacement requires a file owned by the service user")
	}
	return nil
}
