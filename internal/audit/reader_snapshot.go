package audit

import (
	"errors"
	"os"
)

// errOutsideSnapshot means that the opened pathname refers to a segment
// created after the captured observation. It is not an invalid user cursor:
// a history page should stop at its frozen frontier and catch up later.
var errOutsideSnapshot = errors.New("audit segment is outside the captured snapshot")

// journalSnapshot pins writer-acknowledged boundaries and the wake-up channel
// in one atomic observation. It is never refreshed while reading a page.
type journalSnapshot struct {
	observation Observation
}

func (r *Reader) captureSnapshot() (journalSnapshot, error) {
	if r == nil || r.writer == nil {
		return journalSnapshot{}, ErrHistoryDisabled
	}
	snapshot := r.writer.Observe()
	if snapshot.Disabled {
		return journalSnapshot{}, ErrHistoryDisabled
	}
	if snapshot.NextArchive == 0 {
		return journalSnapshot{}, ErrHistoryDisabled
	}
	if uint64(len(snapshot.Sealed)) >= snapshot.NextArchive {
		return journalSnapshot{}, ErrInvalidCursor
	}
	first := snapshot.NextArchive - uint64(len(snapshot.Sealed))
	for i, segment := range snapshot.Sealed {
		if segment.Archive != first+uint64(i) || segment.identity == nil ||
			segment.Size < segment.Baseline || segment.Baseline < 0 {
			return journalSnapshot{}, ErrInvalidCursor
		}
	}
	return journalSnapshot{observation: snapshot}, nil
}

// bounds validates the identity of an already-opened reader handle before
// ANY source bytes are read or fingerprinted. Archive suffixes earlier than
// first are historical and not retroactively OS-acknowledged. Archives
// created by this writer must match its sealed in-process identities.
func (snapshot journalSnapshot) bounds(suffix uint64, info os.FileInfo) (size, baseline int64, err error) {
	s := snapshot.observation
	if info == nil || !info.Mode().IsRegular() || info.Size() < 0 {
		return 0, 0, ErrInvalidCursor
	}
	if s.NextArchive == 0 || uint64(len(s.Sealed)) >= s.NextArchive {
		return 0, 0, ErrInvalidCursor
	}
	first := s.NextArchive - uint64(len(s.Sealed))
	active := s.HasActive && s.Active.SameFile(info)

	// Do not silently accept an ambiguous identity. This would let one
	// physical file masquerade as both a live and a sealed segment.
	for i := range s.Sealed {
		if s.Sealed[i].SameFile(info) {
			if active || suffix != s.Sealed[i].Archive {
				return 0, 0, ErrInvalidCursor
			}
		}
	}
	if suffix == 0 {
		if !active {
			return 0, 0, errOutsideSnapshot
		}
		return checkedSegmentBounds(s.Active, info)
	}

	if suffix < first {
		// Before this process started rotating: complete physical history,
		// but NOT certified as acknowledged by the current process.
		if active {
			return 0, 0, ErrInvalidCursor
		}
		return info.Size(), info.Size(), nil
	}
	if suffix < s.NextArchive {
		segment := s.Sealed[suffix-first]
		if segment.Archive != suffix || !segment.SameFile(info) {
			return 0, 0, ErrInvalidCursor // A writer-owned archive was replaced.
		}
		return checkedSegmentBounds(segment, info)
	}
	if suffix == s.NextArchive && active {
		// This was the active file at snapshot time. It was rotated AFTER
		// observation, so only its previously acknowledged bytes are visible.
		return checkedSegmentBounds(s.Active, info)
	}
	return 0, 0, errOutsideSnapshot
}

func checkedSegmentBounds(segment Segment, info os.FileInfo) (size, baseline int64, err error) {
	if segment.Size < segment.Baseline || segment.Baseline < 0 ||
		info.Size() < segment.Size {
		return 0, 0, ErrInvalidCursor
	}
	return segment.Size, segment.Baseline, nil
}
