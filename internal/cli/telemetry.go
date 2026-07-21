package cli

// The query_knowledge parser behind `cognosis telemetry query`. The daemon's
// query_knowledge log lines (vector= fts= graph= fused=) are the only
// retrieval-quality signal the system emits -- counts only, never query text
// -- and until this existed reading them meant a one-off grep. This turns
// them into a monitorable series as the vault grows.
//
// Derived columns, spelled out because the log format grew over time:
//
//   - and_count: the keyword leg's AND-conjunction candidate count, the
//     quantity that decides whether the OR fallback fires. Taken from fts_and
//     when logged; older events without fts_and but with fts_fallback=false
//     use fts (no fallback means fts IS the AND count); otherwise empty.
//   - graph_min_unique: max(0, fused-vector-fts), a lower bound on candidates
//     only the graph leg surfaced. A bound, not the true count: overlap
//     between the vector and keyword legs raises the true number and the
//     counts alone cannot recover it.
//   - roll_*: trailing-window rates over the events that carry the field,
//     so a mixed-vintage log does not dilute the rate with unknowns.

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// queryEvent is one query_knowledge log line. Pointer fields are attrs the
// line may predate; nil means "not logged", which the CSV renders as empty
// rather than zero -- a zero that means "unknown" is exactly the kind of
// silent lie the per-leg counts were added to end.
type queryEvent struct {
	time     string
	results  int
	vector   *int
	fts      *int
	ftsAnd   *int
	graph    *int
	fused    *int
	fallback *bool
}

// andCount derives the AND-conjunction candidate count, reporting false when
// the event predates the attrs that make it derivable.
func (e queryEvent) andCount() (int, bool) {
	if e.ftsAnd != nil {
		return *e.ftsAnd, true
	}
	if e.fallback != nil && !*e.fallback && e.fts != nil {
		return *e.fts, true
	}
	return 0, false
}

// graphMinUnique is a lower bound on graph-only candidates; false when the
// event lacks the counts.
func (e queryEvent) graphMinUnique() (int, bool) {
	if e.fused == nil || e.vector == nil || e.fts == nil {
		return 0, false
	}
	return max(0, *e.fused-*e.vector-*e.fts), true
}

// parseQueryEvent reads one log line. ok is false for lines that are not
// query_knowledge events at all; an event without per-leg counts is returned
// with nil fields and left to the caller to classify.
func parseQueryEvent(line string) (queryEvent, bool) {
	if !strings.Contains(line, "msg=query_knowledge") {
		return queryEvent{}, false
	}
	e := queryEvent{}
	for tok := range strings.FieldsSeq(line) {
		k, v, found := strings.Cut(tok, "=")
		if !found {
			continue
		}
		switch k {
		case "time":
			e.time = v
		case "results":
			e.results, _ = strconv.Atoi(v)
		case "vector":
			e.vector = atoiPtr(v)
		case "fts":
			e.fts = atoiPtr(v)
		case "fts_and":
			e.ftsAnd = atoiPtr(v)
		case "graph":
			e.graph = atoiPtr(v)
		case "fused":
			e.fused = atoiPtr(v)
		case "fts_fallback":
			b := v == "true"
			e.fallback = &b
		}
	}
	return e, true
}

func atoiPtr(s string) *int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return nil
	}
	return &n
}

// rollRate is the share of true values among the last window known samples.
// Events that do not carry the field contribute nothing, so a mixed-vintage
// log yields a rate over the events that can answer, not a diluted one.
type rollRate struct {
	window  int
	samples []bool
}

func (r *rollRate) add(v bool) {
	r.samples = append(r.samples, v)
	if len(r.samples) > r.window {
		r.samples = r.samples[1:]
	}
}

func (r *rollRate) rate() (float64, bool) {
	if len(r.samples) == 0 {
		return 0, false
	}
	n := 0
	for _, v := range r.samples {
		if v {
			n++
		}
	}
	return float64(n) / float64(len(r.samples)), true
}

