//go:build windows

package audit

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
	"terminalmcp/internal/winsecure"
)

// openAuditReader deliberately shares DELETE as well as READ/WRITE. Ordinary
// os.Open on Windows does not guarantee delete sharing: a held reader handle
// can then prevent the audit writer from renaming a full active journal.
func openAuditReader(path string) (*os.File, error) {
	if err := winsecure.CheckPrivate(filepath.Dir(path)); err != nil {
		return nil, err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, fmt.Errorf("open audit reader: %w", err)
	}
	f := os.NewFile(uintptr(h), path)
	if err := validateOpenedAuditReader(f); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

func validateOpenedAuditReader(f *os.File) error {
	h := windows.Handle(f.Fd())
	kind, err := windows.GetFileType(h)
	if err != nil {
		return err
	}
	if kind != windows.FILE_TYPE_DISK {
		return fmt.Errorf("audit reader requires a disk file")
	}
	// Ask the opened handle, not its pathname: a concurrent writer rotation
	// may rename/replace the path between CreateFile and validation.
	var tag struct {
		attributes uint32
		reparseTag uint32
	}
	if err := windows.GetFileInformationByHandleEx(h, windows.FileAttributeTagInfo,
		(*byte)(unsafe.Pointer(&tag)), uint32(unsafe.Sizeof(tag))); err != nil {
		return err
	}
	if tag.attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		tag.attributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return fmt.Errorf("audit reader refuses reparse points and directories")
	}
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("audit reader requires a regular file")
	}
	return winsecure.CheckPrivateHandle(f)
}
