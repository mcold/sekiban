package main

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

// The collector layer. Everything StarRocks-specific lives here: how to
// connect, how to read a result set whose columns are all VARCHAR, and what
// to ask the server for.

const (
	// sqlCurrentQueries lists the queries running right now. It is the
	// closest StarRocks has to pg_stat_activity: one row per running query
	// with its CPU cost, its scan volume and its memory.
	sqlCurrentQueries = "SHOW PROC '/current_queries'"

	// sqlProcesslist lists the connections. StarRocks 4.0 dropped the
	// /connections proc, so SHOW PROCESSLIST is the only way in.
	sqlProcesslist = "SHOW PROCESSLIST"

	// sqlFrontends carries the server version, which is the only honest
	// source: SELECT version() reports the MySQL compatibility version
	// (5.1.0), not StarRocks.
	sqlFrontends = "SHOW PROC '/frontends'"

	// sqlVersionComment asks the server to identify itself in words.
	sqlVersionComment = "SELECT @@version_comment"
)

// result is a result set read by column name. Every SHOW PROC column comes
// back as VARCHAR, so the values are kept as strings and parsed by the caller
// with the units parser. Reading by name rather than by position keeps a
// collector working when the server adds a column.
type result struct {
	cols []string
	rows [][]sql.NullString
}

// value returns the named column of the row, or "" when the column is absent.
func (r *result) value(row int, name string) string {
	for i, c := range r.cols {
		if strings.EqualFold(c, name) {
			return r.rows[row][i].String
		}
	}
	return ""
}

// queryRows runs a query and reads the whole result set.
func queryRows(db *sql.DB, query string) (*result, error) {
	return queryRowsArgs(db, query)
}

// queryRowsArgs runs a query with parameters and reads the whole result
// set. The parameters are bound, never interpolated: they come from the
// connection and from the page.
func queryRowsArgs(db *sql.DB, query string, args ...any) (*result, error) {
	rows, err := db.Query(query, args...) // #nosec G202 -- the queries are constants, the arguments are bound
	if err != nil {
		return nil, err
	}
	return scanResult(rows)
}

// openSR opens a connection to a StarRocks frontend and checks that the
// server is one this program understands.
//
// A 3.x server is refused with a clear message rather than allowed to fail
// later in obscure ways: the proc paths moved between versions, and a
// dashboard that shows plausible nonsense is worse than one that says no.
func openSR(dsn string) (*sql.DB, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	cfg.Timeout = connectTimeout
	cfg.ReadTimeout = sampleTimeout
	// StarRocks does not support the prepared statement protocol for
	// writes: an UPDATE sent that way is refused with "This command is not
	// supported in the prepared statement protocol yet". The driver
	// interpolates the parameters into the statement instead, and it
	// refuses to do so for the multi-byte collations where that would be
	// unsafe, so the DSN is rejected rather than silently mangled.
	if _, stated := cfg.Params["interpolateParams"]; !stated {
		cfg.InterpolateParams = true
	}

	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, fmt.Errorf("open connection: %w", err)
	}
	// The sampler's two queries must observe the same server state, so the
	// pool is kept at one connection.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect: %w", err)
	}

	version, err := serverVersion(db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if version.major < 4 {
		_ = db.Close()
		return nil, fmt.Errorf("StarRocks %s is not supported: sr-dash needs 4.0 or newer, because the SHOW PROC paths it relies on changed in 4.0", version.string)
	}
	return db, nil
}

// connectionID reports the server-side id of this connection. The sampler
// uses it to recognise its own queries in /current_queries and leave them out
// of the chart: the dashboard measuring itself is a distraction, and on a
// quiet cluster it would be most of what there is.
func connectionID(db *sql.DB) (int64, error) {
	var id int64
	if err := db.QueryRow("SELECT CONNECTION_ID()").Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

// srVersion is a parsed StarRocks version.
type srVersion struct {
	major  int
	minor  int
	string string
}

// serverVersion reads the StarRocks version from the frontend's own record.
func serverVersion(db *sql.DB) (srVersion, error) {
	res, err := queryRows(db, sqlFrontends)
	if err != nil {
		return srVersion{}, fmt.Errorf("read frontend version: %w", err)
	}
	if len(res.rows) == 0 {
		return srVersion{}, fmt.Errorf("read frontend version: the server reported no frontends")
	}
	raw := strings.TrimSpace(res.value(0, "Version"))
	if raw == "" {
		// Fall back to the version comment, which reads like
		// "StarRocks version 4.0.0".
		if res2, err2 := queryRows(db, sqlVersionComment); err2 == nil && len(res2.rows) > 0 {
			raw = res2.value(0, "version_comment")
		}
	}
	return parseSRVersion(raw)
}

// parseSRVersion reads the leading major.minor of a version string. The
// frontend reports "4.0.0-dccb665" and the version comment reads "StarRocks
// version 4.0.0", so the number is found rather than assumed to be at the
// start of the string.
func parseSRVersion(raw string) (srVersion, error) {
	v := srVersion{string: raw}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return v, fmt.Errorf("read frontend version: the server reported no version")
	}

	// Start at the first digit, then take the run of digits and dots.
	start := strings.IndexFunc(trimmed, func(r rune) bool { return r >= '0' && r <= '9' })
	if start < 0 {
		return v, fmt.Errorf("read frontend version: cannot read %q", raw)
	}
	end := start
	for end < len(trimmed) && (trimmed[end] >= '0' && trimmed[end] <= '9' || trimmed[end] == '.') {
		end++
	}
	num := strings.Trim(trimmed[start:end], ".")
	if num == "" {
		return v, fmt.Errorf("read frontend version: cannot read %q", raw)
	}

	parts := strings.Split(num, ".")
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return v, fmt.Errorf("read frontend version: cannot read %q", raw)
	}
	v.major = major
	if len(parts) > 1 {
		if minor, err := strconv.Atoi(parts[1]); err == nil {
			v.minor = minor
		}
	}
	return v, nil
}

