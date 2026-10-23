package main

import (
	"os"
	"strings"
	"testing"
)

// The plan of a query, and the profile the frontend recorded of it, are read
// from text the server wrote. The tests read the same text a live server
// wrote: the files in testdata are real captures, colour codes and all.

// child returns the child of a node with a given name.
func child(node *treeNode, name string) *treeNode {
	if node == nil {
		return nil
	}
	for _, c := range node.Children {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// childValue returns the value of the child of a node with a given name.
func childValue(node *treeNode, name string) string {
	c := child(node, name)
	if c == nil {
		return ""
	}
	return c.Value
}

// grandchild returns the child of a child, by name.
func grandchild(node *treeNode, parent, name string) *treeNode {
	return child(child(node, parent), name)
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

func TestParseExplainAnalyze(t *testing.T) {
	out := parseExplainAnalyze(readFixture(t, "explain_analyze.txt"))

	if len(out.Summary) == 0 {
		t.Fatal("the summary is empty")
	}
	if v := childValue(&treeNode{Children: out.Summary}, "TotalTime"); v != "33ms" {
		t.Errorf("TotalTime is %q, want %q", v, "33ms")
	}
	// Two measurements on one line are two measurements.
	if v := childValue(&treeNode{Children: out.Summary}, "QueryAllocatedMemoryUsage"); v != "2.263 MB" {
		t.Errorf("QueryAllocatedMemoryUsage is %q, want %q", v, "2.263 MB")
	}
	if v := childValue(&treeNode{Children: out.Summary}, "QueryPeakMemoryUsage"); v != "?" {
		t.Errorf("QueryPeakMemoryUsage is %q, want %q", v, "?")
	}
	// A value with a bracketed list stays whole: the colons in the
	// brackets are not labels.
	exec := childValue(&treeNode{Children: out.Summary}, "ExecutionTime")
	if !strings.Contains(exec, "[Scan: 321.170us") {
		t.Errorf("ExecutionTime is %q, want the bracketed list in one piece", exec)
	}

	if len(out.TopTime) != 6 {
		t.Errorf("got %d nodes in the top time list, want 6", len(out.TopTime))
	}
	if !strings.Contains(out.TopTime[0], "OLAP_SCAN") {
		t.Errorf("the top time list starts with %q, want the scan", out.TopTime[0])
	}
	if len(out.Variables) != 3 {
		t.Errorf("got %d variables, want 3", len(out.Variables))
	}

	if len(out.Fragments) != 2 {
		t.Fatalf("got %d fragments, want 2", len(out.Fragments))
	}
	frag := out.Fragments[0]
	if frag.Name != "Fragment 0" {
		t.Errorf("the first fragment is %q, want %q", frag.Name, "Fragment 0")
	}
	if v := childValue(frag, "BackendNum"); v != "1" {
		t.Errorf("BackendNum is %q, want %q", v, "1")
	}
	if v := childValue(frag, "InstanceAllocatedMemoryUsage"); v != "158.195 KB" {
		t.Errorf("InstanceAllocatedMemoryUsage is %q, want %q", v, "158.195 KB")
	}

	// The tree of the fragment: the sink, then the aggregation under it.
	sink := child(frag, "RESULT_SINK")
	if sink == nil {
		t.Fatal("the fragment has no RESULT_SINK")
	}
	if v := childValue(sink, "SinkType"); v != "MYSQL_PROTOCAL" {
		t.Errorf("SinkType is %q, want %q", v, "MYSQL_PROTOCAL")
	}
	agg := child(sink, "AGGREGATION (id=4) [finalize, merge]")
	if agg == nil {
		t.Fatal("the sink has no aggregation under it")
	}
	if v := childValue(agg, "OutputRows"); v != "1" {
		t.Errorf("OutputRows is %q, want %q", v, "1")
	}
	exchange := child(agg, "EXCHANGE (id=3) [GATHER]")
	if exchange == nil {
		t.Fatal("the aggregation has no exchange under it")
	}
	// The timers of the exchange are under it, and not under the
	// aggregation: the column a measurement starts at is what says so.
	timers := child(exchange, "Detail Timers")
	if timers == nil {
		t.Fatal("the exchange has no detail timers")
	}
	if v := childValue(timers, "WaitTime"); !strings.HasPrefix(v, "914.719us") {
		t.Errorf("WaitTime is %q, want the exchange's own", v)
	}

	// The second fragment holds the scan.
	scan := child(out.Fragments[1], "DATA_STREAM_SINK (id=3)")
	if scan == nil {
		t.Fatal("the second fragment has no data stream sink")
	}
	project := grandchild(scan, "AGGREGATION (id=2) [serialize, update]", "PROJECT (id=1)")
	if project == nil {
		t.Fatal("the aggregation has no projection under it")
	}
	olap := child(project, "OLAP_SCAN (id=0)")
	if olap == nil {
		t.Fatal("the scan is not under the projection")
	}
	if v := childValue(olap, "OutputRows"); v != "10.000K (10000)" {
		t.Errorf("OutputRows is %q, want %q", v, "10.000K (10000)")
	}
	// The timers of the scan hang under its own detail timers.
	scanTimers := child(olap, "Detail Timers")
	if scanTimers == nil {
		t.Fatal("the scan has no detail timers")
	}
	if v := childValue(scanTimers, "IOTaskExecTime"); !strings.HasPrefix(v, "162.959us") {
		t.Errorf("IOTaskExecTime is %q, want the scan's own", v)
	}
}

func TestParseExplainAnalyzeTakesTheColourOff(t *testing.T) {
	out := parseExplainAnalyze(readFixture(t, "explain_analyze.txt"))
	if strings.Contains(out.Raw, "\x1b") {
		t.Error("the raw text still has escape sequences in it")
	}
	for _, node := range out.Fragments {
		if strings.Contains(node.Name, "│") {
			t.Errorf("the name %q still has box drawing in it", node.Name)
		}
	}
}

func TestParseQueryProfile(t *testing.T) {
	doc := parseQueryProfile("01a1638e", readFixture(t, "query_profile.txt"))

	if len(doc.Nodes) != 1 {
		t.Fatalf("got %d roots, want 1", len(doc.Nodes))
	}
	root := doc.Nodes[0]
	if root.Name != "Query" {
		t.Errorf("the root is %q, want %q", root.Name, "Query")
	}

	// The sections the frontend writes under "Query:".
	for _, name := range []string{"Summary", "Planner", "Reason", "Execution"} {
		if child(root, name) == nil {
			t.Errorf("the profile has no %q section", name)
		}
	}

	summary := child(root, "Summary")
	if v := childValue(summary, "Query ID"); v != "01a1638e-6fad-75eb-9438-9988600c7460" {
		t.Errorf("Query ID is %q, want the id of the query", v)
	}
	if v := childValue(summary, "Total"); v != "98ms" {
		t.Errorf("Total is %q, want %q", v, "98ms")
	}

	// The planner is a tree of its own, nested by the spaces that follow
	// the dash and not by the indentation.
	planner := child(root, "Planner")
	total := child(planner, "Total[1]")
	if total == nil {
		t.Fatal("the planner has no total")
	}
	if total.Value != "7ms" {
		t.Errorf("the planner's total is %q, want %q", total.Value, "7ms")
	}
	analyzer := child(total, "Analyzer[1]")
	if analyzer == nil {
		t.Fatal("the planner's total has no analyzer under it")
	}
	if child(analyzer, "Lock[1]") == nil {
		t.Error("the analyzer has no lock under it")
	}
	if child(planner, "DeployDataSize") == nil {
		t.Error("the planner has no deploy size")
	}

	// The execution: the fragments, the pipelines, the operators.
	execution := child(root, "Execution")
	fragment := child(execution, "Fragment 0")
	if fragment == nil {
		t.Fatal("the execution has no fragment")
	}
	if v := childValue(fragment, "BackendNum"); v != "1" {
		t.Errorf("BackendNum is %q, want %q", v, "1")
	}
	pipeline := child(fragment, "Pipeline (id=1)")
	if pipeline == nil {
		t.Fatal("the fragment has no pipeline")
	}
	// A measurement of the pipeline, and the measurement under it.
	pending := child(pipeline, "PendingTime")
	if pending == nil {
		t.Fatal("the pipeline has no pending time")
	}
	empty := child(pending, "InputEmptyTime")
	if empty == nil {
		t.Fatal("the pending time has no input empty time under it")
	}
	if child(empty, "FirstInputEmptyTime") == nil {
		t.Error("the input empty time has no first input empty time under it")
	}

	operator := child(pipeline, "RESULT_SINK (plan_node_id=-1)")
	if operator == nil {
		t.Fatal("the pipeline has no result sink")
	}
	common := child(operator, "CommonMetrics")
	if common == nil {
		t.Fatal("the result sink has no common metrics")
	}
	if v := childValue(common, "PushRowNum"); v != "1" {
		t.Errorf("PushRowNum is %q, want %q", v, "1")
	}
	unique := child(operator, "UniqueMetrics")
	if unique == nil {
		t.Fatal("the result sink has no unique metrics")
	}
	if v := childValue(unique, "SinkType"); v != "MYSQL_PROTOCAL" {
		t.Errorf("SinkType is %q, want %q", v, "MYSQL_PROTOCAL")
	}
}

func TestSplitMetrics(t *testing.T) {
	cases := []struct {
		text   string
		names  []string
		values []string
	}{
		{"TotalTime: 33ms", []string{"TotalTime"}, []string{"33ms"}},
		{"A: 1, B: 2", []string{"A", "B"}, []string{"1", "2"}},
		{"TotalTime: 1ms [CPUTime: 1ms]", []string{"TotalTime"}, []string{"1ms [CPUTime: 1ms]"}},
		{"IsFinalSink", []string{"IsFinalSink"}, []string{""}},
		{"SubordinateOperators: ", []string{"SubordinateOperators"}, []string{""}},
		{"InstancePeakMemoryUsage: 125.773 KB, InstanceAllocatedMemoryUsage: 158.195 KB",
			[]string{"InstancePeakMemoryUsage", "InstanceAllocatedMemoryUsage"},
			[]string{"125.773 KB", "158.195 KB"}},
	}
	for _, c := range cases {
		got := splitMetrics(c.text)
		if len(got) != len(c.names) {
			t.Errorf("%q gave %d measurements, want %d", c.text, len(got), len(c.names))
			continue
		}
		for i := range got {
			if got[i].Name != c.names[i] {
				t.Errorf("%q: measurement %d is %q, want %q", c.text, i, got[i].Name, c.names[i])
			}
			if got[i].Value != c.values[i] {
				t.Errorf("%q: measurement %d is %q, want %q", c.text, i, got[i].Value, c.values[i])
			}
		}
	}
}
