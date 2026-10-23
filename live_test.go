//go:build uicheck

package main

// The live checks: the parts that only mean something against a real
// server. They are behind the uicheck build tag because they need one.
//
//   SR_SEKIBAN_DSN="root:@tcp(127.0.0.1:9030)/sr_dash_dev" go test -tags uicheck -v .

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"
)

func liveDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("SR_SEKIBAN_DSN")
	if dsn == "" {
		t.Skip("SR_SEKIBAN_DSN is not set")
	}
	db, err := openSR(dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// The grid's write path, end to end: read the rows, change a cell, see the
// statement that would run, and run it.
func TestGridWritePath(t *testing.T) {
	db := liveDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	res, err := runStatement(ctx, db, "SELECT * FROM pk_users ORDER BY user_id LIMIT 10")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !res.ReturnsRows || len(res.Rows) == 0 {
		t.Fatalf("no rows: %+v", res)
	}

	// The primary key of the first row, read from the catalog rather than
	// from the page.
	pk, err := gridPrimaryKey(db, browserDatabase(db), "pk_users")
	if err != nil {
		t.Fatalf("primary key: %v", err)
	}
	if len(pk) != 1 || pk[0] != "user_id" {
		t.Fatalf("primary key = %v, want [user_id]", pk)
	}

	// Build an edit of the second column of the first row.
	col := res.Columns[1].Name
	old := ""
	if res.Rows[0][1] != nil {
		old = *res.Rows[0][1]
	}
	want := old + "-edited"
	req := gridRequest{
		Table: "pk_users",
		Key:   []string{*res.Rows[0][0]},
		Edits: []gridEdit{{Column: col, Value: &want}},
	}

	stmt, args := buildUpdate(req, pk)
	if !strings.Contains(stmt, "WHERE `user_id` = ?") {
		t.Fatalf("statement = %s", stmt)
	}
	if len(args) != 2 || args[1] != *res.Rows[0][0] {
		t.Fatalf("args = %v", args)
	}

	affected, err := saveUpdate(ctx, db, req, pk)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if affected != 1 {
		t.Errorf("affected = %d, want 1", affected)
	}

	// Read it back.
	after, err := runStatement(ctx, db, "SELECT * FROM pk_users ORDER BY user_id LIMIT 10")
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if *after.Rows[0][1] != want {
		t.Errorf("value = %q, want %q", *after.Rows[0][1], want)
	}
}

// Only a primary key table accepts an update. A duplicate key table and a
// unique key table both refuse.
func TestGridRefusesTablesThatCannotBeUpdated(t *testing.T) {
	db := liveDB(t)
	for _, table := range []string{"dup_events", "uk_orders", "agg_daily"} {
		if _, err := gridPrimaryKey(db, browserDatabase(db), table); err == nil {
			t.Errorf("gridPrimaryKey(%s) should have failed", table)
		}
	}
}

// The query editor reads rows and says so, and a statement that changes
// things reports how many rows it changed.
func TestRunStatement(t *testing.T) {
	db := liveDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	res, err := runStatement(ctx, db, "SELECT 1 AS one, 'two' AS two")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if !res.ReturnsRows {
		t.Errorf("the select did not report rows")
	}
	if len(res.Rows) != 1 || len(res.Rows[0]) != 2 {
		t.Fatalf("rows = %v", res.Rows)
	}
	if *res.Rows[0][0] != "1" || *res.Rows[0][1] != "two" {
		t.Errorf("row = %v", res.Rows[0])
	}
	// A NULL cell stays NULL, and is not the four letters "NULL".
	nullRes, err := runStatement(ctx, db, "SELECT NULL AS nothing")
	if err != nil {
		t.Fatalf("null: %v", err)
	}
	if nullRes.Rows[0][0] != nil {
		t.Errorf("the NULL became %v", *nullRes.Rows[0][0])
	}
}

// The browser tree lists the objects of the connected database, including
// the materialized views that no catalog view lists.
func TestBrowserTree(t *testing.T) {
	db := liveDB(t)
	tree := collectBrowserTree(db)
	if tree.Error != "" {
		t.Fatalf("tree: %s", tree.Error)
	}
	if tree.Database == "" {
		t.Error("the tree names no database")
	}
	groups := map[string]browserGroup{}
	for _, g := range tree.Groups {
		groups[g.Key] = g
	}
	for _, key := range []string{"tables", "views", "materialized_views"} {
		if _, ok := groups[key]; !ok {
			t.Errorf("the tree has no %s group", key)
		}
	}
	// Every table carries its model, so the tree can say whether it can be
	// edited.
	for _, obj := range groups["tables"].Objects {
		if obj.Model == "" {
			t.Errorf("table %s has no model", obj.Name)
		}
	}
}

// The object detail carries the catalog of the object and says whether it
// can be edited.
func TestObjectDetail(t *testing.T) {
	db := liveDB(t)
	name := browserDatabase(db)

	detail := collectObjectDetail(db, name, "pk_users")
	if detail.Error != "" {
		t.Fatalf("detail: %s", detail.Error)
	}
	if detail.Model != "PRIMARY_KEYS" {
		t.Errorf("model = %s", detail.Model)
	}
	if !detail.Editable {
		t.Error("a primary key table is not editable")
	}
	if len(detail.PKColumns) != 1 || detail.PKColumns[0] != "user_id" {
		t.Errorf("pk columns = %v", detail.PKColumns)
	}
	if detail.DDL == "" {
		t.Error("no DDL")
	}
	if len(detail.Columns) != 4 {
		t.Errorf("columns = %d, want 4", len(detail.Columns))
	}

	// A duplicate key table is not editable, and says so.
	dup := collectObjectDetail(db, name, "dup_events")
	if dup.Error != "" {
		t.Fatalf("detail: %s", dup.Error)
	}
	if dup.Editable {
		t.Error("a duplicate key table is editable")
	}
}

// The audit log is the only complete record of the queries that have
// finished. It is read over the frontend's HTTP API.
// The frontend's configuration, read the way the Variables screen reads it:
// every setting, with its type and whether it can be changed at run time.
func TestFrontendVariables(t *testing.T) {
	db := liveDB(t)
	out := collectVariables(db)
	if out.Error != "" {
		t.Fatalf("collect: %s", out.Error)
	}
	if len(out.Rows) < 100 {
		t.Fatalf("got %d variables, want the frontend's whole configuration", len(out.Rows))
	}

	var port *variableEntry
	for i := range out.Rows {
		if out.Rows[i].Key == "http_port" {
			port = &out.Rows[i]
			break
		}
	}
	if port == nil {
		t.Fatal("no http_port among the variables")
	}
	// The port the frontend listens on is not a setting to change while it
	// runs.
	if port.Mutable {
		t.Error("http_port reads as mutable")
	}
	if port.Type == "" || port.Value == "" {
		t.Errorf("http_port = %+v, want a type and a value", *port)
	}
	if out.MutableCount == 0 || out.MutableCount == len(out.Rows) {
		t.Errorf("MutableCount = %d of %d: neither all nor none",
			out.MutableCount, len(out.Rows))
	}
}

func TestAuditLog(t *testing.T) {
	dsn := os.Getenv("SR_SEKIBAN_DSN")
	if dsn == "" {
		t.Skip("SR_SEKIBAN_DSN is not set")
	}
	endpoint := os.Getenv("SR_SEKIBAN_FE_HTTP")
	if endpoint == "" {
		endpoint = "http://127.0.0.1:8030"
	}
	fe := newFEClient(endpoint, dsn)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	body, err := fe.auditLog(ctx)
	if err != nil {
		t.Fatalf("audit log: %v", err)
	}
	if len(body) == 0 {
		t.Fatal("the audit log is empty")
	}
	records := parseAuditLog(body)
	if len(records) == 0 {
		t.Fatal("no record parsed")
	}
	queries := 0
	for _, rec := range records {
		if rec.field("IsQuery") == "true" {
			queries++
		}
		if rec.field("QueryId") == "" {
			t.Errorf("a record has no query id: %v", rec.Fields)
			break
		}
	}
	if queries == 0 {
		t.Errorf("no queries among %d records", len(records))
	}
	t.Logf("%d records, %d of them queries", len(records), queries)

	// The summary ranks the queries by the CPU they cost.
	summary := summariseAuditLog(records)
	if len(summary) == 0 {
		t.Fatal("the summary is empty")
	}
	for i, s := range summary {
		if s.Statement == "" {
			t.Errorf("entry %d has no statement", i)
			break
		}
		if s.Calls == 0 {
			t.Errorf("entry %d has no calls", i)
			break
		}
	}
	t.Logf("top: %s — %d calls, %.2f CPU sec", summary[0].Statement, summary[0].Calls, summary[0].CPUSec)
}
