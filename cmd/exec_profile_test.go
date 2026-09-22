package cmd

import (
	"testing"
	"time"
)

func TestParseProfileTimestamp(t *testing.T) {
	cases := []struct {
		input   string
		wantMs  int64
		wantErr bool
	}{
		{"1700000000000", 1700000000000, false},
		{"2023-11-14T22:13:20Z", 1700000000000, false},
		{"not-a-time", 0, true},
		{"", 0, true},
	}

	for _, tc := range cases {
		got, err := parseProfileTimestamp(tc.input)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseProfileTimestamp(%q) error = %v, wantErr %v", tc.input, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && got != tc.wantMs {
			t.Errorf("parseProfileTimestamp(%q) = %d, want %d", tc.input, got, tc.wantMs)
		}
	}
}

func TestParseProfilingTime_NowRelative(t *testing.T) {
	const now = int64(1_700_000_000_000)
	cases := []struct {
		input   string
		want    int64
		wantErr bool
	}{
		{"now()", now, false},
		{"now()-1h", now - 3600_000, false},
		{"now()-30min", now - 1800_000, false},
		{"now()+15m", now + 900_000, false},
		{"1700000000000", now, false},
		{"now()-", 0, true},
		{"now()*5m", 0, true},
	}
	for _, tc := range cases {
		got, err := parseProfilingTime(tc.input, now)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseProfilingTime(%q) error = %v, wantErr %v", tc.input, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && got != tc.want {
			t.Errorf("parseProfilingTime(%q) = %d, want %d", tc.input, got, tc.want)
		}
	}
}

func TestResolveProfilingTimeframe(t *testing.T) {
	// Defaults: empty start/end -> a ~2h window ending about now.
	from, to, err := resolveProfilingTimeframe("", "")
	if err != nil {
		t.Fatalf("default timeframe errored: %v", err)
	}
	if d := to - from; d != 2*60*60*1000 {
		t.Errorf("default window = %dms, want 2h", d)
	}
	if delta := time.Now().UnixMilli() - to; delta < 0 || delta > 5000 {
		t.Errorf("default end should be ~now, off by %dms", delta)
	}

	// Explicit epoch window is honored.
	from, to, err = resolveProfilingTimeframe("1700000000000", "1700003600000")
	if err != nil || from != 1700000000000 || to != 1700003600000 {
		t.Errorf("explicit window = (%d,%d,%v), want (1700000000000,1700003600000,nil)", from, to, err)
	}

	// end <= start is rejected.
	if _, _, err := resolveProfilingTimeframe("1700003600000", "1700000000000"); err == nil {
		t.Error("expected error when end precedes start")
	}

	// Window wider than 24h is rejected.
	if _, _, err := resolveProfilingTimeframe("0", "90000000"); err == nil {
		t.Error("expected error for window > 24h")
	}
}
