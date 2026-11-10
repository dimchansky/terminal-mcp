// Package activity correlates owner audit records into observed conversations
// and action lifecycles for the local activity dashboard. It is a pure
// projection: it does not import internal/audit, perform I/O, or retain raw
// record bytes. Complete records stay in the journal and are referenced by
// their reader positions.
package activity

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// currentSchemaVersion mirrors audit.EventSchemaVersion. In this schema an
// omitted duration_ms on a finish whose start is known means "under 1 ms".
const currentSchemaVersion = 2

// Kind is the lifecycle role of a decoded audit record.
type Kind uint8

const (
	KindUnknown Kind = iota
	KindRequestStart
	KindRequestFinish
	KindRequestRejected
	KindOperationStart
	KindOperationFinish
	KindServerStart
)

var (
	// ErrMalformedRecord means the record is not a single JSON object.
	ErrMalformedRecord = errors.New("audit record is not a single JSON object")
	// ErrDuplicateField means a correlation field occurs more than once in a
	// record. encoding/json would keep the last value; the reducer must not
	// guess, so the record is reported as a diagnostic instead.
	ErrDuplicateField = errors.New("audit record repeats a correlation field")
	// ErrFieldType means a correlation field has an unexpected JSON type or
	// a string that cannot be represented exactly.
	ErrFieldType = errors.New("audit record has an invalid correlation field")
	// ErrMissingPhase means the record has no phase and cannot be correlated.
	ErrMissingPhase = errors.New("audit record has no phase")
)

// Event holds the correlation fields of one audit JSONL record. Tool
// arguments and other inputs are intentionally not decoded: the owner sees
// them from the original journal bytes, not from this projection.
type Event struct {
	SchemaVersion int // 0 when absent (legacy records).
	Kind          Kind
	Phase         string // Exact recorded phase.
	Legacy        bool   // Unversioned start/finish/rejected record.

	Time        string
	ServerID    string // Empty for legacy records without a server ID.
	RequestID   string
	OperationID string
	SessionID   string
	Tool        string

	Status       string
	Outcome      string
	Reason       string
	RequestState string
	Replayed     bool

	DurationMS  int64
	HasDuration bool

	// Identity is normalized from call_meta. request_start records carry
	// call_meta, and so may request_rejected ones; for other records it is
	// the zero value.
	Identity Identity
}

// correlationFields is ordered so that diagnostics name the same field on
// every run when several fields are invalid.
var correlationFields = []string{
	"phase", "schema_version", "time", "server_id", "request_id",
	"operation_id", "session_id", "tool", "status", "outcome", "reason",
	"request_state", "replayed", "duration_ms", "call_meta",
}

func isCorrelationField(key string) bool {
	for _, name := range correlationFields {
		if key == name {
			return true
		}
	}
	return false
}

// Decode extracts correlation fields from one complete JSONL record (a
// trailing newline is allowed). It fails rather than guessing when the record
// is not a JSON object, repeats or mistypes a correlation field, or has no
// phase; such records remain visible as journal diagnostics.
func Decode(raw []byte) (Event, error) {
	raw = bytes.TrimSuffix(raw, []byte{'\n'})
	fields, duplicates, err := objectFields(raw, isCorrelationField)
	if err != nil {
		return Event{}, fmt.Errorf("%w: %v", ErrMalformedRecord, err)
	}
	if len(duplicates) > 0 {
		return Event{}, fmt.Errorf("%w: %q", ErrDuplicateField, duplicates[0])
	}
	if _, ok := fields["phase"]; !ok {
		return Event{}, ErrMissingPhase
	}

	var e Event
	for _, name := range correlationFields {
		value, ok := fields[name]
		if !ok {
			continue
		}
		if err := e.set(name, value); err != nil {
			return Event{}, fmt.Errorf("%w: %q", ErrFieldType, name)
		}
	}
	e.Kind, e.Legacy = classify(e.Phase, e.SchemaVersion)
	return e, nil
}

var errInvalidValue = errors.New("invalid value")

func (e *Event) set(name string, value json.RawMessage) error {
	switch name {
	case "schema_version":
		n, err := strconv.ParseInt(string(value), 10, 32)
		if err != nil || n < 0 {
			return errInvalidValue
		}
		e.SchemaVersion = int(n)
	case "duration_ms":
		n, err := strconv.ParseInt(string(value), 10, 64)
		if err != nil {
			return errInvalidValue
		}
		e.DurationMS, e.HasDuration = n, true
	case "replayed":
		switch string(value) {
		case "true":
			e.Replayed = true
		case "false":
		default:
			return errInvalidValue
		}
	case "call_meta":
		e.Identity = identityFromCallMeta(value)
	default:
		s, ok := exactString(value)
		if !ok {
			return errInvalidValue
		}
		*e.stringField(name) = s
	}
	return nil
}

func (e *Event) stringField(name string) *string {
	switch name {
	case "phase":
		return &e.Phase
	case "time":
		return &e.Time
	case "server_id":
		return &e.ServerID
	case "request_id":
		return &e.RequestID
	case "operation_id":
		return &e.OperationID
	case "session_id":
		return &e.SessionID
	case "tool":
		return &e.Tool
	case "status":
		return &e.Status
	case "outcome":
		return &e.Outcome
	case "reason":
		return &e.Reason
	case "request_state":
		return &e.RequestState
	}
	panic("activity: unknown string field " + name)
}

func classify(phase string, schemaVersion int) (Kind, bool) {
	switch phase {
	case "request_start":
		return KindRequestStart, false
	case "request_finish":
		return KindRequestFinish, false
	case "request_rejected":
		return KindRequestRejected, false
	case "operation_start":
		return KindOperationStart, false
	case "operation_finish":
		return KindOperationFinish, false
	case "server_start":
		return KindServerStart, false
	}
	if schemaVersion == 0 {
		switch phase {
		case "start":
			return KindRequestStart, true
		case "finish":
			return KindRequestFinish, true
		case "rejected":
			return KindRequestRejected, true
		}
	}
	return KindUnknown, false
}
