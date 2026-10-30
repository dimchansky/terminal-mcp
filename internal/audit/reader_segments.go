package audit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const (
	maxArchiveCandidates = 16
	maxDirectoryEntries  = 100000
)

var (
	ErrHistoryGap       = errors.New("audit history segment disappeared")
	ErrHistoryDisabled  = errors.New("audit history is disabled")
	ErrHistoryScanLimit = errors.New("audit history directory exceeds scan budget")
)

// Reader is a strictly read-only view of the selected running writer's journal.
// It owns no second log, no index, and no writer locks.
type Reader struct {
	writer *Log
	path   string

	cacheMu sync.Mutex // The cache never takes the writer mutex.
	keys    []segmentFingerprintCache
}

type segmentFingerprintCache struct {
	info              os.FileInfo // Captured from an opened descriptor, not path Stat.
	key               [32]byte
	prefixLen         int
	prefixHash        [32]byte
	limit             int64 // Incomplete first line must be recomputed as size changes.
	firstLineComplete bool
}

func NewReader(writer *Log) (*Reader, error) {
	if writer == nil || writer.path == "" || writer.Observe().Disabled {
		return nil, ErrHistoryDisabled
	}
	return &Reader{writer: writer, path: writer.path}, nil
}

type readerSegment struct {
	file     *os.File
	archive  uint64
	size     int64
	baseline int64
	key      [32]byte
}

func (segment *readerSegment) close() error {
	if segment == nil || segment.file == nil {
		return nil
	}
	return segment.file.Close()
}

// openObservedActive detects rotation between reading an observation and
// opening the active pathname. A test can deliberately rotate in afterObserve.
// Production callers supply nil; no lock is taken around file I/O.
func (r *Reader) openObservedActive(ctx context.Context, afterObserve func()) (*readerSegment, error) {
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		snapshot := r.writer.Observe()
		if snapshot.Disabled {
			return nil, ErrHistoryDisabled
		}
		if !snapshot.HasActive {
			return nil, os.ErrNotExist
		}
		if afterObserve != nil {
			afterObserve()
			afterObserve = nil
		}
		opened, err := r.openOne(ctx, 0)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrInvalidCursor) {
			continue // Rotation may have changed the active pathname.
		}
		return opened, err
	}
	return nil, ErrInvalidCursor
}

// openOne uses the latest published observation for individual detail/cursor
// lookups. History pages instead call openFrozen using the SAME snapshot for
// every segment; that prevents the second Observe from including new appends.
func (r *Reader) openOne(ctx context.Context, suffix uint64) (*readerSegment, error) {
	return r.openWithSnapshot(ctx, suffix, nil)
}

func (r *Reader) openFrozen(ctx context.Context, snapshot journalSnapshot, suffix uint64) (*readerSegment, error) {
	return r.openWithSnapshot(ctx, suffix, &snapshot)
}

// openCapturedActive follows the frozen active inode if the writer renamed it
// between capture and opening. Only the two writer-defined names are tried:
// the active pathname and the snapshot's NextArchive suffix. No new writer
// observation is taken, and no new active generation is accepted.
func (r *Reader) openCapturedActive(ctx context.Context, snapshot journalSnapshot) (*readerSegment, error) {
	if !snapshot.observation.HasActive {
		return nil, errOutsideSnapshot
	}
	active, err := r.openFrozen(ctx, snapshot, 0)
	if err == nil {
		return active, nil
	}
	if !errors.Is(err, errOutsideSnapshot) && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return r.openFrozen(ctx, snapshot, snapshot.observation.NextArchive)
}

