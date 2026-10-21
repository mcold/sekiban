package main

import (
	"testing"
)

// The values below are the ones a live StarRocks 4.0.0 actually returned.
// They are the reason the units parser exists at all: every column of every
// SHOW PROC result comes back as a formatted string.
func TestParseHuman(t *testing.T) {
	cases := []struct {
		in    string
		value float64
		kind  unitKind
	}{
		{"0.000 s", 0, unitSeconds},
		{"3.307 s", 3.307, unitSeconds},
		{"47.719 KB", 47.719 * 1024, unitBytes},
		{"0.000 B", 0, unitBytes},
		{"395.433 GB", 395.433 * 1024 * 1024 * 1024, unitBytes},
		{"12.036GB", 12.036 * 1024 * 1024 * 1024, unitBytes},
		{"8388608.000 TB", 8388608 * 1024 * 1024 * 1024 * 1024, unitBytes},
		{"15.03 %", 15.03, unitPercent},
		{"0 rows", 0, unitCount},
		{"93", 93, unitPlain},
		{"0.000 s", 0, unitSeconds},
	}
	for _, c := range cases {
		got, ok := parseHuman(c.in)
		if !ok {
			t.Errorf("parseHuman(%q) failed", c.in)
			continue
		}
		if got.Kind != c.kind {
			t.Errorf("parseHuman(%q) kind = %v, want %v", c.in, got.Kind, c.kind)
		}
		if diff := got.Value - c.value; diff > 1e-6 || diff < -1e-6 {
			t.Errorf("parseHuman(%q) value = %v, want %v", c.in, got.Value, c.value)
		}
	}
}

// A value the server writes in a form the parser does not know must be
// reported as unparseable rather than silently read as zero: a zeroed
// counter is a lie the chart cannot be built on.
func TestParseHumanRejectsNonsense(t *testing.T) {
	for _, in := range []string{"", "   ", "n/a", "-", "KB", "abc"} {
		if _, ok := parseHuman(in); ok {
			t.Errorf("parseHuman(%q) should have failed", in)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	v, ok := humanBytes("47.719 KB")
	if !ok {
		t.Fatal("humanBytes failed on a real value")
	}
	if v < 48000 || v > 49000 {
		t.Errorf("humanBytes = %v, want about 48864", v)
	}
	// A duration is not a size.
	if _, ok := humanBytes("3.307 s"); ok {
		t.Error("humanBytes accepted a duration")
	}
}

func TestHumanSeconds(t *testing.T) {
	v, ok := humanSeconds("3.307 s")
	if !ok {
		t.Fatal("humanSeconds failed on a real value")
	}
	if v < 3.3 || v > 3.31 {
		t.Errorf("humanSeconds = %v, want about 3.307", v)
	}
	if _, ok := humanSeconds("47.719 KB"); ok {
		t.Error("humanSeconds accepted a size")
	}
}
