package main

import (
	"context"

	"github.com/crgimenes/glaze"
)

// openProfileWindow gives one query's recorded profile a window of its own,
// the way a table gets an object window: a profile is a long, wide tree, and
// reading it in a pane squeezed under a list is not reading it.
//
// The window captures which query it shows, but never which server: the
// attached connection is consulted on every call, so a window left open
// never reads a server the user has since left.
//
// It must be called on the UI thread. GTK is not thread-safe, and a bind
// callback runs on a goroutine of its own.
func openProfileWindow(queryID string, attachedConn func() (Connection, bool)) glaze.WebView {
	pw, err := glaze.NewWithOptions(glaze.Options{
		Debug:          debugMode,
		SchemeHandlers: map[string]glaze.SchemeHandler{"app": serveAsset},
	})
	if err != nil {
		debugf("event=profile_window_error query=%s err=%q", queryID, err)
		return nil
	}
	pw.SetTitle("Query profile " + queryID)
	pw.SetSize(1100, 800, glaze.HintNone)
	pw.SetSize(520, 360, glaze.HintMin)

	err = pw.Bind("logError", func(msg string) error {
		debugf("event=ui_error window=profile query=%s msg=%q", queryID, msg)
		return nil
	})
	if err != nil {
		debugf("event=profile_window_error query=%s err=%q", queryID, err)
		return nil
	}

	err = pw.Bind("profileDetail", func() (profileDoc, error) {
		conn, ok := attachedConn()
		if !ok {
			return profileDoc{QueryID: queryID, Error: "not connected"}, nil
		}
		fe := newFEClient(conn.FEHTTP, conn.DSN)
		text, err := fe.queryProfile(context.Background(), queryID)
		if err != nil {
			return profileDoc{QueryID: queryID, Error: err.Error()}, nil
		}
		return parseQueryProfile(queryID, text), nil
	})
	if err != nil {
		debugf("event=profile_window_error query=%s err=%q", queryID, err)
		return nil
	}

	debugf("event=profile_window_open query=%s", queryID)
	pw.Navigate("app://sekiban/profile.html")
	return pw
}
