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
	dsn := os.Getenv("SR_SEKIBAN_DSN")
	if dsn == "" {
		t.Skip("SR_SEKIBAN_DSN is not set")
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

	// The first query the frontend holds a profile of, for the profile
	// window. A frontend that holds none leaves the check out.
	profileID := ""
	if db, dbErr := openSR(dsn); dbErr == nil {
		if res, qErr := queryRows(db, "SHOW PROFILELIST"); qErr == nil && len(res.rows) > 0 {
			profileID = res.value(0, "QueryId")
		}
		_ = db.Close()
	}

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

	w.SetTitle("sekiban uicheck")
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
	var profileWindow glaze.WebView
	mustBind(w, "openProfile", func(queryID string) error {
		w.Dispatch(func() {
			profileWindow = openProfileWindow(queryID, func() (Connection, bool) {
				return Connection{DSN: dsn, FEHTTP: "http://127.0.0.1:8030"}, true
			})
		})
		return nil
	})
	mustBind(w, "compactionsState", func() (compactionsOut, error) {
		db, err := openSR(dsn)
		if err != nil {
			return compactionsOut{Error: err.Error()}, nil
		}
		defer func() { _ = db.Close() }()
		return collectCompactions(db, browserDatabase(db)), nil
	})
	mustBind(w, "loadsState", func() (loadsOut, error) {
		db, err := openSR(dsn)
		if err != nil {
			return loadsOut{Error: err.Error()}, nil
		}
		defer func() { _ = db.Close() }()
		return collectLoads(db, browserDatabase(db)), nil
	})
	mustBind(w, "resourcesState", func() (resourcesOut, error) {
		db, err := openSR(dsn)
		if err != nil {
			return resourcesOut{Error: err.Error()}, nil
		}
		defer func() { _ = db.Close() }()
		return collectResources(db), nil
	})
	mustBind(w, "metricsState", func(filter string) (metricsOut, error) {
		db, err := openSR(dsn)
		if err != nil {
			return metricsOut{Error: err.Error()}, nil
		}
		defer func() { _ = db.Close() }()
		return collectMetrics(db, filter), nil
	})
	mustBind(w, "profileState", func(withHistory bool) (profileOut, error) {
		db, err := openSR(dsn)
		if err != nil {
			return profileOut{Error: err.Error()}, nil
		}
		defer func() { _ = db.Close() }()
		out := collectProfiles(context.Background(), db, newFEClient("http://127.0.0.1:8030", dsn), withHistory)
		return out, nil
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

	w.Navigate("app://sekiban/")

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
		// The five new screens, read back the same way.
		for _, screen := range []string{"compactions", "profile", "loads", "resources", "metrics"} {
			w.Dispatch(func() {
				w.Eval(`goTo("` + screen + `");`)
			})
			time.Sleep(2 * time.Second)
			w.Dispatch(func() {
				w.Eval(`window.logError("UICHECKNEW " + JSON.stringify({
					screen: "` + screen + `",
					rows: document.querySelectorAll("#` + screen + ` .data-table tbody tr").length,
					tables: document.querySelectorAll("#` + screen + ` .data-table").length,
					status: (document.querySelector("#` + screen + ` [id$='-status']") || {}).textContent,
				}));`)
			})
			time.Sleep(1 * time.Second)
		}
		// One profile the frontend holds, in a window of its own.
		w.Dispatch(func() {
			w.Eval(`goTo("profile");`)
		})
		time.Sleep(2 * time.Second)
		if profileID != "" {
			w.Dispatch(func() {
				w.Eval(`window.openProfile("` + profileID + `");`)
			})
			time.Sleep(6 * time.Second)
			if profileWindow != nil {
				profileWindow.Dispatch(func() {
					profileWindow.Eval(`document.getElementById("prof-window-copy").click(); setTimeout(function () {
						window.logError("PROFCHK " + JSON.stringify({
							title: document.getElementById("prof-title").textContent,
							nodes: document.querySelectorAll("#prof-window-tree .tree-node").length,
							fields: document.querySelectorAll("#prof-window-tree .tree-line").length,
							status: document.getElementById("prof-window-status").textContent,
						}));
					}, 500);`)
				})
			}
		}
		time.Sleep(2 * time.Second)
		// The audit log, read on demand: the button, not a poll.
		w.Dispatch(func() {
			w.Eval(`goTo("profile");`)
		})
		time.Sleep(2 * time.Second)
		w.Dispatch(func() {
			w.Eval(`document.getElementById("prof-audit").click();`)
		})
		time.Sleep(8 * time.Second)
		w.Dispatch(func() {
			w.Eval(`window.logError("UICHECKAUDIT " + JSON.stringify({
				status: document.getElementById("prof-status").textContent,
				rows: document.querySelectorAll("#prof-summary tbody tr").length,
				first: (document.querySelector("#prof-summary tbody tr") || {}).textContent,
			}));`)
		})
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
	var diag, sessDiag, cluDiag, brDiag, auditDiag string
	newDiags := []string{}
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
		case strings.HasPrefix(m, "UICHECKNEW "):
			newDiags = append(newDiags, m)
		case strings.HasPrefix(m, "UICHECKAUDIT "):
			auditDiag = m
		}
	}
	if len(newDiags) != 5 {
		t.Errorf("got %d diagnostics for the new screens, want 5: %v", len(newDiags), newDiags)
	}
	for _, d := range newDiags {
		fmt.Println(d)
	}
	if auditDiag == "" {
		t.Errorf("the audit log was never read; messages: %v", messages)
	}
	fmt.Println(auditDiag)
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

	prof := ""
	for _, line := range strings.Split(string(captured), "\n") {
		if i := strings.Index(line, "PROFCHK "); i >= 0 {
			prof = line[i+len("PROFCHK "):]
		}
	}
	if profileID != "" && prof == "" {
		t.Fatalf("the profile window sent no diagnostic; stderr tail: %s",
			tail(string(captured), 800))
	}
	if prof != "" {
		fmt.Println("PROFCHK " + prof)
	}
}

// tail returns the last n characters of a string, for a failure message.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
