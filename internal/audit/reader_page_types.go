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

// HistoryRecord is a single persisted JSONL line or a visible problem at its
// exact source offsets. Valid lines include byte-exact Data (with newline), or
// Deferred means ReadRecord is needed due to the page's byte budget.
// Complete malformed lines up to 4 MiB have Problem and a verifiable Ref for
// fetching the original bytes via ReadRecord, but never inline Data or Deferred.
// Complete oversized lines have no Ref; an incomplete tail stops pagination
// with an error instead of becoming a row. Original bytes remain on disk.
// A Ref alone does not guarantee valid JSON: always inspect Problem.
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
