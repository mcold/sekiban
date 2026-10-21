package main

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	sampleEvery     = time.Second
	sampleKeep      = time.Hour
	sampleTimeout   = 3 * time.Second
	connectTimeout  = 8 * time.Second
	reconnectAfter  = 5 * time.Second
	chartBuckets    = 100
	chartClassLimit = 10
	topLimit        = 10
	maxQueryRunes   = 300
)

// The dimensions the load chart can be cut by. StarRocks has no wait events,
// so what keikiban sliced by wait class is now sliced by the dimensions
// StarRocks actually has.
const (
	dimSQL = iota
	dimUsers
	dimDatabases
	dimResourceGroups
	dimWarehouses
	dimExecStates
	dimFEHosts
	dimCatalogs
	dimCount
)

var sliceDimensions = [dimCount]string{
	dimSQL:            "sql",
	dimUsers:          "users",
	dimDatabases:      "databases",
	dimResourceGroups: "resource_groups",
	dimWarehouses:     "warehouses",
	dimExecStates:     "exec_states",
	dimFEHosts:        "fe_hosts",
	dimCatalogs:       "catalogs",
}

// dimIndex maps a dimension name to its index, so the page can name one.
var dimIndex = func() map[string]int {
	m := make(map[string]int, dimCount)
	for i, name := range sliceDimensions {
		m[name] = i
	}
	return m
}()

// connClasses are the connection states the counter chart counts. The names
// are fixed so the page can map them to fixed colors.
var connClasses = []string{"query", "idle", "other"}

// sliceKey returns the grouping key of a running query for one dimension.
// The menu of dimensions lives in the page; anything unknown falls back to
// the statement.
func sliceKey(r queryRow, dim int) string {
	var value string
	switch dim {
	case dimUsers:
		value = r.user
	case dimDatabases:
		value = r.db
	case dimResourceGroups:
		value = r.resourceGroup
	case dimWarehouses:
		value = r.warehouse
	case dimExecStates:
		value = r.execState
	case dimFEHosts:
		value = r.feIP
	case dimCatalogs:
		value = r.catalog
	default:
		value = r.statement
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "(unset)"
	}
	return value
}

// querySeen remembers what a query looked like the last time it was sampled.
// The CPU a query reports is the total it has consumed so far, so the
// difference between two samples is the CPU it burned in between, and
// dividing that by the elapsed time gives a rate.
type querySeen struct {
	cpu   float64
	scan  float64
	at    time.Time
	start time.Time
}

// sampledQuery is one running query in one sample: the keys it groups under
// in every dimension, and the CPU it was burning at that moment.
type sampledQuery struct {
	keys [dimCount]string
	rate float64
}

// sample is one capture of the running queries plus the connection counts
// taken in the same second.
type sample struct {
	at time.Time
	// rows holds the running queries and their CPU rate. The chart's series
	// are derived from it at aggregation time, so switching the dimension
	// costs no extra queries and no re-sampling.
	rows []sampledQuery
	// cpuTotal is the whole server's CPU consumption at that moment, in
	// CPU seconds per second. A value of 8 means eight cores saturated.
	cpuTotal float64
	// running is how many queries were running.
	running int
	// conns counts the connections by state.
	conns map[string]int
	// counters are the values of the counter charts at this moment. They
	// are refreshed on the slower metrics cadence and carried in between,
	// because a gauge stays true until it is read again.
	counters counters
}

// Sampler keeps a sliding in-memory window of samples for one database:
// CPU time per second over time, sliced by a dimension. History lives only
// while the app runs, by design.
type Sampler struct {
	mu      sync.Mutex
	samples []sample
	status  string
	cancel  context.CancelFunc
	done    chan struct{}

	// seen holds what each running query looked like at the previous
	// sample. It is touched only by the sampling goroutine.
	seen map[string]querySeen
	// lastSampleAt is when the previous sample was taken. A query seen for
	// the first time started at a known moment, and whether that moment is
	// after the last sample decides how its CPU is attributed.
	lastSampleAt time.Time
	// selfID is the server-side id of the sampler's own connection. The
	// sampler's own queries are left out of the chart.
	selfID int64
	// dbID is the id of the connected database, for the transaction counts.
	dbID int64
	// counters holds the last known values, refreshed on the metrics
	// cadence and carried in between.
	counters counters
	// the previous metric readings, for the rates.
	prevFE        feSnapshot
	prevBE        beSnapshot
	prevTxnFinish float64
	// beMemLimit is the cluster's total backend memory, for the share of
	// it the running queries hold.
	beMemLimit float64
	// metricsValid says whether there is a previous reading to compute a
	// rate against.
	metricsValid bool
}

