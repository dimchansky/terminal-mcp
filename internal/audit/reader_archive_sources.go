package audit

import (
	"context"
	"errors"
	"os"
	"slices"
)

// archiveSource is a logical location within an immutable writer snapshot.
// The active source is always last, even though its filename has suffix zero.
// The opened active file may have moved to snapshot.NextArchive since capture.
type archiveSource struct {
	suffix uint64
	active bool
}

const maxPageNonemptySegments = 256

// Empty historical segments are already bounded by maxDirectoryEntries in
// archiveSuffixes. A second lower cap would make a long empty sequence
// permanently unpassable: no legitimate cursor exists within an empty file.

func (r *Reader) frozenSources(ctx context.Context, snapshot journalSnapshot, historical bool) ([]archiveSource, error) {
	state := snapshot.observation
	list := make([]archiveSource, 0, len(state.Sealed)+1)
	if historical {
		suffixes, err := r.archiveSuffixes(ctx)
		if err != nil {
			return nil, err
		}
		first := state.NextArchive - uint64(len(state.Sealed))
		for _, suffix := range suffixes {
			if suffix < first {
				list = append(list, archiveSource{suffix: suffix})
			}
		}
	}
	for _, segment := range state.Sealed {
		list = append(list, archiveSource{suffix: segment.Archive})
	}
	if state.HasActive {
		list = append(list, archiveSource{active: true})
	}
	return list, nil
}

func (r *Reader) openArchiveSource(ctx context.Context, snapshot journalSnapshot, source archiveSource) (*readerSegment, error) {
	if source.active {
		seg, err := r.openCapturedActive(ctx, snapshot)
		if err != nil {
			return nil, err
		}
		// The writer's NEXT suffix is the eventual pathname of this inode,
		// regardless of how many more rotations happen after the snapshot.
		// RecordRef and Cursor use this as a hint; their identity remains the
		// source segment hash and exact bytes, not the hint.
		seg.archive = snapshot.observation.NextArchive
		return seg, nil
	}
	seg, err := r.openFrozen(ctx, snapshot, source.suffix)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrHistoryGap // An inventoried archive vanished mid-request.
	}
	return seg, err
}

func sourceHintMatches(snapshot journalSnapshot, source archiveSource, hint uint64) bool {
	if source.active {
		return hint == 0 || hint == snapshot.observation.NextArchive
	}
	return source.suffix == hint
}

// frozenCursorSource finds an already-audited cursor in one snapshot. It
// checks the hinted segment first, without scanning the full journal history;
// old hint=0 cursors get a bounded fallback across recent archived sources.
// The caller owns the returned handle, and ALL candidate opens validate the
// frozen observation before hashing a single source byte.
func (r *Reader) frozenCursorSource(ctx context.Context, snapshot journalSnapshot,
	cursor cursorState, sources []archiveSource, withHistory bool) (*readerSegment, int, error) {
	var candidateError error
	checked := make(map[int]bool, maxArchiveCandidates+3)
	test := func(index int) (*readerSegment, bool, error) {
		if checked[index] {
			return nil, false, nil
		}
		checked[index] = true
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		source := sources[index]
		seg, err := r.openArchiveSource(ctx, snapshot, source)
		if err != nil {
			if errors.Is(err, errOutsideSnapshot) || errors.Is(err, ErrInvalidCursor) {
				return nil, false, nil
			}
			if errors.Is(err, ErrHistoryGap) && sourceHintMatches(snapshot, source, cursor.hint) {
				return nil, false, err
			}
			if candidateError == nil {
				candidateError = err
			}
			return nil, false, nil
		}
		if seg.key != cursor.segment || cursor.offset > seg.size {
			_ = seg.close()
			return nil, false, nil
		}
		if err := verifyCursorBoundary(ctx, seg.file, seg.size, cursor); err != nil {
			_ = seg.close()
			if errors.Is(err, ErrInvalidCursor) {
				return nil, false, nil
			}
			return nil, false, err
		}
		return seg, true, nil
	}
	for i, source := range sources {
		if sourceHintMatches(snapshot, source, cursor.hint) {
			seg, found, err := test(i)
			if found || err != nil {
				return seg, i, err
			}
		}
	}
	// A relocated old hint=0 cursor may refer to a recently sealed source.
	// New cursors use NextArchive and resolve directly, even after 20+ rotates.
	tried := 0
	for i := len(sources) - 1; i >= 0 && tried < maxArchiveCandidates; i-- {
		if checked[i] {
			continue
		}
		seg, found, err := test(i)
		tried++
		if found || err != nil {
			return seg, i, err
		}
	}
	if candidateError != nil {
		return nil, -1, candidateError
	}
	if cursor.hint < snapshot.observation.NextArchive && !withHistory &&
		cursor.hint > 0 && !slices.ContainsFunc(sources, func(src archiveSource) bool {
		return sourceHintMatches(snapshot, src, cursor.hint)
	}) {
		// Need historical inventory to know whether an old archive existed.
		return nil, -1, errNeedHistorical
	}
	return nil, -1, ErrInvalidCursor
}

// errNeedHistorical signals that the caller should load historical names
// on demand. It never crosses the viewer API boundary.
var errNeedHistorical = errors.New("historical archive inventory required")
