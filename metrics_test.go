package main

import (
	"testing"
	"time"
)

func TestFERates(t *testing.T) {
	prev := feSnapshot{at: time.Unix(0, 0), queryTotal: 100, queryErr: 5, queryTimeout: 1}
	cur := feSnapshot{at: time.Unix(10, 0), queryTotal: 250, queryErr: 8, queryTimeout: 2}

	queries, errors, timeouts := feRates(prev, cur)
	if queries != 15 {
		t.Errorf("queries/s = %v, want 15", queries)
	}
	if errors != 0.3 {
		t.Errorf("errors/s = %v, want 0.3", errors)
	}
	if timeouts != 0.1 {
		t.Errorf("timeouts/s = %v, want 0.1", timeouts)
	}
}

// A reading that is not newer than the one before it yields no rate. A
// negative rate would be a chart that runs backwards.
func TestFERatesNoTime(t *testing.T) {
	prev := feSnapshot{at: time.Unix(10, 0), queryTotal: 100}
	cur := feSnapshot{at: time.Unix(10, 0), queryTotal: 250}
	queries, _, _ := feRates(prev, cur)
	if queries != 0 {
		t.Errorf("queries/s = %v, want 0", queries)
	}
}

func TestBERates(t *testing.T) {
	prev := beSnapshot{at: time.Unix(0, 0), bytesRead: 1000, compaction: 0}
	cur := beSnapshot{at: time.Unix(4, 0), bytesRead: 5000, compaction: 800}

	disk, compaction := beRates(prev, cur)
	if disk != 1000 {
		t.Errorf("disk read/s = %v, want 1000", disk)
	}
	if compaction != 200 {
		t.Errorf("compaction/s = %v, want 200", compaction)
	}
}

// The counter panels are averaged over the samples in a bucket, exactly as
// the load chart is.
func TestAggregateCounterCharts(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	base := now.Add(-100 * time.Second)

	var samples []sample
	for i := 0; i < 100; i++ {
		s := sampleAt(base.Add(time.Duration(i)*time.Second), nil)
		s.counters = counters{
			queriesPS:    10,
			errorsPS:     1,
			queuePending: 3,
		}
		s.conns = map[string]int{"query": 4, "idle": 2, "other": 1}
		samples = append(samples, s)
	}

	out := aggregate(samples, now, 100, "sql", "sql")
	if len(out.CounterCharts) != len(counterCharts) {
		t.Fatalf("got %d panels, want %d", len(out.CounterCharts), len(counterCharts))
	}

	byID := map[string]counterChartOut{}
	for _, c := range out.CounterCharts {
		byID[c.ID] = c
	}

	qps := byID["qps"]
	if v := qps.Buckets[99].V["queries/s"]; v != 10 {
		t.Errorf("queries/s = %v, want 10", v)
	}
	if v := qps.Buckets[99].V["errors/s"]; v != 1 {
		t.Errorf("errors/s = %v, want 1", v)
	}

	queue := byID["queue"]
	if v := queue.Buckets[99].V["pending"]; v != 3 {
		t.Errorf("pending = %v, want 3", v)
	}
	// A series with no value is still present, so the legend does not
	// change shape between refreshes.
	if _, ok := queue.Buckets[99].V["running slots"]; !ok {
		t.Error("the running slots series is missing from the bucket")
	}

	conns := byID["connections"]
	if v := conns.Buckets[99].V["query"]; v != 4 {
		t.Errorf("query connections = %v, want 4", v)
	}
	if v := conns.Buckets[99].V["idle"]; v != 2 {
		t.Errorf("idle connections = %v, want 2", v)
	}
}

// Every panel's series must share a unit, or one axis would mean two
// things. The names are what the charts are drawn from, so a name that
// means a rate must not be reused for a count.
func TestCounterSeriesUnitsAreDistinct(t *testing.T) {
	seen := map[string]string{}
	for _, chart := range counterCharts {
		for _, name := range chart.Series {
			if other, ok := seen[name]; ok {
				t.Errorf("series %q is used by both %q and %q", name, other, chart.ID)
			}
			seen[name] = chart.ID
		}
	}
}
