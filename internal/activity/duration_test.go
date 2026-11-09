package activity

import "testing"

func TestFinishDuration(t *testing.T) {
	tests := []struct {
		name        string
		raw         string
		startLoaded bool
		want        Duration
	}{
		{"measured request", `{"schema_version":2,"phase":"request_finish","duration_ms":38}`, true, Duration{DurationMeasured, 38}},
		{"measured without start", `{"schema_version":2,"phase":"request_finish","duration_ms":38}`, false, Duration{DurationMeasured, 38}},
		{"measured operation", `{"schema_version":2,"phase":"operation_finish","duration_ms":15}`, true, Duration{DurationMeasured, 15}},
		{"omitted with start", `{"schema_version":2,"phase":"request_finish"}`, true, Duration{State: DurationSubMillisecond}},
		{"omitted operation with start", `{"schema_version":2,"phase":"operation_finish"}`, true, Duration{State: DurationSubMillisecond}},
		{"omitted without start", `{"schema_version":2,"phase":"request_finish"}`, false, Duration{}},
		{"explicit zero", `{"schema_version":2,"phase":"request_finish","duration_ms":0}`, false, Duration{State: DurationSubMillisecond}},
		{"negative", `{"schema_version":2,"phase":"request_finish","duration_ms":-5}`, true, Duration{}},
		{"older schema omitted", `{"schema_version":1,"phase":"request_finish"}`, true, Duration{}},
		{"future schema omitted", `{"schema_version":3,"phase":"request_finish"}`, true, Duration{}},
		{"future schema measured", `{"schema_version":3,"phase":"request_finish","duration_ms":4}`, true, Duration{DurationMeasured, 4}},
		{"legacy rejected with duration", `{"phase":"rejected","duration_ms":4}`, true, Duration{}},
		{"legacy omitted", `{"phase":"finish"}`, true, Duration{}},
		{"legacy measured", `{"phase":"finish","duration_ms":2}`, true, Duration{DurationMeasured, 2}},
		{"not a finish", `{"schema_version":2,"phase":"request_start","duration_ms":9}`, true, Duration{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, err := Decode([]byte(tt.raw))
			if err != nil {
				t.Fatal(err)
			}
			if got := FinishDuration(e, tt.startLoaded); got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}
