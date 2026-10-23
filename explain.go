package main

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
)

// The plan of a query, read.
//
// EXPLAIN ANALYZE answers with a text tree, dressed in ANSI colour: the
// colour marks the node that cost the most, which is the only thing it is
// good for. A program that wants the numbers has to take the colour off, and
// a program that wants to draw the plan has to know which node is under
// which — so the tree is read into a structure here, and the interface draws
// that.
//
// The drawing itself says what belongs where: a node's line opens with the
// connectors of the path down to it, and a measurement's line starts at the
// column of the node it belongs to. Nothing else has to be guessed.

// treeNode is one node of a tree: a named thing, with things under it.
//
// A measurement is a node too, with a label where a name would be: the
// interface draws both the same way. The children keep the order they were
// written in, because that order is the order of the plan.
type treeNode struct {
	// Name is what the thing is called.
	Name string `json:"name"`
	// Value is the thing's own measurement, when it has one.
	Value string `json:"value,omitempty"`
	// Children are the things under it: the measurements and the
	// sub-things, in the order they were written.
	Children []*treeNode `json:"children,omitempty"`
	// col is the column the measurement started at. It says whether a
	// measurement is the node's own or hangs under the one above it.
	col int
}

// explainOut is what EXPLAIN ANALYZE answered.
type explainOut struct {
	// Error carries a failure, so the page always gets something to show.
	Error string `json:"error,omitempty"`
	// Summary is what the query cost, in the frontend's own words.
	Summary []*treeNode `json:"summary"`
	// TopTime and TopMemory are the nodes the frontend itself ranked.
	TopTime   []string `json:"topTime"`
	TopMemory []string `json:"topMemory"`
	// Variables are the session variables that were not at their default.
	Variables []*treeNode `json:"variables"`
	// Fragments are the trees the query ran in.
	Fragments []*treeNode `json:"fragments"`
	// Raw is the answer as it came, without the colour. It is the
	// fallback for a tree the reader could not make sense of.
	Raw string `json:"raw,omitempty"`
}

// planStep is one node on the path down to the node being read.
type planStep struct {
	depth int
	node  *treeNode
}

// explainAnalyze runs a statement and returns the plan it ran.
//
// The statement runs again: EXPLAIN ANALYZE is not a plan of a query that
// never happened, it is a query that happens with the plan written down.
func explainAnalyze(ctx context.Context, db *sql.DB, statement string) (explainOut, error) {
	out := explainOut{}
	if strings.TrimSpace(statement) == "" {
		return out, errors.New("there is no statement to explain")
	}
	rows, err := db.QueryContext(ctx, "EXPLAIN ANALYZE "+statement)
	if err != nil {
		return out, err
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		return out, err
	}
	if len(cols) == 0 {
		return out, errors.New("the plan came back with no column")
	}

	var b strings.Builder
	for rows.Next() {
		var cell sql.NullString
		if err := rows.Scan(&cell); err != nil {
			return out, err
		}
		b.WriteString(cell.String)
		b.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	return parseExplainAnalyze(b.String()), nil
}

// ansiPattern matches the escape sequences the server dresses its output in.
var ansiPattern = regexp.MustCompile("\x1b\\[[0-9;?]*[a-zA-Z]")

// stripANSI takes the colour off a string.
func stripANSI(s string) string {
	return ansiPattern.ReplaceAllString(s, "")
}

// parseExplainAnalyze reads the plan the server printed.
//
// The answer has two parts: a summary of the query, and then one tree per
// fragment. The summary names its own parts — "Top Most Time-consuming
// Nodes" and the like — and the trees are drawn with box characters.
func parseExplainAnalyze(text string) explainOut {
	out := explainOut{
		Summary:   []*treeNode{},
		TopTime:   []string{},
		TopMemory: []string{},
		Variables: []*treeNode{},
		Fragments: []*treeNode{},
	}
	clean := stripANSI(text)
	out.Raw = clean

	const (
		summaryPart = iota
		topTimePart
		topMemoryPart
		variablesPart
		fragmentPart
	)
	part := summaryPart
	var fragments []*treeNode
	var fragment *treeNode
	var path []planStep

	for _, line := range strings.Split(clean, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))

		// A fragment header opens a tree. It is the first thing at the
		// left margin that is not the summary's own header.
		if indent == 0 && strings.HasPrefix(trimmed, "Fragment ") {
			part = fragmentPart
			fragment = &treeNode{Name: trimmed}
			fragments = append(fragments, fragment)
			path = nil
			continue
		}

		if part != fragmentPart {
			switch trimmed {
			case "Summary":
				continue
			case "Top Most Time-consuming Nodes:":
				part = topTimePart
				continue
			case "Top Most Memory-consuming Nodes:":
				part = topMemoryPart
				continue
			case "NonDefaultVariables:":
				part = variablesPart
				continue
			}
			switch part {
			case summaryPart:
				out.Summary = append(out.Summary, splitMetrics(trimmed)...)
			case topTimePart:
				out.TopTime = append(out.TopTime, trimmed)
			case topMemoryPart:
				out.TopMemory = append(out.TopMemory, trimmed)
			case variablesPart:
				out.Variables = append(out.Variables, splitMetrics(trimmed)...)
			}
			continue
		}
		if fragment == nil {
			continue
		}

		runes := []rune(line)
		// A node: the connectors of the path down to it, then its name.
		if idx := connectorIndex(runes); idx >= 0 {
			depth := idx / 3
			name := strings.TrimSpace(string(runes[idx+3:]))
			node := &treeNode{Name: name}
			for len(path) > 0 && path[len(path)-1].depth >= depth {
				path = path[:len(path)-1]
			}
			parent := fragment
			if len(path) > 0 {
				parent = path[len(path)-1].node
			}
			parent.Children = append(parent.Children, node)
			path = append(path, planStep{depth: depth, node: node})
			continue
		}

		// A measurement: it belongs to the node whose column it starts
		// at. The column of a node's own measurement is three times the
		// node's depth, plus the width of the box that hangs under it.
		text := trimmed
		col := indent
		if rest, ok := strings.CutPrefix(text, "│"); ok {
			col = indent + 1 + (len(rest) - len(strings.TrimLeft(rest, " ")))
			text = strings.TrimSpace(rest)
		}
		depth := (col - 7) / 3
		owner := fragment
		for i := len(path) - 1; i >= 0; i-- {
			if path[i].depth <= depth {
				owner = path[i].node
				break
			}
		}
		for _, field := range splitMetrics(text) {
			appendMetric(owner, field, col)
		}
	}
	out.Fragments = fragments
	return out
}

