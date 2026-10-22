package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/crgimenes/filo"
	"github.com/go-sql-driver/mysql"
)

// Connection is one database declared in the config file.
type Connection struct {
	// DSN is the raw MySQL DSN, the shape go-sql-driver/mysql accepts:
	// user:pass@tcp(host:port)/database. It is never shown or logged.
	DSN string `json:"-"`
	// Title is the name the user gave the connection.
	Title string `json:"title"`
	// MaskedDSN is the DSN with the password hidden; the only form that
	// ever reaches logs, JSON output, or the UI.
	MaskedDSN string `json:"dsn"`
	// FEHTTP is the frontend's HTTP endpoint, where the audit log and the
	// query profiles live. Derived from the DSN unless the config says
	// otherwise.
	FEHTTP string `json:"feHttp"`
}

// Config is the loaded configuration plus where it came from.
type Config struct {
	Path        string       `json:"path"`
	Exists      bool         `json:"exists"`
	Connections []Connection `json:"connections"`
}

// configPath resolves the config file location: a sekiban_init.filo in the
// current directory wins, otherwise $XDG_CONFIG_HOME/sekiban/init.filo
// (defaulting XDG_CONFIG_HOME to ~/.config).
//
// The program used to be called sr-dash, and a config file written under
// that name is still the user's: when no sekiban config exists but an old
// one does, the old one is used and keeps being written to. A rename must
// not cost anyone a connection.
func configPath() (string, error) {
	path, err := configPathFor("sekiban", "sekiban")
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}

	legacy, err := configPathFor("sr_dash", "sr-dash")
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(legacy); err == nil {
		return legacy, nil
	}
	return path, nil
}

// configPathFor returns where the config file of a program lives: a
// <local>_init.filo in the current directory wins, otherwise
// $XDG_CONFIG_HOME/<dir>/init.filo. The two names differ because the old
// program used an underscore for the file and a dash for the directory.
func configPathFor(local, dir string) (string, error) {
	name := local + "_init.filo"
	if _, err := os.Stat(name); err == nil {
		return name, nil
	}

	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve config path: %w", err)
		}
		configHome = filepath.Join(home, ".config")
	}
	return filepath.Join(configHome, dir, "init.filo"), nil
}

// loadConfig reads and parses the config file. A missing file is not an
// error: the returned Config has Exists=false and the caller decides what to
// do (the GUI opens the first-run setup screen).
func loadConfig() (Config, error) {
	path, err := configPath()
	if err != nil {
		return Config{}, err
	}

	cfg := Config{Path: path}
	b, err := os.ReadFile(filepath.Clean(path))
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("read config %s: %w", path, err)
	}

	cfg.Exists = true
	cfg.Connections, err = parseConfig(string(b))
	if err != nil {
		return cfg, fmt.Errorf("parse config %s: %w", path, err)
	}
	return cfg, nil
}

// parseConfig runs the Filo source and collects the connections it declares
// through the (database dsn [title] [http-url]) builtin.
//
// The third argument overrides the FE HTTP endpoint, which is otherwise
// derived from the DSN's host. It exists because the frontend's HTTP port
// is not always 8030 and is sometimes reached through a proxy under
// another name.
func parseConfig(src string) ([]Connection, error) {
	// Filo rejects a script with no forms ("empty script"), but a config file
	// holding only comments is legitimate: it is what deleting the last
	// connection leaves behind.
	if !hasCode(src) {
		return nil, nil
	}

	f := filo.New()
	defer f.Close()

	var conns []Connection
	err := f.RegisterBuiltin("database", func(_ context.Context, args []filo.Value) (filo.Value, error) {
		if len(args) < 1 {
			return filo.VBool(false), fmt.Errorf("database: dsn is required")
		}

		dsn, err := args[0].AsString()
		if err != nil {
			return filo.VBool(false), fmt.Errorf("database: dsn must be a string: %w", err)
		}
		if dsn == "" {
			return filo.VBool(false), fmt.Errorf("database: dsn must not be empty")
		}

		title := ""
		if len(args) >= 2 {
			title, err = args[1].AsString()
			if err != nil {
				return filo.VBool(false), fmt.Errorf("database: title must be a string: %w", err)
			}
		}

		feHTTP := ""
		if len(args) >= 3 {
			feHTTP, err = args[2].AsString()
			if err != nil {
				return filo.VBool(false), fmt.Errorf("database: http url must be a string: %w", err)
			}
		}

		if title == "" {
			title = defaultTitle(dsn)
		}
		if feHTTP == "" {
			feHTTP = deriveFEHTTP(dsn)
		}

		conns = append(conns, Connection{
			DSN:       dsn,
			Title:     title,
			MaskedDSN: maskDSN(dsn),
			FEHTTP:    feHTTP,
		})
		return filo.VBool(true), nil
	})
	if err != nil {
		return nil, err
	}

	err = f.DoString(src)
	if err != nil {
		return nil, err
	}
	return conns, nil
}

