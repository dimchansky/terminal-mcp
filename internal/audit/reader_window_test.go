package audit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestPagedScannersReuseWindowsAndPreserveAllRows(t *testing.T) {
	var data []byte
	const count = 2000
	for i := 0; i < count; i++ {
		data = append(data, []byte(fmt.Sprintf("{\"i\":%d}\n", i))...)
	}
	f, size := openLineFixture(t, data)
	forward, err := newForwardPageScanner(context.Background(), f, 0, size)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < count; i++ {
		line, _, err := forward.next()
		want := fmt.Sprintf("{\"i\":%d}\n", i)
		if err != nil || string(line) != want {
			t.Fatalf("row %d: %q err %v", i, line, err)
		}
	}
	if _, _, err := forward.next(); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	reverse, err := newBackwardPageScanner(context.Background(), f, size, size)
	if err != nil {
		t.Fatal(err)
	}
	for i := count - 1; i >= 0; i-- {
		line, _, err := reverse.previous()
		want := fmt.Sprintf("{\"i\":%d}\n", i)
		if err != nil || string(line) != want {
			t.Fatalf("reverse row %d: %q err %v", i, line, err)
		}
	}
	if _, _, err := reverse.previous(); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
}

func TestPagedScannersSkipCorruptAndOversizeCompleteRows(t *testing.T) {
	goodA := []byte("{\"ok\":1}\n")
	bad := []byte("not json\n")
	goodB := []byte("{\"ok\":2}\n")
	f, size := openLineFixture(t, bytes.Join([][]byte{goodA, bad, goodB}, nil))
	forward, _ := newForwardPageScanner(context.Background(), f, 0, size)
	_, _, err := forward.next()
	if err != nil {
		t.Fatal(err)
	}
	_, end, err := forward.next()
	var problem *RecordProblem
	if !errors.As(err, &problem) || !errors.Is(err, ErrCorruptRecord) || !problem.Complete || end != int64(len(goodA)+len(bad)) {
		t.Fatalf("corrupt forward range: %d %+v", end, err)
	}
	last, _, err := forward.next()
	if err != nil || !bytes.Equal(last, goodB) {
		t.Fatalf("forward did not recover from malformed line: %v %q", err, last)
	}
	backward, _ := newBackwardPageScanner(context.Background(), f, size, size)
	last, _, err = backward.previous()
	if err != nil || !bytes.Equal(last, goodB) {
		t.Fatalf("reverse first: %v", err)
	}
	_, start, err := backward.previous()
	if !errors.As(err, &problem) || !problem.Complete || start != int64(len(goodA)) {
		t.Fatalf("corrupt reverse range: %d %+v", start, err)
	}
	first, _, err := backward.previous()
	if err != nil || !bytes.Equal(first, goodA) {
		t.Fatalf("reverse did not recover: %v", err)
	}

	tooLarge := append([]byte("{\"content\":\""+strings.Repeat("q", int(maxFileBytes))+"\"}"), '\n')
	good := []byte("{\"ok\":true}\n")
	big, limit := openLineFixture(t, append(tooLarge, good...))
	fwd, _ := newForwardPageScanner(context.Background(), big, 0, limit)
	_, offset, err := fwd.next()
	if !errors.As(err, &problem) || !errors.Is(err, ErrRecordTooLarge) || !problem.Complete || offset != int64(len(tooLarge)) {
		t.Fatalf("oversize forward: %d %v", offset, err)
	}
	next, _, err := fwd.next()
	if err != nil || !bytes.Equal(next, good) {
		t.Fatalf("oversize forward lost next row: %v", err)
	}
	rev, _ := newBackwardPageScanner(context.Background(), big, limit, limit)
	if _, _, err := rev.previous(); err != nil {
		t.Fatal(err)
	}
	_, start, err = rev.previous()
	if !errors.As(err, &problem) || !problem.Complete || start != 0 {
		t.Fatalf("oversize reverse: %d %v", start, err)
	}
}

func TestPagedScannersCancellationIsNotCursorCorruption(t *testing.T) {
	f, limit := openLineFixture(t, []byte("{\"x\":1}\n"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fwd, _ := newForwardPageScanner(ctx, f, 0, limit)
	if _, _, err := fwd.next(); !errors.Is(err, context.Canceled) {
		t.Fatalf("forward canceled: %v", err)
	}
	back, _ := newBackwardPageScanner(ctx, f, limit, limit)
	if _, _, err := back.previous(); !errors.Is(err, context.Canceled) {
		t.Fatalf("reverse canceled: %v", err)
	}
}