// appendMetric puts a measurement under the thing it belongs to: the node
// whose column it starts at, or the measurement above it when it starts
// deeper than that.
//
// A measurement that starts deeper is a detail of the one above it: the
// timers of a scan hang under the scan's own timings, and not beside them.
func appendMetric(owner *treeNode, field *treeNode, col int) {
	if len(owner.Children) > 0 {
		last := owner.Children[len(owner.Children)-1]
		if last.col > 0 && col > last.col {
			last.Children = append(last.Children, field)
			return
		}
	}
	field.col = col
	owner.Children = append(owner.Children, field)
}

// connectorIndex returns where the box drawing opens a node's line, or -1
// when the line is not a node.
func connectorIndex(runes []rune) int {
	for i, r := range runes {
		if r == '└' || r == '├' {
			return i
		}
	}
	return -1
}

// metricSeparator finds a second measurement on one line: the server writes
// two of them as "A: 1, B: 2".
var metricSeparator = regexp.MustCompile(`, ([A-Za-z][A-Za-z0-9_]*(?: [A-Za-z][A-Za-z0-9_]*)*): `)

// splitMetrics reads the labelled measurements out of one line.
//
// A value that carries a bracketed list is left whole, because the brackets
// are full of colons that are not labels: "TotalTime: 1ms [CPUTime: 1ms]" is
// one measurement, not two.
func splitMetrics(text string) []*treeNode {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	label, value, found := strings.Cut(text, ": ")
	if !found {
		// A label with nothing after it: "IsFinalSink", or a name whose
		// value the server left empty.
		return []*treeNode{{Name: strings.TrimSuffix(text, ":")}}
	}
	label = strings.TrimSpace(label)
	value = strings.TrimSpace(value)
	out := []*treeNode{{Name: label}}
	if strings.Contains(value, "[") {
		out[0].Value = value
		return out
	}
	rest := value
	for {
		m := metricSeparator.FindStringSubmatchIndex(rest)
		if m == nil {
			break
		}
		out[len(out)-1].Value = strings.TrimSpace(rest[:m[0]])
		out = append(out, &treeNode{Name: rest[m[2]:m[3]]})
		rest = rest[m[1]:]
	}
	out[len(out)-1].Value = strings.TrimSpace(rest)
	return out
}
