package audit

// ReadBudget caps a single history request. A large record can consume more
// than the remaining scan-byte allowance to reach the next newline; that row
// alone may be returned as deferred, so even sparse searches always progress.
type ReadBudget struct {
	MaxRecords   int
	MaxBytes     int64 // Maximum original JSONL bytes included in this page.
	MaxScanBytes int64 // Soft scan limit, including deferred/corrupt records.
}

func (b ReadBudget) normalize() ReadBudget {
	if b.MaxRecords <= 0 {
		b.MaxRecords = 100
	}
	if b.MaxRecords > 1000 {
		b.MaxRecords = 1000
	}
	if b.MaxBytes <= 0 {
		b.MaxBytes = 1 << 20
	}
	if b.MaxBytes > 8<<20 {
		b.MaxBytes = 8 << 20
	}
	if b.MaxScanBytes <= 0 {
		b.MaxScanBytes = 8 << 20
	}
	if b.MaxScanBytes > 32<<20 {
		b.MaxScanBytes = 32 << 20
	}
	return b
}

// HistoryRecord is a single persisted JSONL record or a visible error at the
// exact source offsets. Data is byte-exact (including newline), unless Deferred
// marks a size-budgeted full-input fetch via ReadRecord. Corrupt records are
// never silently discarded; they retain precise Problem.Start/End offsets,
// but have no valid RecordRef yet. Forensic raw access to damaged ranges is
// assigned to #4d.4; nothing is removed from the original journal.
type HistoryRecord struct {
	Ref        RecordRef
	Data       []byte
	Size       int64
	Deferred   bool
	Historical bool
	Problem    *RecordProblem
}

// HistoryPage describes a bounded historical scan. Ordinary ReadPage and
// the single-segment primitive leave live fields unset. WaitNextPage sets
// them from the SAME immutable observation used for its page scan.
type HistoryPage struct {
	Records      []HistoryRecord
	Next         Cursor
	ScannedBytes int64
	HasMore      bool
	AtLiveEdge   bool            // This page reached the end of its frozen observation.
	Closed       bool            // The writer closed AND the observed backlog is exhausted.
	Changed      <-chan struct{} // Same-generation wake hint; nil when Closed.
}