func newSampler(dsn string) *Sampler {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Sampler{
		status: "connecting",
		cancel: cancel,
		done:   make(chan struct{}),
		seen:   map[string]querySeen{},
	}
	go s.run(ctx, dsn)
	return s
}

// Stop ends the sampling goroutine and waits for it to close its connection.
func (s *Sampler) Stop() {
	s.cancel()
	<-s.done
}

func (s *Sampler) setStatus(status string) {
	s.mu.Lock()
	s.status = status
	s.mu.Unlock()
}

// run is the sampling loop: connect, then one capture per second. Any error
// degrades to a visible status and a reconnect with backoff; a monitoring
// tool must not die with its patient.
func (s *Sampler) run(ctx context.Context, dsn string) {
	defer close(s.done)
	for {
		err := s.sampleUntilError(ctx, dsn)
		if ctx.Err() != nil {
			return
		}
		s.setStatus(fmt.Sprintf("reconnecting: %v", err))
		debugf("event=sampler_reconnect err=%q", err)

		select {
		case <-ctx.Done():
			return
		case <-time.After(reconnectAfter):
		}
	}
}

func (s *Sampler) sampleUntilError(ctx context.Context, dsn string) error {
	db, err := openSR(dsn)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	self, err := connectionID(db)
	if err != nil {
		return err
	}
	s.selfID = self

	// The transaction counts are addressed by database id, and the
	// connection string names the database.
	var dbName string
	if err := db.QueryRow("SELECT DATABASE()").Scan(&dbName); err == nil {
		if id, err := databaseID(db, dbName); err == nil {
			s.dbID = id
		}
	}

	s.setStatus("sampling")
	debugf("event=sampler_connected self=%d", self)

	ticker := time.NewTicker(sampleEvery)
	defer ticker.Stop()
	tick := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}

		smp, err := s.capture(db)
		if err != nil {
			return err
		}
		s.push(smp)

		// The metric views cost about 26ms each and are read far less
		// often than the running queries.
		tick++
		if tick%metricsEvery == 0 {
			if err := s.captureMetrics(db); err != nil {
				return err
			}
		}
	}
}

// captureMetrics reads the cluster's metric views and turns the cumulative
// counters into rates. The gauges are simply read; the counters are
// differenced against the previous reading.
func (s *Sampler) captureMetrics(db *sql.DB) error {
	fe, err := collectFEMetrics(db)
	if err != nil {
		return err
	}
	be, err := collectBEMetrics(db)
	if err != nil {
		return err
	}
	txnRunning, txnFinished, err := collectTransactions(db, s.dbID)
	if err != nil {
		return err
	}

	c := s.counters
	if s.metricsValid {
		c.queriesPS, c.errorsPS, c.timeoutsPS = feRates(s.prevFE, fe)
		c.diskReadPS, c.compactionPS = beRates(s.prevBE, be)
		if dt := fe.at.Sub(s.prevFE.at).Seconds(); dt > 0 {
			c.txnFinishedPS = (txnFinished - s.prevTxnFinish) / dt
		}
	}
	c.queuePending = fe.queuePending
	c.queueRunning = fe.queueRunning
	c.memBEPercent = be.beMemUsedPct
	c.txnRunning = txnRunning

	s.counters = c
	s.prevFE = fe
	s.prevBE = be
	s.prevTxnFinish = txnFinished
	s.beMemLimit = be.beMemLimit
	s.metricsValid = true
	debugf("event=metrics qps=%.2f err=%.2f queue=%.0f disk=%.0f compact=%.0f",
		c.queriesPS, c.errorsPS, c.queuePending, c.diskReadPS, c.compactionPS)
	return nil
}

