package mcpserver

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/enqack/cognosis/internal/cogerr"
	"github.com/enqack/cognosis/internal/config"
	"github.com/enqack/cognosis/internal/query"
	"github.com/enqack/cognosis/internal/store"
)

func TestLoopbackEnforced(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	for _, ok := range []string{"127.0.0.1:7433", "localhost:7433", "[::1]:7433"} {
		if _, err := New(ok, t.TempDir(), log, nil, nil, nil, nil, nil); err != nil {
			t.Errorf("loopback bind %q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"0.0.0.0:7433", "192.168.1.10:7433", "example.com:7433", "7433"} {
		_, err := New(bad, t.TempDir(), log, nil, nil, nil, nil, nil)
		if !cogerr.Is(err, cogerr.Validation) {
			t.Errorf("non-loopback bind %q: err = %v, want Validation", bad, err)
		}
	}

	// Built-in TLS is the only door to a non-loopback bind.
	tls := config.TLS{CertFile: "/etc/cognosis/cert.pem", KeyFile: "/etc/cognosis/key.pem"}
	if _, err := NewTLS("0.0.0.0:7433", t.TempDir(), log, nil, nil, nil, nil, nil, tls); err != nil {
		t.Errorf("non-loopback bind with TLS configured refused: %v", err)
	}
	half := config.TLS{CertFile: "/etc/cognosis/cert.pem"}
	if _, err := NewTLS("0.0.0.0:7433", t.TempDir(), log, nil, nil, nil, nil, nil, half); !cogerr.Is(err, cogerr.Validation) {
		t.Errorf("half-configured TLS must not unlock non-loopback binds")
	}
}

func TestReadNoteFilePathRules(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	s, err := New("127.0.0.1:0", t.TempDir(), log, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"../secrets.md", "/etc/passwd", "outside/x.md"} {
		if _, err := s.readNoteFile(bad); !cogerr.Is(err, cogerr.Validation) {
			t.Errorf("path %q: err = %v, want Validation", bad, err)
		}
	}
	if _, err := s.readNoteFile("entries/missing.md"); !cogerr.Is(err, cogerr.NotFound) {
		t.Errorf("missing note: err = %v, want NotFound", err)
	}
}

func TestFormat(t *testing.T) {
	if Format(nil) != "No results." {
		t.Fatal("empty results")
	}
	out := Format([]query.Result{
		{Path: "entries/a.md", Category: "entry", HeadingPath: "Title > Sec", Content: "body", Score: 0.0328},
	})
	for _, want := range []string{"### 1. entries/a.md", "› Title > Sec", "(entry, score 0.0328)", "body"} {
		if !strings.Contains(out, want) {
			t.Errorf("formatted output missing %q:\n%s", want, out)
		}
	}
}

func TestSnippet(t *testing.T) {
	short := "short content"
	if snippet(short) != short {
		t.Fatal("short content must pass through")
	}
	long := strings.Repeat("line of text here\n", 100)
	got := snippet(long)
	if len(got) > 710 {
		t.Fatalf("snippet too long: %d", len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatal("truncated snippet must end with ellipsis")
	}
}

func TestRenderContextPreamble(t *testing.T) {
	metas := []store.NoteMeta{
		{Path: "entries/a.md", Category: "entry", Status: "active", Updated: time.Now()},
		{Path: "notes/b.md", Category: "concept", Status: "active", Project: "cognosis", Updated: time.Now()},
	}

	out := renderContext(metas, "", 2000)
	if !strings.HasPrefix(out, contextPreamble) {
		t.Fatal("preamble must lead the payload — an index the agent reads before the framing is a list of paths with no stated purpose")
	}
	if i, j := strings.Index(out, contextPreamble), strings.Index(out, "# Cognosis knowledge index"); i > j {
		t.Errorf("preamble must precede the index header (got %d > %d)", i, j)
	}
	for _, want := range []string{"entries/a.md", "notes/b.md", "project cognosis"} {
		if !strings.Contains(out, want) {
			t.Errorf("index missing %q:\n%s", want, out)
		}
	}

	// The empty vault is exactly when the write_note guidance matters most.
	empty := renderContext(nil, "", 2000)
	if !strings.HasPrefix(empty, contextPreamble) {
		t.Error("empty vault must still get the preamble")
	}
	if !strings.Contains(empty, "(vault is empty)") {
		t.Error("empty vault must still say so")
	}

	// The preamble is exempt from the budget, which governs the index alone. 50
	// tokens (~200 chars) could not fit the preamble (~193 tokens) and a note
	// line both, so the notes appearing is what proves the exemption: were the
	// preamble counted, the allowance would be gone before the first line.
	exempt := renderContext(metas, "", 50)
	for _, want := range []string{"entries/a.md", "notes/b.md"} {
		if !strings.Contains(exempt, want) {
			t.Errorf("preamble must not consume the index budget — %q missing:\n%s", want, exempt)
		}
	}

	// The budget still governs the index: too small for even one line collapses
	// it while the preamble survives. This is the only place truncation is
	// proven — scripts/checks/platform.sh injects against an empty vault, so it
	// can only bound the output, not watch an index collapse.
	tiny := renderContext(metas, "", 10)
	if !strings.HasPrefix(tiny, contextPreamble) {
		t.Error("preamble is exempt, so it must survive even a budget of 10")
	}
	if !strings.Contains(tiny, "truncated to budget") {
		t.Errorf("budget 10 must truncate the index:\n%s", tiny)
	}
	if strings.Contains(tiny, "entries/a.md") {
		t.Error("budget 10 must not fit a note line")
	}
}

// registeredTools lists the live tool surface by asking the server over an
// in-memory transport, rather than trusting a hand-copied list to stay honest.
// Registration never touches the store, so nil dependencies are fine here.
func registeredTools(t *testing.T) map[string]string {
	t.Helper()
	ctx := context.Background()

	s := &Server{log: slog.New(slog.DiscardHandler)}
	srv := mcp.NewServer(&mcp.Implementation{Name: "cognosis", Version: "test"}, nil)
	s.addTools(srv)

	st, ct := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	out := map[string]string{}
	for _, tool := range res.Tools {
		out[tool.Name] = tool.Description
	}
	return out
}

// TestPreambleToolNamesExist guards the drift that makes the preamble worse than
// useless: naming a tool the server does not register. The preamble is the first
// thing an agent reads, so a stale name there points it at a tool that isn't there.
func TestPreambleToolNamesExist(t *testing.T) {
	tools := registeredTools(t)
	if len(tools) == 0 {
		t.Fatal("no tools registered")
	}
	// Pull every `backticked` token out of the preamble that looks like a tool name.
	for _, tok := range strings.Split(contextPreamble, "`") {
		if !strings.Contains(tok, "_") || strings.ContainsAny(tok, " /") {
			continue
		}
		if _, ok := tools[tok]; !ok {
			t.Errorf("preamble names %q, which is not a registered tool", tok)
		}
	}
}

// TestToolDescriptionsStateWhenToUse checks the property the descriptions exist
// for: a tool the agent can see but never learns to reach for is dead surface.
// Deliberately shallow — it catches a description reverted to a bare mechanism
// blurb or emptied out, not prose quality.
func TestToolDescriptionsStateWhenToUse(t *testing.T) {
	for name, desc := range registeredTools(t) {
		if len(desc) < 80 {
			t.Errorf("%s: description too thin to say when to use it (%d chars): %q", name, len(desc), desc)
		}
	}
}

// TestToolErrorStripsInternalIdentifiers — cogerr.Error prints as
// "op: kind: cause", which is right for a log line and wrong for a tool result.
// An agent reading "write.Pipeline.Edit: validation: old_string appears 2 times"
// has to parse past two internal identifiers to reach the sentence it can act
// on, and every tool leaked them.
func TestToolErrorStripsInternalIdentifiers(t *testing.T) {
	err := cogerr.Ef("write.Pipeline.Edit", cogerr.Validation,
		"old_string appears %d times in %s; extend it until it identifies one location", 2, "notes/x.md")

	got := srvFor(false).toolError(localReq(), err).Error()
	want := "old_string appears 2 times in notes/x.md; extend it until it identifies one location"
	if got != want {
		t.Errorf("toolError = %q, want %q", got, want)
	}
	for _, leaked := range []string{"write.Pipeline.Edit", "validation:"} {
		if strings.Contains(got, leaked) {
			t.Errorf("tool result still carries %q: %s", leaked, got)
		}
	}
}

// Internal is deliberately not passed through: those causes are raw pgx, os and
// net errors carrying DSNs, unix socket paths and schema names. An agent cannot
// act on any of it, and a tool result is where it would travel furthest.
func TestToolErrorWithholdsInternalDetail(t *testing.T) {
	secret := "failed to connect to `user=sysop database=cognosis`: /Users/sysop/.pg-data/.s.PGSQL.5434"
	err := cogerr.E("store.GetNote", cogerr.Internal, errors.New(secret))

	got := srvFor(false).toolError(localReq(), err).Error()
	if strings.Contains(got, "sysop") || strings.Contains(got, ".s.PGSQL") {
		t.Errorf("internal detail reached the tool result: %s", got)
	}
	if !strings.Contains(got, "daemon log") {
		t.Errorf("message does not say where the detail went: %s", got)
	}
}

// Errors that are not domain errors — argument checks, SDK failures — are
// already plain and must pass through untouched rather than being flattened
// into a generic internal error.
func TestToolErrorPassesThroughPlainErrors(t *testing.T) {
	err := errors.New("path and old_string are required")
	if got := srvFor(false).toolError(localReq(), err); got.Error() != err.Error() {
		t.Errorf("toolError rewrote a plain error: %q", got)
	}
	if srvFor(false).toolError(localReq(), nil) != nil {
		t.Error("srvFor(false).toolError(localReq(), nil) should be nil")
	}
}

// A Kind carrying no cause must still say something. cogerr.E(op, kind, nil) is
// documented as valid, and the naive unwrap returns nil for it.
func TestToolErrorHandlesKindWithoutCause(t *testing.T) {
	err := cogerr.E("store.GetNote", cogerr.NotFound, nil)
	got := srvFor(false).toolError(localReq(), err)
	if got == nil {
		t.Fatal("toolError returned nil for a non-nil error")
	}
	if got.Error() != "not_found" {
		t.Errorf("toolError = %q, want the kind name", got)
	}
}

// TestToolErrorWithholdsUnavailableDetail — the redaction originally gated on
// Internal alone, but the errors that actually carry connection detail are
// classified Unavailable: pool.Begin failures and the embedding provider both
// wrap raw pgx/net errors, and even the author-written ones interpolate the
// endpoint. A remote agent was receiving the database user, database name and
// unix socket path in a tool result.
func TestToolErrorWithholdsUnavailableDetail(t *testing.T) {
	for _, c := range []struct{ name, secret string }{
		{"pgx pool", "failed to connect to `user=sysop database=cognosis`: /Users/sysop/.pg-data/.s.PGSQL.5434"},
		{"embedding endpoint", `Post "http://10.0.0.7:11434/api/embed": dial tcp 10.0.0.7:11434: connect: refused`},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := srvFor(false).toolError(localReq(), cogerr.E("store.UpsertNote", cogerr.Unavailable, errors.New(c.secret))).Error()
			for _, leak := range []string{"user=", "database=", ".s.PGSQL", "10.0.0.7", "/Users/"} {
				if strings.Contains(got, leak) {
					t.Errorf("tool result leaks %q: %s", leak, got)
				}
			}
			if !strings.Contains(got, "unavailable") {
				t.Errorf("message does not say the dependency is down: %s", got)
			}
		})
	}
}

// The two withheld kinds must stay distinguishable: "retry later" and "report a
// bug" are different instructions, and collapsing them into one message would
// make the redaction cost the caller something real.
func TestToolErrorDistinguishesUnavailableFromInternal(t *testing.T) {
	unavail := srvFor(false).toolError(localReq(), cogerr.E("op", cogerr.Unavailable, errors.New("x"))).Error()
	internal := srvFor(false).toolError(localReq(), cogerr.E("op", cogerr.Internal, errors.New("x"))).Error()
	if unavail == internal {
		t.Errorf("both kinds produce %q; the caller cannot tell a transient outage from a bug", unavail)
	}
}

// srvFor builds a bare Server carrying the disclosure setting, bound to
// loopback — so these tests exercise the Kind switch rather than the gate.
func srvFor(trust bool) *Server {
	return &Server{TrustLocalErrors: trust, bindLoopback: true}
}

// localReq is a call that arrived with no forwarding markers: the shape that
// satisfies mayDiscloseTo's third condition. Header must be non-nil — absent
// metadata withholds, which would make every case below pass for the wrong
// reason.
func localReq() *mcp.CallToolRequest {
	return &mcp.CallToolRequest{Extra: &mcp.RequestExtra{Header: http.Header{}}}
}

// TestDisclosureNeedsAllThreeKeys — releasing a withheld cause requires the
// operator's assertion, a loopback bind, *and* a call carrying no forwarding
// markers. Any two without the third must withhold.
//
// The reason all three are needed is concrete: docs/remote.md recommends a
// reverse proxy forwarding from 127.0.0.1, so the bind is loopback and the
// operator may well have opted in, yet the caller is remote. The per-call
// header is the only thing that separates them.
func TestDisclosureNeedsAllThreeKeys(t *testing.T) {
	secret := "failed to connect to `user=sysop database=cognosis`: /Users/sysop/.pg-data/.s.PGSQL.5434"
	err := cogerr.E("store.UpsertNote", cogerr.Unavailable, errors.New(secret))

	for _, c := range []struct {
		name                       string
		trust, loopback, forwarded bool
		wantDisclosure             bool
	}{
		{"default posture", false, true, false, false},
		{"opted in, exposed bind", true, false, false, false},
		{"opted in, loopback, proxied call", true, true, true, false},
		{"not opted in on a clean local call", false, true, false, false},
		{"all three", true, true, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			req := localReq()
			if c.forwarded {
				req.Extra.Header.Set("X-Forwarded-For", "203.0.113.9")
			}
			s := &Server{TrustLocalErrors: c.trust, bindLoopback: c.loopback}
			got := s.toolError(req, err).Error()
			disclosed := strings.Contains(got, ".s.PGSQL") || strings.Contains(got, "user=sysop")
			if disclosed != c.wantDisclosure {
				t.Errorf("disclosure=%v want %v: %s", disclosed, c.wantDisclosure, got)
			}
		})
	}
}