// runTelemetryQuery streams the CSV series to out and a run summary to
// errOut. An input with no per-leg events is an error, not an empty success.
func runTelemetryQuery(in io.Reader, out, errOut io.Writer, window int) error {
	// Write errors on the CSV body surface at the explicit Flush below --
	// bufio holds them -- so the per-line returns carry no extra information.
	// The stderr summary is best-effort by the same logic as every other
	// command's diagnostics.
	w := bufio.NewWriter(out)
	_, _ = fmt.Fprintln(w, "time,results,vector,fts,and_count,graph,fused,fts_fallback,graph_min_unique,roll_fallback_rate,roll_starve_rate,roll_graph_unique_mean")

	fallbackRoll := &rollRate{window: window}
	starveRoll := &rollRate{window: window}
	var uniqueWindow []int

	parsed, skipped := 0, 0
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	for sc.Scan() {
		e, ok := parseQueryEvent(sc.Text())
		if !ok {
			continue
		}
		if e.vector == nil && e.fts == nil && e.graph == nil {
			// Pre-LegStats vintage: results-only, nothing to chart.
			skipped++
			continue
		}
		parsed++

		if e.fallback != nil {
			fallbackRoll.add(*e.fallback)
		}
		if n, ok := e.andCount(); ok {
			starveRoll.add(n < 2)
		}
		var uniqueField string
		if u, ok := e.graphMinUnique(); ok {
			uniqueField = strconv.Itoa(u)
			uniqueWindow = append(uniqueWindow, u)
			if len(uniqueWindow) > window {
				uniqueWindow = uniqueWindow[1:]
			}
		}

		_, _ = fmt.Fprintf(w, "%s,%d,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s\n",
			e.time, e.results,
			intField(e.vector), intField(e.fts), andField(e), intField(e.graph), intField(e.fused),
			boolField(e.fallback), uniqueField,
			rateField(fallbackRoll), rateField(starveRoll), meanField(uniqueWindow))
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if err := w.Flush(); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(errOut, "%d events with per-leg counts", parsed)
	if skipped > 0 {
		_, _ = fmt.Fprintf(errOut, " (%d pre-LegStats events skipped)", skipped)
	}
	_, _ = fmt.Fprintln(errOut)
	if fr, ok := fallbackRoll.rate(); ok {
		_, _ = fmt.Fprintf(errOut, "trailing fallback firing rate (last %d known): %.0f%%\n",
			len(fallbackRoll.samples), fr*100)
	}
	if sr, ok := starveRoll.rate(); ok {
		_, _ = fmt.Fprintf(errOut, "trailing AND-starvation rate, and_count<2 (last %d known): %.0f%%\n",
			len(starveRoll.samples), sr*100)
	}
	if len(uniqueWindow) > 0 {
		_, _ = fmt.Fprintf(errOut, "trailing graph_min_unique mean (last %d): %s\n",
			len(uniqueWindow), meanField(uniqueWindow))
	}
	if parsed == 0 {
		return fmt.Errorf("no query_knowledge events with per-leg counts found")
	}
	return nil
}

func intField(p *int) string {
	if p == nil {
		return ""
	}
	return strconv.Itoa(*p)
}

func andField(e queryEvent) string {
	if n, ok := e.andCount(); ok {
		return strconv.Itoa(n)
	}
	return ""
}

func boolField(p *bool) string {
	if p == nil {
		return ""
	}
	return strconv.FormatBool(*p)
}

func rateField(r *rollRate) string {
	rate, ok := r.rate()
	if !ok {
		return ""
	}
	return strconv.FormatFloat(rate, 'f', 2, 64)
}

func meanField(xs []int) string {
	if len(xs) == 0 {
		return ""
	}
	sum := 0
	for _, x := range xs {
		sum += x
	}
	return strconv.FormatFloat(float64(sum)/float64(len(xs)), 'f', 1, 64)
}