// capture takes one sample: the running queries, their CPU consumption since
// the previous sample, and the connection counts.
func (s *Sampler) capture(db *sql.DB) (sample, error) {
	now := time.Now()
	rows, err := collectCurrentQueries(db)
	if err != nil {
		return sample{}, err
	}
	sessions, err := collectSessions(db)
	if err != nil {
		return sample{}, err
	}

	smp := sample{
		at:    now,
		conns: map[string]int{},
	}
	for _, sess := range sessions {
		switch sess.Command {
		case "Query":
			smp.conns["query"]++
		case "Sleep":
			smp.conns["idle"]++
		default:
			smp.conns["other"]++
		}
	}

	seen := s.seen
	next := make(map[string]querySeen, len(rows))
	smp.rows = make([]sampledQuery, 0, len(rows))
	scanRate := 0.0
	queryMemory := 0.0
	for _, r := range rows {
		// The sampler's own queries are not the cluster's load.
		if s.selfID != 0 && r.connectionID == strconv.FormatInt(s.selfID, 10) {
			continue
		}
		rate := 0.0
		switch prev, known := seen[r.queryID]; {
		case known:
			// The query was running at the last sample too, so the CPU it
			// has consumed since then is known exactly.
			if dt := now.Sub(prev.at).Seconds(); dt > 0 {
				rate = (r.cpuSeconds - prev.cpu) / dt
				scanRate += (r.scanBytes - prev.scan) / dt
			}
		case !s.lastSampleAt.IsZero() && r.start.After(s.lastSampleAt):
			// The query started after the last sample, so all the CPU it
			// has burned was burned inside the interval. Dividing by the
			// interval, not by the query's age, keeps a query that lived
			// for a tenth of a second from being charged a whole second
			// of CPU.
			if dt := now.Sub(s.lastSampleAt).Seconds(); dt > 0 {
				rate = r.cpuSeconds / dt
			}
		case !r.start.IsZero():
			// The query was already running when the sampler started, so
			// how much of it was burned in the last interval is unknown.
			// Its average rate since it started is the best estimate
			// available.
			if elapsed := now.Sub(r.start).Seconds(); elapsed > 0 {
				rate = r.cpuSeconds / elapsed
			}
		}
		if rate < 0 {
			rate = 0
		}
		next[r.queryID] = querySeen{cpu: r.cpuSeconds, scan: r.scanBytes, at: now, start: r.start}
		queryMemory += r.memoryBytes

		sq := sampledQuery{rate: rate}
		for dim := 0; dim < dimCount; dim++ {
			sq.keys[dim] = sliceKey(r, dim)
		}
		smp.rows = append(smp.rows, sq)
		smp.cpuTotal += rate
	}
	smp.running = len(smp.rows)

	// The counters the one-second sample can answer for itself: the rate
	// the running queries are scanning, and the share of the cluster's
	// memory they hold. The rest are read on the metrics cadence and
	// carried.
	c := s.counters
	c.scannedPS = scanRate
	if s.beMemLimit > 0 {
		c.memQueryPercent = 100 * queryMemory / s.beMemLimit
	}
	smp.counters = c

	s.seen = next
	s.lastSampleAt = now

	debugf("event=sample running=%d cpu=%.3f", smp.running, smp.cpuTotal)
	return smp, nil
}

func (s *Sampler) push(smp sample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.samples = append(s.samples, smp)

	cutoff := smp.at.Add(-sampleKeep)
	first := 0
	for first < len(s.samples) && s.samples[first].at.Before(cutoff) {
		first++
	}
	s.samples = s.samples[first:]
}

// bucketOut is one time slice of the stacked chart: CPU seconds per second,
// per series.
type bucketOut struct {
	T int64              `json:"t"` // bucket start, unix milliseconds
	V map[string]float64 `json:"v"`
}

// topSQLOut ranks one entry of the selected Top dimension by the CPU it
// burned. ByClass splits that CPU across the chart's series, so the bar
// reuses the chart colors.
type topSQLOut struct {
	Query   string             `json:"query"`
	CPUSec  float64            `json:"cpuSec"`
	Pct     float64            `json:"pct"`
	ByClass map[string]float64 `json:"byClass"`
}

