package main

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The counter charts do not come from the running queries: they come from
// the cluster's own metric views, which are the SQL projection of the
// Prometheus endpoint the FE and the BEs already publish.
//
// The two views are expensive — the metrics are materialised in full before
// the filter applies — so they are read on a slower cadence than the
// one-second sampler, and the values they produce are carried between reads.

const (
	// metricsEvery is how many one-second samples pass between two reads
	// of the metric views. Ten seconds is a compromise: the views cost
	// about 26ms each, and a rate computed over ten seconds is still a
	// rate, not a smear.
	metricsEvery = 10

	// sqlFEMetrics reads the frontend's own counters.
	sqlFEMetrics = `SELECT NAME, VALUE FROM information_schema.fe_metrics
		WHERE NAME IN ('query_total','query_success','query_err','query_timeout',
		               'query_queue_pending','query_queue_slot_running','query_queue_slot_pending')`

	// sqlBEMetrics reads the backends' counters, one row per backend.
	sqlBEMetrics = `SELECT BE_ID, NAME, VALUE FROM information_schema.be_metrics
		WHERE NAME IN ('bytes_read_total','compaction_bytes_total','bytes_written_total')`
)

// feSnapshot is one reading of the frontend's metrics.
type feSnapshot struct {
	at           time.Time
	queryTotal   float64
	queryErr     float64
	queryTimeout float64
	queuePending float64
	queueRunning float64
	queueSlots   float64
}

// beSnapshot is one reading of the backends' counters, summed over the
// cluster.
type beSnapshot struct {
	at           time.Time
	bytesRead    float64
	bytesWritten float64
	compaction   float64
	beMemLimit   float64
	beMemUsedPct float64
	beCpuUsedPct float64
	beRunning    float64
	backendCount int
	backendAlive int
}

// counters is what the counter charts draw. Every value is the one the
// charts need, already in the chart's unit: rates per second, percentages,
// counts.
type counters struct {
	// queriesPS is the rate of queries the frontend accepted.
	queriesPS float64
	// errorsPS is the rate of queries that failed.
	errorsPS float64
	// timeoutsPS is the rate of queries that timed out.
	timeoutsPS float64
	// queuePending is how many queries are waiting for a slot.
	queuePending float64
	// queueRunning is how many slots are in use.
	queueRunning float64
	// memBEPercent is the most loaded backend's memory use.
	memBEPercent float64
	// memQueryPercent is the share of the cluster's memory held by the
	// queries running right now.
	memQueryPercent float64
	// diskReadPS is the rate the backends read from disk.
	diskReadPS float64
	// scannedPS is the rate the running queries are scanning.
	scannedPS float64
	// compactionPS is the rate the backends are compacting.
	compactionPS float64
	// txnRunning is how many transactions are in flight.
	txnRunning float64
	// txnFinishedPS is the rate at which transactions finish.
	txnFinishedPS float64
}

// feRates returns the per-second rates between two frontend readings. A
// reading that is not newer than the one before it yields no rate: a
// negative one would be a chart that runs backwards.
func feRates(prev, cur feSnapshot) (queriesPS, errorsPS, timeoutsPS float64) {
	dt := cur.at.Sub(prev.at).Seconds()
	if dt <= 0 {
		return 0, 0, 0
	}
	return (cur.queryTotal - prev.queryTotal) / dt,
		(cur.queryErr - prev.queryErr) / dt,
		(cur.queryTimeout - prev.queryTimeout) / dt
}

// beRates returns the per-second rates between two backend readings.
func beRates(prev, cur beSnapshot) (diskReadPS, compactionPS float64) {
	dt := cur.at.Sub(prev.at).Seconds()
	if dt <= 0 {
		return 0, 0
	}
	return (cur.bytesRead - prev.bytesRead) / dt,
		(cur.compaction - prev.compaction) / dt
}

// counterChart is one panel of the counter row: a title, a fixed set of
// series, and a note the heading can show.
type counterChart struct {
	ID    string
	Title string
	// Series are the names of the lines, in drawing order.
	Series []string
	// Note is a short line under the title, for a number the chart cannot
	// carry on its own axis.
	Note string
}

// counterCharts are the panels under the load chart, in order.
//
// Every panel's series share a unit, so one axis means one thing. Where two
// numbers of different kinds are both wanted, the second one becomes a note
// rather than a second scale.
var counterCharts = []counterChart{
	{
		ID:     "connections",
		Title:  "Connections",
		Series: []string{"query", "idle", "other"},
	},
	{
		ID:     "qps",
		Title:  "Queries per second",
		Series: []string{"queries/s", "errors/s"},
	},
	{
		ID:     "queue",
		Title:  "Query queue",
		Series: []string{"pending", "running slots"},
	},
	{
		ID:     "memory",
		Title:  "Memory in use",
		Series: []string{"backend %", "query %"},
	},
	{
		ID:     "io",
		Title:  "Bytes per second",
		Series: []string{"disk read/s", "scanned/s"},
	},
	{
		ID:     "compaction",
		Title:  "Compaction",
		Series: []string{"compacted/s"},
	},
	{
		ID:     "transactions",
		Title:  "Transactions",
		Series: []string{"running"},
		Note:   "finished/s",
	},
}

