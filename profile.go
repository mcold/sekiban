package main

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
)

// The Query Profile screen.
//
// StarRocks has no equivalent of pg_stat_statements: the complete record of
// the queries that have finished lives in the frontend's audit log, which is
// a file on the frontend's own disk. The file is read over the frontend's
// HTTP API, on demand, and accumulated in memory.
//
// The log is the only complete source. SHOW PROFILELIST shows only the
// queries whose client asked to be profiled, or that ran longer than
// big_query_profile_threshold.

// profileEntry is one query of the profile list.
type profileEntry struct {
	QueryID   string `json:"queryId"`
	StartTime string `json:"startTime"`
	Time      string `json:"time"`
	State     string `json:"state"`
	Statement string `json:"statement"`
}

// profileOut is what the screen shows.
type profileOut struct {
	Error string `json:"error,omitempty"`
	// Profiles is the frontend's own list: the queries it holds a profile
	// of.
	Profiles []profileEntry `json:"profiles"`
	// Summary ranks the audit log by the CPU the queries cost.
	Summary []auditSummary `json:"summary"`
	// SummaryTotal is how many distinct statements the audit log holds.
	// Summary itself is the heaviest auditSummaryLimit of them.
	SummaryTotal int `json:"summaryTotal"`
	// HistoryCount is how many queries the audit log holds.
	HistoryCount int `json:"historyCount"`
	// HistoryError says why the audit log could not be read, if it could
	// not be. The screen still works without it.
	HistoryError string `json:"historyError,omitempty"`
}

// collectProfiles reads the profile list and, on demand, the audit log.
func collectProfiles(ctx context.Context, db *sql.DB, fe *feClient, withHistory bool) profileOut {
	out := profileOut{Profiles: []profileEntry{}, Summary: []auditSummary{}}

	res, err := queryRows(db, "SHOW PROFILELIST")
	if err == nil {
		for i := range res.rows {
			out.Profiles = append(out.Profiles, profileEntry{
				QueryID:   res.value(i, "QueryId"),
				StartTime: res.value(i, "StartTime"),
				Time:      res.value(i, "Time"),
				State:     res.value(i, "State"),
				Statement: collapseStatement(res.value(i, "Statement")),
			})
		}
	}

	if !withHistory || fe == nil {
		return out
	}
	fetched, err := fe.auditLog(ctx)
	if err != nil {
		out.HistoryError = err.Error()
		return out
	}
	records := parseAuditLog(fetched)
	out.Summary = summariseAuditLog(records)
	out.HistoryCount = len(records)
	// A day of audit log holds thousands of distinct statements, and the
	// screen can only use the heaviest of them: the list is a ranking, and
	// the tail of it is not worth drawing.
	out.SummaryTotal = len(out.Summary)
	if len(out.Summary) > auditSummaryLimit {
		out.Summary = out.Summary[:auditSummaryLimit]
	}
	return out
}

// auditSummaryLimit is how many statements the ranking keeps.
const auditSummaryLimit = 500

// auditAggregator accumulates the audit log into a ranking of statements.
//
// The same statement is one entry, whether it ran once or a thousand
// times: the digest is the identity, and the text is only the label.
type auditAggregator struct {
	byKey map[string]*auditSummary
	order []string
	count int
}

// add folds more records into the ranking.
func (a *auditAggregator) add(records []auditRecord) {
	if a.byKey == nil {
		a.byKey = map[string]*auditSummary{}
	}
	for _, rec := range records {
		if rec.field("IsQuery") != "true" {
			continue
		}
		stmt := rec.field("Stmt")
		digest := rec.field("Digest")
		key := digest
		if key == "" {
			key = stmt
		}
		if key == "" {
			continue
		}
		entry, ok := a.byKey[key]
		if !ok {
			entry = &auditSummary{
				Statement: stmt,
				Digest:    digest,
				User:      rec.field("User"),
				DB:        rec.field("Db"),
				First:     rec.Timestamp,
				Last:      rec.Timestamp,
			}
			a.byKey[key] = entry
			a.order = append(a.order, key)
		}
		entry.Calls++
		entry.CPUSec += parseNanoseconds(rec.field("CpuCostNs"))
		ms := parseInt64(rec.field("Time"))
		entry.TotalMS += ms
		if ms > entry.MaxMS {
			entry.MaxMS = ms
		}
		if rec.field("State") != "OK" {
			entry.Errors++
		}
		if rec.Timestamp.Before(entry.First) {
			entry.First = rec.Timestamp
		}
		if rec.Timestamp.After(entry.Last) {
			entry.Last = rec.Timestamp
		}
	}
	a.count += len(records)
}

