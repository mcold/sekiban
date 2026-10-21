package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The query editor: run a statement, read the rows back, and — on a primary
// key table and nowhere else — edit them.
//
// Every destructive statement is shown before it runs. The grid's Save shows
// the exact UPDATE, with the values, and the user decides.

const (
	// queryTimeout bounds a statement. A dashboard that hangs on an
	// unbounded query is a dashboard that has stopped being one.
	queryTimeout = 5 * time.Minute
	// queryRowLimit bounds the rows a result set may carry. A million
	// rows would be a million rows of JSON across the WebView bridge.
	queryRowLimit = 2000
	// queryCellRunes bounds one cell.
	queryCellRunes = 4096
)

// queryColumn is one column of a result set.
type queryColumn struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// queryResult is what the editor shows.
type queryResult struct {
	SQL         string        `json:"sql"`
	Columns     []queryColumn `json:"columns"`
	Rows        [][]*string   `json:"rows"`
	ReturnsRows bool          `json:"returnsRows"`
	Truncated   bool          `json:"truncated"`
	Command     string        `json:"command"`
	Affected    int64         `json:"affected"`
	ElapsedMS   float64       `json:"elapsedMS"`
	// Error carries a failure, so the page always gets something to show.
	Error string `json:"error,omitempty"`
}

// gridEdit is one changed cell.
type gridEdit struct {
	Column string `json:"column"`
	// Value is a pointer so that a cell set to NULL stays NULL: an empty
	// string and a NULL are different things, and only one of them is a
	// value the user typed.
	Value *string `json:"value"`
}

// gridRequest is what the grid sends to preview or save an edit.
type gridRequest struct {
	Database string `json:"database"`
	Table    string `json:"table"`
	// Key are the primary key values of the edited row, in the order of
	// the primary key columns.
	Key       []string   `json:"key"`
	Edits     []gridEdit `json:"edits"`
	PKColumns []string   `json:"pkColumns"`
}

// runStatement runs a statement and reads the result.
//
// A statement that returns rows is read as a result set; anything else is
// executed and reports how many rows it changed. The distinction is made
// from the first word, because the MySQL protocol does not say which it is
// until the statement has run.
func runStatement(ctx context.Context, db *sql.DB, statement string) (queryResult, error) {
	statement = strings.TrimSpace(statement)
	out := queryResult{SQL: statement, Columns: []queryColumn{}, Rows: [][]*string{}}
	start := time.Now()

	if isReadStatement(statement) {
		rows, err := db.QueryContext(ctx, statement)
		if err != nil {
			return out, err
		}
		defer func() { _ = rows.Close() }()

		cols, err := rows.Columns()
		if err != nil {
			return out, err
		}
		out.ReturnsRows = true
		out.Columns = make([]queryColumn, len(cols))
		for i, c := range cols {
			out.Columns[i] = queryColumn{Name: c, Type: ""}
		}
		if types, err := rows.ColumnTypes(); err == nil {
			for i, t := range types {
				if i < len(out.Columns) {
					out.Columns[i].Type = t.DatabaseTypeName()
				}
			}
		}

		for rows.Next() {
			if len(out.Rows) >= queryRowLimit {
				out.Truncated = true
				break
			}
			cells := make([]*string, len(cols))
			ptrs := make([]any, len(cols))
			for i := range cells {
				ptrs[i] = &cells[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				return out, err
			}
			for i, c := range cells {
				if c != nil {
					v := *c
					if r := []rune(v); len(r) > queryCellRunes {
						v = string(r[:queryCellRunes])
					}
					cells[i] = &v
				}
			}
			out.Rows = append(out.Rows, cells)
		}
		err = rows.Err()
		out.ElapsedMS = float64(time.Since(start).Microseconds()) / 1000
		return out, err
	}

	res, err := db.ExecContext(ctx, statement)
	if err != nil {
		return out, err
	}
	affected, _ := res.RowsAffected()
	out.Affected = affected
	out.Command = firstWord(statement)
	out.ElapsedMS = float64(time.Since(start).Microseconds()) / 1000
	return out, nil
}

// isReadStatement reports whether a statement returns rows.
func isReadStatement(s string) bool {
	switch firstWord(s) {
	case "SELECT", "SHOW", "DESC", "DESCRIBE", "EXPLAIN", "WITH", "VALUES":
		return true
	}
	return false
}

// firstWord returns the first word of a statement, upper-cased.
func firstWord(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, " \t\r\n("); i >= 0 {
		s = s[:i]
	}
	return strings.ToUpper(s)
}

// buildUpdate builds the UPDATE that applies a set of edits to one row.
//
// The primary key goes in the WHERE clause, and the edited columns in the
// SET. Nothing else is touched.
func buildUpdate(req gridRequest, pkColumns []string) (string, []any) {
	if len(pkColumns) == 0 || len(req.Key) != len(pkColumns) {
		return "", nil
	}
	var sets []string
	var args []any
	for _, e := range req.Edits {
		sets = append(sets, quoteIdent(e.Column)+" = ?")
		if e.Value == nil {
			args = append(args, nil)
		} else {
			args = append(args, *e.Value)
		}
	}
	if len(sets) == 0 {
		return "", nil
	}
	var where []string
	for i, col := range pkColumns {
		where = append(where, quoteIdent(col)+" = ?")
		args = append(args, req.Key[i])
	}
	return fmt.Sprintf("UPDATE %s SET %s WHERE %s",
		quoteIdent(req.Table), strings.Join(sets, ", "), strings.Join(where, " AND ")), args
}

// gridPrimaryKey reads the primary key columns of a table from the
// catalog.
//
// Only a primary key table accepts an UPDATE. A unique key table rejects it
// as firmly as a duplicate key one, which is not what the name suggests, so
// the catalog's word is the last word.
func gridPrimaryKey(db *sql.DB, database, table string) ([]string, error) {
	detail := collectObjectDetail(db, database, table)
	if detail.Error != "" {
		return nil, errors.New(detail.Error)
	}
	if detail.Model != "PRIMARY_KEYS" {
		return nil, fmt.Errorf("table %s does not accept an update: only a primary key table does, and this one is %s", table, detail.Model)
	}
	if len(detail.PKColumns) == 0 {
		return nil, fmt.Errorf("table %s has no primary key column", table)
	}
	return detail.PKColumns, nil
}

// saveUpdate applies the edits of one row.
func saveUpdate(ctx context.Context, db *sql.DB, req gridRequest, pkColumns []string) (int64, error) {
	stmt, args := buildUpdate(req, pkColumns)
	if stmt == "" {
		return 0, errors.New("the edit names no column, or the primary key does not match the row")
	}
	res, err := db.ExecContext(ctx, stmt, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