// queryRow is one running query, as seen in SHOW PROC '/current_queries'.
type queryRow struct {
	queryID       string
	connectionID  string
	user          string
	db            string
	warehouse     string
	resourceGroup string
	execState     string
	feIP          string
	catalog       string
	statement     string
	start         time.Time
	cpuSeconds    float64
	execSeconds   float64
	scanBytes     float64
	scanRows      float64
	memoryBytes   float64
	spillBytes    float64
}

// collectCurrentQueries reads the queries running right now.
//
// The statement text is not in this result: /current_queries carries only the
// QueryId. It is joined in from SHOW PROCESSLIST, which knows the statement
// of every connection, matched on the connection id both sides carry.
//
// The process list is read FIRST, on purpose. A query that finishes between
// the two reads is then visible in the process list but not in
// /current_queries, which is harmless; the other order would show a query
// that has already finished with no statement to name it.
func collectCurrentQueries(db *sql.DB) ([]queryRow, error) {
	sessions, err := collectSessions(db)
	if err != nil {
		return nil, err
	}
	byConn := make(map[string]session, len(sessions))
	for _, s := range sessions {
		byConn[s.ID] = s
	}
	res, err := queryRows(db, sqlCurrentQueries)
	if err != nil {
		return nil, err
	}

	// A query that started between the two reads is in /current_queries but
	// not in the process list, and would show up with no statement to name
	// it. One extra read, only when that actually happened, recovers them.
	missing := false
	for i := range res.rows {
		if _, ok := byConn[strings.TrimSpace(res.value(i, "ConnectionId"))]; !ok {
			missing = true
			break
		}
	}
	if missing {
		if later, err := collectSessions(db); err == nil {
			for _, s := range later {
				if _, ok := byConn[s.ID]; !ok {
					byConn[s.ID] = s
				}
			}
		}
	}

	out := make([]queryRow, 0, len(res.rows))
	for i := range res.rows {
		r := queryRow{
			queryID:       res.value(i, "QueryId"),
			connectionID:  strings.TrimSpace(res.value(i, "ConnectionId")),
			user:          res.value(i, "User"),
			db:            res.value(i, "Database"),
			warehouse:     res.value(i, "Warehouse"),
			resourceGroup: res.value(i, "ResourceGroup"),
			execState:     res.value(i, "ExecState"),
			feIP:          res.value(i, "feIp"),
		}
		if v, ok := humanSeconds(res.value(i, "CPUTime")); ok {
			r.cpuSeconds = v
		}
		if v, ok := humanSeconds(res.value(i, "ExecTime")); ok {
			r.execSeconds = v
		}
		if v, ok := humanBytes(res.value(i, "ScanBytes")); ok {
			r.scanBytes = v
		}
		if v, ok := humanCount(res.value(i, "ScanRows")); ok {
			r.scanRows = v
		}
		if v, ok := humanBytes(res.value(i, "MemoryUsage")); ok {
			r.memoryBytes = v
		}
		if v, ok := humanBytes(res.value(i, "DiskSpillSize")); ok {
			r.spillBytes = v
		}
		if t, err := time.Parse("2006-01-02 15:04:05", strings.TrimSpace(res.value(i, "StartTime"))); err == nil {
			r.start = t
		}
		if s, ok := byConn[r.connectionID]; ok {
			r.statement = collapseStatement(s.Info)
			r.catalog = s.Catalog
		}
		out = append(out, r)
	}
	return out, nil
}

// collapseStatement makes a statement fit on one line: the server keeps the
// text exactly as the client sent it, newlines and tabs included, and a
// chart legend wants a single readable line. It is capped at the same length
// keikiban used, so one absurd statement cannot take over the legend.
func collapseStatement(s string) string {
	collapsed := strings.Join(strings.Fields(s), " ")
	r := []rune(collapsed)
	if len(r) > maxQueryRunes {
		return string(r[:maxQueryRunes]) + "..."
	}
	return collapsed
}
