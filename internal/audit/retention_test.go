package audit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuditArchivesAreNeverEvicted(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	recorder, err := OpenWithLimit(path, 30<<20)
	if err != nil {
		t.Fatal(err)
	}
	// Each event fits into a 4 MiB JSONL file but forces rotation after
	// the previous one, exceeding the old three-archive retention window.
	payload := strings.Repeat("x", 3<<20)
	for i := range 6 {
		event := NewEvent(Event{RequestID: fmt.Sprintf("retained-%d", i), Tool: "terminal_start",
			Phase: "request_start", Args: map[string]any{"argv": []string{payload}}})
		if err := recorder.Start(event); err != nil {
			t.Fatalf("record %d failed: %v", i, err)
		}
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	var count int
	var total int64
	for _, suffix := range []string{".1", ".2", ".3", ".4", ".5", ""} {
		p := path + suffix
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("archive %q was silently evicted: %v", suffix, err)
		}
		if len(data) == 0 || data[len(data)-1] != '\n' || !json.Valid(bytes.TrimSpace(data)) {
			t.Fatalf("archive %q contains an invalid record", suffix)
		}
		var entry Event
		if err := json.Unmarshal(bytes.TrimSpace(data), &entry); err != nil {
			t.Fatal(err)
		}
		if entry.RequestID != fmt.Sprintf("retained-%d", count) {
			t.Fatalf("archive %q lost chronological event %d: %s", suffix, count, entry.RequestID)
		}
		total += int64(len(data))
		count++
	}
	if count != 6 {
		t.Fatalf("kept %d records", count)
	}
	reopened, err := OpenWithLimit(path, 30<<20)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Status().BytesUsed != total {
		t.Fatalf("reopen accounting %d, expected %d", reopened.Status().BytesUsed, total)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultHistoryIsUnlimitedAndReportsActualUsage(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	original := []byte("{\"historical\":true}\n")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenWithLimit(path, int64(len(original))-1); !errors.Is(err, ErrHistoryLimit) {
		t.Fatalf("explicit budget unexpectedly ignored: %v", err)
	}
	recorder, err := Open(path)
	if err != nil {
		t.Fatalf("default audit should accept existing history without total cap: %v", err)
	}
	event := NewEvent(Event{Tool: "terminal_list", Phase: "request_start"})
	if err := recorder.Start(event); err != nil {
		t.Fatal(err)
	}
	status := recorder.Status()
	if status.State != "ok" || status.BytesLimit != 0 || status.BytesUsed <= int64(len(original)) {
		t.Fatalf("unlimited audit status: %+v", status)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAuditHistoryBudgetRefusesNewEffectsAndRetainsOldBytes(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	event := NewEvent(Event{Tool: "file_write", Phase: "request_start", Args: map[string]any{
		"content": strings.Repeat("!", 250),
	}})
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	recordSize := int64(len(encoded) + 1)
	limit := 3*recordSize + 1
	recorder, err := OpenWithLimit(path, limit)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := recorder.Start(event); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := recorder.Status(); got.BytesUsed != int64(len(before)) || got.BytesLimit != limit ||
		got.State != "ok" {
		t.Fatalf("incorrect live budget: %+v", got)
	}
	if err := recorder.Start(event); !errors.Is(err, ErrHistoryLimit) {
		t.Fatalf("history limit did not fail with a distinct error: %v", err)
	}
	if got := recorder.Status(); got.State != "degraded" || !strings.Contains(got.LastError, "audit_history_limit") {
		t.Fatalf("limit did not latch degraded health: %+v", got)
	}
	if data, err := os.ReadFile(path); err != nil || !bytes.Equal(before, data) {
		t.Fatalf("storage budget rejection changed existing history: %q %v", data, err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenWithLimit(path, limit)
	if err != nil {
		t.Fatalf("restart could not inspect full history: %v", err)
	}
	if err := reopened.Start(event); !errors.Is(err, ErrHistoryLimit) {
		t.Fatalf("restart bypassed historical budget: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestExplicitBudgetExhaustionCannotRecoverViaSmallerRequest(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	recorder, err := OpenWithLimit(path, 1024)
	if err != nil {
		t.Fatal(err)
	}
	// A large post-effect completion cannot fit, even though a small
	// request_start would fit. Missing evidence must not be ignored.
	largeFinish := NewEvent(Event{Tool: "file_write", Phase: "operation_finish",
		Args: strings.Repeat("x", 2048)})
	if err := recorder.Write(largeFinish); !errors.Is(err, ErrHistoryLimit) {
		t.Fatalf("expected history cap: %v", err)
	}
	if err := recorder.Start(NewEvent(Event{Tool: "terminal_list", Phase: "request_start"})); !errors.Is(err, ErrDegraded) {
		t.Fatalf("new effect admitted despite exhausted explicit cap: %v", err)
	}
	if got := recorder.Status(); got.State != "degraded" {
		t.Fatalf("budget denial silently cleared: %+v", got)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryBudgetCountsExistingLegacyArchives(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	oldFiles := map[string][]byte{
		"":   []byte("{\"active\":true}\n"),
		".1": []byte("{\"historical\":1}\n"),
		".2": []byte("{\"historical\":2}\n"),
		".3": []byte("{\"historical\":3}\n"),
		".5": []byte("{\"historical\":5}\n"),
	}
	var size int64
	for suffix, data := range oldFiles {
		if err := os.WriteFile(path+suffix, data, 0600); err != nil {
			t.Fatal(err)
		}
		size += int64(len(data))
	}
	if _, err := OpenWithLimit(path, size-1); !errors.Is(err, ErrHistoryLimit) {
		t.Fatalf("startup ignored archives exceeding budget: %v", err)
	}
	recorder, err := OpenWithLimit(path, size+maxFileBytes*2)
	if err != nil {
		t.Fatal(err)
	}
	if got := recorder.Status().BytesUsed; got != size {
		t.Fatalf("existing archives not accounted: used %d want %d", got, size)
	}
	// An intentionally migrated non-consecutive archive prevents the newly
	// rotated log from reusing the old .1-.5 names.
	if err := recorder.Write(NewEvent(Event{Tool: "file_read", Phase: "request_finish", Args: strings.Repeat("k", int(maxFileBytes)-1024)})); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Write(NewEvent(Event{Tool: "file_read", Phase: "request_finish", Args: strings.Repeat("m", 2000)})); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".6"); err != nil {
		t.Fatalf("next archive did not advance beyond legacy maximum: %v", err)
	}
	for suffix, data := range oldFiles {
		if suffix == "" {
			continue
		}
		got, err := os.ReadFile(path + suffix)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("historical archive %q was modified: %q %v", suffix, got, err)
		}
	}
}

func TestLimitDenialBeforeRotationLeavesDescriptorAndArchivesUntouched(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	big := almostFullLogLine()
	limit := int64(len(big) + 15)
	sink, err := openFileSinkWithLimit(path, limit)
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.appendLine(big); err != nil {
		t.Fatal(err)
	}
	if err := sink.appendLine(bytes.Repeat([]byte{'a'}, 32)); !errors.Is(err, ErrHistoryLimit) {
		t.Fatalf("expected budget failure before rotation, got %v", err)
	}
	if _, err := os.Stat(path + ".1"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("hard limit triggered unexpected history rotation: %v", err)
	}
	if err := sink.close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, big) {
		t.Fatalf("active journal changed after denied event: %v", err)
	}
}

func TestMigratedArchivesCountAgainstHistoryBudget(t *testing.T) {
	source, target := migrationTestPaths(t)
	files := writeAuditMigrationFixture(t, source)
	var used int64
	for _, data := range files {
		used += int64(len(data))
	}
	if _, err := OpenWithMigrationLimit(target, source, used-1); !errors.Is(err, ErrHistoryLimit) {
		t.Fatalf("migration bypassed total history budget: %v", err)
	}
	for suffix, want := range files {
		got, err := os.ReadFile(source + suffix)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("failed bounded migration altered legacy history: %q %v", got, err)
		}
	}
	log, err := OpenWithMigrationLimit(target, source, used+1024)
	if err != nil {
		t.Fatalf("could not resume migration with adequate budget: %v", err)
	}
	if got := log.Status().BytesUsed; got != used {
		t.Fatalf("migrated history size = %d want %d", got, used)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAuditRejectsUntrustedExistingArchive(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	target := filepath.Join(filepath.Dir(path), "other")
	if err := os.WriteFile(target, []byte("sensitive"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path+".1"); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("audit followed symlink in numbered history")
	}
}
