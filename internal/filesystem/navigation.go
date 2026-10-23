//go:build linux || darwin

package filesystem

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

func resultLimit(n, defaultN, maxN int) (int, error) {
	if n == 0 {
		n = defaultN
	}
	if n < 1 || n > maxN {
		return 0, fmt.Errorf("limit must be 1..%d", maxN)
	}
	return n, nil
}
func (m *Manager) List(ctx context.Context, a ListArgs) (ListResult, error) {
	out := ListResult{Path: a.Path, Entries: []Entry{}}
	rel, err := m.relative(a.Path)
	if err != nil {
		return out, err
	}
	limit, err := resultLimit(a.Limit, 200, 1000)
	if err != nil {
		return out, err
	}
	if len(a.Glob) > 512 {
		return out, fmt.Errorf("glob too long")
	}
	if _, err = filepath.Match(a.Glob, ""); err != nil {
		return out, err
	}
	dir, err := m.root.OpenRoot(rel)
	if err != nil {
		return out, err
	}
	defer dir.Close()
	f, err := dir.Open(".")
	if err != nil {
		return out, err
	}
	defer f.Close()
	for {
		if err = ctx.Err(); err != nil {
			return out, err
		}
		entries, e := f.ReadDir(128)
		for _, entry := range entries {
			if a.Glob != "" {
				match, _ := filepath.Match(a.Glob, entry.Name())
				if !match {
					continue
				}
			}
			if len(out.Entries) == limit {
				out.Truncated = true
				break
			}
			kind := "file"
			if entry.IsDir() {
				kind = "directory"
			} else if entry.Type()&os.ModeSymlink != 0 {
				kind = "symlink"
			} else if !entry.Type().IsRegular() {
				kind = "special"
			}
			out.Entries = append(out.Entries, Entry{Name: entry.Name(), Type: kind})
		}
		if out.Truncated || errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			return out, e
		}
	}
	sort.Slice(out.Entries, func(i, j int) bool { return out.Entries[i].Name < out.Entries[j].Name })
	return out, nil
}

type cappedWriter struct{ b strings.Builder }

func (w *cappedWriter) Write(p []byte) (int, error) {
	n := len(p)
	left := 2048 - w.b.Len()
	if left > 0 {
		w.b.Write(p[:min(left, n)])
	}
	return n, nil
}
func splitNUL(data []byte, atEOF bool) (int, []byte, error) {
	if i := strings.IndexByte(string(data), 0); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func (m *Manager) Search(ctx context.Context, a SearchArgs) (SearchResult, error) {
	out := SearchResult{Matches: []Match{}, Reason: "completed"}
	rel, err := m.relative(a.Path)
	if err != nil {
		return out, err
	}
	limit, err := resultLimit(a.MaxResults, 100, 500)
	if err != nil {
		return out, err
	}
	if a.Pattern == "" || len(a.Pattern) > 1024 || len(a.Glob) > 512 || strings.IndexByte(a.Glob, 0) >= 0 {
		return out, fmt.Errorf("pattern must be 1..1024 bytes; glob at most 512 without NUL")
	}
	var re *regexp.Regexp
	if a.Regex {
		re, err = regexp.Compile(a.Pattern)
		if err != nil {
			return out, err
		}
	}
	dir, err := m.root.OpenRoot(rel)
	if err != nil {
		return out, err
	}
	dir.Close()
	rg, err := exec.LookPath("rg")
	if err != nil {
		return out, fmt.Errorf("ripgrep (rg) is required for ignore-aware file discovery; install it or use terminal tools")
	}
	timeout, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	args := []string{"--files", "--null", "--no-config", "--glob", "!.git/**"}
	if a.Hidden {
		args = append(args, "--hidden")
	}
	if a.Glob != "" {
		args = append(args, "--glob", a.Glob)
	}
	args = append(args, "--", ".")
	cmd := exec.CommandContext(timeout, rg, args...)
	cmd.Dir = filepath.Join(m.path, rel)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return out, err
	}
	var stderr cappedWriter
	cmd.Stderr = &stderr
	if err = cmd.Start(); err != nil {
		return out, err
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Split(splitNUL)
	scanner.Buffer(make([]byte, 4096), 8192)
	budget := maxContent
	stopped := false
	skipped := 0
	for scanner.Scan() {
		if timeout.Err() != nil {
			break
		}
		name := scanner.Text()
		if !filepath.IsLocal(name) {
			continue
		}
		// ripgrep selects paths only; content access still goes through os.Root, not rg.
		child := filepath.Join(rel, name)
		data, _, e := readText(m.root, child)
		if e != nil {
			skipped++
			continue
		}
		for i, line := range strings.Split(string(data), "\n") {
			matches := strings.Contains(line, a.Pattern)
			if re != nil {
				matches = re.MatchString(line)
			}
			if !matches {
				continue
			}
			truncated := len(line) > 2048
			if truncated {
				line = line[:2048]
				for !utf8.ValidString(line) {
					line = line[:len(line)-1]
				}
			}
			path := filepath.Join(m.path, child)
			if len(out.Matches) >= limit || len(path)+len(line) > budget {
				out.Truncated = true
				out.Reason = "limit"
				stopped = true
				break
			}
			budget -= len(path) + len(line)
			out.Matches = append(out.Matches, Match{Path: path, Line: i + 1, Text: line, Truncated: truncated})
		}
		if stopped {
			break
		}
	}
	scanErr := scanner.Err()
	if stopped || scanErr != nil || timeout.Err() != nil {
		cancel()
	}
	_ = stdout.Close()
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return out, ctx.Err()
	}
	if stopped {
		return out, nil
	}
	if errors.Is(timeout.Err(), context.DeadlineExceeded) {
		out.Truncated = true
		out.Reason = "deadline"
		return out, nil
	}
	if scanErr != nil {
		return out, scanErr
	}
	if waitErr != nil {
		var ex *exec.ExitError
		if !errors.As(waitErr, &ex) || ex.ExitCode() != 1 {
			return out, fmt.Errorf("ripgrep discovery failed: %s", stderr.b.String())
		}
	}
	if skipped > 0 {
		out.Reason = fmt.Sprintf("completed; skipped %d non-text, oversized, changed or unreadable files", skipped)
	}
	return out, nil
}
