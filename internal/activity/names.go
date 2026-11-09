package activity

import "fmt"

// Wire names of the view enums. JSON carries these names, never the
// numeric values, so clients do not depend on declaration order.

var (
	attributionNames   = []string{OriginNotLoaded: "origin_not_loaded", Unattributed: "unattributed", Attributed: "attributed"}
	inputStateNames    = []string{InputUnknown: "unknown", InputRecorded: "recorded", InputLegacy: "legacy", InputNotRecorded: "not_recorded"}
	requestStateNames  = []string{RequestFinished: "finished", RequestOpen: "open", RequestUnknown: "unknown"}
	effectStateNames   = []string{EffectFinished: "finished", EffectInFlight: "in_flight", EffectUnknown: "unknown"}
	linkKindNames      = []string{LinkOwn: "own", LinkReplay: "replay", LinkObserve: "observe"}
	durationStateNames = []string{DurationUnknown: "unknown", DurationMeasured: "measured", DurationSubMillisecond: "sub_millisecond"}
)

func enumName(names []string, v uint8) string {
	if int(v) < len(names) {
		return names[v]
	}
	return ""
}

func (a Attribution) String() string   { return enumName(attributionNames, uint8(a)) }
func (s InputState) String() string    { return enumName(inputStateNames, uint8(s)) }
func (s RequestState) String() string  { return enumName(requestStateNames, uint8(s)) }
func (s EffectState) String() string   { return enumName(effectStateNames, uint8(s)) }
func (k LinkKind) String() string      { return enumName(linkKindNames, uint8(k)) }
func (s DurationState) String() string { return enumName(durationStateNames, uint8(s)) }

func (a Attribution) MarshalText() ([]byte, error)   { return []byte(a.String()), nil }
func (s InputState) MarshalText() ([]byte, error)    { return []byte(s.String()), nil }
func (s RequestState) MarshalText() ([]byte, error)  { return []byte(s.String()), nil }
func (s EffectState) MarshalText() ([]byte, error)   { return []byte(s.String()), nil }
func (k LinkKind) MarshalText() ([]byte, error)      { return []byte(k.String()), nil }
func (s DurationState) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

func parseEnum(names []string, text []byte) (uint8, error) {
	if len(text) == 0 {
		return 0, nil // An unset value.
	}
	for i, n := range names {
		if n != "" && n == string(text) {
			return uint8(i), nil
		}
	}
	return 0, fmt.Errorf("activity: unknown value %q", text)
}

func (a *Attribution) UnmarshalText(b []byte) error {
	v, err := parseEnum(attributionNames, b)
	*a = Attribution(v)
	return err
}

func (s *InputState) UnmarshalText(b []byte) error {
	v, err := parseEnum(inputStateNames, b)
	*s = InputState(v)
	return err
}

func (s *RequestState) UnmarshalText(b []byte) error {
	v, err := parseEnum(requestStateNames, b)
	*s = RequestState(v)
	return err
}

func (s *EffectState) UnmarshalText(b []byte) error {
	v, err := parseEnum(effectStateNames, b)
	*s = EffectState(v)
	return err
}

func (k *LinkKind) UnmarshalText(b []byte) error {
	v, err := parseEnum(linkKindNames, b)
	*k = LinkKind(v)
	return err
}

func (s *DurationState) UnmarshalText(b []byte) error {
	v, err := parseEnum(durationStateNames, b)
	*s = DurationState(v)
	return err
}
