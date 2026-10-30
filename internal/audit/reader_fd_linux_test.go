//go:build linux

package audit

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

// auditOpenDescriptors checks *only* this fixture's private journal directory.
// Its result is unaffected by Go's unrelated runtime, test or process handles.
func auditOpenDescriptors(dir string) ([]string, error) {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return nil, err
	}
	var result []string
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
		if errors.Is(err, os.ErrNotExist) {
			continue // The directory iterator itself may have closed.
		}
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(target, dir+string(os.PathSeparator)) {
			result = append(result, target)
		}
	}
	return result, nil
}

func TestHistoryReaderRepeatedSuccessAndFailureDoNotLeakDescriptors(t *testing.T) {
	dir, err := filepath.EvalSymlinks(privateAuditDir(t))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "audit.jsonl")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	if err := writer.Start(liveEvent("linux-fd")); err != nil {
		t.Fatal(err)
	}
	reader, err := NewReader(writer)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	page, err := reader.ReadPage(ctx, "", Forward, ReadBudget{MaxRecords: 1})
	if err != nil || len(page.Records) != 1 {
		t.Fatalf("fixture page: %+v %v", page, err)
	}
	ref := page.Records[0].Ref
	invalid := ref
	invalid.Hint = 0 // The eventual archive suffix does not exist yet.
	invalid.Pos.digest[0] ^= 0xff
	baseline, err := auditOpenDescriptors(dir)
	if err != nil || len(baseline) == 0 {
		t.Fatalf("fixture has no observable journal descriptors: %v %v", baseline, err)
	}
	oldGCPercent := debug.SetGCPercent(-1) // Do not let finalizers disguise leaked descriptors.
	defer debug.SetGCPercent(oldGCPercent)

	for range 120 {
		next, err := reader.ReadPage(ctx, "", Forward, ReadBudget{MaxRecords: 1})
		if err != nil || len(next.Records) != 1 || next.Records[0].Ref != ref {
			t.Fatalf("repeated page lost history: %+v %v", next, err)
		}
		if _, historical, err := reader.ReadRecord(ctx, ref); err != nil || historical {
			t.Fatalf("repeated detail failed: historical=%t err=%v", historical, err)
		}
		if _, _, err := reader.ReadRecord(ctx, invalid); !errors.Is(err, ErrInvalidCursor) {
			t.Fatalf("forged detail unexpectedly succeeded: %v", err)
		}
		edge, err := reader.ReadPage(ctx, page.Next, Forward, ReadBudget{})
		if err != nil || len(edge.Records) != 0 || edge.HasMore {
			t.Fatalf("read edge leaked a descriptor or returned data: %+v %v", edge, err)
		}
	}
	after, err := auditOpenDescriptors(dir)
	if err != nil || len(after) != len(baseline) {
		t.Fatalf("journal descriptors accumulated: before=%v after=%v err=%v", baseline, after, err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	final, err := auditOpenDescriptors(dir)
	if err != nil || len(final) != 0 {
		t.Fatalf("reader or writer left journal descriptors open: %v err=%v", final, err)
	}
}
