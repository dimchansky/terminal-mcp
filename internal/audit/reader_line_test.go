package audit

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openLineFixture(t *testing.T, content []byte) (*os.File, int64) {
	t.Helper()
	path := filepath.Join(privateAuditDir(t), "history.jsonl")
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	f, err := openAuditReader(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f, int64(len(content))
}

func TestRecordScannerForwardBackwardPreservesOriginalBytes(t *testing.T) {
	lines := [][]byte{
		[]byte("{\"time\":\"a\",\"args\":{\"argv\":[\"go\",\"test\"]}}\n"),
		[]byte("{\"args\":{\"duplicate\":1,\"duplicate\":2,\"number\":9007199254740993}}\n"),
		[]byte("{\"phase\":\"finish\",\"status\":\"applied\"}\n"),
	}
	f, limit := openLineFixture(t, bytes.Join(lines, nil))
	offset := int64(0)
	for i, expected := range lines {
		got, next, err := readRecordForward(context.Background(), f, offset, limit)
		if err != nil || !bytes.Equal(expected, got) || next != offset+int64(len(expected)) {
			t.Fatalf("forward row %d: got=%q next=%d err=%v", i, got, next, err)
		}
		offset = next
	}
	if _, _, err := readRecordForward(context.Background(), f, offset, limit); !errors.Is(err, io.EOF) {
		t.Fatalf("expected live edge, got %v", err)
	}
	for i := len(lines) - 1; i >= 0; i-- {
		got, start, err := readRecordBefore(context.Background(), f, offset, limit)
		if err != nil || !bytes.Equal(got, lines[i]) || start != offset-int64(len(lines[i])) {
			t.Fatalf("reverse row %d: got=%q start=%d err=%v", i, got, start, err)
		}
		offset = start
	}
	if _, _, err := readRecordBefore(context.Background(), f, 0, limit); !errors.Is(err, io.EOF) {
		t.Fatalf("expected reverse edge, got %v", err)
	}
}

func TestRecordScannerMaxLineAndCorruption(t *testing.T) {
	template := []byte("{\"d\":\"\"}\n")
	exact := append([]byte("{\"d\":\""+strings.Repeat("x", int(maxFileBytes)-len(template))+"\"}"), '\n')
	if len(exact) != int(maxFileBytes) {
		t.Fatalf("fixture wrong size %d", len(exact))
	}
	f, maxSize := openLineFixture(t, exact)
	one, end, err := readRecordForward(context.Background(), f, 0, maxSize)
	if err != nil || len(one) != int(maxFileBytes) || end != maxFileBytes {
		t.Fatalf("exact record boundary: len %d end %d err %v", len(one), end, err)
	}
	one, start, err := readRecordBefore(context.Background(), f, maxFileBytes, maxFileBytes)
	if err != nil || start != 0 || len(one) != int(maxFileBytes) {
		t.Fatalf("reverse exact record: start %d len %d err %v", start, len(one), err)
	}

	tooLong := append(bytes.Clone(exact[:len(exact)-3]), []byte("xx\"}\n")...)
	oversize, limit := openLineFixture(t, tooLong)
	if _, _, err := readRecordForward(context.Background(), oversize, 0, limit); !errors.Is(err, ErrRecordTooLarge) {
		t.Fatalf("too-long forward record: %v", err)
	}
	if _, _, err := readRecordBefore(context.Background(), oversize, limit, limit); !errors.Is(err, ErrRecordTooLarge) {
		t.Fatalf("too-long reverse record: %v", err)
	}

	for _, contents := range [][]byte{
		[]byte("not json\n"),
		[]byte("{\"incomplete\":true}"),
		[]byte("\n"),
	} {
		f, limit := openLineFixture(t, contents)
		if _, _, err := readRecordForward(context.Background(), f, 0, limit); !errors.Is(err, ErrCorruptRecord) {
			t.Fatalf("corrupt %q accepted: %v", contents, err)
		}
	}
}

func TestRecordScannerRejectsInvalidBoundariesAndCancellation(t *testing.T) {
	f, limit := openLineFixture(t, []byte("{\"a\":1}\n{\"b\":2}\n"))
	for _, tc := range []struct{ start, end int64 }{{-1, limit}, {limit + 1, limit}, {2, limit}} {
		if _, _, err := readRecordForward(context.Background(), f, tc.start, tc.end); !errors.Is(err, ErrRecordBoundary) {
			t.Fatalf("forward %v should reject boundary: %v", tc, err)
		}
	}
	if _, _, err := readRecordBefore(context.Background(), f, 3, limit); !errors.Is(err, ErrRecordBoundary) {
		t.Fatalf("reverse must reject interior offset: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := readRecordForward(ctx, f, 0, limit); !errors.Is(err, context.Canceled) {
		t.Fatalf("forward cancellation: %v", err)
	}
	if _, _, err := readRecordBefore(ctx, f, limit, limit); !errors.Is(err, context.Canceled) {
		t.Fatalf("reverse cancellation: %v", err)
	}
	// A valid newline past the acknowledged frontier is still not readable.
	if _, _, err := readRecordForward(context.Background(), f, 0, 2); !errors.Is(err, ErrCorruptRecord) {
		t.Fatalf("partial acknowledged line should remain unreadable: %v", err)
	}
}
