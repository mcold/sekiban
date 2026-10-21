//go:build uicheck

package main

// A manual smoke test of the interface: it opens the real window against a
// real server, waits for the dashboard to draw, and reads the DOM back out of
// the live WebView.
//
// It is behind the uicheck build tag because it needs a display and a running
// StarRocks, which a CI box has neither of:
//
//	go test -tags uicheck -run TestInterface -v ./...
//
// The diagnostic travels through the page's own logError channel, which is
// the only path the WebView has back into Go on Linux.

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crgimenes/glaze"
)

func TestInterface(t *testing.T) {
	dsn := os.Getenv("SR_DASH_DSN")
	if dsn == "" {
		t.Skip("SR_DASH_DSN is not set")
	}

	debugMode = true
	var mu sync.Mutex
	var messages []string

	// One sampler for the whole test: the window polls it every couple of
	// seconds, and a fresh one per call would never hold any history.
	sampler := newSampler(dsn)
	defer sampler.Stop()

	w, err := glaze.NewWithOptions(glaze.Options{
		Debug:          true,
		SchemeHandlers: map[string]glaze.SchemeHandler{"app": serveAsset},
	})
	if err != nil {
		t.Fatalf("open window: %v", err)
	}
	defer w.Destroy()

	w.SetTitle("sr-dash uicheck")
	w.SetSize(1200, 860, glaze.HintNone)

	mustBind(w, "configState", func() (uiState, error) {
		return uiState{
			Path:   "uicheck",
			Exists: true,
			Connections: []Connection{{
				DSN:       dsn,
				Title:     "uicheck",
				MaskedDSN: "uicheck",
			}},
			Active: 0,
		}, nil
	})
	mustBind(w, "dashboardState", func(windowSeconds int, sliceBy, topBy string) (dashOut, error) {
		out := sampler.Snapshot(time.Now(), windowSeconds, sliceBy, topBy)
		out.Title = "uicheck"
		return out, nil
	})
	mustBind(w, "logError", func(msg string) error {
		mu.Lock()
		messages = append(messages, msg)
		mu.Unlock()
		return nil
	})
	for _, name := range []string{"connectTo", "disconnect", "makeDefault",
		"addConnection", "updateConnection", "deleteConnection"} {
		mustBind(w, name, func() (uiState, error) { return uiState{}, nil })
	}
	mustBind(w, "connectionDSN", func(int) (string, error) { return dsn, nil })
	mustBind(w, "testConnection", func(string) (testResult, error) {
		return testResult{OK: true, Version: "4.0.0"}, nil
	})

	w.Navigate("app://sr-dash/")

	go func() {
		time.Sleep(9 * time.Second)
		// Ask the live page what it drew. The diagnostic is JSON, so the
		// assertions below can read it without parsing prose.
		w.Dispatch(func() {
			w.Eval(`window.logError("UICHECK " + JSON.stringify({
				legend: Array.from(document.querySelectorAll("#legend .legend-item")).map(e => e.textContent),
				topRows: Array.from(document.querySelectorAll("#topsql .cell")).map(e => e.textContent.slice(0, 60)),
				chartWidth: document.getElementById("chart").width,
				chartHeight: document.getElementById("chart").height,
				status: document.getElementById("dash-status").textContent,
				connLegend: Array.from(document.querySelectorAll("#conns-legend .legend-item")).map(e => e.textContent),
				title: document.getElementById("dash-title").textContent,
			}));`)
		})
		time.Sleep(2 * time.Second)
		w.Terminate()
	}()

	w.Run()

	mu.Lock()
	defer mu.Unlock()
	var diag string
	for _, m := range messages {
		if strings.HasPrefix(m, "UICHECK ") {
			diag = m
		}
	}
	if diag == "" {
		t.Fatalf("the page sent no diagnostic; messages: %v", messages)
	}
	fmt.Println(diag)
}
