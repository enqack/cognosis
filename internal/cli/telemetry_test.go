package cli

import (
	"bytes"
	"strings"
	"testing"
)

// The three vintages of query_knowledge line the production daemon.log
// actually contains, verbatim shapes: results-only (pre-LegStats), per-leg
// counts without fts_and, and the full current attr set.
const (
	tlOld   = "time=2026-07-14T18:47:35.320-04:00 level=INFO msg=query_knowledge component=mcpserver results=0"
	tlMid   = "time=2026-07-19T19:21:18.512-04:00 level=INFO msg=query_knowledge component=mcpserver results=3 vector=50 fts=17 graph=50 fused=95 fused_sources=25 sources=3 fts_fallback=true token=desktop"
	tlFull  = "time=2026-07-20T11:25:32.839-04:00 level=INFO msg=query_knowledge component=mcpserver results=8 vector=50 fts=50 fts_and=0 graph=50 fused=102 fused_sources=35 sources=6 fts_fallback=true token=desktop"
	tlNoFB  = "time=2026-07-19T03:00:12.384-04:00 level=INFO msg=query_knowledge component=mcpserver results=8 vector=19 fts=0 graph=19 fused=19"
	tlOther = "time=2026-07-19T03:00:00.000-04:00 level=INFO msg=write_note component=mcpserver path=entries/x.md"
)

func TestParseQueryEventVintages(t *testing.T) {
	if _, ok := parseQueryEvent(tlOther); ok {
		t.Error("non-query_knowledge line parsed as event")
	}

	e, ok := parseQueryEvent(tlOld)
	if !ok {
		t.Fatal("results-only line not recognized as an event")
	}
	if e.vector != nil || e.fallback != nil {
		t.Error("results-only line grew per-leg fields from nowhere")
	}

	e, ok = parseQueryEvent(tlFull)
	if !ok {
		t.Fatal("full line not parsed")
	}
	if e.time != "2026-07-20T11:25:32.839-04:00" {
		t.Errorf("time = %q", e.time)
	}
	if *e.vector != 50 || *e.fts != 50 || *e.ftsAnd != 0 || *e.graph != 50 || *e.fused != 102 {
		t.Errorf("counts = v=%d fts=%d and=%d g=%d fused=%d",
			*e.vector, *e.fts, *e.ftsAnd, *e.graph, *e.fused)
	}
	if e.fallback == nil || !*e.fallback {
		t.Error("fts_fallback=true not parsed")
	}
}

// and_count is derivable exactly when the log can answer: directly from
// fts_and, from fts when no fallback replaced it, and not at all when a
// fallback fired on a line that predates fts_and.
func TestAndCountDerivation(t *testing.T) {
	full, _ := parseQueryEvent(tlFull)
	if n, ok := full.andCount(); !ok || n != 0 {
		t.Errorf("full line and_count = %d,%v, want 0,true", n, ok)
	}

	mid, _ := parseQueryEvent(tlMid)
	if _, ok := mid.andCount(); ok {
		t.Error("mid-vintage line with fts_fallback=true derived an and_count it cannot know")
	}

	noFallback, _ := parseQueryEvent(strings.Replace(tlMid, "fts_fallback=true", "fts_fallback=false", 1))
	if n, ok := noFallback.andCount(); !ok || n != 17 {
		t.Errorf("fallback=false and_count = %d,%v, want 17,true (fts is the AND count)", n, ok)
	}
}

func TestGraphMinUniqueIsLowerBound(t *testing.T) {
	full, _ := parseQueryEvent(tlFull)
	// fused=102, vector=50, fts=50: at least 2 candidates came from the graph.
	if u, ok := full.graphMinUnique(); !ok || u != 2 {
		t.Errorf("graph_min_unique = %d,%v, want 2,true", u, ok)
	}
	// fused=19 = vector=19, fts=0: bound clamps at zero, never negative.
	nofb, _ := parseQueryEvent(tlNoFB)
	if u, ok := nofb.graphMinUnique(); !ok || u != 0 {
		t.Errorf("graph_min_unique = %d,%v, want 0,true", u, ok)
	}
}

func TestTelemetryQuerySeries(t *testing.T) {
	in := strings.Join([]string{tlOld, tlOther, tlNoFB, tlMid, tlFull}, "\n")
	var out, errOut bytes.Buffer
	if err := runTelemetryQuery(strings.NewReader(in), &out, &errOut, 10); err != nil {
		t.Fatal(err)
	}

	rows := strings.Split(strings.TrimSpace(out.String()), "\n")
	// Header + the three events carrying per-leg counts; the results-only
	// event is skipped, the non-event line ignored.
	if len(rows) != 4 {
		t.Fatalf("got %d rows, want 4:\n%s", len(rows), out.String())
	}
	if !strings.HasPrefix(rows[0], "time,results,vector,fts,and_count,graph,fused") {
		t.Errorf("header = %q", rows[0])
	}
	// Final row: fallback known on 2 of 3 events (both true), starvation known
	// on 1 (and_count=0 < 2), so the rolling rates read 1.00 over their known
	// samples rather than being diluted by the unknown-vintage event.
	last := rows[len(rows)-1]
	if !strings.Contains(last, ",1.00,1.00,") {
		t.Errorf("final row lacks rolling rates over known samples: %q", last)
	}
	if !strings.Contains(errOut.String(), "3 events with per-leg counts (1 pre-LegStats events skipped)") {
		t.Errorf("summary = %q", errOut.String())
	}
}

func TestTelemetryQueryEmptyInputIsAnError(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := runTelemetryQuery(strings.NewReader(tlOther), &out, &errOut, 10); err == nil {
		t.Fatal("no events parsed must be an error, not an empty success")
	}
}

// The rolling window must actually slide: after window-many false samples the
// early true ones fall out entirely.
func TestRollRateWindowSlides(t *testing.T) {
	r := &rollRate{window: 3}
	r.add(true)
	r.add(true)
	for range 3 {
		r.add(false)
	}
	rate, ok := r.rate()
	if !ok || rate != 0 {
		t.Errorf("rate = %v,%v after window slid past the trues, want 0,true", rate, ok)
	}
}

// A window below 1 must be refused loudly: rollRate would trim every sample
// straight back out, blanking the rolling columns in a way that reads as "no
// data" rather than "bad flag".
func TestTelemetryQueryRejectsNonPositiveWindow(t *testing.T) {
	root := newRoot()
	root.SetIn(strings.NewReader(tlFull))
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"telemetry", "query", "--window", "0", "-"})

	if err := root.Execute(); err == nil {
		t.Fatal("window=0 accepted; rolling columns would be silently empty")
	}
}

// The command surface: stdin routing via "-" and the window flag reaching the
// series. Exercised through the root command, as a user invocation would be.
func TestTelemetryQueryCommandReadsStdin(t *testing.T) {
	root := newRoot()
	root.SetIn(strings.NewReader(tlFull))
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"telemetry", "query", "--window", "5", "-"})

	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "2026-07-20T11:25:32.839-04:00,8,50,50,0,50,102,true,2,") {
		t.Errorf("stdin event missing from CSV:\n%s", out.String())
	}
	if !strings.Contains(errOut.String(), "1 events with per-leg counts") {
		t.Errorf("summary = %q", errOut.String())
	}
}
