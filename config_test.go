package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseConfig(t *testing.T) {
	src := `; sr-dash configuration
(database "root:pw@tcp(fe.internal:9030)/analytics" "Production")
(database "root:@tcp(127.0.0.1:9030)/sr_dash_dev")
`
	conns, err := parseConfig(src)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if len(conns) != 2 {
		t.Fatalf("got %d connections, want 2", len(conns))
	}
	if conns[0].Title != "Production" {
		t.Errorf("title = %q, want Production", conns[0].Title)
	}
	if conns[0].DSN != "root:pw@tcp(fe.internal:9030)/analytics" {
		t.Errorf("dsn = %q", conns[0].DSN)
	}
	// The password must not survive into anything the UI shows.
	if conns[0].MaskedDSN == conns[0].DSN {
		t.Errorf("masked dsn still carries the password: %q", conns[0].MaskedDSN)
	}
	if got := conns[0].MaskedDSN; got != "root:...@tcp(fe.internal:9030)/analytics" {
		t.Errorf("masked dsn = %q", got)
	}
	// An untitled connection is named after its database.
	if conns[1].Title != "sr_dash_dev" {
		t.Errorf("default title = %q, want sr_dash_dev", conns[1].Title)
	}
	// The FE endpoint is derived from the host.
	if conns[0].FEHTTP != "http://fe.internal:8030" {
		t.Errorf("fe http = %q", conns[0].FEHTTP)
	}
}

// A third argument overrides the derived endpoint, for the deployments where
// the frontend's HTTP port is not 8030.
func TestParseConfigHTTPOverride(t *testing.T) {
	src := `(database "root:@tcp(fe.internal:9030)/db" "Prod" "http://proxy.lan:18030")`
	conns, err := parseConfig(src)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if conns[0].FEHTTP != "http://proxy.lan:18030" {
		t.Errorf("fe http = %q, want the override", conns[0].FEHTTP)
	}
}

// A config file holding only comments is what deleting the last connection
// leaves behind, and it must not be an error.
func TestParseConfigOnlyComments(t *testing.T) {
	conns, err := parseConfig("; sr-dash configuration\n; nothing here\n")
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if len(conns) != 0 {
		t.Errorf("got %d connections, want 0", len(conns))
	}
}

func TestMaskDSN(t *testing.T) {
	cases := map[string]string{
		"root:pw@tcp(fe.internal:9030)/analytics": "root:...@tcp(fe.internal:9030)/analytics",
		"root:@tcp(127.0.0.1:9030)/db":            "root@tcp(127.0.0.1:9030)/db",
	}
	for in, want := range cases {
		if got := maskDSN(in); got != want {
			t.Errorf("maskDSN(%q) = %q, want %q", in, got, want)
		}
	}
	// A string the driver cannot read is redacted whole, not leaked.
	if got := maskDSN("this is not a dsn"); got != "(unparseable connection string)" {
		t.Errorf("maskDSN of nonsense = %q", got)
	}
}

func TestDeriveFEHTTP(t *testing.T) {
	if got := deriveFEHTTP("root:pw@tcp(fe.internal:9030)/db"); got != "http://fe.internal:8030" {
		t.Errorf("deriveFEHTTP = %q", got)
	}
	// A DSN with no TCP host yields nothing rather than a broken URL.
	if got := deriveFEHTTP("root:pw@unix(/tmp/mysql.sock)/db"); got != "" {
		t.Errorf("deriveFEHTTP of a socket = %q, want empty", got)
	}
}

func TestAppendAndUpdateConnection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sr_dash_init.filo")

	if err := appendConnection(path, "root:pw@tcp(a:9030)/one", "One"); err != nil {
		t.Fatalf("appendConnection: %v", err)
	}
	if err := appendConnection(path, "root:pw@tcp(b:9030)/two", "Two"); err != nil {
		t.Fatalf("appendConnection: %v", err)
	}

	cfg, err := loadConfigAt(path)
	if err != nil {
		t.Fatalf("loadConfigAt: %v", err)
	}
	if len(cfg.Connections) != 2 {
		t.Fatalf("got %d connections, want 2", len(cfg.Connections))
	}

	if err := updateConnection(path, 0, "root:pw@tcp(c:9030)/three", "Three"); err != nil {
		t.Fatalf("updateConnection: %v", err)
	}
	cfg, err = loadConfigAt(path)
	if err != nil {
		t.Fatalf("loadConfigAt: %v", err)
	}
	if cfg.Connections[0].Title != "Three" {
		t.Errorf("title = %q, want Three", cfg.Connections[0].Title)
	}
	if cfg.Connections[1].Title != "Two" {
		t.Errorf("the second connection changed: %q", cfg.Connections[1].Title)
	}

	// Deleting one leaves the other alone.
	if err := deleteConnection(path, 0); err != nil {
		t.Fatalf("deleteConnection: %v", err)
	}
	cfg, err = loadConfigAt(path)
	if err != nil {
		t.Fatalf("loadConfigAt: %v", err)
	}
	if len(cfg.Connections) != 1 || cfg.Connections[0].Title != "Two" {
		t.Errorf("after delete: %+v", cfg.Connections)
	}

	// The file is 0600: it holds a password.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config file mode = %o, want 600", perm)
	}
}

// loadConfigAt reads a specific config file, for the tests that need one
// outside the working directory.
func loadConfigAt(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	conns, err := parseConfig(string(b))
	if err != nil {
		return Config{}, err
	}
	return Config{Path: path, Exists: true, Connections: conns}, nil
}
