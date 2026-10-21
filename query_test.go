package main

import (
	"strings"
	"testing"
)

func strptr(s string) *string { return &s }

// The UPDATE the grid previews must address exactly one row, by the primary
// key, and change nothing else.
func TestBuildUpdate(t *testing.T) {
	req := gridRequest{
		Table: "pk_users",
		Key:   []string{"7"},
		Edits: []gridEdit{
			{Column: "name", Value: strptr("ana")},
			{Column: "balance", Value: strptr("12.50")},
		},
	}
	stmt, args := buildUpdate(req, []string{"user_id"})
	want := "UPDATE `pk_users` SET `name` = ?, `balance` = ? WHERE `user_id` = ?"
	if stmt != want {
		t.Errorf("statement =\n  %s\nwant\n  %s", stmt, want)
	}
	if len(args) != 3 {
		t.Fatalf("args = %d, want 3", len(args))
	}
	if args[0] != "ana" || args[1] != "12.50" || args[2] != "7" {
		t.Errorf("args = %v", args)
	}
}

// A cell set to NULL must reach the server as NULL, not as the four letters
// "NULL".
func TestBuildUpdateKeepsNull(t *testing.T) {
	req := gridRequest{
		Table: "pk_users",
		Key:   []string{"7"},
		Edits: []gridEdit{{Column: "email", Value: nil}},
	}
	_, args := buildUpdate(req, []string{"user_id"})
	if len(args) != 2 {
		t.Fatalf("args = %d, want 2", len(args))
	}
	if args[0] != nil {
		t.Errorf("the NULL value became %v", args[0])
	}
}

// A composite primary key goes into the WHERE clause in full: a partial key
// would address more than one row.
func TestBuildUpdateCompositeKey(t *testing.T) {
	req := gridRequest{
		Table: "t",
		Key:   []string{"1", "2"},
		Edits: []gridEdit{{Column: "c", Value: strptr("x")}},
	}
	stmt, args := buildUpdate(req, []string{"a", "b"})
	if !strings.Contains(stmt, "WHERE `a` = ? AND `b` = ?") {
		t.Errorf("statement = %s", stmt)
	}
	if len(args) != 3 {
		t.Errorf("args = %v", args)
	}
}

// A request that does not match the key cannot be turned into a statement:
// editing a row without knowing which one is not an edit.
func TestBuildUpdateRefusesAMismatch(t *testing.T) {
	req := gridRequest{
		Table: "t",
		Key:   []string{"1"},
		Edits: []gridEdit{{Column: "c", Value: strptr("x")}},
	}
	if stmt, _ := buildUpdate(req, []string{"a", "b"}); stmt != "" {
		t.Errorf("statement = %s, want none", stmt)
	}
	// An edit that changes nothing is not a statement either.
	req = gridRequest{Table: "t", Key: []string{"1"}}
	if stmt, _ := buildUpdate(req, []string{"a"}); stmt != "" {
		t.Errorf("statement = %s, want none", stmt)
	}
}

// An identifier with a backtick in it is quoted, not injected.
func TestQuoteIdent(t *testing.T) {
	if got := quoteIdent("a`b"); got != "`a``b`" {
		t.Errorf("quoteIdent = %q", got)
	}
}

func TestIsReadStatement(t *testing.T) {
	reads := []string{"SELECT 1", "  show tables", "DESC t", "explain select 1", "WITH x AS (SELECT 1) SELECT * FROM x"}
	for _, s := range reads {
		if !isReadStatement(s) {
			t.Errorf("isReadStatement(%q) = false, want true", s)
		}
	}
	writes := []string{"UPDATE t SET a=1", "insert into t values (1)", "DROP TABLE t", "CREATE TABLE t (a int)"}
	for _, s := range writes {
		if isReadStatement(s) {
			t.Errorf("isReadStatement(%q) = true, want false", s)
		}
	}
}