// dashOut is the dashboard snapshot handed to the UI.
type dashOut struct {
	Connected     bool   `json:"connected"`
	Status        string `json:"status"`
	Title         string `json:"title"`
	DSN           string `json:"dsn"`
	WindowSeconds int    `json:"windowSeconds"`
	BucketSeconds int    `json:"bucketSeconds"`
	// Samples is how many captures landed in the window; it lets the UI
	// distinguish "sampling an idle database" from "not sampling at all".
	Samples int    `json:"samples"`
	SliceBy string `json:"sliceBy"`
	TopBy   string `json:"topBy"`
	// Classes are the chart series for the current slice dimension.
	Classes []string    `json:"classes"`
	Buckets []bucketOut `json:"buckets"`
	TopSQL  []topSQLOut `json:"topSQL"`

	// CounterCharts are the panels under the load chart, on the same
	// closed-bucket timeline as the load chart itself.
	CounterCharts []counterChartOut `json:"counterCharts"`
}

// counterChartOut is one panel of the counter row: a fixed set of series and
// the values over the same bucket timeline as the load chart.
type counterChartOut struct {
	ID      string      `json:"id"`
	Title   string      `json:"title"`
	Series  []string    `json:"series"`
	Note    string      `json:"note,omitempty"`
	NoteVal float64     `json:"noteValue"`
	Buckets []bucketOut `json:"buckets"`
}

// Snapshot aggregates the sliding window ending now into chart buckets and a
// top ranking.
func (s *Sampler) Snapshot(now time.Time, windowSeconds int, sliceBy, topBy string) dashOut {
	s.mu.Lock()
	samples := s.samples
	status := s.status
	s.mu.Unlock()

	out := aggregate(samples, now, windowSeconds, sliceBy, topBy)
	out.Connected = status == "sampling"
	out.Status = status
	return out
}

// rankAndFold orders series by load, keeps the heaviest chartClassLimit and
// folds the tail into "Other". It returns the ordered series and the mapping
// that sends a dropped series to "Other".
func rankAndFold(seen map[string]bool, total map[string]float64) ([]string, func(string) string) {
	var ranked []string
	for c := range seen {
		ranked = append(ranked, c)
	}
	sort.Slice(ranked, func(i, j int) bool {
		if total[ranked[i]] != total[ranked[j]] {
			return total[ranked[i]] > total[ranked[j]]
		}
		return ranked[i] < ranked[j]
	})

	kept := map[string]bool{}
	for _, c := range ranked {
		if len(kept) >= chartClassLimit {
			break
		}
		kept[c] = true
	}

	out := make([]string, 0, len(ranked))
	hasOther := false
	for _, c := range ranked {
		if kept[c] {
			out = append(out, c)
			continue
		}
		hasOther = true
	}
	if hasOther {
		out = append(out, "Other")
	}

	mapTo := func(c string) string {
		if kept[c] {
			return c
		}
		return "Other"
	}
	return out, mapTo
}

