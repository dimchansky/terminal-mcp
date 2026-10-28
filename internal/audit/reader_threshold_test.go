package audit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestBackwardScannerCancellationDoesNotAdvanceCursor(t *testing.T) {
	raw := []byte("{\"content\":\"" + strings.Repeat("x", 220<<10) + "\"}\n")
	f, size := openLineFixture(t, raw)

	// First count how often the context is consulted. Then cancel precisely
	// on the last check, inside finish(), after the preceding boundary is found.
	referenceCtx := &checkedContext{Context: context.Background(), max: 100}
	reference, err := newBackwardPageScanner(referenceCtx, f, size, size)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := reference.previous(); err != nil {
		t.Fatal(err)
	}
	if referenceCtx.calls < 3 {
		t.Fatalf("reverse scanner did not traverse multiple windows: %d", referenceCtx.calls)
	}

	ctx := &checkedContext{Context: context.Background(), max: referenceCtx.calls - 1}
	scanner, err := newBackwardPageScanner(ctx, f, size, size)
	if err != nil {
		t.Fatal(err)
	}
	_, pos, err := scanner.previous()
	if !errors.Is(err, context.Canceled) || pos != size || scanner.offset != size {
		t.Fatalf("canceled backward scanner advanced: %d offset %d error %v", pos, scanner.offset, err)
	}
	_, again, err := scanner.previous()
	if !errors.Is(err, context.Canceled) || again != size {
		t.Fatalf("failed backward scanner resumed after canceled row: pos=%d error=%v", again, err)
	}
}

func TestCorruptLineMaxSpanAgreesWithCursorProof(t *testing.T) {
	prefix := []byte("{\"prefix\":true}\n") // Intentionally unaligned to 64 KiB.
	for _, extra := range []int{0, 1, 65535} {
		t.Run(fmt.Sprint(extra), func(t *testing.T) {
			row := []byte(strings.Repeat("x", int(maxCorruptScan)-1+extra) + "\n")
			f, limit := openLineFixture(t, append(append([]byte{}, prefix...), row...))
			first := int64(len(prefix))
			fwd, err := newForwardPageScanner(context.Background(), f, first, limit)
			if err != nil {
				t.Fatal(err)
			}
			_, next, err := fwd.next()
			var issue *RecordProblem
			if !errors.As(err, &issue) || !errors.Is(err, ErrRecordTooLarge) {
				t.Fatalf("expected oversized row: %v", err)
			}
			if issue.Complete != (extra == 0) {
				t.Fatalf("forward completion disagrees with 16 MiB span boundary: %v extra=%d", issue, extra)
			}
			if issue.Complete {
				if next != limit || next-first > maxCorruptScan {
					t.Fatalf("complete row exceeds allowed proof range: %+v next=%d", issue, next)
				}
				if _, err := boundaryProof(context.Background(), f, next, limit); err != nil {
					t.Fatalf("complete oversized row cannot be cursor-proven: %v", err)
				}
			} else if next != first || fwd.offset != first {
				t.Fatalf("incomplete row unexpectedly advanced cursor: %d offset=%d", next, fwd.offset)
			}
			reverse, err := newBackwardPageScanner(context.Background(), f, limit, limit)
			if err != nil {
				t.Fatal(err)
			}
			_, start, err := reverse.previous()
			if !errors.As(err, &issue) || !errors.Is(err, ErrRecordTooLarge) {
				t.Fatalf("reverse should find oversized row: %v", err)
			}
			if issue.Complete != (extra == 0) {
				t.Fatalf("reverse completion disagrees with 16 MiB span boundary: %v extra=%d", issue, extra)
			}
			if issue.Complete {
				if limit-start > maxCorruptScan {
					t.Fatalf("reverse complete row > proof cap: %+v", issue)
				}
				if _, err := boundaryProof(context.Background(), f, limit, limit); err != nil {
					t.Fatalf("reverse complete row cannot be cursor-proven: %v", err)
				}
			} else if start != limit || reverse.offset != limit {
				t.Fatalf("incomplete reverse scan advanced cursor: %d offset=%d", start, reverse.offset)
			}
		})
	}
}