func (r *Reader) openWithSnapshot(ctx context.Context, suffix uint64, frozen *journalSnapshot) (*readerSegment, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if frozen != nil {
		if suffix == 0 && !frozen.observation.HasActive {
			return nil, errOutsideSnapshot
		}
		if suffix > frozen.observation.NextArchive ||
			(suffix == frozen.observation.NextArchive && !frozen.observation.HasActive) {
			return nil, errOutsideSnapshot
		}
	}
	name := r.path
	if suffix != 0 {
		name = fmt.Sprintf("%s.%d", r.path, suffix)
	}
	f, err := openAuditReader(name)
	if err != nil {
		return nil, err
	}
	// Capture the selected observation after opening for ordinary lookups,
	// but never refresh the supplied frozen snapshot in the page path.
	var snapshot journalSnapshot
	if frozen != nil {
		snapshot = *frozen
	} else {
		snapshot, err = r.captureSnapshot()
		if err != nil {
			return nil, errors.Join(err, f.Close())
		}
	}
	// Check the OPEN DESCRIPTOR's identity before fingerprinting any bytes.
	info, err := f.Stat()
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	limit, baseline, err := snapshot.bounds(suffix, info)
	if err != nil {
		// Legacy non-paged consumers already classify transient replacement
		// of the active path as an invalid cursor and retry independently.
		if frozen == nil && errors.Is(err, errOutsideSnapshot) {
			err = ErrInvalidCursor
		}
		return nil, errors.Join(err, f.Close())
	}
	result := &readerSegment{file: f, archive: suffix, size: limit, baseline: baseline}
	if limit > 0 {
		result.key, err = r.fingerprint(ctx, f, info, limit)
		if err != nil {
			return nil, errors.Join(err, f.Close())
		}
	}
	return result, nil
}

func (r *Reader) fingerprint(ctx context.Context, f *os.File, info os.FileInfo, limit int64) ([32]byte, error) {
	if err := ctx.Err(); err != nil {
		return [32]byte{}, err
	}
	prefix := make([]byte, int(min(int64(readerBlockBytes), limit)))
	if err := readExactAt(f, prefix, 0); err != nil {
		return [32]byte{}, err
	}
	sample := sha256.Sum256(prefix)
	complete := bytes.IndexByte(prefix, '\n') >= 0
	r.cacheMu.Lock()
	for i, previous := range r.keys {
		if !os.SameFile(previous.info, info) {
			continue
		}
		if previous.prefixLen == len(prefix) && previous.prefixHash == sample &&
			(previous.firstLineComplete || previous.limit == limit) {
			key := previous.key
			r.cacheMu.Unlock()
			return key, nil
		}
		// The same inode may have been rewritten or reused. Invalidate its
		// cached fingerprint before recomputing the actual first-line key.
		r.keys = append(r.keys[:i], r.keys[i+1:]...)
		break
	}
	r.cacheMu.Unlock()
	key, err := segmentFingerprint(ctx, f, limit)
	if err != nil {
		return key, err
	}
	r.cacheMu.Lock()
	if len(r.keys) == 32 {
		copy(r.keys, r.keys[1:])
		r.keys = r.keys[:31]
	}
	r.keys = append(r.keys, segmentFingerprintCache{info: info, key: key,
		prefixLen: len(prefix), prefixHash: sample, firstLineComplete: complete, limit: limit})
	r.cacheMu.Unlock()
	return key, nil
}

