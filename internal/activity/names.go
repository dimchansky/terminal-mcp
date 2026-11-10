package activity

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

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

// String is the occurrence's wire ID: the raw reader reference for rows that
// have one; weak keys are spelled out ("w:" positioned after an older
// reference, "p:" provisional) and cannot be loaded by reference.
func (k OccurrenceKey) String() string {
	if k.Ref != "" {
		return k.Ref
	}
	w := k.Weak
	if w.Provisional != "" {
		return fmt.Sprintf("p:%s:%d-%d", w.Provisional, w.Start, w.End)
	}
	return fmt.Sprintf("w:%s:%d:%d-%d", w.OlderRef, w.Between, w.Start, w.End)
}

func (k OccurrenceKey) MarshalText() ([]byte, error) { return []byte(k.String()), nil }

// UnmarshalText parses a wire ID written by String.
func (k *OccurrenceKey) UnmarshalText(b []byte) error {
	s := string(b)
	*k = OccurrenceKey{}
	invalid := fmt.Errorf("activity: invalid occurrence %q", s)
	if s == "" {
		return invalid
	}
	weak := strings.HasPrefix(s, "w:")
	if !weak && !strings.HasPrefix(s, "p:") {
		k.Ref = s
		return nil
	}
	rest := s[2:]
	i := strings.LastIndexByte(rest, ':')
	if i < 0 {
		return invalid
	}
	start, end, ok := strings.Cut(rest[i+1:], "-")
	var err1, err2 error
	k.Weak.Start, err1 = strconv.ParseInt(start, 10, 64)
	k.Weak.End, err2 = strconv.ParseInt(end, 10, 64)
	if !ok || err1 != nil || err2 != nil {
		return invalid
	}
	head := rest[:i]
	if !weak {
		k.Weak.Provisional = head
		return nil
	}
	j := strings.LastIndexByte(head, ':')
	if j < 0 {
		return invalid
	}
	between, err := strconv.Atoi(head[j+1:])
	if err != nil {
		return invalid
	}
	k.Weak.OlderRef, k.Weak.Between = head[:j], between
	return nil
}

// orderJSON is the wire form of an order: live keys sort after history keys,
// then by rank (valid within one window generation).
type orderJSON struct {
	Live bool  `json:"live"`
	Rank int64 `json:"rank"`
}

func (o Order) MarshalJSON() ([]byte, error) { return json.Marshal(orderJSON{o.live, o.rank}) }

func (o *Order) UnmarshalJSON(b []byte) error {
	var w orderJSON
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	o.live, o.rank = w.Live, w.Rank
	return nil
}
