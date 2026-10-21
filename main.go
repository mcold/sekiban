package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"mime"
	"net/url"
	"os"
	"path"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crgimenes/glaze"
	"github.com/crgimenes/glaze/menu"
)

//go:embed ui
var uiFS embed.FS

var debugMode bool

func init() { runtime.LockOSThread() }

// debugf prints one key=value diagnostic line to stderr when -debug is on.
// stderr keeps -json stdout clean for machine consumption.
func debugf(format string, args ...any) {
	if !debugMode {
		return
	}
	fmt.Fprintf(os.Stderr, "sr-dash: "+format+"\n", args...)
}

func main() {
	debugFlag := flag.Bool("debug", false, "print key=value diagnostic lines to stderr")
	jsonFlag := flag.Bool("json", false, "one-shot mode: run a command, print JSON to stdout, exit")
	flag.Parse()
	debugMode = *debugFlag

	cfg, err := loadConfig()
	configErr := ""
	if err != nil {
		configErr = err.Error()
	}
	debugf("event=config_loaded path=%s exists=%v connections=%d err=%q",
		cfg.Path, cfg.Exists, len(cfg.Connections), configErr)

	if *jsonFlag {
		runJSON(cfg, configErr, flag.Args())
		return
	}
	runGUI(cfg, configErr)
}

