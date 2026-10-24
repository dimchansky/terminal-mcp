package audit

import (
	"os"
	"testing"
)

// privateAuditDir makes test permissions independent of CI runner umask.
func privateAuditDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// testSink is constructed before newLog launches its worker.
type testSink struct {
	write  func([]byte) error
	finish func() error
}

func (s *testSink) appendLine(data []byte) error {
	if s.write == nil {
		return nil
	}
	return s.write(data)
}
func (s *testSink) close() error {
	if s.finish == nil {
		return nil
	}
	return s.finish()
}
