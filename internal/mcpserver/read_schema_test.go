//go:build linux || darwin

package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"terminalmcp/internal/filesystem"
	"terminalmcp/internal/session"
)

func TestReadSchemaContainsEnforcedLimits(t *testing.T) {
	schema := fileReadSchema()
	size := schema.Properties["max_bytes"]
	files := schema.Properties["files"]
	if size.Minimum == nil || *size.Minimum != 0 || size.Maximum == nil || *size.Maximum != 256<<10 {
		t.Fatalf("wrong max_bytes schema: %+v", size)
	}
	if files.MinItems == nil || *files.MinItems != 1 || files.MaxItems == nil || *files.MaxItems != 8 {
		t.Fatalf("wrong file array schema: %+v", files)
	}
	if files.Items == nil {
		t.Fatal("no file-range item schema")
	}
	for _, key := range []string{"start_line", "end_line"} {
		field := files.Items.Properties[key]
		if field == nil || field.Minimum == nil || *field.Minimum != 0 {
			t.Fatalf("missing nonnegative constraint on %q: %+v", key, field)
		}
	}
}

func TestFileReadLargeSDKResponses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	root := t.TempDir()
	filename := filepath.Join(root, "large.txt")
	// Each line has 1,025 bytes, so reads must stop on line boundaries.
	content := strings.Repeat(strings.Repeat("X", 1024)+"\n", 400)
	if err := os.WriteFile(filename, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	manager := session.New(nil)
	defer manager.Close()
	files, err := filesystem.New(manager, root)
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	a, b := mcp.NewInMemoryTransports()
	server, err := New(manager, files).Connect(ctx, b, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Close() }()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "large-read-test", Version: "1"}, nil).Connect(ctx, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	tools, err := client.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var readSchema map[string]any
	for _, tool := range tools.Tools {
		if tool.Name != "file_read" {
			continue
		}
		payload, err := json.Marshal(tool.InputSchema)
		if err != nil || json.Unmarshal(payload, &readSchema) != nil {
			t.Fatalf("cannot parse published input schema: %v", err)
		}
	}
	props, ok := readSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("missing public tool properties: %+v", readSchema)
	}
	maximum, ok := props["max_bytes"].(map[string]any)["maximum"].(float64)
	if !ok || maximum != 256<<10 {
		t.Fatalf("missing published maximum, got %v", props["max_bytes"])
	}

	for _, kiB := range []int{16, 64, 128, 256} {
		t.Run(fmt.Sprintf("%dKiB", kiB), func(t *testing.T) {
			result := call[filesystem.ReadResult](t, ctx, client, "file_read", filesystem.ReadArgs{
				Files:    []filesystem.Range{{Path: filename, StartLine: 1, EndLine: 400}},
				MaxBytes: kiB << 10,
			})
			if len(result.Files) != 1 {
				t.Fatalf("expected one file, got %d", len(result.Files))
			}
			file := result.Files[0]
			if file.Error != "" || file.Version == "" || !file.Truncated || file.NextLine != len(file.Lines)+1 {
				t.Fatalf("bad %d KiB response: total=%d lines=%d next=%d err=%q trunc=%v", kiB, file.TotalLines, len(file.Lines), file.NextLine, file.Error, file.Truncated)
			}
			if len(file.Lines) < (kiB<<10)/1025-1 {
				t.Fatalf("lost read budget: %d KiB yielded only %d lines", kiB, len(file.Lines))
			}
			for i, line := range file.Lines {
				if line.Number != i+1 || len(line.Text) != 1025 {
					t.Fatalf("incorrect line %d: %+v", i, line)
				}
			}
		})
	}
	out, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "file_read", Arguments: filesystem.ReadArgs{
		Files:    []filesystem.Range{{Path: filename}},
		MaxBytes: filesystem.MaxReadBytes + 1,
	}})
	if err != nil || out == nil || !out.IsError {
		t.Fatalf("oversized read not rejected by SDK schema: out=%+v err=%v", out, err)
	}
	// The raw Go handler retains the same enforcement for non-MCP callers.
	if _, err := files.Read(ctx, filesystem.ReadArgs{
		Files:    []filesystem.Range{{Path: filename}},
		MaxBytes: filesystem.MaxReadBytes + 1,
	}); err == nil || !strings.Contains(err.Error(), "262144") {
		t.Fatalf("oversized read not rejected in Go handler: %v", err)
	}
}

// Ensure a rejected MCP request does not alter the file on disk.
func TestReadSchemaInvalidArrayLength(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m := session.New(nil)
	defer m.Close()
	dir := t.TempDir()
	files, err := filesystem.New(m, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	a, b := mcp.NewInMemoryTransports()
	ss, err := New(m, files).Connect(ctx, b, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ss.Close() }()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "bounds-test", Version: "1"}, nil).Connect(ctx, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	for _, count := range []int{0, 9} {
		items := make([]filesystem.Range, count)
		for i := range items {
			items[i].Path = filepath.Join(dir, "missing.txt")
		}
		out, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "file_read", Arguments: filesystem.ReadArgs{Files: items}})
		if err != nil || out == nil || !out.IsError {
			t.Fatalf("array size %d not rejected: %+v %v", count, out, err)
		}
	}
}
