package mcpserver

import (
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/enqack/cognosis/internal/cogerr"
	"github.com/enqack/cognosis/internal/config"
	"github.com/enqack/cognosis/internal/query"
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
