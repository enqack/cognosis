package mcpserver

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// Request-scoped log calls in this package must use slog's *Context variants,
// or the caller's token identity is silently dropped (see
// auth.NewIdentityHandler). A plain Info() compiles, runs, and produces a line
// that looks correct and is unattributable -- so this is enforced statically
// rather than left to review.
//
// A Go test rather than a script under scripts/checks/: check-all.sh treats
// exit 2 as "skipped, carry on" and every check there needs COGNOSIS_DSN plus a
// live Ollama, so a gate living there reports itself skipped in most
// environments. This repo already has history with gates that pass because they
// never ran. This one needs nothing and runs under `mage test`.
//
// Allowlisted by message literal, not line number, so moving code does not
// require touching this list -- but deleting or renaming an allowlisted call
// does, which is the point.
var contextlessLogAllowed = map[string]string{
	"mcp server listening": "startup, before any request; there is no caller to attribute it to",
}

func TestRequestScopedLogsCarryContext(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	seenAllowed := map[string]bool{}
	contextCalls := 0
	scanned := 0

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		scanned++
		{
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				// Match <anything>.log.<Method>(...)
				recv, ok := sel.X.(*ast.SelectorExpr)
				if !ok || recv.Sel.Name != "log" {
					return true
				}
				method := sel.Sel.Name
				if strings.HasSuffix(method, "Context") {
					contextCalls++
					return true
				}
				if method != "Info" && method != "Warn" && method != "Error" && method != "Debug" {
					return true
				}
				msg := literalArg(call.Args)
				if _, allowed := contextlessLogAllowed[msg]; allowed {
					seenAllowed[msg] = true
					return true
				}
				t.Errorf("%s: s.log.%s(%q) drops the caller's identity -- use %sContext(ctx, ...) "+
					"or add it to contextlessLogAllowed with a reason",
					fset.Position(call.Pos()), method, msg, method)
				return true
			})
		}
	}

	// A stale allowlist is a silent hole: the entry stops matching anything and
	// nobody re-reads why it was exempt.
	for msg := range contextlessLogAllowed {
		if !seenAllowed[msg] {
			t.Errorf("allowlisted message %q was not found -- delete the entry or fix the scan", msg)
		}
	}

	// Without this the scan can match nothing at all -- a renamed `log` field, a
	// changed AST shape -- and the check above passes vacuously forever.
	if scanned == 0 {
		t.Fatal("scanned no non-test .go files: the directory walk is broken")
	}

	const wantContextCalls = 6
	if contextCalls < wantContextCalls {
		t.Errorf("found %d *Context log calls, want >= %d: the scan is probably not matching "+
			"anything, which would make this whole test vacuous", contextCalls, wantContextCalls)
	}
}

// literalArg returns the first argument as an unquoted string when it is a
// literal, else "". Only used for allowlist matching and error messages.
func literalArg(args []ast.Expr) string {
	if len(args) == 0 {
		return ""
	}
	lit, ok := args[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return ""
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return ""
	}
	return s
}
