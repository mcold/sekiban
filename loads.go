package main

import (
	"database/sql"
	"strings"
)

// The Loads screen: the data coming in.
//
// A StarRocks cluster is fed by imports, and the interesting question about
// an import is not whether it finished but whether it finished cleanly:
// the rows it rejected, the error it ended with, and how long it took.

// load is one import.
type load struct {
	ID        string  `json:"id"`
	Label     string  `json:"label"`
	DBName    string  `json:"dbName"`
	TableName string  `json:"tableName"`
	User      string  `json:"user"`
	State     string  `json:"state"`
	Progress  string  `json:"progress"`
	Type      string  `json:"type"`
	Priority  string  `json:"priority"`
	ScanRows  float64 `json:"scanRows"`
	ScanBytes float64 `json:"scanBytes"`
	SinkRows  float64 `json:"sinkRows"`
	Filtered  float64 `json:"filteredRows"`
	Created   string  `json:"created"`
	Started   string  `json:"started"`
	Committed string  `json:"committed"`
	Finished  string  `json:"finished"`
	ErrorMsg  string  `json:"errorMsg"`
	// Failed says the import did not end cleanly.
	Failed bool `json:"failed"`
}

// routineLoad is one routine load job: a stream that keeps feeding a table.
type routineLoad struct {
	Name   string `json:"name"`
	ID     string `json:"id"`
	DBName string `json:"dbName"`
	// Statistic and TaskStatistic are the server's own JSON reports of
	// what the job has done.
	Statistic     string `json:"statistic"`
	TaskStatistic string `json:"taskStatistic"`
}

// streamLoad is one stream load.
type streamLoad struct {
	Label     string `json:"label"`
	ID        string `json:"id"`
	DBName    string `json:"dbName"`
	TableName string `json:"tableName"`
	State     string `json:"state"`
}

// loadsOut is what the screen shows.
type loadsOut struct {
	Error   string        `json:"error,omitempty"`
	Loads   []load        `json:"loads"`
	Routine []routineLoad `json:"routineLoads"`
	Stream  []streamLoad  `json:"streamLoads"`
}

// loadLimit bounds how many loads the screen carries. The history table
// holds one row per import, and a busy cluster does thousands of them.
const loadLimit = 200

// collectLoads reads the import history and the running jobs.
func collectLoads(db *sql.DB, database string) loadsOut {
	out := loadsOut{Loads: []load{}, Routine: []routineLoad{}, Stream: []streamLoad{}}

	// The history table of the connected database. The information_schema
	// view holds the same rows, but only the ones of the current session's
	// database; the statistics database holds every database's.
	res, err := queryRowsArgs(db, sqlLoadsHistory, database, loadLimit)
	if err == nil {
		for i := range res.rows {
			l := load{
				ID:        res.value(i, "id"),
				Label:     res.value(i, "label"),
				DBName:    res.value(i, "db_name"),
				TableName: res.value(i, "table_name"),
				User:      res.value(i, "user"),
				State:     res.value(i, "state"),
				Progress:  res.value(i, "progress"),
				Type:      res.value(i, "type"),
				Priority:  res.value(i, "priority"),
				ScanRows:  numOrZero(res.value(i, "scan_rows")),
				ScanBytes: numOrZero(res.value(i, "scan_bytes")),
				SinkRows:  numOrZero(res.value(i, "sink_rows")),
				Filtered:  numOrZero(res.value(i, "filtered_rows")),
				Created:   res.value(i, "create_time"),
				Started:   res.value(i, "load_start_time"),
				Committed: res.value(i, "load_commit_time"),
				Finished:  res.value(i, "load_finish_time"),
				ErrorMsg:  res.value(i, "error_msg"),
			}
			l.Failed = loadFailed(l)
			out.Loads = append(out.Loads, l)
		}
	}

	rl, err := queryRows(db, "SHOW PROC '/routine_loads'")
	if err == nil {
		for i := range rl.rows {
			out.Routine = append(out.Routine, routineLoad{
				Name:          rl.value(i, "Name"),
				ID:            rl.value(i, "Id"),
				DBName:        rl.value(i, "DbName"),
				Statistic:     rl.value(i, "Statistic"),
				TaskStatistic: rl.value(i, "TaskStatistic"),
			})
		}
	}

	sl, err := queryRows(db, "SHOW PROC '/stream_loads'")
	if err == nil {
		for i := range sl.rows {
			out.Stream = append(out.Stream, streamLoad{
				Label:     sl.value(i, "Label"),
				ID:        sl.value(i, "Id"),
				DBName:    sl.value(i, "DbName"),
				TableName: sl.value(i, "TableName"),
				State:     sl.value(i, "State"),
			})
		}
	}
	return out
}

// loadFailed reports whether a load ended badly.
func loadFailed(l load) bool {
	if l.ErrorMsg != "" {
		return true
	}
	return !strings.EqualFold(l.State, "FINISHED") && !strings.EqualFold(l.State, "VISIBLE")
}

// sqlLoadsHistory is the import history of one database.
const sqlLoadsHistory = `SELECT id, label, db_name, table_name, user, state, progress,
	                                type, priority, scan_rows, scan_bytes, sink_rows,
	                                filtered_rows, create_time, load_start_time,
	                                load_commit_time, load_finish_time, error_msg
	                           FROM ` + "`_statistics_`" + `.loads_history
	                           WHERE db_name = ?
	                           ORDER BY id DESC
	                           LIMIT ?`
