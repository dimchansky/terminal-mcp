package audit

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type faultFile struct {
	auditFile
	writeFn    func([]byte) (int, error)
	truncateFn func(int64) error
	syncFn     func() error
	closeFn    func() error
}

func (f *faultFile) Write(b []byte) (int, error) {
	if f.writeFn != nil {
		return f.writeFn(b)
	}
	return f.auditFile.Write(b)
}
func (f *faultFile) Truncate(n int64) error {
	if f.truncateFn != nil {
		return f.truncateFn(n)
	}
	return f.auditFile.Truncate(n)
}
func (f *faultFile) Sync() error {
	if f.syncFn != nil {
		return f.syncFn()
	}
	return f.auditFile.Sync()
}
func (f *faultFile) Close() error {
	if f.closeFn != nil {
		return f.closeFn()
	}
	return f.auditFile.Close()
}

func TestFileSinkRecoversAfterPartialWriteAndFailedTruncate(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	s, err := openFileSink(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.close() })
	underlying := s.f
	failTruncate := true
	s.f = &faultFile{
		auditFile: underlying,
		writeFn: func(b []byte) (int, error) {
			n, err := underlying.Write(b[:min(len(b), 6)])
			if err != nil {
				return n, err
			}
			return n, io.ErrShortWrite
		},
		truncateFn: func(size int64) error {
			if failTruncate {
				failTruncate = false
				return errors.New("injected truncate error")
			}
			return underlying.Truncate(size)
		},
	}
	failed := []byte("{\"partial\":true}\n")
	if err := s.appendLine(failed); !errors.Is(err, io.ErrShortWrite) || !strings.Contains(err.Error(), "injected truncate error") {
		t.Fatalf("injected partial write/truncate failure not reported: %v", err)
	}
	if !s.repair {
		t.Fatal("incomplete record not marked for repair")
	}
	s.f = underlying
	good := []byte("{\"complete\":true}\n")
	if err := s.appendLine(good); err != nil {
		t.Fatalf("could not recover from partially written record: %v", err)
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, good) {
		t.Fatalf("incomplete record was retained: %q, %v", data, err)
	}
}

func almostFullLogLine() []byte {
	line := bytes.Repeat([]byte("x"), int(maxFileBytes)-32)
	line[len(line)-1] = '\n'
	return line
}

func TestFileSinkRecoversAfterRotationCloseError(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	s, err := openFileSink(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.close() })
	if err := s.appendLine(almostFullLogLine()); err != nil {
		t.Fatal(err)
	}
	old := s.f
	closeCalls := 0
	s.f = &faultFile{
		auditFile: old,
		closeFn: func() error {
			closeCalls++
			if err := old.Close(); err != nil {
				return err
			}
			return errors.New("injected close error")
		},
	}
	next := bytes.Repeat([]byte("n"), 64)
	next[len(next)-1] = '\n'
	if err := s.appendLine(next); err == nil || !strings.Contains(err.Error(), "injected close error") {
		t.Fatalf("rotation close error not reported: %v", err)
	}
	if s.f != nil || closeCalls != 1 {
		t.Fatalf("sink retained potentially closed file: f=%v closes=%d", s.f, closeCalls)
	}
	if err := s.appendLine(next); err != nil {
		t.Fatalf("sink could not retry rotation after close failure: %v", err)
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path + ".1")
	if err != nil || info.Size() != int64(len(almostFullLogLine())) {
		t.Fatalf("archived log corrupted: %v %v", info, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, next) {
		t.Fatalf("active log after rotation: %q %v", data, err)
	}
}

func TestFileSinkRecoversAfterRenameFailure(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	s, err := openFileSink(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.close() })
	if err := s.appendLine(almostFullLogLine()); err != nil {
		t.Fatal(err)
	}
	renameCalls := 0
	s.rename = func(old, new string) error {
		renameCalls++
		if old == s.path {
			return errors.New("injected rename error")
		}
		return os.Rename(old, new)
	}
	line := []byte(strings.Repeat("a", 80) + "\n")
	if err := s.appendLine(line); err == nil || !strings.Contains(err.Error(), "injected rename error") {
		t.Fatalf("rename failure not reported: %v", err)
	}
	if s.f != nil || renameCalls == 0 {
		t.Fatalf("rename failure kept open descriptor: f=%v count=%d", s.f, renameCalls)
	}
	s.rename = os.Rename
	if err := s.appendLine(line); err != nil {
		t.Fatalf("failed to resume after retryable rename error: %v", err)
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, line) {
		t.Fatalf("post-failure rotation incorrect: %q %v", data, err)
	}
}

func TestFileSinkRemoveFailureIsRetryable(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	s, err := openFileSink(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.close() })
	if err := s.appendLine(almostFullLogLine()); err != nil {
		t.Fatal(err)
	}
	s.remove = func(name string) error { return fmt.Errorf("injected remove error for %s", filepath.Base(name)) }
	line := []byte(strings.Repeat("a", 100) + "\n")
	if err := s.appendLine(line); err == nil || !strings.Contains(err.Error(), "injected remove error") {
		t.Fatalf("remove failure not reported: %v", err)
	}
	s.remove = os.Remove
	if err := s.appendLine(line); err != nil {
		t.Fatalf("could not resume after remove failure: %v", err)
	}
}

func TestFileSinkSyncFailureStillReleasesLock(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	s, err := openFileSink(path)
	if err != nil {
		t.Fatal(err)
	}
	actual := s.f
	s.f = &faultFile{
		auditFile: actual,
		syncFn:    func() error { return errors.New("injected sync error") },
	}
	if err := s.appendLine([]byte("{\"event\":1}\n")); err != nil {
		t.Fatal(err)
	}
	if err := s.close(); err == nil || !strings.Contains(err.Error(), "injected sync error") {
		t.Fatalf("sync failure not surfaced: %v", err)
	}
	reopened, err := openFileSink(path)
	if err != nil {
		t.Fatalf("audit lock not released after sync failure: %v", err)
	}
	if err := reopened.close(); err != nil {
		t.Fatal(err)
	}
}
