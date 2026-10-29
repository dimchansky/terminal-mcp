package audit

import "os"

// Segment is a journal file known to the running writer. Archive 0 is the
// active filename; positive archive numbers are sealed, numbered files.
// Sealed covers only rotations by this process; earlier archives are found
// separately by the reader. Archive is a pathname hint, not proof of identity.
// [0, Baseline) was already on disk when this writer opened the file and is
// historical (not independently certified by a successful append in this
// process). [Baseline, Size) consists of writer-acknowledged bytes.
// Size never includes a failed append, even if its complete line remains on
// disk after failed repair. Identity is captured from the opened writer handle.
type Segment struct {
	Archive  uint64
	Size     int64
	Baseline int64

	identity os.FileInfo
}

// SameFile checks the identity of an already-opened reader handle. A pathname
// stat is not sufficient: rotation can replace the active name between calls.
func (s Segment) SameFile(info os.FileInfo) bool {
	return s.identity != nil && info != nil && os.SameFile(s.identity, info)
}

// Observation is an immutable point-in-time boundary for the audit reader.
// Sealed is copied on Observe so caller modifications cannot affect the writer.
// Changed closes on a successful new append, or on terminal Close (check
// Closed after wake-up); rotation or failed writes never signal new data.
// No audit mutex or disk I/O is needed to take an observation.
type Observation struct {
	Active    Segment
	HasActive bool
	Sealed    []Segment
	// NextArchive is the next suffix the writer could create. Together with
	// Sealed it distinguishes preexisting archives from in-process rotations.
	// It is published atomically with the committed byte boundary and Changed.
	NextArchive uint64
	Closed      bool
	Disabled    bool
	Changed     <-chan struct{}
}

type observationState struct {
	value   Observation
	changed chan struct{}
}

var disabledWake = func() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}()

// Observe does not acquire Log.mu and remains responsive if the sink stalls.
// A nil log or a non-file test sink has no journal to observe.
func (l *Log) Observe() Observation {
	if l == nil {
		return Observation{Disabled: true, Changed: disabledWake}
	}
	state := l.observation.Load()
	if state == nil {
		return Observation{Disabled: true, Changed: disabledWake}
	}
	value := state.value
	value.Sealed = append([]Segment(nil), value.Sealed...)
	return value
}

// publishObservation is called under Log.mu, except during newLog's initial
// construction before the pointer can escape. Sealed slices are
// copy-on-rotate at the sink, and the snapshot never references sink-owned
// mutable storage. Publish the file relocation even if the append failed,
// but advance/wake only on successfully acknowledged new record bytes.
func (l *Log) publishObservation(s *fileSink, appended, closed bool) {
	if s == nil {
		return
	}
	previous := l.observation.Load()
	next := &observationState{}
	if previous == nil {
		next.changed = make(chan struct{})
	} else {
		next.changed = previous.changed
	}
	if appended {
		next.changed = make(chan struct{})
	}
	next.value = Observation{
		Sealed:      s.sealed,
		NextArchive: s.next,
		Closed:      closed,
		Changed:     next.changed,
	}
	if s.identity != nil {
		next.value.HasActive = true
		next.value.Active = Segment{
			Size: s.size, Baseline: s.baseline, identity: s.identity,
		}
	}
	l.observation.Store(next)
	if previous != nil && (appended || closed) {
		close(previous.changed)
	} else if previous == nil && closed {
		close(next.changed)
	}
}
