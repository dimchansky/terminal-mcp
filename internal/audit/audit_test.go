package audit

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestFullCommandArgumentExceedsOldMetadataLimit(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "audit.jsonl")
	log, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	command := strings.Repeat("x", 64000)
	if err := log.Write(Event{RequestID: RequestID(), Tool: "terminal_start", Phase: "start", Args: map[string]any{"argv": []string{"/bin/sh", "-c", command}}}); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), command) {
		t.Fatalf("full command truncated or unavailable: %v", err)
	}
}

func TestEventsPrivateAndMetadataOnly(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "audit.jsonl")
	log, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	id := RequestID()
	if err := log.Write(Event{RequestID: id, Tool: "terminal_start", Phase: "start"}); err != nil {
		t.Fatal(err)
	}
	if err := log.Write(Event{RequestID: id, Tool: "terminal_start", Phase: "finish", Status: "applied", SessionID: "s1", DurationMS: 2}); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("unsafe log permissions: %v %v", info, err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "password") || strings.Count(string(b), "\n") != 2 {
		t.Fatalf("unexpected audit content: %q", b)
	}
	scanner := bufio.NewScanner(strings.NewReader(string(b)))
	for scanner.Scan() {
		var event Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		if event.Time == "" || event.RequestID != id || event.Tool != "terminal_start" {
			t.Fatalf("bad event: %+v", event)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestRefusesPublicDirectoryAndSymlink(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(dir, "audit.jsonl")); err == nil {
		t.Fatal("accepted public audit directory")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(dir, "victim")
	if err := os.WriteFile(secret, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "audit.jsonl")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(link); err == nil {
		t.Fatal("accepted symlink audit path")
	}
	if data, err := os.ReadFile(secret); err != nil || string(data) != "preserve" {
		t.Fatalf("victim changed: %q %v", data, err)
	}
}

func TestConcurrentCallsRotateWithBoundedBackups(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "audit.jsonl")
	log, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	const workers = 6
	const perWorker = 9000
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perWorker; j++ {
				if err := log.Write(Event{RequestID: RequestID(), Tool: "file_read", Phase: "finish", Status: "ok"}); err != nil {
					t.Errorf("write: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatal("expected rotated log:", err)
	}
	for _, file := range []string{path, path + ".1"} {
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > maxFileBytes || info.Mode().Perm() != 0600 {
			t.Fatalf("bad rotation size/permission: %s %+v", file, info)
		}
	}
	if err := log.Write(Event{Tool: "terminal_list", Phase: "start"}); err == nil {
		t.Fatal("accepted write after close")
	}
}
