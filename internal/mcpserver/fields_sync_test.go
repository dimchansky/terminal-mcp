package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"terminalmcp/internal/filesystem"
	"terminalmcp/internal/session"
)

// TestDashboardFieldClassesMatchToolSchemas (#11): the dashboard page shows
// "main" fields of a call at once and folds "protocol" fields into Details
// (internal/viewer/static/fields.js). Every input field of every tool must
// be classified there exactly once, so a new tool or field cannot slip into
// the folded part, or out of the page's knowledge, unnoticed.
func TestDashboardFieldClassesMatchToolSchemas(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	manager := session.New(nil)
	defer manager.Close()
	files, err := filesystem.New(manager, t.TempDir())
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
	client, err := mcp.NewClient(&mcp.Implementation{Name: "fields-sync-test", Version: "1"}, nil).Connect(ctx, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	list, err := client.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	source, err := os.ReadFile(filepath.Join("..", "viewer", "static", "fields.js"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	begin, end := strings.Index(text, "/*BEGIN-FIELDS*/"), strings.Index(text, "/*END-FIELDS*/")
	if begin < 0 || end < begin {
		t.Fatal("fields.js has no BEGIN-FIELDS/END-FIELDS block")
	}
	var classes map[string]struct {
		Main     []string `json:"main"`
		Protocol []string `json:"protocol"`
	}
	dec := json.NewDecoder(strings.NewReader(text[begin+len("/*BEGIN-FIELDS*/") : end]))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&classes); err != nil {
		t.Fatalf("fields.js block is not the expected JSON: %v", err)
	}
	if dec.More() {
		t.Fatal("fields.js block has more than one JSON value")
	}
	if _, ok := classes["__proto__"]; ok {
		t.Fatal("fields.js block names a tool __proto__, which a JS object literal treats as its prototype")
	}

	seen := map[string]bool{}
	walked := map[string][]string{}
	for _, tool := range list.Tools {
		seen[tool.Name] = true
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var schema map[string]any
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatal(err)
		}
		var paths []string
		if err := schemaPaths(schema, schema, "", &paths); err != nil {
			t.Fatalf("%s: %v", tool.Name, err)
		}
		sort.Strings(paths)
		walked[tool.Name] = paths
		c, ok := classes[tool.Name]
		if !ok {
			t.Errorf("fields.js does not classify tool %s (fields %v)", tool.Name, paths)
			continue
		}
		listed := append(slices.Clone(c.Main), c.Protocol...)
		sort.Strings(listed)
		for i := 1; i < len(listed); i++ {
			if listed[i] == listed[i-1] {
				t.Errorf("%s: %s is classified twice", tool.Name, listed[i])
			}
		}
		listed = slices.Compact(listed)
		if !slices.Equal(listed, paths) {
			t.Errorf("%s: fields.js classifies %v, the schema has %v", tool.Name, listed, paths)
		}
	}
	for name := range classes {
		if !seen[name] {
			t.Errorf("fields.js classifies %s, which is not a tool", name)
		}
	}
	// The walk itself reaches nested fields (a silent walker regression
	// would make both lists shrink together).
	for tool, path := range map[string]string{"file_edit": "edits[].old_text", "file_receive": "file.download_url", "terminal_write": "input[].text"} {
		if !slices.Contains(walked[tool], path) {
			t.Errorf("the schema walk of %s misses %s: %v", tool, path, walked[tool])
		}
	}
}

// schemaPaths lists the field paths of a JSON schema: object properties by
// name, array items with [], following $ref into $defs. Maps (objects
// without properties, such as env) are leaves. A shape it cannot walk is
// an error, so a new nested field cannot pass unclassified.
func schemaPaths(root, s map[string]any, prefix string, out *[]string) error {
	for _, k := range []string{"anyOf", "oneOf", "allOf", "not", "patternProperties", "prefixItems", "dependentSchemas"} {
		if _, ok := s[k]; ok {
			return fmt.Errorf("%q uses %s, which the field walk does not support", prefix, k)
		}
	}
	if ref, ok := s["$ref"].(string); ok {
		name := ref[strings.LastIndex(ref, "/")+1:]
		defs, _ := root["$defs"].(map[string]any)
		def, ok := defs[name].(map[string]any)
		if !strings.HasPrefix(ref, "#/$defs/") || !ok {
			return fmt.Errorf("%q: unresolved $ref %q", prefix, ref)
		}
		return schemaPaths(root, def, prefix, out)
	}
	if extra, ok := s["additionalProperties"].(map[string]any); ok {
		if _, nested := extra["properties"]; nested {
			return fmt.Errorf("%q: additionalProperties with properties", prefix)
		}
		if _, nested := extra["items"]; nested {
			return fmt.Errorf("%q: additionalProperties with items", prefix)
		}
	}
	if props, ok := s["properties"].(map[string]any); ok {
		for name, v := range props {
			p := name
			if prefix != "" {
				p = prefix + "." + name
			}
			*out = append(*out, p)
			sub, ok := v.(map[string]any)
			if !ok {
				return fmt.Errorf("%q: property schema is not an object", p)
			}
			if err := schemaPaths(root, sub, p, out); err != nil {
				return err
			}
		}
	}
	if items, present := s["items"]; present {
		obj, ok := items.(map[string]any)
		if !ok {
			return fmt.Errorf("%q: items is not one schema", prefix)
		}
		return schemaPaths(root, obj, prefix+"[]", out)
	}
	return nil
}
