package main

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

// The Sessions screen.
//
// StarRocks 4.0 dropped the /connections proc, so SHOW PROCESSLIST is the
// only way to list the connections, and KILL is the only way to end one.
// There is no separate "cancel the query" and "terminate the connection":
// KILL ends the connection, and the query running on it goes with it.

// session is one connection, as seen in SHOW PROCESSLIST.
type session struct {
	ID      string `json:"id"`
	User    string `json:"user"`
	Host    string `json:"host"`
	Db      string `json:"db"`
	Command string `json:"command"`
	State   string `json:"state"`
	Info    string `json:"info"`
	Pending bool   `json:"pending"`
	Catalog string `json:"catalog"`
	// Seconds is how long the connection has been in its current state.
	Seconds int `json:"seconds"`
	// Since is when the connection started.
	Since string `json:"since"`
}

// sessionsOut is the session list.
type sessionsOut struct {
	Error string    `json:"error,omitempty"`
	Rows  []session `json:"rows"`
}

// collectSessions reads the current connections.
func collectSessions(db *sql.DB) ([]session, error) {
	res, err := queryRows(db, sqlProcesslist)
	if err != nil {
		return nil, err
	}
	out := make([]session, 0, len(res.rows))
	for i := range res.rows {
		s := session{
			ID:      strings.TrimSpace(res.value(i, "Id")),
			User:    res.value(i, "User"),
			Host:    res.value(i, "Host"),
			Db:      res.value(i, "Db"),
			Command: res.value(i, "Command"),
			State:   res.value(i, "State"),
			Info:    collapseStatement(res.value(i, "Info")),
			Pending: strings.EqualFold(res.value(i, "IsPending"), "true"),
			Catalog: res.value(i, "Catalog"),
			Since:   res.value(i, "ConnectionStartTime"),
		}
		// Time is the only properly typed column of the result: seconds.
		if v, err := strconv.Atoi(strings.TrimSpace(res.value(i, "Time"))); err == nil {
			s.Seconds = v
		}
		out = append(out, s)
	}
	return out, nil
}

// collectSessionsList opens a connection, reads the sessions, and closes it.
func collectSessionsList(_ context.Context, dsn string) (sessionsOut, error) {
	db, err := openSR(dsn)
	if err != nil {
		return sessionsOut{Error: err.Error()}, err
	}
	defer func() { _ = db.Close() }()

	rows, err := collectSessions(db)
	if err != nil {
		return sessionsOut{Error: err.Error()}, err
	}
	return sessionsOut{Rows: rows}, nil
}

// killSession ends a connection and whatever it was running.
//
// The id is checked before it reaches the statement: it comes from the page,
// and a connection id that is not a number is not a connection id.
func killSession(dsn string, id int64) error {
	if id <= 0 {
		return fmt.Errorf("%d is not a connection id", id)
	}
	db, err := openSR(dsn)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	_, err = db.Exec(fmt.Sprintf("KILL %d", id))
	return err
}
