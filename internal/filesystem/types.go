package filesystem

type Range struct {
	Path      string `json:"path" jsonschema:"Absolute path inside the configured file root."`
	StartLine int    `json:"start_line,omitempty" jsonschema:"One-based inclusive line; default 1."`
	EndLine   int    `json:"end_line,omitempty" jsonschema:"One-based inclusive end; default at most 200 lines from start. At most 1000 lines per file."`
}
type ReadArgs struct {
	Files    []Range `json:"files" jsonschema:"1..8 file ranges; each regular UTF-8 file is at most 2 MiB."`
	MaxBytes int     `json:"max_bytes,omitempty" jsonschema:"Total returned text budget: default 16384, maximum 262144 bytes."`
}
type Line struct {
	Number int    `json:"number"`
	Text   string `json:"text"`
}
type FileRead struct {
	Path       string `json:"path"`
	Version    string `json:"version,omitempty"`
	TotalLines int    `json:"total_lines,omitempty"`
	Lines      []Line `json:"lines"`
	NextLine   int    `json:"next_line,omitempty"`
	Truncated  bool   `json:"truncated"`
	Error      string `json:"error,omitempty"`
}
type ReadResult struct {
	Root  string     `json:"root"`
	Files []FileRead `json:"files"`
}
type Edit struct {
	OldText string `json:"old_text"`
	NewText string `json:"new_text"`
}
type EditArgs struct {
	ServerID        string `json:"server_id"`
	OperationID     string `json:"operation_id"`
	Path            string `json:"path"`
	ExpectedVersion string `json:"expected_version" jsonschema:"SHA-256 version from file_read of the entire file."`
	Edits           []Edit `json:"edits" jsonschema:"1..32 exact, unique, non-overlapping replacements against the original content; combined input at most 64 KiB."`
}
type WriteArgs struct {
	ServerID        string `json:"server_id"`
	OperationID     string `json:"operation_id"`
	Path            string `json:"path"`
	Content         string `json:"content" jsonschema:"Exact UTF-8 text up to 64 KiB. No implicit newline conversion."`
	ExpectedVersion string `json:"expected_version,omitempty" jsonschema:"Omit only to create a new file; replacing an existing file requires its current SHA-256 version."`
}
type FileReceipt struct {
	OperationID   string `json:"operation_id"`
	Status        string `json:"status"`
	Path          string `json:"path,omitempty"`
	BeforeVersion string `json:"before_version,omitempty"`
	Version       string `json:"version,omitempty"`
	Bytes         int    `json:"bytes"`
	Edits         int    `json:"edits,omitempty"`
	Preview       string `json:"preview,omitempty" jsonschema:"Bounded change preview, not an applyable patch."`
	Error         string `json:"error,omitempty"`
}
type ActionResult struct {
	Receipt  FileReceipt `json:"receipt"`
	Replayed bool        `json:"replayed"`
}
type ListArgs struct {
	Path  string `json:"path" jsonschema:"Absolute directory path; non-recursive listing."`
	Glob  string `json:"glob,omitempty" jsonschema:"Optional basename glob using Go filepath.Match (for example *.go)."`
	Limit int    `json:"limit,omitempty" jsonschema:"Default 200, maximum 1000 entries. Narrow the directory or glob if truncated."`
}
type Entry struct {
	Name string `json:"name"`
	Type string `json:"type"`
}
type ListResult struct {
	Path      string  `json:"path"`
	Entries   []Entry `json:"entries"`
	Truncated bool    `json:"truncated"`
}
type SearchArgs struct {
	Path       string `json:"path" jsonschema:"Absolute directory inside the root; recursive search without following symlinks."`
	Pattern    string `json:"pattern"`
	Regex      bool   `json:"regex,omitempty" jsonschema:"Default literal search; true uses Go regular expressions, matched within each line."`
	Glob       string `json:"glob,omitempty" jsonschema:"Optional ripgrep path glob such as *.go."`
	Hidden     bool   `json:"hidden,omitempty"`
	MaxResults int    `json:"max_results,omitempty" jsonschema:"Default 100, maximum 500; bounded text output and 5-second time limit."`
}
type Match struct {
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated,omitempty"`
}
type SearchResult struct {
	Matches   []Match `json:"matches"`
	Truncated bool    `json:"truncated"`
	Reason    string  `json:"reason"`
}