// runJSON is the AI-friendly one-shot mode: execute one command, print a JSON
// document to stdout, exit. Passwords never appear in the output.
func runJSON(cfg Config, configErr string, args []string) {
	command := ""
	if len(args) > 0 {
		command = args[0]
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")

	fail := func(msg string) {
		_ = enc.Encode(map[string]string{"error": msg})
		os.Exit(1)
	}
	if configErr != "" {
		fail(configErr)
	}

	switch command {
	case "connections":
		if cfg.Connections == nil {
			cfg.Connections = []Connection{}
		}
		_ = enc.Encode(cfg)
	case "dashboard":
		if len(cfg.Connections) == 0 {
			fail("no connections configured")
		}
		// The load chart is built from samples over time, so this command
		// samples for a while before answering. Default 30s; the caller can
		// ask for another duration.
		seconds := 30
		if len(args) > 1 {
			n, err := strconv.Atoi(args[1])
			if err != nil || n < 1 {
				fail("usage: sr-dash -json dashboard [seconds]")
			}
			seconds = n
		}
		sampler := newSampler(cfg.Connections[0].DSN)
		time.Sleep(time.Duration(seconds) * time.Second)
		out := sampler.Snapshot(time.Now(), seconds, "sql", "sql")
		sampler.Stop()
		out.Title = cfg.Connections[0].Title
		out.DSN = cfg.Connections[0].MaskedDSN
		_ = enc.Encode(out)
		if !out.Connected {
			os.Exit(1)
		}
	case "sessions":
		if len(cfg.Connections) == 0 {
			fail("no connections configured")
		}
		out, err := collectSessionsList(context.Background(), cfg.Connections[0].DSN)
		if err != nil {
			_ = enc.Encode(map[string]string{"error": err.Error()})
			os.Exit(1)
		}
		_ = enc.Encode(out)
	case "browser":
		if len(cfg.Connections) == 0 {
			fail("no connections configured")
		}
		out, err := collectBrowserTreeJSON(context.Background(), cfg.Connections[0].DSN)
		if err != nil {
			_ = enc.Encode(map[string]string{"error": err.Error()})
			os.Exit(1)
		}
		_ = enc.Encode(out)
	case "describe":
		if len(cfg.Connections) == 0 {
			fail("no connections configured")
		}
		if len(args) < 2 {
			fail("usage: sr-dash -json describe <name>")
		}
		out, err := collectObjectDetailJSON(context.Background(), cfg.Connections[0].DSN, args[1])
		if err != nil {
			_ = enc.Encode(map[string]string{"error": err.Error()})
			os.Exit(1)
		}
		_ = enc.Encode(out)
	default:
		_ = enc.Encode(map[string]any{
			"error": fmt.Sprintf("unknown command %q", command),
			"commands": []string{
				"browser", "connections", "dashboard", "describe", "sessions",
			},
		})
		os.Exit(1)
	}
}

// uiState is what the page needs to render: where the config lives, what it
// declares, and any load error, stated plainly instead of hidden.
type uiState struct {
	Path        string       `json:"path"`
	Exists      bool         `json:"exists"`
	Error       string       `json:"error,omitempty"`
	Connections []Connection `json:"connections"`
	// Active is the index of the one connection in use; -1 when none.
	Active int `json:"active"`
}

// testResult reports a connection attempt. Failure is data, not a rejected
// promise: the page always gets something to show.
type testResult struct {
	OK      bool   `json:"ok"`
	Version string `json:"version,omitempty"`
	Error   string `json:"error,omitempty"`
}

// mustBind registers a bind the window cannot work without. A failure here is
// a programming error (duplicate or reserved name), never a runtime condition,
// so it stops the app before a half-wired page could reach the user.
func mustBind(w glaze.WebView, name string, fn any) {
	err := w.Bind(name, fn)
	if err != nil {
		log.Fatal(err)
	}
}

func runGUI(cfg Config, configErr string) {
	w, err := glaze.NewWithOptions(glaze.Options{
		Debug:          debugMode,
		SchemeHandlers: map[string]glaze.SchemeHandler{"app": serveAsset},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer w.Destroy()

	// Dashboard-sized; user-resizable with a sane floor.
	w.SetTitle("sr-dash")
	w.SetSize(1200, 860, glaze.HintNone)
	w.SetSize(560, 480, glaze.HintMin)

	// mu guards cfg, configErr, activeIndex and the sampler: Bind callbacks
	// run on background goroutines and may overlap.
	var mu sync.Mutex
	var sampler *Sampler
	samplerDSN := ""
	// activeIndex is the one connection sr-dash is attached to. Exactly one
	// at a time, on purpose: with several open at once it is too easy to run
	// a command against production while believing you are on staging.
	// A fresh start always attaches to the first (the default).
	activeIndex := 0

	// activeConn returns the connection in use. Callers hold mu.
	activeConn := func() (Connection, bool) {
		if activeIndex < 0 || activeIndex >= len(cfg.Connections) {
			return Connection{}, false
		}
		return cfg.Connections[activeIndex], true
	}

	// activeDSN is the connection string of the attached database. Every
	// collector reaches the server through it, so no screen can read a
	// database other than the one named in the sidebar.
	activeDSN := func() (string, error) {
		conn, ok := activeConn()
		if !ok {
			if len(cfg.Connections) == 0 {
				return "", errors.New("no connections configured")
			}
			return "", errors.New("not connected")
		}
		return conn.DSN, nil
	}

	// ensureSampler keeps one sampler alive for the active connection and
	// restarts it whenever that connection changes. Callers hold mu.
	ensureSampler := func() {
		if activeIndex >= len(cfg.Connections) {
			activeIndex = 0
		}
		want := ""
		conn, ok := activeConn()
		if ok {
			want = conn.DSN
		}
		if want == samplerDSN {
			return
		}
		if sampler != nil {
			sampler.Stop()
			sampler = nil
		}
		samplerDSN = want
		if want != "" {
			sampler = newSampler(want)
			debugf("event=connected index=%d title=%q dsn=%s",
				activeIndex, conn.Title, conn.MaskedDSN)
		}
		// The window title names the attached database, so even the OS
		// window switcher says which server a keystroke would reach.
		title := "sr-dash"
		if ok && conn.Title != "" {
			title = "sr-dash - " + conn.Title
		}
		w.Dispatch(func() { w.SetTitle(title) })
	}
	ensureSampler()
	defer func() {
		if sampler != nil {
			sampler.Stop()
		}
	}()

	// state assumes mu is held.
	state := func() uiState {
		if cfg.Connections == nil {
			cfg.Connections = []Connection{}
		}
		active := activeIndex
		if len(cfg.Connections) == 0 {
			active = -1
		}
		return uiState{
			Path:        cfg.Path,
			Exists:      cfg.Exists,
			Error:       configErr,
			Connections: cfg.Connections,
			Active:      active,
		}
	}

	mustBind(w, "configState", func() (uiState, error) {
		mu.Lock()
		defer mu.Unlock()
		return state(), nil
	})

	// connectTo attaches to one connection and, by doing so, detaches from
	// the previous one: sr-dash is never attached to two servers at once.
	mustBind(w, "connectTo", func(index int) (uiState, error) {
		mu.Lock()
		defer mu.Unlock()
		if index < 0 || index >= len(cfg.Connections) {
			return uiState{}, fmt.Errorf("connection %d does not exist", index)
		}
		activeIndex = index
		ensureSampler()
		return state(), nil
	})

	// disconnect closes the open connection and leaves sr-dash attached to
	// nothing: the sampler stops polling and the window title stops naming a
	// server. Being attached to no database is a legitimate resting state, not
	// an error to recover from.
	mustBind(w, "disconnect", func() (uiState, error) {
		mu.Lock()
		defer mu.Unlock()
		activeIndex = -1
		ensureSampler()
		debugf("event=disconnected")
		return state(), nil
	})

	mustBind(w, "dashboardState", func(windowSeconds int, sliceBy, topBy string) (dashOut, error) {
		mu.Lock()
		defer mu.Unlock()
		if sampler == nil {
			status := "no connection configured"
			if len(cfg.Connections) > 0 {
				status = "not connected"
			}
			return dashOut{
				Status:        status,
				Classes:       []string{},
				Buckets:       []bucketOut{},
				TopSQL:        []topSQLOut{},
				CounterCharts: []counterChartOut{},
			}, nil
		}
		out := sampler.Snapshot(time.Now(), windowSeconds, sliceBy, topBy)
		conn, _ := activeConn()
		out.Title = conn.Title
		out.DSN = conn.MaskedDSN
		return out, nil
	})

	mustBind(w, "sessionList", func() (sessionsOut, error) {
		mu.Lock()
		dsn, err := activeDSN()
		mu.Unlock()
		if err != nil {
			return sessionsOut{Error: err.Error()}, nil
		}
		out, err := collectSessionsList(context.Background(), dsn)
		if err != nil {
			return sessionsOut{Error: err.Error()}, nil
		}
		return out, nil
	})

	// killSession ends a connection on the attached database. It is the one
	// destructive action of the Sessions screen, and the page shows the
	// exact statement before it runs.
	mustBind(w, "killSession", func(id int) error {
		mu.Lock()
		dsn, err := activeDSN()
		mu.Unlock()
		if err != nil {
			return err
		}
		debugf("event=session_killed id=%d", id)
		return killSession(dsn, int64(id))
	})

	mustBind(w, "browserTree", func() (browserTreeOut, error) {
		mu.Lock()
		dsn, err := activeDSN()
		mu.Unlock()
		if err != nil {
			return browserTreeOut{Error: err.Error()}, nil
		}
		out, err := collectBrowserTreeJSON(context.Background(), dsn)
		if err != nil {
			return browserTreeOut{Error: err.Error()}, nil
		}
		return out, nil
	})

	mustBind(w, "openObject", func(name string) error {
		mu.Lock()
		dsn, err := activeDSN()
		mu.Unlock()
		if err != nil {
			return err
		}
		// The window is created on the UI thread. GTK is not thread-safe,
		// and a bind callback runs on a goroutine of its own, so building
		// a window here would crash the process on Linux.
		w.Dispatch(func() {
			openObjectWindow(name, func() (string, error) {
				mu.Lock()
				defer mu.Unlock()
				return activeDSN()
			})
		})
		debugf("event=object_window_open name=%s dsn=%s", name, dsn)
		return nil
	})

	mustBind(w, "clusterState", func() (clusterOut, error) {
		mu.Lock()
		dsn, err := activeDSN()
		mu.Unlock()
		if err != nil {
			return clusterOut{Error: err.Error()}, nil
		}
		db, err := openSR(dsn)
		if err != nil {
			return clusterOut{Error: err.Error()}, nil
		}
		defer func() { _ = db.Close() }()
		out, err := collectCluster(db)
		if err != nil {
			return clusterOut{Error: err.Error()}, nil
		}
		return out, nil
	})

	mustBind(w, "testConnection", func(dsn string) (testResult, error) {
		return testConnection(dsn), nil
	})

	// A JS exception inside the webview is invisible from the terminal; the
	// page forwards them here so -debug shows UI failures too.
	mustBind(w, "logError", func(msg string) error {
		debugf("event=ui_error msg=%q", msg)
		return nil
	})

	// addConnection appends a new connection to the config file and attaches
	// to it. The file is the single source of truth: it is re-read after every
	// change, so what the page shows is what is on disk.
	mustBind(w, "addConnection", func(dsn, title string) (uiState, error) {
		mu.Lock()
		defer mu.Unlock()
		dsn = strings.TrimSpace(dsn)
		if dsn == "" {
			return state(), errors.New("the connection string is required")
		}
		if err := appendConnection(cfg.Path, dsn, strings.TrimSpace(title)); err != nil {
			return state(), err
		}
		reloaded, err := loadConfig()
		if err != nil {
			return state(), err
		}
		cfg = reloaded
		configErr = ""
		activeIndex = len(cfg.Connections) - 1
		ensureSampler()
		return state(), nil
	})

	// updateConnection rewrites one entry in place.
	mustBind(w, "updateConnection", func(index int, dsn, title string) (uiState, error) {
		mu.Lock()
		defer mu.Unlock()
		if index < 0 || index >= len(cfg.Connections) {
			return state(), fmt.Errorf("connection %d does not exist", index)
		}
		dsn = strings.TrimSpace(dsn)
		if dsn == "" {
			return state(), errors.New("the connection string is required")
		}
		if err := updateConnection(cfg.Path, index, dsn, strings.TrimSpace(title)); err != nil {
			return state(), err
		}
		reloaded, err := loadConfig()
		if err != nil {
			return state(), err
		}
		cfg = reloaded
		configErr = ""
		ensureSampler()
		return state(), nil
	})

	// deleteConnection removes one entry and detaches if it was the attached
	// one.
	mustBind(w, "deleteConnection", func(index int) (uiState, error) {
		mu.Lock()
		defer mu.Unlock()
		if index < 0 || index >= len(cfg.Connections) {
			return state(), fmt.Errorf("connection %d does not exist", index)
		}
		if err := deleteConnection(cfg.Path, index); err != nil {
			return state(), err
		}
		reloaded, err := loadConfig()
		if err != nil {
			return state(), err
		}
		cfg = reloaded
		configErr = ""
		ensureSampler()
		return state(), nil
	})

	// makeDefault promotes a connection to be the one the app attaches to at
	// startup.
	mustBind(w, "makeDefault", func(index int) (uiState, error) {
		mu.Lock()
		defer mu.Unlock()
		if index < 0 || index >= len(cfg.Connections) {
			return state(), fmt.Errorf("connection %d does not exist", index)
		}
		if err := makeDefaultConnection(cfg.Path, index); err != nil {
			return state(), err
		}
		reloaded, err := loadConfig()
		if err != nil {
			return state(), err
		}
		cfg = reloaded
		configErr = ""
		return state(), nil
	})

	// connectionDSN hands the real connection string to the edit form and to
	// nothing else; every displayed or logged form stays masked.
	mustBind(w, "connectionDSN", func(index int) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if index < 0 || index >= len(cfg.Connections) {
			return "", fmt.Errorf("connection %d does not exist", index)
		}
		return cfg.Connections[index].DSN, nil
	})

	// The Edit menu is not cosmetic on macOS: its native selectors are the only
	// route Cocoa gives Cmd+X/C/V/A into the WKWebView's editing commands.
	// Windows and Linux deliver those shortcuts to the focused control directly,
	// so the menu (where supported at all) carries only Quit.
	items := []menu.Item{
		{Title: "sr-dash", Submenu: []menu.Item{
			{Title: "Quit sr-dash", Shortcut: "cmd+q", OnClick: w.Terminate},
		}},
	}
	if runtime.GOOS == "darwin" {
		// This is a desktop app, and with one window per object it is a
		// multi-window one: Cmd+W closing the key window is basic macOS
		// hygiene. performClose: targets whichever window is key, so the same
		// menu item serves the main window and every object window.
		items = append(items, menu.Item{Title: "File", Submenu: []menu.Item{
			{Title: "Close Window", Shortcut: "cmd+w", Selector: "performClose:"},
		}})
		items = append(items, menu.Item{Title: "Edit", Submenu: []menu.Item{
			{Title: "Undo", Shortcut: "cmd+z", Selector: "undo:"},
			{Title: "Redo", Shortcut: "cmd+shift+z", Selector: "redo:"},
			{Separator: true},
			{Title: "Cut", Shortcut: "cmd+x", Selector: "cut:"},
			{Title: "Copy", Shortcut: "cmd+c", Selector: "copy:"},
			{Title: "Paste", Shortcut: "cmd+v", Selector: "paste:"},
			{Title: "Select All", Shortcut: "cmd+a", Selector: "selectAll:"},
		}})
	}
	_, err = menu.Set(items, menu.Options{Window: w.Window()})
	if err != nil && !errors.Is(err, menu.ErrUnsupported) {
		debugf("event=menu_error err=%q", err)
	}

	w.Navigate("app://sr-dash/")
	w.Run()
}

// testConnection connects, asks the server its version, and disconnects.
// Bounded by a timeout so a dead host answers in seconds, not minutes.
func testConnection(dsn string) testResult {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return testResult{Error: "the connection string is required"}
	}

	db, err := openSR(dsn)
	if err != nil {
		debugf("event=test_connection ok=false err=%q", err)
		return testResult{Error: err.Error()}
	}
	defer func() { _ = db.Close() }()

	version, err := serverVersion(db)
	if err != nil {
		debugf("event=test_connection ok=false err=%q", err)
		return testResult{Error: err.Error()}
	}

	debugf("event=test_connection ok=true version=%q", version.string)
	return testResult{OK: true, Version: version.string}
}

// serveAsset maps an app:// request to the embedded ui/ directory, following
// glaze's examples/scheme shape: root falls back to index.html.
func serveAsset(req *glaze.SchemeRequest) *glaze.SchemeResponse {
	name := assetName(req.URL)
	data, err := uiFS.ReadFile("ui/" + name)
	if err != nil {
		return nil
	}
	ct := mime.TypeByExtension(path.Ext(name))
	if ct == "" {
		ct = "application/octet-stream"
	}
	return &glaze.SchemeResponse{Body: data, MIMEType: ct}
}

// assetName turns a request URL into a clean embedded-FS name, defaulting the
// root to index.html.
func assetName(reqURL string) string {
	p := reqURL
	u, err := url.Parse(reqURL)
	if err == nil && u.Path != "" {
		p = u.Path
	}
	p = strings.TrimPrefix(path.Clean("/"+p), "/")
	if p == "" || p == "." {
		return "index.html"
	}
	return p
}
