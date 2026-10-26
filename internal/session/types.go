package session

import (
	"io"
	"os/exec"
	"time"
)

// Terminal is the actual PTY/screen boundary; pipes do not implement it.
type Terminal interface {
	io.ReadWriteCloser
	SetWriteDeadline(time.Time) error
	Resize(rows, cols int) error
	ForegroundGroup() (int, error)
	Feed([]byte) ([]byte, error)
	Snapshot(maxBytes int) Screen
	Key(string) ([]byte, error)
}

type PTYStarter func(*exec.Cmd, int, int) (Terminal, error)

type Limits struct {
	ActiveSessions   int `json:"active_sessions"`
	RetainedSessions int `json:"retained_sessions"`
	RetentionSeconds int `json:"retention_seconds"`
	JournalBytes     int `json:"journal_bytes"`
	JournalEvents    int `json:"journal_events"`
	ResponseEvents   int `json:"response_events"`
	DefaultReadBytes int `json:"default_read_bytes"`
	MaxReadBytes     int `json:"max_read_bytes"`
	MaxInputBytes    int `json:"max_input_bytes"`
	MaxWaitMS        int `json:"max_wait_ms"`
	WriteTimeoutMS   int `json:"write_timeout_ms"`
	MaxOperations    int `json:"max_operations"`
	MaxRows          int `json:"max_rows"`
	MaxCols          int `json:"max_cols"`
}

func defaultLimits() Limits {
	return Limits{32, 128, 3600, 1 << 20, 2048, 256, 16 << 10, 64 << 10,
		64 << 10, 10000, 1000, 16384, 80, 200}
}

type StartArgs struct {
	ServerID     string            `json:"server_id" jsonschema:"Current server_id from terminal_list. Never substitute a new server_id when retrying an old operation."`
	OperationID  string            `json:"operation_id" jsonschema:"Unique retry key. Reuse only for the identical action."`
	Argv         []string          `json:"argv" jsonschema:"Program and arguments. No implicit shell interpretation."`
	Cwd          string            `json:"cwd,omitempty"`
	Env          map[string]string `json:"env,omitempty" jsonschema:"Overrides to inherited environment."`
	Mode         string            `json:"mode,omitempty" jsonschema:"pipes (default) or pty."`
	Rows         int               `json:"rows,omitempty"`
	Cols         int               `json:"cols,omitempty"`
	WaitMS       int               `json:"wait_ms,omitempty" jsonschema:"Observation wait only; 0 is immediate; maximum 10000."`
	MaxBytes     int               `json:"max_bytes,omitempty"`
	CloseOnExit  bool              `json:"close_on_exit,omitempty" jsonschema:"Release the session automatically after process exit and output drain; retained output remains readable. Default false."`
	WaitFor      string            `json:"wait_for,omitempty" jsonschema:"output (default) or completion; bounded by wait_ms, output limits and gaps."`
	MaxRuntimeMS int64             `json:"max_runtime_ms,omitempty" jsonschema:"Independent process lifetime limit; 0 disables it."`
}

type ReadArgs struct {
	WaitFor   string `json:"wait_for,omitempty" jsonschema:"output (default) or completion; completion requires process exit and output drain, and automatic cleanup when requested."`
	SessionID string `json:"session_id"`
	Cursor    string `json:"cursor,omitempty" jsonschema:"Opaque next_cursor from a previous read. Omit to start at byte zero."`
	View      string `json:"view,omitempty" jsonschema:"events (default) or screen (PTY only)."`
	WaitMS    int    `json:"wait_ms,omitempty" jsonschema:"Absolute observation wait; additional output never extends it."`
	MaxBytes  int    `json:"max_bytes,omitempty"`
	Until     string `json:"until,omitempty" jsonschema:"Literal substring, not a regular expression; checked only in the bounded returned window."`
}

type Input struct {
	Text        *string `json:"text,omitempty" jsonschema:"Exact text; no newline is appended."`
	BytesBase64 *string `json:"bytes_base64,omitempty"`
	Key         string  `json:"key,omitempty" jsonschema:"Named PTY key, for example ENTER, CTRL_C, UP. Exactly one input kind per item."`
}

type WriteArgs struct {
	ServerID    string  `json:"server_id"`
	OperationID string  `json:"operation_id"`
	SessionID   string  `json:"session_id"`
	Input       []Input `json:"input,omitempty"`
	CloseStdin  bool    `json:"close_stdin,omitempty" jsonschema:"Pipes only. Closes stdin after sending the supplied bytes."`
	Cursor      string  `json:"cursor,omitempty"`
	WaitMS      int     `json:"wait_ms,omitempty"`
	MaxBytes    int     `json:"max_bytes,omitempty"`
}

