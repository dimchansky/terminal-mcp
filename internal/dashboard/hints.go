package dashboard

import (
	"encoding/json"
	"strings"
	"sync"
	"unicode/utf8"

	"terminalmcp/internal/activity"
)

// hintRunes bounds the compact summary of an action's input. The summary
// is a navigation hint for compact views, never the owner-visible input.
const hintRunes = 160

// Hint is a bounded, possibly truncated summary of a request's input.
type Hint struct {
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`
}

// hints are computed when a request_start is applied, because the
// projection keeps no arguments.
type hints struct {
	mu sync.Mutex
	m  map[activity.RequestKey]Hint
}

func newHints() *hints { return &hints{m: make(map[activity.RequestKey]Hint)} }

func (h *hints) reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	clear(h.m)
}

func (h *hints) get(key activity.RequestKey) (Hint, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v, ok := h.m[key]
	return v, ok
}

// prune drops hints of actions the window no longer holds.
func (h *hints) prune(keep func(activity.RequestKey) bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for k := range h.m {
		if !keep(k) {
			delete(h.m, k)
		}
	}
}

func (h *hints) observe(_ activity.OccurrenceKey, raw []byte, change activity.Change) {
	if change.Request == nil {
		return
	}
	var rec struct {
		Phase string          `json:"phase"`
		Args  json.RawMessage `json:"args"`
	}
	if json.Unmarshal(raw, &rec) != nil || (rec.Phase != "request_start" && rec.Phase != "start") {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.m[*change.Request]; !ok {
		h.m[*change.Request] = summarize(rec.Args)
	}
}

// summarize picks the most recognizable input: a command line, a path or a
// URL, else the compact arguments. It is never shell-quoted.
func summarize(args json.RawMessage) Hint {
	var fields map[string]json.RawMessage
	if json.Unmarshal(args, &fields) != nil {
		return truncate(string(args))
	}
	var argv []string
	if json.Unmarshal(fields["argv"], &argv) == nil && len(argv) > 0 {
		return truncate(strings.Join(argv, " "))
	}
	for _, name := range []string{"path", "url"} {
		var s string
		if json.Unmarshal(fields[name], &s) == nil && s != "" {
			return truncate(s)
		}
	}
	var paths []string
	if json.Unmarshal(fields["paths"], &paths) == nil && len(paths) > 0 {
		return truncate(strings.Join(paths, " "))
	}
	return truncate(string(args))
}

func truncate(s string) Hint {
	if utf8.RuneCountInString(s) <= hintRunes {
		return Hint{Text: s}
	}
	n := 0
	for i := range s {
		if n == hintRunes {
			return Hint{Text: s[:i], Truncated: true}
		}
		n++
	}
	return Hint{Text: s}
}
