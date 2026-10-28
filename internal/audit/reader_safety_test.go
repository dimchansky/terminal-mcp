package audit

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeclaredFrontierTruncatedFileIsNotLiveEdge(t *testing.T) {
	content := []byte("{\"first\":true}\n{\"second\":true}\n")
	f, oldFrontier := openLineFixture(t, content)
	if err := os.Truncate(f.Name(), 2); err != nil {
		t.Fatal(err)
	}
	check := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			t.Fatalf("%s: truncated physical file must be unexpected EOF, not a live edge: %v", name, err)
		}
	}
	_, _, err := readRecordForward(context.Background(), f, 0, oldFrontier)
	check("single forward", err)
	_, _, err = readRecordBefore(context.Background(), f, oldFrontier, oldFrontier)
	check("single reverse", err)
	fwd, err := newForwardPageScanner(context.Background(), f, 0, oldFrontier)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = fwd.next()
	check("window forward", err)
	_, err = newBackwardPageScanner(context.Background(), f, oldFrontier, oldFrontier)
	check("window reverse", err)
	_, err = boundaryProof(context.Background(), f, oldFrontier, oldFrontier)
	check("cursor proof", err)
}

type checkedContext struct {
	context.Context
	calls int
	max   int
}

func (c *checkedContext) Err() error {
	c.calls++
	if c.calls > c.max {
		return context.Canceled
	}
	return c.Context.Err()
}

func TestForwardPageScannerIncompleteFailureIsSticky(t *testing.T) {
	cases := []struct {
		name string
		text string
		ctx  context.Context
	}{
		{"incomplete-final-line", "{\"incomplete\":true", context.Background()},
		{"canceled-in-middle", "{\"data\":\"" + strings.Repeat("x", 220<<10) + "\"}\n", &checkedContext{Context: context.Background(), max: 2}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, size := openLineFixture(t, []byte(tc.text))
			scanner, err := newForwardPageScanner(tc.ctx, f, 0, size)
			if err != nil {
				t.Fatal(err)
			}
			_, next, firstErr := scanner.next()
			if firstErr == nil || next != 0 || scanner.offset != 0 {
				t.Fatalf("incomplete scan returned cursor interior to row: %d %v", next, firstErr)
			}
			_, again, secondErr := scanner.next()
			if again != 0 || !errors.Is(secondErr, firstErr) {
				t.Fatalf("failed scanner resumed at interior byte: %d %v, original %v", again, secondErr, firstErr)
			}
		})
	}
}

func TestCursorAfterCorruptOrOversizeRowRemainsResumable(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"malformed", []byte("not JSON\n")},
		{"oversize", []byte("{\"data\":\"" + strings.Repeat("x", int(maxFileBytes)) + "\"}\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			goodFirst := []byte("{\"ok\":1}\n")
			goodLast := []byte("{\"ok\":2}\n")
			contents := append(append(append([]byte{}, goodFirst...), tc.body...), goodLast...)
			f, limit := openLineFixture(t, contents)
			key, err := segmentFingerprint(context.Background(), f, limit)
			if err != nil {
				t.Fatal(err)
			}
			before := int64(len(goodFirst) + len(tc.body))
			digest, err := boundaryProof(context.Background(), f, before, limit)
			if err != nil {
				t.Fatal(err)
			}
			if digest != cursorProof(tc.body) {
				t.Fatal("cursor proof differs for original and persisted raw record")
			}
			cur, err := decodePageCursor(newPageCursor(Forward, key, 0, before, cursorProof(tc.body)))
			if err != nil {
				t.Fatal(err)
			}
			if err := verifyCursorBoundary(context.Background(), f, limit, cur); err != nil {
				t.Fatalf("cannot resume immediately after %s row: %v", tc.name, err)
			}
			read, next, err := readRecordForward(context.Background(), f, before, limit)
			if err != nil || string(read) != string(goodLast) || next != limit {
				t.Fatalf("resumed at wrong record: %q %d %v", read, next, err)
			}
			backward, err := decodePageCursor(newPageCursor(Backward, key, 0, before, cursorProof(tc.body)))
			if err != nil || verifyCursorBoundary(context.Background(), f, limit, backward) != nil {
				t.Fatal("backward cursor from completed damaged row should remain valid", err)
			}
		})
	}
}

func TestReaderFingerprintCacheRejectsSameInodeMutation(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "old-history.jsonl")
	original := []byte("{\"alpha\":1}\n")
	changed := []byte("{\"alpha\":2}\n")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	f, err := openAuditReader(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	r := new(Reader)
	before, err := r.fingerprint(context.Background(), f, info, int64(len(original)))
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if err := os.WriteFile(path, changed, 0600); err != nil {
		t.Fatal(err)
	}
	f, err = openAuditReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	afterInfo, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(info, afterInfo) {
		t.Skip("filesystem replaced inode instead of rewriting the same file")
	}
	after, err := r.fingerprint(context.Background(), f, afterInfo, int64(len(changed)))
	if err != nil || before == after {
		t.Fatalf("stale fingerprint after in-place rewrite: %x -> %x err %v", before, after, err)
	}
}
