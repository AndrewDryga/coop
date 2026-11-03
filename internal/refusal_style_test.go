package internal_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// refusalStyleCard is the rule these refusals break: rejected input is a ui.UsageError (exit 2), and
// a command that could not do its job draws the same block through ui.CommandFailed (exit 1).
const refusalStyleCard = ".agent/kb/rules/command-output-tiers.md"

// adHocRefusals counts, per file, the ad-hoc refusals written before the shared block existed. A
// count may only fall: a new one fails, and so does a fixed one until its entry is lowered here, so
// the debt cannot grow back. A file not listed may have none.
var adHocRefusals = map[string]int{
	"box/derived_image.go":       1,
	"box/network_setup.go":       1,
	"cli/acp_cmd.go":             1,
	"cli/context.go":             3,
	"cli/fork_cmd.go":            18,
	"cli/help.go":                1,
	"cli/net_diagnostic.go":      1,
	"cli/session_cmd.go":         1,
	"forkctl/merge.go":           1,
	"forkctl/review.go":          3,
	"forkctl/rm.go":              4,
	"forkctl/supervise.go":       3,
	"networkstate/diagnostic.go": 2,
	"tasks/cmd.go":               13,
	"tasks/lease_cmd.go":         7,
	"tasks/queue.go":             2,
}

// TestRefusalsUseTheSharedBlock is the check for refusalStyleCard's error shapes. It reads source:
// every non-test .go file under internal/ (testdata excluded) is parsed, and three shapes count as
// an ad-hoc refusal, each printed as a lone "✗ <lowercase sentence>" line:
//
//   - errors.New or fmt.Errorf with a message that opens with a lowercase "coop …", most often the
//     old way a refusal named its command ("coop tasks add: …");
//   - return 2, errors.New(…) or return 2, fmt.Errorf(…): exit 2 is rejected input, which is data
//     for ui.UsageError, not a sentence;
//   - ui.Error with a message that starts in lowercase.
//
// What it cannot see, and does not pretend to: an ad-hoc sentence returned with exit 1 or built
// away from its return, which review still has to catch.
func TestRefusalsUseTheSharedBlock(t *testing.T) {
	found := map[string][]string{}
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		seen := map[token.Pos]bool{}
		record := func(node ast.Node, why string) {
			if seen[node.Pos()] {
				return
			}
			seen[node.Pos()] = true
			found[filepath.ToSlash(path)] = append(found[filepath.ToSlash(path)],
				fmt.Sprintf("%s: %s", fset.Position(node.Pos()), why))
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.ReturnStmt:
				if len(n.Results) == 2 && isIntLiteral(n.Results[0], "2") {
					if call, ok := n.Results[1].(*ast.CallExpr); ok && isErrorConstructor(call) {
						record(call, "return 2 with a hand-written error")
					}
				}
			case *ast.CallExpr:
				if msg, ok := errorMessage(n); ok && namesCommand(msg) {
					record(n, fmt.Sprintf("error opens with a lowercase %q: %q", "coop", msg))
				}
				if msg, ok := uiErrorMessage(n); ok && startsLower(msg) {
					record(n, fmt.Sprintf("ui.Error starts in lowercase: %q", msg))
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]bool{}
	for file := range found {
		files[file] = true
	}
	for file := range adHocRefusals {
		files[file] = true
	}
	for _, file := range slices.Sorted(maps.Keys(files)) {
		got, allowed := len(found[file]), adHocRefusals[file]
		_, listed := adHocRefusals[file]
		switch {
		case got > allowed:
			t.Errorf("%s has %d ad-hoc refusals; adHocRefusals allows %d. Build a ui.UsageError for rejected "+
				"input (exit 2) or a ui.CommandFailed for a command that could not do its job (exit 1); see %s.\n  %s",
				file, got, allowed, refusalStyleCard, strings.Join(found[file], "\n  "))
		case got < allowed || listed && allowed == 0:
			t.Errorf("%s has %d ad-hoc refusals and adHocRefusals allows %d: lower its entry, or remove it "+
				"at 0, so the fixed ones stay fixed", file, got, allowed)
		}
	}
}

func isIntLiteral(expr ast.Expr, value string) bool {
	lit, ok := expr.(*ast.BasicLit)
	return ok && lit.Kind == token.INT && lit.Value == value
}

// isErrorConstructor reports whether call is errors.New(…) or fmt.Errorf(…).
func isErrorConstructor(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && (pkg.Name == "errors" && sel.Sel.Name == "New" || pkg.Name == "fmt" && sel.Sel.Name == "Errorf")
}

// errorMessage is the literal message of an errors.New or fmt.Errorf call.
func errorMessage(call *ast.CallExpr) (string, bool) {
	if !isErrorConstructor(call) {
		return "", false
	}
	return firstStringLiteral(call)
}

// uiErrorMessage is the literal message of a ui.Error call.
func uiErrorMessage(call *ast.CallExpr) (string, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Error" {
		return "", false
	}
	if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "ui" {
		return "", false
	}
	return firstStringLiteral(call)
}

func firstStringLiteral(call *ast.CallExpr) (string, bool) {
	if len(call.Args) == 0 {
		return "", false
	}
	lit, ok := call.Args[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	msg, err := strconv.Unquote(lit.Value)
	return msg, err == nil
}

// namesCommand reports whether msg opens with a command path, "coop tasks add: …", which a person
// reads as a lowercase sentence after the ✗.
func namesCommand(msg string) bool {
	rest, ok := strings.CutPrefix(msg, "coop ")
	return ok && rest != "" && (startsLower(rest) || rest[0] == '%')
}

func startsLower(msg string) bool {
	return msg != "" && msg[0] >= 'a' && msg[0] <= 'z'
}