// summary returns the ranking, heaviest first.
func (a *auditAggregator) summary() []auditSummary {
	out := make([]auditSummary, 0, len(a.order))
	for _, key := range a.order {
		out = append(out, *a.byKey[key])
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CPUSec != out[j].CPUSec {
			return out[i].CPUSec > out[j].CPUSec
		}
		return out[i].Statement < out[j].Statement
	})
	return out
}

// auditCache holds what the app has already read of the audit log.
//
// The endpoint serves the whole file every time, with no Range support, and
// the file grows without bound, so the cache keeps only a short prefix of
// it and the ranking it has built. The prefix is what tells a grown file
// from a rolled one: the log only grows until it rolls, and after a roll
// the new file does not start with the old one's beginning.
type auditCache struct {
	mu      sync.Mutex
	prefix  string
	length  int
	ranking auditAggregator
}

// auditPrefixLength is how much of the log the cache keeps to recognize a
// file that has grown rather than rolled.
const auditPrefixLength = 4096

// update takes the freshly fetched log and folds whatever is new into the
// ranking.
func (a *auditCache) update(fetched string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	switch {
	case a.length == 0:
		a.ranking.add(parseAuditLog(fetched))
	case strings.HasPrefix(fetched, a.prefix) && len(fetched) >= a.length:
		a.ranking.add(parseAuditLog(fetched[a.length:]))
	default:
		// The file rolled, or it was rewritten: start again.
		a.ranking = auditAggregator{}
		a.ranking.add(parseAuditLog(fetched))
	}
	a.length = len(fetched)
	if len(fetched) > auditPrefixLength {
		a.prefix = fetched[:auditPrefixLength]
	} else {
		a.prefix = fetched
	}
}

// summary returns the ranking, and how many records it was built from.
func (a *auditCache) summary() ([]auditSummary, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ranking.summary(), a.ranking.count
}

// --- The frontend's HTTP API ---

// feClient talks to the frontend's HTTP API over Basic auth.
type feClient struct {
	baseURL string
	user    string
	pass    string
	client  *http.Client
}

// newFEClient builds a client for the frontend's HTTP endpoint. The
// credentials are the ones the connection string carries: a user holding
// OPERATE can read the audit log.
func newFEClient(baseURL, dsn string) *feClient {
	c := &feClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: 60 * time.Second},
	}
	cfg, err := parseMySQLDSN(dsn)
	if err == nil {
		c.user = cfg.user
		c.pass = cfg.pass
	}
	return c
}

// dsnParts are the pieces of a MySQL connection string.
type dsnParts struct {
	user string
	pass string
}

// parseMySQLDSN reads the user and the password out of a MySQL DSN.
func parseMySQLDSN(dsn string) (dsnParts, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return dsnParts{}, err
	}
	return dsnParts{user: cfg.User, pass: cfg.Passwd}, nil
}

