package audit

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestHistoricalTailReportsIncompleteInputWithoutLosingEarlierRecords(t *testing.T) {
	for _, tc := range []struct {
		name       string
		data       string
		validEnd   int64
		incomplete bool
	}{
		{"empty", "", 0, false},
		{"only-complete", "{\"a\":1}\n{\"b\":2}\n", 16, false},
		{"one-incomplete", "{\"a\":1}\n{\"partial\":true", 8, true},
		{"no-complete-record", "{\"partial\":true", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, limit := openLineFixture(t, []byte(tc.data))
			end, problem, err := historicalTail(context.Background(), f, limit)
			if err != nil || end != tc.validEnd {
				t.Fatalf("historical tail: end=%d want=%d err=%v", end, tc.validEnd, err)
			}
			if tc.incomplete {
				if problem == nil || !errors.Is(problem, ErrCorruptRecord) || problem.Complete ||
					problem.Start != end || problem.End != limit {
					t.Fatalf("incomplete source not represented honestly: %+v", problem)
				}
			} else if problem != nil {
				t.Fatalf("complete history incorrectly called corrupt: %+v", problem)
			}
		})
	}
}

func TestHistoricalTailCancellationAndDeclaredFrontierTruncation(t *testing.T) {
	f, size := openLineFixture(t, []byte("{\"a\":1}\n{\"pending\":true"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := historicalTail(ctx, f, size); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled reader continued: %v", err)
	}
	if _, _, err := historicalTail(context.Background(), f, -1); !errors.Is(err, ErrRecordBoundary) {
		t.Fatalf("negative frontier accepted: %v", err)
	}
	if _, _, err := historicalTail(context.Background(), f, size+10); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("physical truncation mislabeled as complete historical range: %v", err)
	}
}

func TestHistoricalTailStopsBeforeUnboundedCorruption(t *testing.T) {
	f, size := openLineFixture(t, []byte(strings.Repeat("z", int(maxCorruptScan)+1)))
	end, issue, err := historicalTail(context.Background(), f, size)
	if !errors.Is(err, ErrRecordTooLarge) || end != 0 || issue == nil ||
		issue.Complete || !errors.Is(issue, ErrRecordTooLarge) || issue.End != size {
		t.Fatalf("unbounded corrupt tail must be a visible non-skippable problem: %+v, %v", issue, err)
	}
}

func TestHistoricalTailAcrossBlockBoundaries(t *testing.T) {
	prefix := []byte("{\"valid\":true}\n")
	for _, tailLen := range []int{readerBlockBytes - 1, readerBlockBytes, readerBlockBytes + 1,
		int(maxCorruptScan) - readerBlockBytes} {
		data := append(append([]byte{}, prefix...), strings.Repeat("z", tailLen)...)
		f, size := openLineFixture(t, data)
		end, issue, err := historicalTail(context.Background(), f, size)
		if err != nil || end != int64(len(prefix)) || issue == nil || issue.Complete ||
			issue.Start != end || issue.End != size {
			t.Fatalf("partial tail of %d bytes: end=%d issue=%+v err=%v", tailLen, end, issue, err)
		}
	}
}