// aggregate is the pure core of Snapshot, separated for testing.
//
// Buckets align to ABSOLUTE time boundaries (multiples of the bucket size),
// not to "now": a sample lands in one bucket and stays there forever, so the
// past never redraws differently between refreshes. The in-progress bucket is
// not charted at all — its average wobbles as samples arrive, which reads as
// the chart rewriting itself; it appears once its period closes.
func aggregate(samples []sample, now time.Time, windowSeconds int, sliceBy, topBy string) dashOut {
	if windowSeconds < chartBuckets {
		windowSeconds = chartBuckets
	}
	bucketSeconds := windowSeconds / chartBuckets
	bs := time.Duration(bucketSeconds) * time.Second
	end := now.Truncate(bs) // exclusive: the forming bucket stays out
	start := end.Add(-time.Duration(chartBuckets) * bs)

	sliceDim, ok := dimIndex[sliceBy]
	if !ok {
		sliceDim = dimSQL
		sliceBy = sliceDimensions[dimSQL]
	}
	topDim, ok := dimIndex[topBy]
	if !ok {
		topDim = dimSQL
		topBy = sliceDimensions[dimSQL]
	}

	type agg struct {
		cpu     map[string]float64
		samples int
		conns   map[string]int
		counter map[string]float64
	}
	buckets := make([]agg, chartBuckets)
	for i := range buckets {
		buckets[i].cpu = map[string]float64{}
		buckets[i].conns = map[string]int{}
		buckets[i].counter = map[string]float64{}
	}

	classSeen := map[string]bool{}
	classTotal := map[string]float64{}
	topTotal := map[string]float64{}
	topClass := map[string]map[string]float64{}
	totalSamples := 0
	totalCPU := 0.0

	for _, smp := range samples {
		if smp.at.Before(start) || !smp.at.Before(end) {
			continue
		}
		idx := int(smp.at.Sub(start) / bs)
		if idx >= chartBuckets {
			idx = chartBuckets - 1
		}
		b := &buckets[idx]
		b.samples++
		totalSamples++
		totalCPU += smp.cpuTotal
		for _, name := range connClasses {
			b.conns[name] += smp.conns[name]
		}
		for _, chart := range counterCharts {
			for _, name := range chart.Series {
				if name == "query" || name == "idle" || name == "other" {
					// The connection counts are read every second,
					// not on the metrics cadence.
					b.counter[name] += float64(smp.conns[name])
					continue
				}
				b.counter[name] += smp.counters.value(name)
			}
			if chart.Note != "" {
				b.counter[chart.Note] += smp.counters.note(chart.Note)
			}
		}
		for _, row := range smp.rows {
			class := row.keys[sliceDim]
			b.cpu[class] += row.rate
			classSeen[class] = true
			classTotal[class] += row.rate

			key := row.keys[topDim]
			topTotal[key] += row.rate
			if topClass[key] == nil {
				topClass[key] = map[string]float64{}
			}
			topClass[key][class] += row.rate
		}
	}

	classes, mapClass := rankAndFold(classSeen, classTotal)
	if classes == nil {
		// nil marshals to JSON null and a null breaks the UI's iteration; an
		// idle window is an empty list, not an absence.
		classes = []string{}
	}

	outBuckets := make([]bucketOut, chartBuckets)
	for i, b := range buckets {
		t := start.Add(time.Duration(i) * bs).UnixMilli()

		v := map[string]float64{}
		if b.samples > 0 {
			merged := map[string]float64{}
			for class, cv := range b.cpu {
				merged[mapClass(class)] += cv
			}
			for class, cv := range merged {
				v[class] = round2(cv / float64(b.samples))
			}

		}
		outBuckets[i] = bucketOut{T: t, V: v}
	}

	var top []topSQLOut
	for key, v := range topTotal {
		entry := topSQLOut{Query: key, CPUSec: round2(v), ByClass: map[string]float64{}}
		if totalCPU > 0 {
			entry.Pct = round2(100 * v / totalCPU)
		}
		for class, cv := range topClass[key] {
			entry.ByClass[class] = round2(cv)
		}
		top = append(top, entry)
	}
	sort.Slice(top, func(i, j int) bool {
		if top[i].CPUSec != top[j].CPUSec {
			return top[i].CPUSec > top[j].CPUSec
		}
		return top[i].Query < top[j].Query
	})
	if len(top) > topLimit {
		top = top[:topLimit]
	}
	if top == nil {
		top = []topSQLOut{}
	}

	outCharts := make([]counterChartOut, len(counterCharts))
	for ci, chart := range counterCharts {
		chartBucketsOut := make([]bucketOut, chartBuckets)
		noteVal := 0.0
		for i, b := range buckets {
			t := start.Add(time.Duration(i) * bs).UnixMilli()
			v := map[string]float64{}
			if b.samples > 0 {
				for _, name := range chart.Series {
					v[name] = round2(b.counter[name] / float64(b.samples))
				}
			}
			if chart.Note != "" && b.samples > 0 {
				noteVal = round2(b.counter[chart.Note] / float64(b.samples))
			}
			chartBucketsOut[i] = bucketOut{T: t, V: v}
		}
		outCharts[ci] = counterChartOut{
			ID:      chart.ID,
			Title:   chart.Title,
			Series:  chart.Series,
			Note:    chart.Note,
			NoteVal: noteVal,
			Buckets: chartBucketsOut,
		}
	}

	return dashOut{
		WindowSeconds: windowSeconds,
		BucketSeconds: bucketSeconds,
		SliceBy:       sliceBy,
		TopBy:         topBy,
		Samples:       totalSamples,
		Classes:       classes,
		Buckets:       outBuckets,
		TopSQL:        top,
		CounterCharts: outCharts,
	}
}

// round2 trims a value to two decimals for display.
func round2(f float64) float64 {
	return math.Round(f*100) / 100
}
