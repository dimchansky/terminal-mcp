package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"terminalmcp/internal/audit"
	"terminalmcp/internal/filesystem"
	"terminalmcp/internal/session"
)

func TestFileReceivePublishesChatGPTFileParamsSchema(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	host := session.New(nil)
	defer host.Close()
	files, err := filesystem.New(host, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	ss, err := New(host, files).Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ss.Close() }()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "file-param-test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	list, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var tool *mcp.Tool
	for _, candidate := range list.Tools {
		if candidate.Name == "file_receive" {
			tool = candidate
			break
		}
	}
	if tool == nil {
		t.Fatal("file_receive missing from tools/list")
	}
	if tool.Annotations == nil || tool.Annotations.ReadOnlyHint || tool.Annotations.DestructiveHint == nil ||
		!(*tool.Annotations.DestructiveHint) || tool.Annotations.OpenWorldHint == nil || !(*tool.Annotations.OpenWorldHint) {
		t.Fatalf("file_receive requires accurate network/mutation annotations: %+v", tool.Annotations)
	}
	if !reflect.DeepEqual(tool.Meta["openai/fileParams"], []any{"file"}) &&
		!reflect.DeepEqual(tool.Meta["openai/fileParams"], []string{"file"}) {
		t.Fatalf("missing ChatGPT file parameter marker: %#v", tool.Meta)
	}
	encoded, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(encoded, &schema); err != nil {
		t.Fatal(err)
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("no properties in input schema: %s", encoded)
	}
	file, ok := properties["file"].(map[string]any)
	if !ok {
		t.Fatalf("missing file schema: %s", encoded)
	}
	if ref, ok := file["$ref"].(string); ok {
		defs, ok := schema["$defs"].(map[string]any)
		if !ok {
			t.Fatalf("missing definitions for %s: %s", ref, encoded)
		}
		file, ok = defs[strings.TrimPrefix(ref, "#/$defs/")].(map[string]any)
		if !ok {
			t.Fatalf("unresolved %s: %s", ref, encoded)
		}
	}
	fields, ok := file["properties"].(map[string]any)
	if !ok {
		t.Fatalf("file object missing properties: %s", encoded)
	}
	for _, key := range []string{"download_url", "file_id", "mime_type", "file_name"} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("ChatGPT file field %s missing: %s", key, encoded)
		}
	}
	var required []string
	for _, v := range file["required"].([]any) {
		required = append(required, v.(string))
	}
	slices.Sort(required)
	if !slices.Equal(required, []string{"download_url", "file_id"}) {
		t.Fatalf("file required fields must be exactly [download_url file_id], got %v", required)
	}
}

func TestFileReceiveAuditsEverythingExceptTemporarySignedURL(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	logfile := filepath.Join(dir, "audit.jsonl")
	recorder, err := audit.Open(logfile)
	if err != nil {
		t.Fatal(err)
	}
	host := session.New(nil)
	files, err := filesystem.New(host, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		host.Close()
		files.Close()
		if err := recorder.Close(); err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ct, st := mcp.NewInMemoryTransports()
	ss, err := NewWithAudit(host, recorder, files).Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ss.Close() }()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "redaction-test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	const secret = "DO_NOT_LOG_THIS_SIGNED_SECRET"
	args := filesystem.ReceiveArgs{
		ServerID: host.ID(), OperationID: "receive-secret", Path: filepath.Join(dir, "not-created.zip"),
		File: filesystem.OpenAIFile{
			FileID: "file_123456", DownloadURL: "https://127.0.0.1/private?signature=" + secret,
			FileName: "archive.zip", MIMEType: "application/zip",
		},
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "file_receive", Arguments: args})
	if err != nil || res == nil || !res.IsError {
		t.Fatalf("unsafe URL was not rejected: %+v %v", res, err)
	}
	bad, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "file_receive", Arguments: map[string]any{
		"server_id": host.ID(), "operation_id": "bad-file-object",
		"path": args.Path, "file": "https://example.org/?signature=" + secret,
	}})
	if err != nil || bad == nil || !bad.IsError {
		t.Fatalf("malformed file object was not rejected: %+v %v", bad, err)
	}
	b, err := os.ReadFile(logfile)
	if err != nil {
		t.Fatal(err)
	}
	if bytes := string(b); strings.Contains(bytes, secret) ||
		!strings.Contains(bytes, "[REDACTED]") ||
		!strings.Contains(bytes, "file_123456") ||
		!strings.Contains(bytes, "archive.zip") ||
		!strings.Contains(bytes, "unsafe_file_arguments") {
		t.Fatalf("signed URL not sanitized or remaining audit data missing: %s", bytes)
	}
}