// get fetches a path from the frontend's API.
func (c *feClient) get(ctx context.Context, path string) ([]byte, error) {
	if c.baseURL == "" {
		return nil, fmt.Errorf("the frontend's HTTP endpoint is not known")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	if c.user != "" {
		req.SetBasicAuth(c.user, c.pass)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the frontend answered %s", resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// auditLogPath is the endpoint that serves the audit log.
//
// type=system is the only working value: type=file and type=file_i are
// rejected. The response is the whole file, with no Range support.
const auditLogPath = "/api/get_log_file?type=system&file=fe.audit.log"

// auditLog reads the frontend's audit log, whole.
//
// It comes back whole every time: the endpoint has no Range support. The
// diff against what was read before is the caller's job.
func (c *feClient) auditLog(ctx context.Context) (string, error) {
	body, err := c.get(ctx, auditLogPath)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// queryProfile fetches the full profile of one query.
func (c *feClient) queryProfile(ctx context.Context, queryID string) (string, error) {
	body, err := c.get(ctx, "/api/profile?query_id="+url.QueryEscape(queryID))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// profileDoc is the profile the frontend recorded of one query, read.
//
// It is the same text the frontend's own web page shows, under
// /query_profile?query_id=..., taken apart so the interface can draw it.
type profileDoc struct {
	// Error carries a failure, so the page always gets something to show.
	Error string `json:"error,omitempty"`
	// QueryID is the query the profile is of.
	QueryID string `json:"queryId"`
	// Nodes are the sections of the profile.
	Nodes []*treeNode `json:"nodes"`
	// Raw is the profile as it came.
	Raw string `json:"raw,omitempty"`
}

// rootSections are the sections the frontend writes under "Query:".
//
// They are recognised by name and not by their place in the tree, because
// the frontend prints "Reason:" one step deeper than its siblings.
var rootSections = map[string]bool{
	"Summary":   true,
	"Planner":   true,
	"Reason":    true,
	"Execution": true,
}

// parseQueryProfile reads the profile the frontend recorded of a query.
//
// The text is an indented tree: a line that ends in a colon names a thing,
// and a line that starts with a dash is a measurement of the thing above it.
// The indentation says how deep a thing is.
func parseQueryProfile(queryID, text string) profileDoc {
	doc := profileDoc{QueryID: queryID, Nodes: []*treeNode{}, Raw: text}
	root := &treeNode{Name: "Query"}
	doc.Nodes = append(doc.Nodes, root)

	// The stack is the path of open things: the root, then every section
	// and measurement that is still open.
	type frame struct {
		indent int
		node   *treeNode
	}
	stack := []frame{{indent: -1, node: root}}
	// The planner has a tree of its own, nested by the spaces that follow
	// the dash rather than by the indentation.
	var planner []planStep

	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))

		if strings.HasPrefix(trimmed, "- ") {
			for len(stack) > 1 && stack[len(stack)-1].indent >= indent {
				stack = stack[:len(stack)-1]
			}
			parent := stack[len(stack)-1].node
			item := trimmed[2:]
			if entry, ok := parsePlannerEntry(item); ok {
				for len(planner) > 0 && planner[len(planner)-1].depth >= entry.depth {
					planner = planner[:len(planner)-1]
				}
				if len(planner) == 0 {
					parent.Children = append(parent.Children, entry.node)
				} else {
					holder := planner[len(planner)-1].node
					holder.Children = append(holder.Children, entry.node)
				}
				planner = append(planner, planStep{depth: entry.depth, node: entry.node})
				continue
			}
			for _, field := range splitMetrics(item) {
				parent.Children = append(parent.Children, field)
				stack = append(stack, frame{indent: indent, node: field})
			}
			continue
		}

		if strings.HasSuffix(trimmed, ":") {
			name := strings.TrimSpace(strings.TrimSuffix(trimmed, ":"))
			for len(stack) > 1 && stack[len(stack)-1].indent >= indent {
				stack = stack[:len(stack)-1]
			}
			parent := stack[len(stack)-1].node
			if rootSections[name] {
				parent = root
			}
			// The profile opens with "Query:", which is the root itself
			// and not a section under it.
			if name == "Query" && len(root.Children) == 0 {
				stack = append(stack, frame{indent: indent, node: root})
				continue
			}
			node := &treeNode{Name: name}
			parent.Children = append(parent.Children, node)
			stack = append(stack, frame{indent: indent, node: node})
			planner = nil
			continue
		}

		// A line that is neither a section nor a measurement: keep it as
		// a measurement of its own, so that nothing is lost.
		for len(stack) > 1 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}
		parent := stack[len(stack)-1].node
		parent.Children = append(parent.Children, splitMetrics(trimmed)...)
	}
	return doc
}

// parsePlannerEntry reads one step of the planner's own tree.
//
// The step is written as a dash, some dashes, a name and a time: "--
// Parser[1] 6ms". How deep it sits is in the spaces between the first dash
// and the second.
func parsePlannerEntry(item string) (planStep, bool) {
	trimmed := strings.TrimLeft(item, " ")
	extra := len(item) - len(trimmed)
	if !strings.HasPrefix(trimmed, "--") {
		return planStep{}, false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "--"))
	name, value := rest, ""
	if i := strings.LastIndex(rest, " "); i > 0 {
		name, value = rest[:i], rest[i+1:]
	}
	return planStep{depth: extra / 4, node: &treeNode{Name: name, Value: value}}, true
}

// --- The audit log ---

// auditRecord is one line of the frontend's audit log.
//
// A line looks like this:
//
//	2026-10-21 12:01:30.162Z [query] |Timestamp=...|Client=...|...|Stmt=SELECT 1|...
//
// The fields are pipe separated, and the statement may itself contain a
// pipe, so the parser is careful about where a field ends.
type auditRecord struct {
	Timestamp time.Time         `json:"timestamp"`
	Module    string            `json:"module"`
	Fields    map[string]string `json:"fields"`
}

