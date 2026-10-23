//go:build linux || darwin

package filesystem

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSearchDoesNotReportPhantomLines(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep not installed")
	}
	tests := []struct {
		name string
		text string
		want []int
	}{
		{"empty file", "", nil},
		{"final newline", "first\n", nil},
		{"actual empty line", "first\n\n", []int{2}},
		{"empty first line", "\nlast", []int{1}},
		{"no trailing newline", "first\nlast", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, dir, _ := fixture(t)
			put(t, dir, "test.txt", tt.text)
			result, err := m.Search(context.Background(), SearchArgs{Path: dir, Pattern: "^$", Regex: true, Glob: "*.txt"})
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Matches) != len(tt.want) {
				t.Fatalf("got %d matches, want %d: %+v", len(result.Matches), len(tt.want), result)
			}
			for i, match := range result.Matches {
				if match.Line != tt.want[i] || match.Text != "" {
					t.Fatalf("match %d = %+v, want line %d", i, match, tt.want[i])
				}
			}
		})
	}
}

func TestListAndSearchReportInvalidPathAndInput(t *testing.T) {
	m, dir, _ := fixture(t)
	outside := t.TempDir()
	_, err := m.List(context.Background(), ListArgs{Path: outside})
	if err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("list outside root: %v", err)
	}
	_, err = m.Search(context.Background(), SearchArgs{Path: outside, Pattern: "x"})
	if err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("search outside root: %v", err)
	}
	if _, err := m.List(context.Background(), ListArgs{Path: dir, Glob: "["}); err == nil {
		t.Fatal("invalid glob was accepted")
	}
	if _, err := m.Search(context.Background(), SearchArgs{Path: dir, Pattern: "[", Regex: true}); err == nil {
		t.Fatal("invalid regular expression was accepted")
	}
	if _, err := m.Search(context.Background(), SearchArgs{Path: dir, Pattern: ""}); err == nil {
		t.Fatal("empty pattern was accepted")
	}
}

func TestNavigationRejectsSymlinkDirectoryOutsideRoot(t *testing.T) {
	m, dir, _ := fixture(t)
	outside := t.TempDir()
	put(t, outside, "secret.txt", "secret")
	link := filepath.Join(dir, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := m.List(context.Background(), ListArgs{Path: link}); err == nil {
		t.Fatal("file_list followed symlink outside root")
	}
	if _, err := m.Search(context.Background(), SearchArgs{Path: link, Pattern: "secret"}); err == nil {
		t.Fatal("file_search followed symlink outside root")
	}
}

func TestFileManagerCloseMakesResourcesUnavailable(t *testing.T) {
	m, dir, id := fixture(t)
	path := put(t, dir, "a.txt", "example")
	m.Close()
	// Read reports failures for each path instead of failing the whole batch.
	result, err := m.Read(context.Background(), ReadArgs{Files: []Range{{Path: path}}})
	if err != nil || len(result.Files) != 1 || result.Files[0].Error == "" {
		t.Fatalf("closed manager allowed file read: %+v, %v", result, err)
	}
	if _, err := m.List(context.Background(), ListArgs{Path: dir}); err == nil {
		t.Fatal("file_list succeeded after close")
	}
	if _, err := m.Search(context.Background(), SearchArgs{Path: dir, Pattern: "example"}); err == nil {
		t.Fatal("file_search succeeded after close")
	}
	action, err := m.Write(context.Background(), WriteArgs{
		ServerID: id, OperationID: "after-close", Path: filepath.Join(dir, "new.txt"), Content: "new",
	})
	if err != nil || action.Receipt.Status != "failed" {
		t.Fatalf("closed manager accepted file mutation: %+v, %v", action, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("file was created after manager close: %v", err)
	}
}

func TestReadDetectsExternalRenameBeforeEdit(t *testing.T) {
	m, dir, id := fixture(t)
	path := put(t, dir, "source.txt", "first")
	original := mustRead(t, m, path).Version
	replacement := filepath.Join(dir, "new-source.txt")
	if err := os.WriteFile(replacement, []byte("external"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	result, err := m.Edit(context.Background(), EditArgs{
		ServerID: id, OperationID: "external-replace", Path: path,
		ExpectedVersion: original, Edits: []Edit{{OldText: "first", NewText: "second"}},
	})
	if err != nil || result.Receipt.Status != "failed" || !strings.Contains(result.Receipt.Error, "version_conflict") {
		t.Fatalf("external replacement was not rejected: %+v, %v", result, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "external" {
		t.Fatalf("external data changed: %q, %v", data, err)
	}
}
