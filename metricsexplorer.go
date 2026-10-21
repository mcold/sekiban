package main

import (
	"database/sql"
	"sort"
	"strings"
)

// The Metrics Explorer: every metric the cluster publishes about itself.
//
// The frontend and the backends both publish Prometheus metrics, and
// information_schema.fe_metrics and be_metrics are the SQL projection of
// them. It is a long list — 525 rows on the frontend, 2460 on the
// backends — so it is browsed by name rather than read.

// metricRow is one metric of one node.
type metricRow struct {
	// Scope is "fe" or "be".
	Scope string `json:"scope"`
	// Owner is the frontend or backend the metric belongs to.
	Owner string `json:"owner"`
	Name  string `json:"name"`
	// Labels is the metric's label set, as the server writes it.
	Labels string `json:"labels"`
	Value  string `json:"value"`
}

// metricsOut is what the screen shows.
type metricsOut struct {
	Error string `json:"error,omitempty"`
	// Names is every distinct metric name, for the search.
	Names []string `json:"names"`
	// Rows are the metrics that match the filter.
	Rows []metricRow `json:"rows"`
	// Total is how many metrics there are before the filter.
	Total int `json:"total"`
}

// metricLimit bounds how many rows the screen carries at once. The whole
// set is nearly three thousand rows, and the WebView bridge is not the
// place to carry all of it.
const metricLimit = 500

// collectMetrics reads the cluster's metrics, keeping the ones whose name
// contains the filter. An empty filter keeps the first metricLimit rows.
func collectMetrics(db *sql.DB, filter string) metricsOut {
	out := metricsOut{Rows: []metricRow{}, Names: []string{}}
	filter = strings.ToLower(strings.TrimSpace(filter))

	fe, err := queryRows(db, sqlFEMetricsAll)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	be, err := queryRows(db, sqlBEMetricsAll)
	if err != nil {
		out.Error = err.Error()
		return out
	}

	names := map[string]bool{}
	add := func(scope, owner, name, labels, value string) {
		names[name] = true
		if len(out.Rows) >= metricLimit {
			return
		}
		if filter != "" && !strings.Contains(strings.ToLower(name), filter) {
			return
		}
		out.Rows = append(out.Rows, metricRow{
			Scope:  scope,
			Owner:  owner,
			Name:   name,
			Labels: labels,
			Value:  value,
		})
	}
	for i := range fe.rows {
		add("fe", fe.value(i, "FE_ID"), fe.value(i, "NAME"), fe.value(i, "LABELS"), fe.value(i, "VALUE"))
	}
	for i := range be.rows {
		add("be", be.value(i, "BE_ID"), be.value(i, "NAME"), be.value(i, "LABELS"), be.value(i, "VALUE"))
	}
	out.Total = len(fe.rows) + len(be.rows)

	for n := range names {
		out.Names = append(out.Names, n)
	}
	sort.Strings(out.Names)
	return out
}

// The whole metric sets. They are the SQL projection of the Prometheus
// endpoints the frontend and the backends already publish.
const (
	sqlFEMetricsAll = `SELECT FE_ID, NAME, LABELS, VALUE FROM information_schema.fe_metrics`

	sqlBEMetricsAll = `SELECT BE_ID, NAME, LABELS, VALUE FROM information_schema.be_metrics`
)
