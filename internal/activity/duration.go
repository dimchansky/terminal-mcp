package activity

// DurationState distinguishes recorded evidence about an elapsed time.
type DurationState uint8

const (
	// DurationUnknown means the record does not support any duration claim.
	DurationUnknown DurationState = iota
	// DurationMeasured means duration_ms was recorded.
	DurationMeasured
	// DurationSubMillisecond means the elapsed time was under 1 ms.
	DurationSubMillisecond
)

// Duration is a finish record's elapsed time. For request_finish it is the
// MCP request time including audit and any waiting or polling; for
// operation_finish it is the admitted mutation lifecycle, which for
// terminal_start is process launch, not the child's runtime. Durations are
// never derived from wall-clock timestamps.
type Duration struct {
	State        DurationState
	Milliseconds int64 // Valid only for DurationMeasured.
}

// FinishDuration interprets duration_ms of a request_finish or
// operation_finish. startLoaded reports whether the matching start record is
// known: request_start for request_finish, operation_start for
// operation_finish (the writer measures an operation only after its start was
// recorded). The writer omits zero durations (omitempty), so an absent value
// means "under 1 ms" only for a finish of exactly the current schema whose
// matching start is known; otherwise it is unknown, never zero. Omission rules
// of later schema versions are not assumed.
func FinishDuration(finish Event, startLoaded bool) Duration {
	if finish.Kind != KindRequestFinish && finish.Kind != KindOperationFinish {
		return Duration{}
	}
	if finish.HasDuration {
		switch {
		case finish.DurationMS > 0:
			return Duration{State: DurationMeasured, Milliseconds: finish.DurationMS}
		case finish.DurationMS == 0:
			// The writer never emits an explicit zero; it still states <1 ms.
			return Duration{State: DurationSubMillisecond}
		default:
			return Duration{}
		}
	}
	if !finish.Legacy && finish.SchemaVersion == currentSchemaVersion && startLoaded {
		return Duration{State: DurationSubMillisecond}
	}
	return Duration{}
}
