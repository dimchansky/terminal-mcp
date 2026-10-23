//go:build linux || darwin

package filesystem

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"terminalmcp/internal/session"
)

func fixture(t *testing.T) (*Manager, string, string) {
	t.Helper()
	host := session.New(nil)
	t.Cleanup(host.Close)
	dir := t.TempDir()
	m, err := New(host, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	list, err := host.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return m, dir, list.ServerID
}
func put(t *testing.T, dir, name, text string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(text), 0640); err != nil {
		t.Fatal(err)
	}
	return p
}
func mustRead(t *testing.T, m *Manager, path string) FileRead {
	t.Helper()
	r, err := m.Read(context.Background(), ReadArgs{Files: []Range{{Path: path}}})
	if err != nil || len(r.Files) != 1 || r.Files[0].Error != "" {
		t.Fatalf("%+v %v", r, err)
	}
	return r.Files[0]
}
func TestReadRangesVersionsAndBudgets(t *testing.T) {
	m, dir, _ := fixture(t)
	path := put(t, dir, "a.txt", "one\r\ntwo\r\nthree")
	r, err := m.Read(context.Background(), ReadArgs{Files: []Range{{Path: path, StartLine: 2, EndLine: 3}}, MaxBytes: 5})
	if err != nil || r.Files[0].Version != version([]byte("one\r\ntwo\r\nthree")) || len(r.Files[0].Lines) != 1 || r.Files[0].Lines[0].Number != 2 || r.Files[0].Lines[0].Text != "two\r\n" || !r.Files[0].Truncated || r.Files[0].NextLine != 3 {
		t.Fatalf("%+v %v", r, err)
	}
	empty := put(t, dir, "empty", "")
	if f := mustRead(t, m, empty); len(f.Lines) != 0 {
		t.Fatal(f)
	}
	r, err = m.Read(context.Background(), ReadArgs{Files: []Range{{Path: path}, {Path: path}}, MaxBytes: 5})
	if err != nil || len(r.Files[1].Lines) != 0 || !r.Files[1].Truncated {
		t.Fatalf("budget not shared: %+v %v", r, err)
	}
}
func TestFileBoundaries(t *testing.T) {
	m, dir, _ := fixture(t)
	outside := put(t, t.TempDir(), "outside", "secret")
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	binary := put(t, dir, "binary", "\x00\xff")
	for _, path := range []string{outside, filepath.Join(dir, "escape"), filepath.Join(dir, "fifo"), binary, dir} {
		r, err := m.Read(context.Background(), ReadArgs{Files: []Range{{Path: path}}})
		if err == nil && r.Files[0].Error == "" {
			t.Fatalf("accepted %s", path)
		}
	}
}
func TestEditUniqueAtomicAndRetry(t *testing.T) {
	m, dir, id := fixture(t)
	path := put(t, dir, "file.go", "alpha\r\nbeta\r\n")
	a := EditArgs{ServerID: id, OperationID: "edit", Path: path, ExpectedVersion: mustRead(t, m, path).Version, Edits: []Edit{{"alpha", "ALPHA"}, {"beta", "BETA"}}}
	first, err := m.Edit(context.Background(), a)
	if err != nil || first.Receipt.Status != "applied" {
		t.Fatalf("%+v %v", first, err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "ALPHA\r\nBETA\r\n" {
		t.Fatalf("%q", data)
	}
	mode, _ := os.Stat(path)
	if mode.Mode().Perm() != 0640 {
		t.Fatal("permissions changed")
	}
	retry, err := m.Edit(context.Background(), a)
	if err != nil || !retry.Replayed || retry.Receipt.Version != first.Receipt.Version {
		t.Fatalf("%+v %v", retry, err)
	}
	a.OperationID = "stale"
	r, err := m.Edit(context.Background(), a)
	if err != nil || r.Receipt.Status != "failed" || !strings.Contains(r.Receipt.Error, "version_conflict") {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err = m.host.Start(context.Background(), session.StartArgs{ServerID: id, OperationID: "edit", Argv: []string{"/bin/true"}}); err == nil {
		t.Fatal("file and terminal ledgers do not share retry identity")
	}
	a.ServerID = "old"
	if _, err = m.Edit(context.Background(), a); err == nil {
		t.Fatal("accepted obsolete server")
	}
}
func TestEditRejectsAmbiguousAndOverlappingChanges(t *testing.T) {
	for _, edits := range [][]Edit{{{"aa", "x"}}, {{"aaa", "x"}, {"aa", "y"}}, {{"aaa", "x"}, {"absent", "y"}}} {
		if _, _, _, err := applyEdits([]byte("aaa"), edits); err == nil {
			t.Fatal("accepted ambiguous/invalid edits")
		}
	}
	m, dir, id := fixture(t)
	path := put(t, dir, "file", "hello")
	r, err := m.Edit(context.Background(), EditArgs{ServerID: id, OperationID: "bad", Path: path, ExpectedVersion: mustRead(t, m, path).Version, Edits: []Edit{{"hello", "bye"}, {"absent", "x"}}})
	data, _ := os.ReadFile(path)
	if err != nil || r.Receipt.Status != "failed" || string(data) != "hello" {
		t.Fatalf("partial edit: %+v %v %q", r, err, data)
	}
}
func TestWriteCreationAndConcurrentNoClobber(t *testing.T) {
	m, dir, id := fixture(t)
	path := filepath.Join(dir, "new")
	var wg sync.WaitGroup
	results := make(chan ActionResult, 2)
	for _, key := range []string{"one", "two"} {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			r, e := m.Write(context.Background(), WriteArgs{ServerID: id, OperationID: key, Path: path, Content: key})
			if e != nil {
				t.Error(e)
			}
			results <- r
		}(key)
	}
	wg.Wait()
	close(results)
	applied := 0
	for r := range results {
		if r.Receipt.Status == "applied" {
			applied++
		}
	}
	if applied != 1 {
		t.Fatalf("got %d creations", applied)
	}
	old := mustRead(t, m, path)
	r, err := m.Write(context.Background(), WriteArgs{ServerID: id, OperationID: "replace", Path: path, ExpectedVersion: old.Version, Content: "new contents"})
	if err != nil || r.Receipt.Status != "applied" {
		t.Fatalf("%+v %v", r, err)
	}
	if r, err = m.Write(context.Background(), WriteArgs{ServerID: id, OperationID: "missing", Path: filepath.Join(dir, "missing-parent", "x"), Content: "x"}); err != nil || r.Receipt.Status != "failed" {
		t.Fatalf("%+v %v", r, err)
	}
}
func TestEditRefusesSymlinkHardlinkAndReadonlyTargets(t *testing.T) {
	m, dir, id := fixture(t)
	target := put(t, dir, "target", "hello")
	os.Symlink(target, filepath.Join(dir, "link"))
	os.Link(target, filepath.Join(dir, "hard"))
	for i, name := range []string{"link", "hard"} {
		r, e := m.Write(context.Background(), WriteArgs{ServerID: id, OperationID: []string{"link", "hard"}[i], Path: filepath.Join(dir, name), ExpectedVersion: version([]byte("hello")), Content: "bye"})
		if e != nil || r.Receipt.Status != "failed" {
			t.Fatalf("%+v %v", r, e)
		}
	}
	ro := put(t, dir, "ro", "hello")
	os.Chmod(ro, 0440)
	r, e := m.Write(context.Background(), WriteArgs{ServerID: id, OperationID: "ro", Path: ro, ExpectedVersion: version([]byte("hello")), Content: "bye"})
	if e != nil || r.Receipt.Status != "failed" {
		t.Fatalf("%+v %v", r, e)
	}
}
func TestListAndSearch(t *testing.T) {
	m, dir, _ := fixture(t)
	put(t, dir, "a.go", "first\nneedle here\nneedle twice\n")
	put(t, dir, "b.txt", "needle too\n")
	list, err := m.List(context.Background(), ListArgs{Path: dir, Glob: "*.go"})
	if err != nil || len(list.Entries) != 1 || list.Entries[0].Name != "a.go" {
		t.Fatalf("%+v %v", list, err)
	}
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep not installed")
	}
	result, err := m.Search(context.Background(), SearchArgs{Path: dir, Pattern: "needle", Glob: "*.go", MaxResults: 1})
	if err != nil || len(result.Matches) != 1 || result.Matches[0].Line != 2 || !result.Truncated {
		t.Fatalf("%+v %v", result, err)
	}
	result, err = m.Search(context.Background(), SearchArgs{Path: dir, Pattern: "absent"})
	if err != nil || len(result.Matches) != 0 || result.Truncated {
		t.Fatalf("%+v %v", result, err)
	}
	if _, err = m.Search(context.Background(), SearchArgs{Path: dir, Pattern: "[", Regex: true}); err == nil {
		t.Fatal("invalid regex accepted")
	}
}