// archiveSuffixes reads bounded directory batches rather than os.ReadDir's
// unbounded list allocation. Sparse suffixes are legitimate and never in
// themselves prove that history was deleted.
func (r *Reader) archiveSuffixes(ctx context.Context) ([]uint64, error) {
	dir, err := os.Open(filepath.Dir(r.path))
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()
	prefix := filepath.Base(r.path) + "."
	var result []uint64
	count := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		names, err := dir.Readdirnames(256)
		count += len(names)
		if count > maxDirectoryEntries {
			return nil, ErrHistoryScanLimit
		}
		for _, name := range names {
			text, ok := strings.CutPrefix(name, prefix)
			if !ok || text == "" || strings.Trim(text, "0123456789") != "" {
				continue
			}
			n, convErr := strconv.ParseUint(text, 10, 64)
			if convErr == nil && n > 0 && strconv.FormatUint(n, 10) == text {
				result = append(result, n)
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result, nil
}

// resolveCursor tries the exact pathname hint and current file first; this
// hot path needs neither an archive directory scan nor an index. On relocation,
// search a small bounded group of recent archives. A known missing assigned
// pathname cannot be replaced by a byte-identical row from another archive.
func (r *Reader) resolveCursor(ctx context.Context, cursor cursorState) (*readerSegment, error) {
	seen := make(map[uint64]bool, maxArchiveCandidates+2)
	knownMissing := false
	var candidateFailure error
	try := func(suffix uint64) (*readerSegment, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		snapshot := r.writer.Observe()
		if snapshot.Disabled {
			return nil, ErrHistoryDisabled
		}
		if suffix == 0 && !snapshot.HasActive {
			if cursor.hint == 0 {
				knownMissing = true
			}
			return nil, nil
		}
		opened, err := r.openOne(ctx, suffix)
		if errors.Is(err, os.ErrNotExist) {
			if suffix == cursor.hint {
				knownMissing = true
			}
			return nil, nil
		}
		if errors.Is(err, ErrInvalidCursor) {
			return nil, nil // Path reused or opened during active rotation.
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if errors.Is(err, ErrHistoryDisabled) {
				return nil, err
			}
			// An unrelated archive can be corrupt, inaccessible, or pending
			// Windows deletion. It must not make an intact cursor unreachable.
			if candidateFailure == nil {
				candidateFailure = err
			}
			return nil, nil
		}
		if opened.key != cursor.segment {
			_ = opened.close()
			return nil, nil
		}
		if err := verifyCursorBoundary(ctx, opened.file, opened.size, cursor); err != nil {
			_ = opened.close()
			if errors.Is(err, ErrInvalidCursor) {
				return nil, nil // Another segment can share its first-line key.
			}
			return nil, err
		}
		return opened, nil
	}
	for _, suffix := range []uint64{cursor.hint, 0} {
		if seen[suffix] {
			continue
		}
		seen[suffix] = true
		// A nonzero hint identifies the suffix this generation will get on
		// rotation. A different active generation cannot substitute for a
		// missing archive merely because its record bytes match.
		if suffix == 0 && cursor.hint != 0 && r.writer.Observe().NextArchive != cursor.hint {
			continue
		}
		opened, err := try(suffix)
		if opened != nil && suffix == 0 && cursor.hint != 0 &&
			r.writer.Observe().NextArchive != cursor.hint {
			_ = opened.close()
			opened = nil // Rotation won; retry the archive below.
		}
		if opened != nil || err != nil {
			return opened, err
		}
	}
	if knownMissing && cursor.hint != 0 {
		// The assigned file may have appeared during a concurrent rotation.
		// Retry the hinted pathname, but do not substitute another archive
		// merely because its first line and record bytes happen to match.
		knownMissing = false
		opened, err := try(cursor.hint)
		if opened != nil || err != nil {
			return opened, err
		}
		if knownMissing {
			return nil, ErrHistoryGap
		}
	}
	suffixes, err := r.archiveSuffixes(ctx)
	if err != nil {
		return nil, err
	}
	tried := 0
	for i := len(suffixes) - 1; i >= 0 && tried < maxArchiveCandidates; i-- {
		suffix := suffixes[i]
		if seen[suffix] {
			continue
		}
		seen[suffix] = true
		tried++
		opened, err := try(suffix)
		if opened != nil || err != nil {
			return opened, err
		}
	}
	if knownMissing && cursor.hint != 0 {
		return nil, ErrHistoryGap
	}
	if candidateFailure != nil {
		return nil, fmt.Errorf("%w: candidate unavailable: %w", ErrInvalidCursor, candidateFailure)
	}
	return nil, ErrInvalidCursor
}
