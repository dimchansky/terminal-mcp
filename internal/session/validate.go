package session

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var namedKeys = []string{"ENTER", "TAB", "BACKSPACE", "ESCAPE", "UP", "DOWN", "LEFT", "RIGHT", "HOME", "END", "PAGE_UP", "PAGE_DOWN", "INSERT", "DELETE", "SHIFT_TAB", "F1", "F2", "F3", "F4", "F5", "F6", "F7", "F8", "F9", "F10", "F11", "F12"}

func supportedKeys() []string {
	keys := append([]string(nil), namedKeys...)
	for c := 'A'; c <= 'Z'; c++ {
		keys = append(keys, "CTRL_"+string(c))
	}
	return keys
}

func validKey(k string) bool {
	for _, s := range supportedKeys() {
		if s == k {
			return true
		}
	}
	return false
}

func (m *Manager) readOptions(a ReadArgs) (ReadArgs, error) {
	if a.WaitFor == "" {
		a.WaitFor = "output"
	}
	if a.WaitFor != "output" && a.WaitFor != "completion" {
		return a, fmt.Errorf("wait_for must be output or completion")
	}
	if a.WaitFor == "completion" && a.Until != "" {
		return a, fmt.Errorf("until cannot be combined with wait_for=completion")
	}
	if a.View == "" {
		a.View = "events"
	}
	if a.View != "events" && a.View != "screen" {
		return a, fmt.Errorf("view must be events or screen")
	}
	if a.WaitMS < 0 || a.WaitMS > m.limits.MaxWaitMS {
		return a, fmt.Errorf("wait_ms must be 0..%d", m.limits.MaxWaitMS)
	}
	if a.MaxBytes == 0 {
		a.MaxBytes = m.limits.DefaultReadBytes
	}
	if a.MaxBytes < 1 || a.MaxBytes > m.limits.MaxReadBytes {
		return a, fmt.Errorf("max_bytes must be 1..%d", m.limits.MaxReadBytes)
	}
	if len(a.Until) > 1024 {
		return a, fmt.Errorf("until exceeds 1024 bytes")
	}
	if len(a.Cursor) > 160 {
		return a, fmt.Errorf("cursor too long")
	}
	return a, nil
}

func (m *Manager) normalizeStart(a StartArgs) (StartArgs, error) {
	if len(a.Argv) == 0 || len(a.Argv) > 128 || a.Argv[0] == "" {
		return a, fmt.Errorf("argv must contain a program and at most 128 strings")
	}
	total := 0
	for _, v := range a.Argv {
		total += len(v)
		if strings.ContainsRune(v, 0) {
			return a, fmt.Errorf("argv contains NUL")
		}
	}
	if total > 65536 {
		return a, fmt.Errorf("argv exceeds 64 KiB")
	}
	if a.Mode == "" {
		a.Mode = "pipes"
	}
	if a.Mode != "pipes" && a.Mode != "pty" {
		return a, fmt.Errorf("mode must be pipes or pty")
	}
	if a.Mode == "pty" {
		if m.startPTY == nil {
			return a, fmt.Errorf("PTY backend is unavailable")
		}
		if a.Rows == 0 {
			a.Rows = 24
		}
		if a.Cols == 0 {
			a.Cols = 120
		}
		if err := m.dimensions(a.Rows, a.Cols); err != nil {
			return a, err
		}
	} else if a.Rows != 0 || a.Cols != 0 {
		return a, fmt.Errorf("rows/cols require pty mode")
	}
	if a.MaxRuntimeMS < 0 || a.MaxRuntimeMS > 7*24*60*60*1000 {
		return a, fmt.Errorf("max_runtime_ms must be 0..604800000 (7 days)")
	}
	if len(a.Cwd) > 4096 || strings.ContainsRune(a.Cwd, 0) {
		return a, fmt.Errorf("invalid cwd")
	}
	if a.Cwd == "" {
		var err error
		a.Cwd, err = os.Getwd()
		if err != nil {
			return a, err
		}
	}
	var err error
	a.Cwd, err = filepath.Abs(a.Cwd)
	if err != nil {
		return a, err
	}
	if len(a.Env) > 256 {
		return a, fmt.Errorf("too many environment overrides")
	}
	total = 0
	for k, v := range a.Env {
		if k == "" || strings.ContainsAny(k, "=\x00") || strings.ContainsRune(v, 0) {
			return a, fmt.Errorf("invalid environment override")
		}
		total += len(k) + len(v)
	}
	if total > 65536 {
		return a, fmt.Errorf("environment overrides exceed 64 KiB")
	}
	options, err := m.readOptions(ReadArgs{WaitMS: a.WaitMS, MaxBytes: a.MaxBytes, WaitFor: a.WaitFor})
	a.WaitFor = options.WaitFor
	return a, err
}