// field returns one field of the record.
func (r auditRecord) field(name string) string {
	return r.Fields[name]
}

// parseAuditLog reads the audit log. A line it cannot read is skipped: a
// truncated last line is normal when the file is being written.
func parseAuditLog(src string) []auditRecord {
	out := []auditRecord{}
	sc := bufio.NewScanner(strings.NewReader(src))
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		if rec, ok := parseAuditLine(sc.Text()); ok {
			out = append(out, rec)
		}
	}
	return out
}

// parseAuditLine reads one line of the audit log.
func parseAuditLine(line string) (auditRecord, bool) {
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return auditRecord{}, false
	}
	// The timestamp and the module come first, then the fields.
	parts := strings.SplitN(line, "|", 3)
	if len(parts) < 3 {
		return auditRecord{}, false
	}
	head := strings.TrimSpace(parts[0])
	// head is "<timestamp> [module] ".
	if i := strings.Index(head, "["); i >= 0 {
		if j := strings.Index(head, "]"); j > i {
			head = head[:i]
		}
	}
	ts, err := time.Parse("2006-01-02 15:04:05.000Z", strings.TrimSpace(head))
	if err != nil {
		// The timestamp format carries a fractional part whose length
		// varies; try the other shape before giving up.
		ts, err = time.Parse("2006-01-02 15:04:05Z", strings.TrimSpace(head))
		if err != nil {
			return auditRecord{}, false
		}
	}
	module := ""
	if i := strings.Index(parts[0], "["); i >= 0 {
		if j := strings.Index(parts[0][i:], "]"); j > 0 {
			module = parts[0][i+1 : i+j]
		}
	}

	rec := auditRecord{Timestamp: ts, Module: module, Fields: map[string]string{}}
	fields := strings.Split(parts[2], "|")
	for i := 0; i < len(fields); i++ {
		key, value, found := strings.Cut(fields[i], "=")
		if !found {
			continue
		}
		// The statement is the one field that may contain a pipe, so
		// it swallows the fields that follow it until the next known
		// key. Everything after Stmt in the log is known.
		if key == "Stmt" {
			for i+1 < len(fields) && !knownAuditField(fields[i+1]) {
				i++
				value += "|" + fields[i]
			}
		}
		rec.Fields[key] = value
	}
	return rec, true
}

// knownAuditField reports whether a pipe-separated chunk starts a field
// the audit log writes. It is how the statement's own pipes are told apart
// from the fields that follow it.
func knownAuditField(chunk string) bool {
	key, _, found := strings.Cut(chunk, "=")
	if !found {
		return false
	}
	switch key {
	case "Timestamp", "Client", "User", "AuthorizedUser", "ResourceGroup", "Catalog",
		"Db", "State", "ErrorCode", "Time", "ScanBytes", "ScanRows", "ReturnRows",
		"CpuCostNs", "MemCostBytes", "StmtId", "QueryId", "IsQuery", "feIp", "Stmt",
		"Digest", "PlanCpuCost", "PlanMemCost", "Warehouse", "CNGroup",
		"QueryFEAllocatedMemory", "SessionId", "CustomQueryId", "TransmittedBytes",
		"QuerySource":
		return true
	}
	return false
}

// auditSummary is the ranking of the audit log: what the queries cost.
type auditSummary struct {
	// Statement is the text of the query, or its digest when there is
	// none.
	Statement string `json:"statement"`
	// Digest is the server's digest of the statement, when it has one.
	Digest  string  `json:"digest"`
	Calls   int     `json:"calls"`
	CPUSec  float64 `json:"cpuSec"`
	TotalMS int64   `json:"totalMs"`
	MaxMS   int64   `json:"maxMs"`
	Errors  int     `json:"errors"`
	User    string  `json:"user"`
	DB      string  `json:"db"`
	// First and Last are when the query ran, first and last.
	First time.Time `json:"first"`
	Last  time.Time `json:"last"`
}

// summariseAuditLog ranks the audit log by the CPU the queries cost.
func summariseAuditLog(records []auditRecord) []auditSummary {
	var a auditAggregator
	a.add(records)
	return a.summary()
}

// parseNanoseconds reads a nanosecond count.
func parseNanoseconds(s string) float64 {
	return float64(parseInt64(s)) / 1e9
}

// parseInt64 reads an integer, and reads a missing one as zero.
func parseInt64(s string) int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return v
}
