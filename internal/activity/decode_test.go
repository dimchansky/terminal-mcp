package activity

import (
	"errors"
	"testing"
)

func TestDecodeClassifiesRecordedPhases(t *testing.T) {
	tests := []struct {
		name   string
		raw    string
		kind   Kind
		legacy bool
	}{
		{"request start", `{"schema_version":2,"phase":"request_start"}`, KindRequestStart, false},
		{"request finish", `{"schema_version":2,"phase":"request_finish"}`, KindRequestFinish, false},
		{"request rejected", `{"schema_version":2,"phase":"request_rejected"}`, KindRequestRejected, false},
		{"operation start", `{"schema_version":2,"phase":"operation_start"}`, KindOperationStart, false},
		{"operation finish", `{"schema_version":2,"phase":"operation_finish"}`, KindOperationFinish, false},
		{"server start", `{"schema_version":2,"phase":"server_start"}`, KindServerStart, false},
		{"legacy start", `{"phase":"start"}`, KindRequestStart, true},
		{"legacy finish", `{"phase":"finish"}`, KindRequestFinish, true},
		{"legacy rejected", `{"phase":"rejected"}`, KindRequestRejected, true},
		{"legacy name in versioned record", `{"schema_version":2,"phase":"start"}`, KindUnknown, false},
		{"unknown phase", `{"schema_version":2,"phase":"future_phase"}`, KindUnknown, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, err := Decode([]byte(tt.raw + "\n"))
			if err != nil {
				t.Fatal(err)
			}
			if e.Kind != tt.kind || e.Legacy != tt.legacy {
				t.Fatalf("kind=%v legacy=%v, want %v %v", e.Kind, e.Legacy, tt.kind, tt.legacy)
			}
		})
	}
}

func TestDecodeKeepsExactCorrelationFields(t *testing.T) {
	raw := `{"schema_version":2,"operation_id":"op-\u00e9-\ud83d\ude00-` + "\u00e9" + `","receipt":{"x":1},` +
		`"time":"2026-11-09T10:00:00.123456789Z","server_id":"srv","request_id":"req","tool":"terminal_start",` +
		`"phase":"request_finish","status":"ok","duration_ms":12,"session_id":"s1","args":{"argv":["a","a"],"unknown":[1,2]},` +
		`"request_state":"state","outcome":"unknown","reason":"r","replayed":true}`
	e, err := Decode([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	want := Event{
		SchemaVersion: 2, Kind: KindRequestFinish, Phase: "request_finish",
		Time: "2026-11-09T10:00:00.123456789Z", ServerID: "srv", RequestID: "req",
		OperationID: "op-\u00e9-\U0001F600-\u00e9", SessionID: "s1", Tool: "terminal_start",
		Status: "ok", Outcome: "unknown", Reason: "r", RequestState: "state",
		Replayed: true, DurationMS: 12, HasDuration: true,
	}
	if e != want {
		t.Fatalf("got  %+v\nwant %+v", e, want)
	}
}

func TestDecodeRefusesToGuess(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want error
	}{
		{"duplicate request id", `{"phase":"request_start","request_id":"a","request_id":"b"}`, ErrDuplicateField},
		{"duplicate phase", `{"phase":"request_start","phase":"request_finish"}`, ErrDuplicateField},
		{"lone surrogate", `{"phase":"request_start","operation_id":"\ud800"}`, ErrFieldType},
		{"reversed surrogates", `{"phase":"request_start","operation_id":"\udc00\ud800"}`, ErrFieldType},
		{"invalid utf-8", "{\"phase\":\"request_start\",\"request_id\":\"\xff\"}", ErrFieldType},
		{"string type", `{"phase":"request_start","request_id":7}`, ErrFieldType},
		{"null string", `{"phase":"request_start","server_id":null}`, ErrFieldType},
		{"fractional duration", `{"phase":"request_finish","duration_ms":1.5}`, ErrFieldType},
		{"replayed string", `{"phase":"request_finish","replayed":"true"}`, ErrFieldType},
		{"negative schema", `{"phase":"request_finish","schema_version":-1}`, ErrFieldType},
		{"array", `["phase"]`, ErrMalformedRecord},
		{"trailing data", `{"phase":"request_start"} {}`, ErrMalformedRecord},
		{"not JSON", `{"phase":`, ErrMalformedRecord},
		{"schema overflow", `{"phase":"request_finish","schema_version":2147483648}`, ErrFieldType},
		{"missing phase", `{"schema_version":2,"request_id":"a"}`, ErrMissingPhase},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Decode([]byte(tt.raw)); !errors.Is(err, tt.want) {
				t.Fatalf("err=%v, want %v", err, tt.want)
			}
		})
	}
}

func TestDecodeIgnoresDuplicatesOutsideCorrelationFields(t *testing.T) {
	// Duplicate keys inside args belong to the owner-visible raw record and
	// must not make the call uncorrelatable.
	e, err := Decode([]byte(`{"phase":"request_start","request_id":"r","args":{"a":1,"a":2},"x":1,"x":2}`))
	if err != nil || e.RequestID != "r" {
		t.Fatalf("event=%+v err=%v", e, err)
	}
}

func TestExactStringEscapes(t *testing.T) {
	tests := map[string]string{
		`""`:                         "",
		`"\"\\\/\b\f\n\r\t"`:         "\"\\/\b\f\n\r\t",
		`"\u0000"`:                   "\x00",
		`"\uFFFD"`:                   "\uFFFD",
		"\"\uFFFD\"":                 "\uFFFD",
		`"\ud83d\ude00"`:             "\U0001F600",
		`"\u00E9e\u0301"`:            "\u00e9e\u0301",
		`"plain ASCII 123/abc_DEF-"`: "plain ASCII 123/abc_DEF-",
	}
	for raw, want := range tests {
		got, ok := exactString([]byte(raw))
		if !ok || got != want {
			t.Errorf("%s: got %q %v, want %q", raw, got, ok, want)
		}
	}
	for _, raw := range []string{`"\ud800x"`, `"\udc00"`, `"\x"`, `"\u12"`, "\"a\nb\"", `abc`, `"`} {
		if got, ok := exactString([]byte(raw)); ok {
			t.Errorf("%s: unexpectedly exact %q", raw, got)
		}
	}
}

func TestDecodeReportsFieldsDeterministically(t *testing.T) {
	raw := []byte(`{"tool":1,"status":2,"request_id":3,"phase":"request_start","server_id":4}`)
	for i := 0; i < 20; i++ {
		_, err := Decode(raw)
		if err == nil || err.Error() != ErrFieldType.Error()+`: "server_id"` {
			t.Fatalf("run %d: %v", i, err)
		}
	}
}

func TestDecodePopulatesIdentityWithOtherFields(t *testing.T) {
	e, err := Decode([]byte(`{"schema_version":2,"phase":"request_start","server_id":"srv","request_id":"r",` +
		`"call_meta":{"openai/session":"s"},"args":{"call_meta":{"openai/session":"other"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	key, ok := e.Identity.Key()
	if !ok || key != (ConversationKey{IdentitySource: "openai/session", ConversationID: "s"}) || e.RequestID != "r" {
		t.Fatalf("event=%+v key=%v", e, key)
	}
}
