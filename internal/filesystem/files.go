//go:build linux || darwin

package filesystem

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"unicode/utf8"

	"terminalmcp/internal/mutation"
	"terminalmcp/internal/session"
)

const maxFile = 2 << 20
const maxContent = 64 << 10

// MaxReadBytes limits read output independently of the 64 KiB write budget.
const MaxReadBytes = 256 << 10

type Manager struct {
	mu     sync.Mutex
	host   *session.Manager
	root   *os.Root
	path   string
	alias  string
	closed bool
}

func New(host *session.Manager, path string) (*Manager, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	physical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(physical)
	if err != nil {
		return nil, err
	}
	return &Manager{host: host, root: root, path: physical, alias: abs}, nil
}
func (m *Manager) Root() string { return m.path }
func (m *Manager) Close()       { m.mu.Lock(); defer m.mu.Unlock(); m.closed = true; _ = m.root.Close() }

func (m *Manager) relative(path string) (string, error) {
	if len(path) == 0 || len(path) > 4096 || strings.IndexByte(path, 0) >= 0 || !filepath.IsAbs(path) {
		return "", fmt.Errorf("path must be absolute, without NUL, at most 4096 bytes")
	}
	for _, base := range []string{m.path, m.alias} {
		rel, err := filepath.Rel(base, path)
		if err == nil && (rel == "." || filepath.IsLocal(rel)) {
			return rel, nil
		}
	}
	return "", fmt.Errorf("path is outside the configured file root")
}
func version(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
func validVersion(v string) bool {
	if len(v) != 71 || !strings.HasPrefix(v, "sha256:") {
		return false
	}
	b, err := hex.DecodeString(v[7:])
	return err == nil && len(b) == 32
}
func textOK(data []byte) bool { return utf8.Valid(data) && !strings.ContainsRune(string(data), 0) }

func readText(root *os.Root, name string) ([]byte, os.FileInfo, error) {
	// Nonblocking open prevents an accidentally selected FIFO from hanging the server.
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }() // Read-only descriptor.
	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxFile {
		return nil, nil, fmt.Errorf("requires a regular UTF-8 file at most 2 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil {
		return nil, nil, err
	}
	after, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if info.Size() != after.Size() || !info.ModTime().Equal(after.ModTime()) {
		return nil, nil, fmt.Errorf("file_changed_during_read")
	}
	if len(data) > maxFile || !textOK(data) {
		return nil, nil, fmt.Errorf("requires UTF-8 text without NUL at most 2 MiB")
	}
	return data, info, nil
}

func (m *Manager) Read(ctx context.Context, a ReadArgs) (ReadResult, error) {
	out := ReadResult{Root: m.path, Files: []FileRead{}}
	if len(a.Files) < 1 || len(a.Files) > 8 {
		return out, fmt.Errorf("files must have 1..8 ranges")
	}
	if a.MaxBytes == 0 {
		a.MaxBytes = 16384
	}
	if a.MaxBytes < 1 || a.MaxBytes > MaxReadBytes {
		return out, fmt.Errorf("max_bytes must be 1..%d", MaxReadBytes)
	}
	budget := a.MaxBytes
	for _, item := range a.Files {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		r := FileRead{Path: item.Path, Lines: []Line{}}
		rel, err := m.relative(item.Path)
		if err != nil {
			r.Error = err.Error()
			out.Files = append(out.Files, r)
			continue
		}
		start, end := item.StartLine, item.EndLine
		if start == 0 {
			start = 1
		}
		if end == 0 {
			end = start + 199
		}
		if start < 1 || end < start || end-start >= 1000 {
			r.Error = "invalid line range (maximum 1000 lines)"
			out.Files = append(out.Files, r)
			continue
		}
		data, _, err := readText(m.root, rel)
		if err != nil {
			r.Error = err.Error()
			out.Files = append(out.Files, r)
			continue
		}
		r.Version = version(data)
		lines := strings.SplitAfter(string(data), "\n")
		if lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		r.TotalLines = len(lines)
		if start > len(lines)+1 {
			r.Error = "start_line is beyond EOF"
			out.Files = append(out.Files, r)
			continue
		}
		end = min(end, len(lines))
		for i := start; i <= end; i++ {
			if len(lines[i-1]) > budget {
				r.Truncated = true
				r.NextLine = i
				break
			}
			r.Lines = append(r.Lines, Line{Number: i, Text: lines[i-1]})
			budget -= len(lines[i-1])
		}
		if r.NextLine == 0 && end < len(lines) {
			r.NextLine = end + 1
		}
		if r.Truncated && len(r.Lines) == 0 {
			r.Error = "next line exceeds remaining response budget; increase max_bytes or use a narrower read"
		}
		out.Files = append(out.Files, r)
	}
	return out, nil
}

func (m *Manager) mutate(ctx context.Context, server, id, method string, args any, fn func() (FileReceipt, error)) (ActionResult, error) {
	r, replayed, err := m.host.Mutate(ctx, server, id, method, args, func() mutation.Receipt {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.closed {
			return mutation.Receipt{Status: "failed", Error: "file service is closed"}
		}
		receipt, e := fn()
		status := "applied"
		message := ""
		if e != nil {
			status, message = "failed", e.Error()
		}
		data, e := json.Marshal(receipt)
		if e != nil {
			panic(e)
		}
		return mutation.Receipt{Status: status, Error: message, Data: data}
	})
	out := ActionResult{Replayed: replayed}
	if len(r.Data) > 0 {
		if e := json.Unmarshal(r.Data, &out.Receipt); e != nil {
			return out, e
		}
	}
	out.Receipt.OperationID, out.Receipt.Status, out.Receipt.Error = r.OperationID, r.Status, r.Error
	return out, err
}

func (m *Manager) Write(ctx context.Context, a WriteArgs) (ActionResult, error) {
	rel, err := m.relative(a.Path)
	if err != nil {
		return ActionResult{}, err
	}
	if len(a.Content) > maxContent || !textOK([]byte(a.Content)) {
		return ActionResult{}, fmt.Errorf("content must be UTF-8 without NUL, at most 64 KiB")
	}
	if a.ExpectedVersion != "" && !validVersion(a.ExpectedVersion) {
		return ActionResult{}, fmt.Errorf("invalid expected_version")
	}
	effect := a
	effect.ServerID, effect.OperationID = "", ""
	return m.mutate(ctx, a.ServerID, a.OperationID, "file_write", effect, func() (FileReceipt, error) {
		return m.replace(rel, a.Path, a.ExpectedVersion, func([]byte) ([]byte, int, string, error) { return []byte(a.Content), 0, "", nil })
	})
}
func (m *Manager) Edit(ctx context.Context, a EditArgs) (ActionResult, error) {
	rel, err := m.relative(a.Path)
	if err != nil {
		return ActionResult{}, err
	}
	if !validVersion(a.ExpectedVersion) {
		return ActionResult{}, fmt.Errorf("expected_version is required")
	}
	if len(a.Edits) < 1 || len(a.Edits) > 32 {
		return ActionResult{}, fmt.Errorf("edits must have 1..32 replacements")
	}
	total := 0
	for _, e := range a.Edits {
		total += len(e.OldText) + len(e.NewText)
		if e.OldText == "" || !textOK([]byte(e.OldText+e.NewText)) {
			return ActionResult{}, fmt.Errorf("edits require nonempty old_text and UTF-8 text without NUL")
		}
	}
	if total > maxContent {
		return ActionResult{}, fmt.Errorf("edit input exceeds 64 KiB")
	}
	effect := a
	effect.ServerID, effect.OperationID = "", ""
	return m.mutate(ctx, a.ServerID, a.OperationID, "file_edit", effect, func() (FileReceipt, error) {
		return m.replace(rel, a.Path, a.ExpectedVersion, func(data []byte) ([]byte, int, string, error) { return applyEdits(data, a.Edits) })
	})
}
func applyEdits(data []byte, edits []Edit) ([]byte, int, string, error) {
	type span struct {
		start, end int
		new        string
		old        string
		index      int
	}
	source := string(data)
	spans := []span{}
	for i, e := range edits {
		start := strings.Index(source, e.OldText)
		matches := 0
		if start >= 0 {
			matches = 1
			for offset := start + 1; offset < len(source); {
				next := strings.Index(source[offset:], e.OldText)
				if next < 0 {
					break
				}
				matches++
				offset += next + 1
			}
		}
		if matches == 0 {
			return nil, 0, "", fmt.Errorf("edit[%d]: old_text not found; read the file again", i+1)
		}
		if matches != 1 {
			return nil, 0, "", fmt.Errorf("edit[%d]: old_text matches %d locations; use a longer unique fragment", i+1, matches)
		}
		spans = append(spans, span{start: start, end: start + len(e.OldText), new: e.NewText, old: e.OldText, index: i + 1})
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	var b, preview strings.Builder
	pos := 0
	previous := 0
	for _, s := range spans {
		if s.start < pos {
			return nil, 0, "", fmt.Errorf("edit[%d] overlaps edit[%d]", s.index, previous)
		}
		previous = s.index
		b.WriteString(source[pos:s.start])
		b.WriteString(s.new)
		pos = s.end
		preview.WriteString("- " + s.old + "\n+ " + s.new + "\n")
	}
	b.WriteString(source[pos:])
	result := []byte(b.String())
	if len(result) > maxFile {
		return nil, 0, "", fmt.Errorf("edited file exceeds 2 MiB")
	}
	p := preview.String()
	if len(p) > 4096 {
		p = strings.ToValidUTF8(p[:4096], "�") + "\n[preview truncated]"
	}
	return result, len(edits), p, nil
}

// verifySnapshot checks that a file still contains the version used to
// compute a replacement. Another process can still write after this check.
func verifySnapshot(parent *os.Root, name string, original os.FileInfo, expected string) error {
	current, readInfo, readErr := readText(parent, name)
	if readErr != nil {
		return fmt.Errorf("version_conflict: cannot reread file: %w", readErr)
	}
	leaf, statErr := parent.Lstat(name)
	if statErr != nil || !leaf.Mode().IsRegular() || !os.SameFile(original, readInfo) ||
		!os.SameFile(readInfo, leaf) || original.Mode() != readInfo.Mode() || version(current) != expected {
		return fmt.Errorf("version_conflict: file changed before replacement")
	}
	return nil
}

func (m *Manager) replace(rel, path, expected string, transform func([]byte) ([]byte, int, string, error)) (FileReceipt, error) {
	result := FileReceipt{Path: path}
	parent, err := m.root.OpenRoot(filepath.Dir(rel))
	if err != nil {
		return result, err
	}
	defer func() { _ = parent.Close() }() // Directory descriptor.
	name := filepath.Base(rel)
	info, err := parent.Lstat(name)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return result, err
	}
	var before []byte
	mode := fs.FileMode(0600)
	if exists {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0222 == 0 {
			return result, fmt.Errorf("refusing symlink, special, or read-only file")
		}
		if st, ok := info.Sys().(*syscall.Stat_t); !ok || st.Nlink != 1 || int(st.Uid) != os.Geteuid() {
			return result, fmt.Errorf("replacement requires a single-link file owned by the service user")
		}
		if expected == "" {
			return result, fmt.Errorf("file_exists: expected_version required to overwrite")
		}
		var readInfo os.FileInfo
		before, readInfo, err = readText(parent, name)
		if err != nil {
			return result, err
		}
		if !os.SameFile(info, readInfo) {
			return result, fmt.Errorf("version_conflict: file changed")
		}
		result.BeforeVersion = version(before)
		if expected != result.BeforeVersion {
			return result, fmt.Errorf("version_conflict: reread the local file")
		}
		mode = info.Mode().Perm()
	} else if expected != "" {
		return result, fmt.Errorf("version_conflict: file does not exist")
	}
	after, count, preview, err := transform(before)
	if err != nil {
		return result, err
	}
	result.Version, result.Bytes, result.Edits, result.Preview = version(after), len(after), count, preview
	if exists && string(before) == string(after) {
		return result, verifySnapshot(parent, name, info, expected)
	}
	var id [16]byte
	if _, err = rand.Read(id[:]); err != nil {
		return result, err
	}
	temp := ".terminal-mcp-" + hex.EncodeToString(id[:]) + ".tmp"
	f, err := parent.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return result, err
	}
	defer func() {
		if err := parent.Remove(temp); err != nil && !errors.Is(err, fs.ErrNotExist) {
			log.Printf("filesystem: clean up temporary file: %v", err)
		}
	}()
	if _, err = f.Write(after); err == nil {
		err = f.Chmod(mode)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return result, err
	}
	if exists {
		if err := verifySnapshot(parent, name, info, expected); err != nil {
			return result, err
		}
		// External writers can race the last check; use separate worktrees for concurrent editors.
		err = parent.Rename(temp, name)
	} else {
		// Link publishes a complete inode without overwriting a concurrently created destination.
		err = parent.Link(temp, name)
	}
	return result, err
}