func (m *Manager) dimensions(rows, cols int) error {
	if rows < 1 || rows > m.limits.MaxRows || cols < 1 || cols > m.limits.MaxCols {
		return fmt.Errorf("dimensions must be rows=1..%d, cols=1..%d", m.limits.MaxRows, m.limits.MaxCols)
	}
	return nil
}

func childEnv(overrides map[string]string, pty bool) []string {
	env := make(map[string]string)
	for _, item := range os.Environ() {
		k, v, ok := strings.Cut(item, "=")
		if ok && !strings.EqualFold(k, "TERMINAL_MCP_TOKEN") && !strings.EqualFold(k, "CONTROL_PLANE_API_KEY") {
			env[k] = v
		}
	}
	if pty {
		env["TERM"] = "xterm-256color"
	}
	for k, v := range overrides {
		env[k] = v
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, k := range keys {
		result = append(result, k+"="+env[k])
	}
	return result
}

func executable(name, cwd string, env []string) (string, error) {
	if strings.ContainsRune(name, '/') || strings.ContainsRune(name, os.PathSeparator) {
		if filepath.IsAbs(name) {
			return name, nil
		}
		return filepath.Join(cwd, name), nil
	}
	path := ""
	for _, item := range env {
		if strings.HasPrefix(item, "PATH=") {
			path = strings.TrimPrefix(item, "PATH=")
		}
	}
	for _, dir := range filepath.SplitList(path) {
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(cwd, dir)
		}
		file := filepath.Join(dir, name)
		info, err := os.Stat(file)
		if err == nil && !info.IsDir() && executableFile(file, info) {
			return file, nil
		}
		for _, suffix := range executableSuffixes(name) {
			candidate := file + suffix
			info, err := os.Stat(candidate)
			if err == nil && !info.IsDir() && executableFile(candidate, info) {
				return candidate, nil
			}
		}
	}
	return "", fmt.Errorf("executable %q not found in effective PATH", name)
}

func (m *Manager) validateInput(a WriteArgs, pty bool) error {
	if len(a.Input) > 256 {
		return fmt.Errorf("at most 256 input items are allowed")
	}
	if a.CloseStdin && pty {
		return fmt.Errorf("close_stdin is for pipes; send CTRL_D explicitly for a PTY")
	}
	total := 0
	for _, item := range a.Input {
		kinds := 0
		if item.Text != nil {
			kinds++
			total += len(*item.Text)
		}
		if item.BytesBase64 != nil {
			kinds++
			if len(*item.BytesBase64) > base64.StdEncoding.EncodedLen(m.limits.MaxInputBytes) {
				return fmt.Errorf("input exceeds limit")
			}
			b, err := base64.StdEncoding.Strict().DecodeString(*item.BytesBase64)
			if err != nil {
				return fmt.Errorf("invalid bytes_base64: %w", err)
			}
			total += len(b)
		}
		if item.Key != "" {
			kinds++
			if !pty || !validKey(item.Key) {
				return fmt.Errorf("unsupported key or non-PTY session: %q", item.Key)
			}
			total += 8
		}
		if kinds != 1 {
			return fmt.Errorf("each input item needs exactly one of text, bytes_base64, key")
		}
	}
	if total == 0 && !a.CloseStdin {
		return fmt.Errorf("input is empty")
	}
	if total > m.limits.MaxInputBytes {
		return fmt.Errorf("input exceeds %d bytes", m.limits.MaxInputBytes)
	}
	return nil
}
