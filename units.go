package main

import (
	"strconv"
	"strings"
)

// StarRocks writes every value in a SHOW PROC result as a human-readable
// string: "0.000 s", "47.719 KB", "15.03 %", "0 rows", "93". The MySQL
// protocol reports all of them as VARCHAR, so the collector layer has to turn
// them back into numbers. Doing that in one place keeps every screen from
// doing string arithmetic on bytes.
//
// The units are 1024-based, which the server itself confirms: an unlimited
// quota comes back as "8388608.000 TB", and 8388608 * 1024^4 is exactly 2^63.

type unitKind int

const (
	unitPlain unitKind = iota // no suffix: the number already is the value
	unitBytes
	unitSeconds
	unitCount
	unitPercent
)

// humanValue is one parsed value together with the dimension it was written
// in, so a caller can insist on the unit it needs.
type humanValue struct {
	Value float64
	Kind  unitKind
	Unit  string // the suffix as written, lowercased
}

// parseHuman reads a StarRocks-formatted number: a numeric part, optionally
// followed by a unit.
//
// The unit may or may not be separated by a space. The server writes
// "47.719 KB" for a capacity and "12.036GB" for a memory limit, in the same
// result set, so the split cannot assume a separator.
//
// A value it cannot read is reported as not-ok rather than as zero: a
// silently zeroed counter is a lie the chart cannot be built on.
func parseHuman(s string) (humanValue, bool) {
	t := strings.TrimSpace(s)
	if t == "" {
		return humanValue{}, false
	}

	numEnd := scanNumber(t)
	if numEnd == 0 {
		return humanValue{}, false
	}
	f, err := strconv.ParseFloat(t[:numEnd], 64)
	if err != nil {
		return humanValue{}, false
	}

	suffix := strings.ToLower(strings.TrimSpace(t[numEnd:]))
	mult, kind, ok := unitMultiplier(suffix)
	if !ok {
		return humanValue{}, false
	}
	return humanValue{Value: f * mult, Kind: kind, Unit: suffix}, true
}

// scanNumber returns the length of the numeric prefix of t, or 0 when t does
// not start with one. It accepts a sign, digits with an optional decimal
// point, and an exponent, and it stops before a unit: the "E" of "1.5 EB" is
// an exponent only when a digit or a sign follows it.
func scanNumber(t string) int {
	i := 0
	if i < len(t) && (t[i] == '-' || t[i] == '+') {
		i++
	}
	digits := 0
	for i < len(t) && t[i] >= '0' && t[i] <= '9' {
		i++
		digits++
	}
	if i < len(t) && t[i] == '.' {
		i++
		for i < len(t) && t[i] >= '0' && t[i] <= '9' {
			i++
			digits++
		}
	}
	if digits == 0 {
		return 0
	}
	if i < len(t) && (t[i] == 'e' || t[i] == 'E') {
		j := i + 1
		if j < len(t) && (t[j] == '-' || t[j] == '+') {
			j++
		}
		expDigits := 0
		for j < len(t) && t[j] >= '0' && t[j] <= '9' {
			j++
			expDigits++
		}
		if expDigits > 0 {
			i = j
		}
	}
	return i
}

// unitMultiplier maps a unit suffix to its multiplier and dimension.
func unitMultiplier(suffix string) (float64, unitKind, bool) {
	const (
		kb = 1024.0
		mb = kb * 1024
		gb = mb * 1024
		tb = gb * 1024
		pb = tb * 1024
	)
	switch suffix {
	case "", "-":
		return 1, unitPlain, true
	case "b":
		return 1, unitBytes, true
	case "kb":
		return kb, unitBytes, true
	case "mb":
		return mb, unitBytes, true
	case "gb":
		return gb, unitBytes, true
	case "tb":
		return tb, unitBytes, true
	case "pb":
		return pb, unitBytes, true
	case "s":
		return 1, unitSeconds, true
	case "ms":
		return 1e-3, unitSeconds, true
	case "us":
		return 1e-6, unitSeconds, true
	case "ns":
		return 1e-9, unitSeconds, true
	case "row", "rows":
		return 1, unitCount, true
	case "%":
		return 1, unitPercent, true
	}
	return 0, unitPlain, false
}

// humanBytes reads a size. A value written without a unit is taken as bytes,
// which is what the server means when it prints a bare count.
func humanBytes(s string) (float64, bool) {
	v, ok := parseHuman(s)
	if !ok {
		return 0, false
	}
	switch v.Kind {
	case unitBytes, unitPlain:
		return v.Value, true
	}
	return 0, false
}

// humanSeconds reads a duration.
func humanSeconds(s string) (float64, bool) {
	v, ok := parseHuman(s)
	if !ok {
		return 0, false
	}
	switch v.Kind {
	case unitSeconds, unitPlain:
		return v.Value, true
	}
	return 0, false
}

// humanCount reads a count of things.
func humanCount(s string) (float64, bool) {
	v, ok := parseHuman(s)
	if !ok {
		return 0, false
	}
	switch v.Kind {
	case unitCount, unitPlain:
		return v.Value, true
	}
	return 0, false
}
