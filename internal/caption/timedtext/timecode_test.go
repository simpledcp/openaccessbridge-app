package timedtext

import "testing"

func TestTimecodeToMS(t *testing.T) {
	cases := []struct {
		name  string
		tc    string
		units float64
		want  int64
	}{
		{"smpte 24fps whole sec", "00:00:01:00", 24, 1000},
		{"smpte 24fps half frame", "00:00:04:12", 24, 4500},
		{"smpte 25fps quarter", "00:00:02:00", 25, 2000},
		{"smpte 30fps frame", "00:00:00:15", 30, 500},
		{"smpte 48fps", "00:00:01:24", 48, 1500},
		{"interop ticks 250 zero", "00:00:01:000", 250, 1000},
		{"interop ticks 250 half", "00:00:04:125", 250, 4500},
		{"interop ticks 250 fifth", "00:00:06:050", 250, 6200},
		{"hours and minutes", "01:02:03:00", 24, 3723000},
		{"decimal seconds", "00:00:01.500", 0, 1500},
		{"decimal seconds ms", "00:00:00.250", 0, 250},
		{"whole seconds 3-part", "00:01:30", 0, 90000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := TimecodeToMS(c.tc, c.units)
			if err != nil {
				t.Fatalf("TimecodeToMS(%q,%v) error: %v", c.tc, c.units, err)
			}
			if got != c.want {
				t.Fatalf("TimecodeToMS(%q,%v) = %d, want %d", c.tc, c.units, got, c.want)
			}
		})
	}
}

func TestTimecodeToMS_Errors(t *testing.T) {
	cases := []struct {
		tc    string
		units float64
	}{
		{"", 24},
		{"not-a-time", 24},
		{"00:00:01:00", 0},  // colon form needs units
		{"00:00:01:-5", 24}, // negative units
		{"00:99:00:00", 24}, // minutes out of range
	}
	for _, c := range cases {
		if _, err := TimecodeToMS(c.tc, c.units); err == nil {
			t.Errorf("TimecodeToMS(%q,%v) expected error, got nil", c.tc, c.units)
		}
	}
}

func TestParseEditRate(t *testing.T) {
	er, err := ParseEditRate("24 1")
	if err != nil || er.Num != 24 || er.Den != 1 || er.PerSecond() != 24 {
		t.Fatalf("ParseEditRate(\"24 1\") = %+v err=%v", er, err)
	}
	er, err = ParseEditRate("30000 1001")
	if err != nil || er.Num != 30000 || er.Den != 1001 {
		t.Fatalf("ParseEditRate fractional = %+v err=%v", er, err)
	}
	if ps := er.PerSecond(); ps < 29.9 || ps > 29.98 {
		t.Fatalf("29.97 PerSecond = %v", ps)
	}
	if _, err := ParseEditRate("garbage x y z"); err == nil {
		t.Fatalf("expected error for malformed edit rate")
	}
}
