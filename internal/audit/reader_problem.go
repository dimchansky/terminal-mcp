package audit

import "fmt"

// RecordProblem reports damaged or oversized source input without silently
// discarding it. Complete indicates whether End is a verified newline
// boundary: only complete problems may be skipped by a paged history reader.
// A partial tail or exhausted scan budget is an explicit non-skippable gap.
type RecordProblem struct {
	Cause    error
	Start    int64
	End      int64
	Complete bool
}

func (e *RecordProblem) Error() string {
	return fmt.Sprintf("%v at journal byte range [%d,%d)", e.Cause, e.Start, e.End)
}

func (e *RecordProblem) Unwrap() error { return e.Cause }
