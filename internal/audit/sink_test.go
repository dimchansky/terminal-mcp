package audit

import (
	"path/filepath"
	"testing"
)

// privateAuditDir provisions the platform's actual privacy contract:
// 0700 on Unix, an explicit private DACL on Windows.
func privateAuditDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "audit")
	if err := PrepareDirectory(dir); err != nil {
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
