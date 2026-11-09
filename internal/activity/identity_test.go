package activity

import (
	"encoding/json"
	"strings"
	"testing"
)

func ptr(s string) *string { return &s }

func TestIdentityNormalization(t *testing.T) {
	long := strings.Repeat("c7e42ab19d", 40) // Never shortened.
	tests := []struct {
		name string
		meta string
		want Identity
	}{
		{"no metadata", ``, Identity{}},
		{"empty object", `{}`, Identity{}},
		{"openai session and client", `{"openai/session":"` + long + `","io.modelcontextprotocol/clientInfo":{"name":"openai-mcp","version":"1"}}`,
			Identity{ClientName: ptr("openai-mcp"), ConversationID: ptr(long), IdentitySource: ptr("openai/session")}},
		{"escaped value kept exactly", `{"openai/session":"a\u00e9\ud83d\ude00\n"}`,
			Identity{ConversationID: ptr("a\u00e9\U0001F600\n"), IdentitySource: ptr("openai/session")}},
		{"client only", `{"io.modelcontextprotocol/clientInfo":{"name":"claude-code"}}`,
			Identity{ClientName: ptr("claude-code")}},
		{"empty client name preserved", `{"io.modelcontextprotocol/clientInfo":{"name":""}}`,
			Identity{ClientName: ptr("")}},
		{"client without name", `{"io.modelcontextprotocol/clientInfo":{"version":"1"}}`, Identity{}},
		{"unrecognized namespace", `{"openai/subject":"u","other/session":"x","openai/session2":"y","toolUseId":"t","progressToken":"p"}`, Identity{}},
		{"empty session", `{"openai/session":""}`, Identity{MetadataInvalid: true}},
		{"numeric session", `{"openai/session":42}`, Identity{MetadataInvalid: true}},
		{"null session", `{"openai/session":null}`, Identity{MetadataInvalid: true}},
		{"object session", `{"openai/session":{"id":"x"}}`, Identity{MetadataInvalid: true}},
		{"lone surrogate session", `{"openai/session":"a\udc00"}`, Identity{MetadataInvalid: true}},
		{"duplicate session", `{"openai/session":"a","openai/session":"b"}`, Identity{MetadataInvalid: true}},
		{"duplicate client info", `{"openai/session":"a","io.modelcontextprotocol/clientInfo":{"name":"x"},"io.modelcontextprotocol/clientInfo":{"name":"y"}}`,
			Identity{ConversationID: ptr("a"), IdentitySource: ptr("openai/session"), MetadataInvalid: true}},
		{"duplicate client name", `{"io.modelcontextprotocol/clientInfo":{"name":"x","name":"y"}}`, Identity{MetadataInvalid: true}},
		{"non-string client name", `{"io.modelcontextprotocol/clientInfo":{"name":7}}`, Identity{MetadataInvalid: true}},
		{"client info not object", `{"io.modelcontextprotocol/clientInfo":"x"}`, Identity{MetadataInvalid: true}},
		{"client info null", `{"openai/session":"a","io.modelcontextprotocol/clientInfo":null}`,
			Identity{ConversationID: ptr("a"), IdentitySource: ptr("openai/session"), MetadataInvalid: true}},
		{"duplicate session with equal values", `{"openai/session":"a","openai/session":"a"}`, Identity{MetadataInvalid: true}},
		{"metadata not object", `["openai/session"]`, Identity{MetadataInvalid: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := `{"schema_version":2,"phase":"request_start"}`
			if tt.meta != "" {
				raw = `{"schema_version":2,"phase":"request_start","call_meta":` + tt.meta + `}`
			}
			e, err := Decode([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			if !equalIdentity(e.Identity, tt.want) {
				t.Fatalf("got %s, want %s", showIdentity(e.Identity), showIdentity(tt.want))
			}
			if e.Identity.IdentityVerified {
				t.Fatal("client-supplied metadata must never be verified")
			}
		})
	}
}

func TestConversationKeysArePairsOfFullValues(t *testing.T) {
	a := identityFromCallMeta([]byte(`{"openai/session":"7fa2c0ffee-1"}`))
	b := identityFromCallMeta([]byte(`{"openai/session":"7fa2c0ffee-2"}`)) // Shared visible prefix.
	ka, okA := a.Key()
	kb, okB := b.Key()
	if !okA || !okB || ka == kb {
		t.Fatalf("prefix-sharing IDs must be distinct lanes: %v %v", ka, kb)
	}
	if _, ok := (Identity{ClientName: ptr("claude-code")}).Key(); ok {
		t.Fatal("a client name alone must not form a conversation")
	}
	if ka != (ConversationKey{IdentitySource: "openai/session", ConversationID: "7fa2c0ffee-1"}) {
		t.Fatalf("unexpected key %v", ka)
	}
}

func equalIdentity(a, b Identity) bool {
	eq := func(x, y *string) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	return eq(a.ClientName, b.ClientName) && eq(a.ConversationID, b.ConversationID) &&
		eq(a.IdentitySource, b.IdentitySource) && a.IdentityVerified == b.IdentityVerified &&
		a.MetadataInvalid == b.MetadataInvalid
}

func showIdentity(id Identity) string {
	s := func(p *string) string {
		if p == nil {
			return "nil"
		}
		return "\"" + *p + "\""
	}
	return "{client=" + s(id.ClientName) + " conv=" + s(id.ConversationID) + " source=" + s(id.IdentitySource) +
		" invalid=" + map[bool]string{true: "true", false: "false"}[id.MetadataInvalid] + "}"
}

// TestIdentityFromWriterEncoding builds call_meta the way the server records it:
// the SDK's map is marshaled by encoding/json, which sorts keys, HTML-escapes
// <, > and &, escapes U+2028 and replaces invalid UTF-8. The decoded hint must
// equal exactly the value stored in the journal.
func TestIdentityFromWriterEncoding(t *testing.T) {
	supplied := "a<b>&c" + string(rune(0x2028)) + "d" + string([]byte{0xff}) + "e" + string(rune(0x1F600))
	meta, err := json.Marshal(map[string]any{
		"openai/session":                     supplied,
		"io.modelcontextprotocol/clientInfo": map[string]any{"name": "<client>", "version": "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	record, err := json.Marshal(map[string]any{"schema_version": 2, "phase": "request_start", "call_meta": json.RawMessage(meta)})
	if err != nil {
		t.Fatal(err)
	}
	for _, escaped := range []string{`\u003c`, `\u0026`, `\u2028`, "\uFFFD"} {
		if !strings.Contains(string(record), escaped) {
			t.Fatalf("record %s lacks writer escaping %q", record, escaped)
		}
	}
	e, err := Decode(record)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.ToValidUTF8(supplied, string(rune(0xFFFD)))
	if e.Identity.ConversationID == nil || *e.Identity.ConversationID != want || e.Identity.MetadataInvalid {
		t.Fatalf("identity %s, want conversation %q", showIdentity(e.Identity), want)
	}
	if e.Identity.ClientName == nil || *e.Identity.ClientName != "<client>" {
		t.Fatalf("client %s", showIdentity(e.Identity))
	}
}
