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
	"context"
	"fmt"
	"io"
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

	// The object window reports through the same stderr the app's debug
	// lines go to, so the diagnostic is captured there.
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	oldStderr := os.Stderr
	os.Stderr = stderrW
	defer func() { os.Stderr = oldStderr }()

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
	mustBind(w, "browserTree", func() (browserTreeOut, error) {
		db, err := openSR(dsn)
		if err != nil {
			return browserTreeOut{Error: err.Error()}, nil
		}
		defer func() { _ = db.Close() }()
		return collectBrowserTree(db), nil
	})
	var objectWindow glaze.WebView
	mustBind(w, "openObject", func(name string) error {
		// On the UI thread: GTK is not thread-safe, and a bind callback
		// runs on a goroutine of its own.
		w.Dispatch(func() {
			objectWindow = openObjectWindow(name, func() (string, error) { return dsn, nil })
		})
		return nil
	})
	mustBind(w, "clusterState", func() (clusterOut, error) {
		db, err := openSR(dsn)
		if err != nil {
			return clusterOut{Error: err.Error()}, nil
		}
		defer func() { _ = db.Close() }()
		return collectCluster(db)
	})
	mustBind(w, "sessionList", func() (sessionsOut, error) {
		return collectSessionsList(context.Background(), dsn)
	})
	// The kill is a no-op here: a smoke test has no business ending
	// somebody's connection.
	mustBind(w, "killSession", func(int) error { return nil })
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
		// The Sessions screen, read back the same way.
		w.Dispatch(func() {
			w.Eval(`goTo("sessions"); loadSessions();`)
		})
		time.Sleep(2 * time.Second)
		// Ask the live page what it drew. The diagnostic is JSON, so the
		// assertions below can read it without parsing prose.
		w.Dispatch(func() {
			w.Eval(`window.logError("UICHECK " + JSON.stringify({
				legend: Array.from(document.querySelectorAll("#legend .legend-item")).map(e => e.textContent),
				topRows: Array.from(document.querySelectorAll("#topsql .cell")).map(e => e.textContent.slice(0, 60)),
				chartWidth: document.getElementById("chart").width,
				chartHeight: document.getElementById("chart").height,
				status: document.getElementById("dash-status").textContent,
				panels: Array.from(document.querySelectorAll("#counters .counter-panel")).map(e => e.querySelector("h3").textContent),
				counterLegends: Array.from(document.querySelectorAll("#counters .mini-legend")).map(e => e.textContent),
				title: document.getElementById("dash-title").textContent,
			}));`)
		})
		time.Sleep(2 * time.Second)
		w.Dispatch(func() {
			w.Eval(`window.logError("UICHECKSESS " + JSON.stringify({
				sessions: document.querySelectorAll("#sess-list .cell").length,
				first: (document.querySelector("#sess-list .cell") || {}).textContent,
				status: document.getElementById("sess-status").textContent,
			}));`)
		})
		time.Sleep(2 * time.Second)
		w.Dispatch(func() {
			w.Eval(`goTo("cluster"); loadCluster();`)
		})
		time.Sleep(2 * time.Second)
		w.Dispatch(func() {
			w.Eval(`goTo("browser"); loadBrowser();`)
		})
		time.Sleep(2 * time.Second)
		w.Dispatch(func() {
			w.Eval(`window.logError("UICHECKBR " + JSON.stringify({
				groups: Array.from(document.querySelectorAll("#br-tree .br-group")).map(e => e.textContent),
				nodes: document.querySelectorAll("#br-tree .br-node").length,
				first: (document.querySelector("#br-tree .br-node") || {}).textContent,
				db: (document.querySelector("#br-tree .br-db") || {}).textContent,
			}));`)
		})
		time.Sleep(2 * time.Second)
		w.Dispatch(func() {
			w.Eval(`window.openObject("pk_users");`)
		})
		time.Sleep(4 * time.Second)
		// The object window's own DOM, read back through its own channel.
		if objectWindow != nil {
			objectWindow.Dispatch(func() {
				objectWindow.Eval(`window.logError("OBJCHK " + [
					"title=" + document.getElementById("obj-title").textContent,
					"cols=" + document.querySelectorAll("#obj-properties tbody tr").length,
					"ddl=" + (document.querySelector("pre.ddl") || {}).textContent.length,
					"sql=" + document.getElementById("sql-text").value.length,
				].join(" "));`)
			})
		}
		time.Sleep(2 * time.Second)
		w.Dispatch(func() {
			w.Eval(`window.logError("UICHECKCLU " + JSON.stringify({
				totals: (document.querySelector("#cluster-totals p") || {}).textContent,
				feRows: document.querySelectorAll("#cluster-frontends tbody tr").length,
				beRows: document.querySelectorAll("#cluster-backends tbody tr").length,
				beHead: Array.from(document.querySelectorAll("#cluster-backends th")).map(e => e.textContent),
				firstBE: (document.querySelector("#cluster-backends tbody tr") || {}).textContent,
			}));`)
		})
		time.Sleep(2 * time.Second)
		w.Terminate()
	}()

	w.Run()

	mu.Lock()
	defer mu.Unlock()
	var diag, sessDiag, cluDiag, brDiag string
	for _, m := range messages {
		switch {
		case strings.HasPrefix(m, "UICHECK "):
			diag = m
		case strings.HasPrefix(m, "UICHECKSESS "):
			sessDiag = m
		case strings.HasPrefix(m, "UICHECKCLU "):
			cluDiag = m
		case strings.HasPrefix(m, "UICHECKBR "):
			brDiag = m
		}
	}
	if diag == "" {
		t.Fatalf("the page sent no diagnostic; messages: %v", messages)
	}
	fmt.Println(diag)
	if sessDiag == "" {
		t.Fatalf("the Sessions screen sent no diagnostic; messages: %v", messages)
	}
	fmt.Println(sessDiag)
	if cluDiag == "" {
		t.Fatalf("the Cluster screen sent no diagnostic; messages: %v", messages)
	}
	fmt.Println(cluDiag)
	if brDiag == "" {
		t.Fatalf("the Browser sent no diagnostic; messages: %v", messages)
	}
	fmt.Println(brDiag)

	_ = stderrW.Close()
	captured, _ := io.ReadAll(stderrR)
	obj := ""
	for _, line := range strings.Split(string(captured), "\n") {
		if i := strings.Index(line, "OBJCHK "); i >= 0 {
			obj = line[i+len("OBJCHK "):]
		}
	}
	if obj == "" {
		t.Fatalf("the object window sent no diagnostic; stderr tail: %s",
			tail(string(captured), 800))
	}
	fmt.Println("OBJCHK " + obj)
}

// tail returns the last n characters of a string, for a failure message.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
