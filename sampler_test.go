package main

import (
	"testing"
	"time"
)

// sampleAt builds one sample with the given rows, one second apart.
func sampleAt(at time.Time, rows []sampledQuery) sample {
	s := sample{at: at, conns: map[string]int{}}
	for _, r := range rows {
		s.rows = append(s.rows, r)
		s.cpuTotal += r.rate
	}
	return s
}

// row builds a running query that groups under the given keys.
func row(rate float64, sqlKey, user string) sampledQuery {
	r := sampledQuery{rate: rate}
	r.keys[dimSQL] = sqlKey
	r.keys[dimUsers] = user
	return r
}

func TestAggregateCPUPerSecond(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	base := now.Add(-100 * time.Second)

	var samples []sample
	for i := 0; i < 100; i++ {
		samples = append(samples, sampleAt(base.Add(time.Duration(i)*time.Second), []sampledQuery{
			row(2, "select 1", "alice"),
			row(1, "select 2", "bob"),
		}))
	}

	out := aggregate(samples, now, 100, "sql", "sql")

	if out.Samples != 100 {
		t.Fatalf("samples = %d, want 100", out.Samples)
	}
	// The heaviest series comes first.
	if len(out.Classes) != 2 || out.Classes[0] != "select 1" {
		t.Fatalf("classes = %v, want [select 1 select 2]", out.Classes)
	}
	// Every bucket holds one sample, so the bucket value is that sample's
	// rate: two CPU-seconds per second for the heavy query.
	last := out.Buckets[99]
	if v := last.V["select 1"]; v != 2 {
		t.Errorf("bucket 99 select 1 = %v, want 2", v)
	}
	if v := last.V["select 2"]; v != 1 {
		t.Errorf("bucket 99 select 2 = %v, want 1", v)
	}

	// The Top list is ranked by CPU, and its share is of the window total.
	if len(out.TopSQL) != 2 {
		t.Fatalf("topSQL = %d entries, want 2", len(out.TopSQL))
	}
	if out.TopSQL[0].Query != "select 1" {
		t.Errorf("top[0] = %q, want select 1", out.TopSQL[0].Query)
	}
	if p := out.TopSQL[0].Pct; p < 66.6 || p > 66.7 {
		t.Errorf("top[0] pct = %v, want about 66.67", p)
	}
	if c := out.TopSQL[0].CPUSec; c != 200 {
		t.Errorf("top[0] cpuSec = %v, want 200", c)
	}
}

// The chart is sliced by one dimension and the Top list by another. The two
// must not collide: a user called "root" and a database called "root" are
// different things.
func TestAggregateDimensionsDoNotCollide(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	base := now.Add(-100 * time.Second)

	r := sampledQuery{rate: 1}
	r.keys[dimUsers] = "root"
	r.keys[dimDatabases] = "root"

	var samples []sample
	for i := 0; i < 100; i++ {
		samples = append(samples, sampleAt(base.Add(time.Duration(i)*time.Second), []sampledQuery{r}))
	}

	byUser := aggregate(samples, now, 100, "users", "users")
	if len(byUser.Classes) != 1 || byUser.Classes[0] != "root" {
		t.Fatalf("classes by user = %v, want [root]", byUser.Classes)
	}
	if v := byUser.Buckets[99].V["root"]; v != 1 {
		t.Errorf("bucket by user = %v, want 1", v)
	}

	byDB := aggregate(samples, now, 100, "databases", "databases")
	if len(byDB.Classes) != 1 || byDB.Classes[0] != "root" {
		t.Fatalf("classes by database = %v, want [root]", byDB.Classes)
	}
}

// The chart keeps the heaviest series and folds the tail into "Other", so a
// hundred distinct statements do not produce a hundred legend entries.
func TestAggregateFoldsTheTail(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	base := now.Add(-100 * time.Second)

	var samples []sample
	for i := 0; i < 100; i++ {
		var rows []sampledQuery
		for j := 0; j < 15; j++ {
			// The first series is the heaviest; the rest are all equal.
			rate := 1.0
			if j == 0 {
				rate = 100
			}
			rows = append(rows, row(rate, string(rune('a'+j)), "u"))
		}
		samples = append(samples, sampleAt(base.Add(time.Duration(i)*time.Second), rows))
	}

	out := aggregate(samples, now, 100, "sql", "sql")
	if len(out.Classes) != chartClassLimit+1 {
		t.Fatalf("classes = %d entries, want %d", len(out.Classes), chartClassLimit+1)
	}
	if out.Classes[len(out.Classes)-1] != "Other" {
		t.Errorf("last class = %q, want Other", out.Classes[len(out.Classes)-1])
	}
	// The folded tail keeps its weight: 5 of the 15 series fall outside the
	// limit of 10, and each contributes 1.
	last := out.Buckets[99]
	if v := last.V["Other"]; v != 5 {
		t.Errorf("Other = %v, want 5", v)
	}
}

// A sample that lands in the forming bucket is not charted: its average
// wobbles as samples arrive, which reads as the chart rewriting itself.
func TestAggregateLeavesTheFormingBucketOut(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 30, 0, time.UTC)
	base := now.Add(-100 * time.Second)

	var samples []sample
	for i := 0; i < 100; i++ {
		samples = append(samples, sampleAt(base.Add(time.Duration(i)*time.Second), []sampledQuery{
			row(1, "select 1", "alice"),
		}))
	}
	// A sample in the forming bucket.
	samples = append(samples, sampleAt(now, []sampledQuery{row(999, "select 1", "alice")}))

	out := aggregate(samples, now, 100, "sql", "sql")
	for i, b := range out.Buckets {
		if v := b.V["select 1"]; v > 100 {
			t.Errorf("bucket %d = %v: the forming bucket leaked into the chart", i, v)
		}
	}
}

func TestRankAndFold(t *testing.T) {
	seen := map[string]bool{"a": true, "b": true, "c": true}
	total := map[string]float64{"a": 3, "b": 2, "c": 1}
	classes, mapTo := rankAndFold(seen, total)
	if len(classes) != 3 {
		t.Fatalf("classes = %v, want 3", classes)
	}
	if classes[0] != "a" {
		t.Errorf("classes[0] = %q, want a", classes[0])
	}
	if mapTo("a") != "a" {
		t.Errorf("mapTo(a) = %q, want a", mapTo("a"))
	}
}