// appendConnection adds one (database ...) form to the config file, creating
// the file and its directory on first use. It only ever appends; the rest of
// the file belongs to the user and is never rewritten.
func appendConnection(path, dsn, title string) error {
	err := os.MkdirAll(filepath.Dir(path), 0o700)
	if err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	_, statErr := os.Stat(path)
	isNew := errors.Is(statErr, os.ErrNotExist)

	f, err := os.OpenFile(filepath.Clean(path), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open config %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	var b strings.Builder
	if isNew {
		b.WriteString("; sekiban configuration\n")
	}
	b.WriteString("(database ")
	b.WriteString(filoQuote(dsn))
	if title != "" {
		b.WriteString(" ")
		b.WriteString(filoQuote(title))
	}
	b.WriteString(")\n")

	_, err = f.WriteString(b.String())
	if err != nil {
		return fmt.Errorf("write config %s: %w", path, err)
	}
	return f.Close()
}

// updateConnection replaces the index-th (database ...) form in the config
// file, keeping the line's indentation and every other line untouched.
func updateConnection(path string, index int, dsn, title string) error {
	form := "(database " + filoQuote(dsn)
	if title != "" {
		form += " " + filoQuote(title)
	}
	form += ")"
	return rewriteConnectionLine(path, index, form)
}

// deleteConnection removes the index-th (database ...) form from the config
// file, leaving every other line untouched.
func deleteConnection(path string, index int) error {
	return rewriteConnectionLine(path, index, "")
}

// makeDefaultConnection moves the index-th (database ...) form to where the
// first one sits. The default connection is by definition the first declared,
// so promoting one is a line move: the line travels whole, carrying its
// indentation and any same-line comment, and nothing else in the file moves.
func makeDefaultConnection(path string, index int) error {
	lines, dbLines, err := readConnectionLines(path)
	if err != nil {
		return err
	}
	if index < 0 || index >= len(dbLines) {
		return fmt.Errorf("connection %d does not exist", index)
	}
	if index == 0 {
		return nil
	}

	// index > 0, so the source line is always below the destination and
	// removing it cannot shift where the first one sits.
	from, to := dbLines[index], dbLines[0]
	line := lines[from]
	lines = slices.Insert(slices.Delete(lines, from, from+1), to, line)
	return writeConfigLines(path, lines)
}

// rewriteConnectionLine performs the surgical edit behind update/delete: it
// replaces (or, with an empty form, removes) the index-th (database ...) line.
// Everything else in the file, comments and unrelated settings included, is
// preserved verbatim.
func rewriteConnectionLine(path string, index int, form string) error {
	lines, dbLines, err := readConnectionLines(path)
	if err != nil {
		return err
	}
	if index < 0 || index >= len(dbLines) {
		return fmt.Errorf("connection %d does not exist", index)
	}

	n := dbLines[index]
	if form == "" {
		lines = slices.Delete(lines, n, n+1)
	} else {
		trimmed := strings.TrimLeft(lines[n], " \t")
		indent := lines[n][:len(lines[n])-len(trimmed)]
		// Keep anything after the form, e.g. a same-line ; comment.
		suffix := trimmed[formEnd(trimmed):]
		lines[n] = indent + form + suffix
	}
	return writeConfigLines(path, lines)
}

// readConnectionLines reads the config and reports which of its lines hold
// exactly one complete (database ...) form each, in declaration order. When the
// file states connections in a shape line surgery cannot edit safely
// (multi-line or computed forms), it refuses with an error instead of guessing;
// the user edits the file directly.
func readConnectionLines(path string) (lines []string, dbLines []int, err error) {
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, nil, fmt.Errorf("read config %s: %w", path, err)
	}

	conns, err := parseConfig(string(b))
	if err != nil {
		return nil, nil, fmt.Errorf("parse config %s: %w", path, err)
	}

	lines = strings.Split(string(b), "\n")
	for i, line := range lines {
		if isDatabaseLine(line) {
			dbLines = append(dbLines, i)
		}
	}
	if len(dbLines) != len(conns) {
		return nil, nil, fmt.Errorf("config %s declares connections in a shape sekiban cannot edit safely (multi-line or computed forms); edit the file directly", path)
	}
	return lines, dbLines, nil
}

