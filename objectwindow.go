package main

import (
	"context"
	"errors"
	"sync"

	"github.com/crgimenes/glaze"
)

// openObjectWindow gives one database object its own native window, so
// several can stay open side by side.
//
// The window captures which object it shows, but never which database: the
// attached connection is consulted on every call instead. A window that
// kept the connection it was born with would go on reading a server the
// user has since left, which is exactly what the one-connection-at-a-time
// rule exists to prevent.
func openObjectWindow(name string, attachedDSN func() (string, error)) glaze.WebView {
	ow, err := glaze.NewWithOptions(glaze.Options{
		Debug:          debugMode,
		SchemeHandlers: map[string]glaze.SchemeHandler{"app": serveAsset},
	})
	if err != nil {
		debugf("event=object_window_error name=%s err=%q", name, err)
		return nil
	}
	ow.SetTitle(name)
	ow.SetSize(900, 680, glaze.HintNone)
	ow.SetSize(420, 320, glaze.HintMin)

	err = ow.Bind("objectDetail", func() (objectDetailOut, error) {
		dsn, dbErr := attachedDSN()
		if dbErr != nil {
			out := objectDetailOut{
				Name:       name,
				Columns:    []browserColumn{},
				Partitions: []browserPartition{},
				PKColumns:  []string{},
				Error:      dbErr.Error(),
			}
			return out, nil
		}
		db, err := openSR(dsn)
		if err != nil {
			return objectDetailOut{Error: err.Error()}, nil
		}
		defer func() { _ = db.Close() }()
		out := collectObjectDetail(db, browserDatabase(db), name)
		debugf("event=object_detail name=%s type=%s model=%s editable=%v",
			name, out.Type, out.Model, out.Editable)
		return out, nil
	})
	if err != nil {
		debugf("event=object_window_error name=%s err=%q", name, err)
		return nil
	}

	err = ow.Bind("initialQuery", func() (string, error) {
		return "SELECT *\nFROM " + quoteIdent(name) + "\nLIMIT 100;", nil
	})
	if err != nil {
		debugf("event=object_window_error name=%s err=%q", name, err)
		return nil
	}

	// One query at a time per window, and always cancellable: a tool
	// that cannot stop what it started is the complaint people file
	// about the ones this replaces.
	var queryMu sync.Mutex
	var queryCancel context.CancelFunc

	err = ow.Bind("runQuery", func(statement string) (queryResult, error) {
		dsn, dbErr := attachedDSN()
		if dbErr != nil {
			return queryResult{SQL: statement, Error: dbErr.Error()}, nil
		}
		db, err := openSR(dsn)
		if err != nil {
			return queryResult{SQL: statement, Error: err.Error()}, nil
		}
		defer func() { _ = db.Close() }()

		ctx, cancel := context.WithCancel(context.Background())
		queryMu.Lock()
		// A second Run replaces the first: the window shows one
		// result, so leaving the previous statement running would
		// burn a backend nobody is waiting on.
		if queryCancel != nil {
			queryCancel()
		}
		queryCancel = cancel
		queryMu.Unlock()
		defer func() {
			queryMu.Lock()
			if queryCancel != nil {
				queryCancel()
				queryCancel = nil
			}
			queryMu.Unlock()
		}()

		ctx, cancelTimeout := context.WithTimeout(ctx, queryTimeout)
		defer cancelTimeout()
		return runStatement(ctx, db, statement)
	})
	if err != nil {
		debugf("event=object_window_error name=%s err=%q", name, err)
		return nil
	}

	err = ow.Bind("cancelQuery", func() error {
		queryMu.Lock()
		defer queryMu.Unlock()
		if queryCancel == nil {
			return nil
		}
		queryCancel()
		queryCancel = nil
		debugf("event=query_cancelled name=%s", name)
		return nil
	})
	if err != nil {
		debugf("event=object_window_error name=%s err=%q", name, err)
		return nil
	}

	// The grid's write path: preview first, then save. Same anatomy as
	// every other write in this app, and for the same reason — the
	// attached database may be production.
	err = ow.Bind("gridPreview", func(req gridRequest) (string, error) {
		dsn, dbErr := attachedDSN()
		if dbErr != nil {
			return "", dbErr
		}
		db, err := openSR(dsn)
		if err != nil {
			return "", err
		}
		defer func() { _ = db.Close() }()
		pk, err := gridPrimaryKey(db, browserDatabase(db), req.Table)
		if err != nil {
			return "", err
		}
		stmt, _ := buildUpdate(req, pk)
		if stmt == "" {
			return "", errors.New("the edit names no column, or the primary key does not match the row")
		}
		return stmt, nil
	})
	if err != nil {
		debugf("event=object_window_error name=%s err=%q", name, err)
		return nil
	}

	err = ow.Bind("gridSave", func(req gridRequest) (int64, error) {
		dsn, dbErr := attachedDSN()
		if dbErr != nil {
			return 0, dbErr
		}
		db, err := openSR(dsn)
		if err != nil {
			return 0, err
		}
		defer func() { _ = db.Close() }()
		pk, err := gridPrimaryKey(db, browserDatabase(db), req.Table)
		if err != nil {
			return 0, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
		defer cancel()
		debugf("event=grid_saved table=%q", req.Table)
		return saveUpdate(ctx, db, req, pk)
	})
	if err != nil {
		debugf("event=object_window_error name=%s err=%q", name, err)
		return nil
	}

	err = ow.Bind("logError", func(msg string) error {
		debugf("event=ui_error window=%s msg=%q", name, msg)
		return nil
	})
	if err != nil {
		debugf("event=object_window_error name=%s err=%q", name, err)
		return nil
	}

	debugf("event=object_window_open name=%s", name)
	ow.Navigate("app://sekiban/object.html")
	return ow
}