type SignalArgs struct {
	ServerID    string `json:"server_id"`
	OperationID string `json:"operation_id"`
	SessionID   string `json:"session_id"`
	Signal      string `json:"signal" jsonschema:"Unix: INT, TERM, KILL, HUP, CONT or STOP. Windows: TERM or KILL only."`
	Target      string `json:"target,omitempty" jsonschema:"group (default), process, or foreground (Unix PTY only). Always local."`
}

type ResizeArgs struct {
	ServerID    string `json:"server_id"`
	OperationID string `json:"operation_id"`
	SessionID   string `json:"session_id"`
	Rows        int    `json:"rows"`
	Cols        int    `json:"cols"`
}

type CloseArgs struct {
	ServerID    string `json:"server_id"`
	OperationID string `json:"operation_id"`
	SessionID   string `json:"session_id"`
	GraceMS     int    `json:"grace_ms,omitempty" jsonschema:"TERM grace before KILL; 0 means immediate escalation, maximum 2000."`
}

type Exit struct {
	Code   *int   `json:"code,omitempty"`
	Signal string `json:"signal,omitempty"`
	Error  string `json:"error,omitempty"`
}

type Info struct {
	SessionID        string   `json:"session_id"`
	Mode             string   `json:"mode"`
	PID              int      `json:"pid"`
	Argv             []string `json:"argv"`
	Cwd              string   `json:"cwd"`
	StartedAt        string   `json:"started_at"`
	ProcessState     string   `json:"process_state"`
	Exit             *Exit    `json:"exit,omitempty"`
	Closed           bool     `json:"closed"`
	CloseOnExit      bool     `json:"close_on_exit"`
	OutputDone       bool     `json:"output_done"`
	OutputIncomplete bool     `json:"output_incomplete"`
	EndCursor        string   `json:"end_cursor"`
	Diagnostics      []string `json:"diagnostics,omitempty"`
}

type Event struct {
	Stream      string  `json:"stream"`
	Offset      string  `json:"offset" jsonschema:"Absolute decimal byte position, encoded as a string."`
	Bytes       int     `json:"bytes"`
	Text        *string `json:"text,omitempty"`
	BytesBase64 string  `json:"bytes_base64,omitempty"`
}

type Gap struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type Screen struct {
	Rows          int    `json:"rows"`
	Cols          int    `json:"cols"`
	CursorX       int    `json:"cursor_x"`
	CursorY       int    `json:"cursor_y"`
	CursorVisible bool   `json:"cursor_visible"`
	Alternate     bool   `json:"alternate"`
	Text          string `json:"text"`
	Truncated     bool   `json:"truncated"`
	Revision      uint64 `json:"revision"`
	Error         string `json:"error,omitempty"`
}

type Observation struct {
	Session      Info    `json:"session"`
	Events       []Event `json:"events"`
	Screen       *Screen `json:"screen,omitempty"`
	NextCursor   string  `json:"next_cursor"`
	HasMore      bool    `json:"has_more"`
	Gap          *Gap    `json:"gap,omitempty"`
	ReturnReason string  `json:"return_reason"`
}

type Receipt struct {
	OperationID  string `json:"operation_id"`
	SessionID    string `json:"session_id,omitempty"`
	Status       string `json:"status" jsonschema:"applied, failed, partial, or unknown. Inspect this before interpreting output."`
	BytesWritten int    `json:"bytes_written"`
	BeforeCursor string `json:"before_cursor,omitempty"`
	Error        string `json:"error,omitempty"`
}

type ActionResult struct {
	Receipt          Receipt      `json:"receipt"`
	Replayed         bool         `json:"replayed"`
	Observation      *Observation `json:"observation,omitempty"`
	ObservationError string       `json:"observation_error,omitempty"`
}

// Summary deliberately excludes argv/cwd: listing sessions must not amplify large inputs.
type Summary struct {
	SessionID    string `json:"session_id"`
	Mode         string `json:"mode"`
	PID          int    `json:"pid"`
	ProcessState string `json:"process_state"`
	Closed       bool   `json:"closed"`
	OutputDone   bool   `json:"output_done"`
}

// ListArgs controls which retained sessions appear in the returned list.
type ListArgs struct {
	IncludeClosed bool `json:"include_closed,omitempty" jsonschema:"Include retained closed sessions in sessions (default false). Counts always include all retained sessions."`
}

type SessionCounts struct {
	Running        int `json:"running"`
	ExitedUnclosed int `json:"exited_unclosed"`
	ClosedRetained int `json:"closed_retained"`
}

type ListResult struct {
	Counts           SessionCounts `json:"counts"`
	Draining         bool          `json:"draining"`
	ServerID         string        `json:"server_id"`
	Limits           Limits        `json:"limits"`
	Sessions         []Summary     `json:"sessions"`
	Modes            []string      `json:"modes"`
	Keys             []string      `json:"keys"`
	OperationsUsed   int           `json:"operations_used"`
	ActiveOperations int           `json:"active_operations"`
}
