//go:build linux || darwin

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
	if _, ok := properties["wait_ms"]; !ok {
		t.Fatalf("receive schema lacks optional wait_ms: %s", encoded)
	}
	for _, field := range schema["required"].([]any) {
		if field == "wait_ms" {
			t.Fatalf("wait_ms must be optional: %s", encoded)
		}
	}
	var resultTool *mcp.Tool
	for _, candidate := range list.Tools {
		if candidate.Name == "file_receive_status" {
			t.Fatal("obsolete file_receive_status is still advertised")
		}
		if candidate.Name == "file_receive_result" {
			resultTool = candidate
		}
	}
	if resultTool == nil || resultTool.Annotations == nil || !resultTool.Annotations.ReadOnlyHint {
		t.Fatalf("file_receive_result must be available and read-only: %+v", resultTool)
	}
	raw, err := json.Marshal(resultTool.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	var resultSchema map[string]any
	if err := json.Unmarshal(raw, &resultSchema); err != nil {
		t.Fatal(err)
	}
	resultProperties, ok := resultSchema["properties"].(map[string]any)
	if !ok || resultProperties["wait_ms"] == nil {
		t.Fatalf("result schema lacks optional wait_ms: %s", raw)
	}
	for _, field := range resultSchema["required"].([]any) {
		if field == "wait_ms" {
			t.Fatalf("result wait_ms must be optional: %s", raw)
		}
	}
}

func TestFileReceiveAuditsFullSensitiveInputsEvenOnRejection(t *testing.T) {
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
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "full-owner-audit-test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()

	const secret = "PRESIGNED_URL_SECRET_MUST_BE_IN_OWNER_AUDIT"
	const extraSecret = "UNKNOWN_SENSITIVE_FIELD_MUST_BE_IN_OWNER_AUDIT"
	signedURL := "http://127.0.0.1:1/private?signature=" + secret
	path := filepath.Join(dir, "not-created.zip")
	// Even schema-rejected calls must preserve exactly what the client
	// attempted to send, including a local HTTP URL and sensitive parameters.
	args := map[string]any{
		"server_id": host.ID(), "operation_id": "receive-secret", "path": path,
		"file": map[string]any{
			"download_url": signedURL, "file_id": "file_123456",
			"file_name": "archive.zip", "mime_type": "application/zip",
		},
		"unknown_sensitive_field": extraSecret,
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "file_receive", Arguments: args})
	if err != nil || res == nil || !res.IsError {
		t.Fatalf("schema-invalid request was not rejected: %+v %v", res, err)
	}

	// Even an incorrectly typed attachment must be audited as supplied,
	// rather than dropped because its shape is invalid.
	malformedURL := "https://example.org/?signature=" + secret
	bad, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "file_receive", Arguments: map[string]any{
		"server_id": host.ID(), "operation_id": "bad-file-object",
		"path": path, "file": malformedURL, "unknown_sensitive_field": extraSecret,
	}})
	if err != nil || bad == nil || !bad.IsError {
		t.Fatalf("malformed file object was not rejected: %+v %v", bad, err)
	}
	// A schema-valid call to an unavailable local HTTP source must retain the
	// operation ID even when its download fails.
	zero := 0
	valid := filesystem.ReceiveArgs{
		ServerID: host.ID(), OperationID: "schema-valid-unavailable-source", Path: path, WaitMS: &zero,
		File: filesystem.FileReference{
			DownloadURL: signedURL, FileID: "file_123456",
			FileName: "archive.zip", MIMEType: "application/zip",
		},
	}
	typed := call[filesystem.ReceiveResult](t, ctx, cs, "file_receive", valid)
	if typed.OperationID != valid.OperationID {
		t.Fatalf("async submission did not return operation ID: %+v", typed)
	}
	var failed filesystem.ReceiveResult
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		failed = call[filesystem.ReceiveResult](t, ctx, cs,
			"file_receive_result", filesystem.ReceiveResultArgs{ServerID: host.ID(), OperationID: valid.OperationID})
		if failed.Status != "running" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if failed.Status != "failed" || failed.Receipt == nil {
		t.Fatalf("unavailable local source did not report failure: %+v", failed)
	}
	again := call[filesystem.ReceiveResult](t, ctx, cs,
		"file_receive_result", filesystem.ReceiveResultArgs{ServerID: host.ID(), OperationID: valid.OperationID})
	if again.Status != "failed" || again.Receipt.Error != failed.Receipt.Error {
		t.Fatalf("terminal status changed after reread: %+v vs %+v", failed, again)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("rejected tool created a file: %v", err)
	}

	var starts, finishes int
	for _, event := range readLifecycleAudit(t, logfile) {
		if event.Tool != "file_receive" {
			continue
		}
		switch event.Phase {
		case "request_start":
			starts++
			input, ok := event.Args.(map[string]any)
			if !ok || (event.OperationID != "schema-valid-unavailable-source" && input["unknown_sensitive_field"] != extraSecret) {
				t.Fatalf("audit lost unknown sensitive arguments: %+v", event)
			}
			switch event.OperationID {
			case "receive-secret":
				file, ok := input["file"].(map[string]any)
				if !ok || file["download_url"] != signedURL ||
					file["file_id"] != "file_123456" ||
					file["file_name"] != "archive.zip" ||
					file["mime_type"] != "application/zip" {
					t.Fatalf("audit altered signed URL or file metadata: %+v", event)
				}
			case "bad-file-object":
				if input["file"] != malformedURL {
					t.Fatalf("audit discarded malformed sensitive field: %+v", event)
				}
			case "schema-valid-unavailable-source":
				file, ok := input["file"].(map[string]any)
				if !ok || file["download_url"] != signedURL || input["wait_ms"] != float64(0) {
					t.Fatalf("audit lost signed URL or requested observation wait: %+v", event)
				}
			default:
				t.Fatalf("unexpected operation: %+v", event)
			}
		case "request_finish":
			finishes++
			if event.OperationID != "receive-secret" && event.OperationID != "bad-file-object" && event.OperationID != "schema-valid-unavailable-source" {
				t.Fatalf("request_finish lost the file_receive operation_id: %+v", event)
			}
		case "request_rejected":
			t.Fatalf("auditable input rejected before full capture: %+v", event)
		}
	}
	if starts != 3 || finishes != 3 {
		t.Fatalf("missing complete request pairs: starts=%d finishes=%d", starts, finishes)
	}
}