// value returns the value of one series of one panel.
func (c counters) value(name string) float64 {
	switch name {
	case "queries/s":
		return c.queriesPS
	case "errors/s":
		return c.errorsPS
	case "pending":
		return c.queuePending
	case "running slots":
		return c.queueRunning
	case "backend %":
		return c.memBEPercent
	case "query %":
		return c.memQueryPercent
	case "disk read/s":
		return c.diskReadPS
	case "scanned/s":
		return c.scannedPS
	case "compacted/s":
		return c.compactionPS
	case "running":
		return c.txnRunning
	}
	return 0
}

// note returns the number a panel shows under its title.
func (c counters) note(name string) float64 {
	switch name {
	case "finished/s":
		return c.txnFinishedPS
	}
	return 0
}

// collectFEMetrics reads the frontend's counters.
func collectFEMetrics(db *sql.DB) (feSnapshot, error) {
	snap := feSnapshot{at: time.Now()}
	res, err := queryRows(db, sqlFEMetrics)
	if err != nil {
		return snap, err
	}
	for i := range res.rows {
		name := res.value(i, "NAME")
		v, err := strconv.ParseFloat(strings.TrimSpace(res.value(i, "VALUE")), 64)
		if err != nil {
			continue
		}
		switch name {
		case "query_total":
			snap.queryTotal = v
		case "query_err":
			snap.queryErr = v
		case "query_timeout":
			snap.queryTimeout = v
		case "query_queue_pending":
			snap.queuePending = v
		case "query_queue_slot_running":
			snap.queueRunning = v
		case "query_queue_slot_pending":
			snap.queueSlots = v
		}
	}
	return snap, nil
}

// collectBEMetrics reads the backends' counters, summed over the cluster,
// and the backends' own resource use.
func collectBEMetrics(db *sql.DB) (beSnapshot, error) {
	snap := beSnapshot{at: time.Now()}
	res, err := queryRows(db, sqlBEMetrics)
	if err != nil {
		return snap, err
	}
	for i := range res.rows {
		name := res.value(i, "NAME")
		v, err := strconv.ParseFloat(strings.TrimSpace(res.value(i, "VALUE")), 64)
		if err != nil {
			continue
		}
		switch name {
		case "bytes_read_total":
			snap.bytesRead += v
		case "bytes_written_total":
			snap.bytesWritten += v
		case "compaction_bytes_total":
			snap.compaction += v
		}
	}

	backends, err := collectBackends(db)
	if err != nil {
		return snap, err
	}
	for _, b := range backends {
		snap.backendCount++
		if !b.Alive {
			continue
		}
		snap.backendAlive++
		if b.MemLimit > 0 {
			snap.beMemLimit += b.MemLimit
		}
		if b.MemUsedPct > snap.beMemUsedPct {
			snap.beMemUsedPct = b.MemUsedPct
		}
		if b.CPUUsedPct > snap.beCpuUsedPct {
			snap.beCpuUsedPct = b.CPUUsedPct
		}
		snap.beRunning += b.RunningQueries
	}
	return snap, nil
}

// numOrZero reads a bare number, the kind the server writes for a count.
func numOrZero(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return v
}

// collectTransactions reads the transaction counts of one database. The
// states are lower case: "running" and "finished".
func collectTransactions(db *sql.DB, dbID int64) (running, finished float64, err error) {
	if dbID <= 0 {
		return 0, 0, nil
	}
	res, err := queryRows(db, fmt.Sprintf("SHOW PROC '/transactions/%d'", dbID))
	if err != nil {
		return 0, 0, err
	}
	for i := range res.rows {
		n := numOrZero(res.value(i, "Number"))
		switch strings.ToLower(strings.TrimSpace(res.value(i, "State"))) {
		case "running":
			running = n
		case "finished":
			finished = n
		}
	}
	return running, finished, nil
}

// databaseID finds the id of a database by name. The transaction counts are
// addressed by id, and the connection string names the database.
func databaseID(db *sql.DB, name string) (int64, error) {
	if name == "" {
		return 0, nil
	}
	res, err := queryRows(db, "SHOW PROC '/dbs'")
	if err != nil {
		return 0, err
	}
	for i := range res.rows {
		if !strings.EqualFold(strings.TrimSpace(res.value(i, "DbName")), name) {
			continue
		}
		id, err := strconv.ParseInt(strings.TrimSpace(res.value(i, "DbId")), 10, 64)
		if err != nil {
			return 0, nil
		}
		return id, nil
	}
	return 0, nil
}