func writeConfigLines(path string, lines []string) error {
	err := os.WriteFile(filepath.Clean(path), []byte(strings.Join(lines, "\n")), 0o600) // #nosec G703 -- path comes from configPath (env/home), not untrusted input
	if err != nil {
		return fmt.Errorf("write config %s: %w", path, err)
	}
	return nil
}

// defaultTitle names an untitled connection without ever exposing the DSN on
// screen (a DSN carries a password): the database name when the connection
// string states one, otherwise the host.
func defaultTitle(dsn string) string {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return "connection"
	}
	if cfg.DBName != "" {
		return cfg.DBName
	}
	if host, _, ok := strings.Cut(cfg.Addr, ":"); ok && host != "" {
		return host
	}
	return "connection"
}

// deriveFEHTTP guesses the frontend's HTTP endpoint from the DSN's host. The
// query port and the HTTP port live on the same machine in a normal
// deployment, so the host carries over and the port becomes 8030.
//
// A DSN that does not name a TCP host (a unix socket, say) yields nothing:
// the config then has to state the endpoint itself.
func deriveFEHTTP(dsn string) string {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return ""
	}
	host, _, ok := strings.Cut(cfg.Addr, ":")
	if !ok || host == "" {
		return ""
	}
	return "http://" + host + ":8030"
}

// hasCode reports whether the Filo source contains anything beyond whitespace
// and ; comments.
func hasCode(src string) bool {
	src = strings.TrimPrefix(src, "\uFEFF")
	for line := range strings.SplitSeq(src, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, ";") {
			continue
		}
		return true
	}
	return false
}

// isDatabaseLine reports whether line starts a (database ...) form that is
// complete on that same line.
func isDatabaseLine(line string) bool {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "(database ") {
		return false
	}
	return formEnd(t) > 0
}

// formEnd scans a line that starts with "(" and returns the index just past
// the parenthesis that closes the first form (string literals and their
// escapes respected), or 0 when the form does not close on this line.
func formEnd(t string) int {
	depth := 0
	inString := false
	escaped := false
	for i, r := range t {
		if inString {
			switch {
			case escaped:
				escaped = false
			case r == '\\':
				escaped = true
			case r == '"':
				inString = false
			}
			continue
		}
		switch r {
		case '"':
			inString = true
		case ';':
			return 0
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return 0
}

// filoQuote renders s as a Filo string literal.
func filoQuote(s string) string {
	r := strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		"\n", `\n`,
		"\t", `\t`,
		"\r", `\r`,
	)
	return `"` + r.Replace(s) + `"`
}

// maskDSN hides the password in a connection string so it can be shown or
// logged. A string the driver cannot parse is fully redacted rather than
// leaked.
func maskDSN(dsn string) string {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return "(unparseable connection string)"
	}
	if cfg.Passwd == "" {
		return cfg.FormatDSN()
	}
	cfg.Passwd = "..."
	return cfg.FormatDSN()
}
