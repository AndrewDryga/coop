package internal_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// Every failure names both of these: the card says why, the table is where a justified exception
// goes.
const (
	deviceFenceCard  = ".agent/kb/identity-fences-compare-the-inode.md"
	deviceFenceTable = "the deviceComparisons table in internal/device_fence_test.go"
)

// deviceComparisons are the device comparisons that are not identity fences — one line each, with
// the reason. An entry is checked in both directions: one that no longer matches the tree fails too.
var deviceComparisons = map[string]string{
	"fingerprint.Device != task.Generation.Device": "two fields of one host-written pending-review record, " +
		"always captured together: a corruption tripwire, not a comparison with the live filesystem",
	"current.Dev != children.Dev": "two namespace stats taken together in one process, never a recorded value",
}

// TestIdentityFencesNeverCompareTheDevice is the check for deviceFenceCard. A volume's device number
// is assigned when it is mounted, so a device recorded before a reboot never equals the live one
// after it: task ownership, completion evidence, network approvals, fork workspaces and session
// discard plans each compared one and each broke at the host's next reboot.
//
// It reads source: every non-test .go file under internal/ (testdata excluded) is parsed, and any
// == or != whose operand is a device value — an identifier or field named Dev or dev, or ending in
// "device", optionally through a one-argument conversion such as uint64(stat.Dev) — fails unless
// the other side is a literal (a zero or a string) or the expression is in the table above.
//
// What it cannot see, and does not pretend to: a device under another name, and a device compared
// inside a struct — `a == b` on two records that embed a TaskGeneration, or a reflect.DeepEqual of
// two identities. The fences that could carry one are held by types instead: CompletionFingerprint
// and sessionWorkspaceIdentity are not comparable, and task instances go through sameTaskInstance;
// the records that still compare whole task instances with == compare two copies of one capture.
func TestIdentityFencesNeverCompareTheDevice(t *testing.T) {
	seen := map[string]bool{}
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
		ast.Inspect(file, func(node ast.Node) bool {
			cmp, ok := node.(*ast.BinaryExpr)
			if !ok || (cmp.Op != token.EQL && cmp.Op != token.NEQ) {
				return true
			}
			_, leftLiteral := cmp.X.(*ast.BasicLit)
			_, rightLiteral := cmp.Y.(*ast.BasicLit)
			if leftLiteral || rightLiteral || (!deviceOperand(cmp.X) && !deviceOperand(cmp.Y)) {
				return true
			}
			expr := types.ExprString(cmp)
			if _, ok := deviceComparisons[expr]; ok {
				seen[expr] = true
				return true
			}
			t.Errorf("internal/%s: %s compares a device, which a reboot renumbers — compare the inode "+
				"(%s), or add the expression to %s with the reason it is not an identity fence",
				fset.Position(cmp.Pos()), expr, deviceFenceCard, deviceFenceTable)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for expr := range deviceComparisons {
		if !seen[expr] {
			t.Errorf("%s lists %q, which no longer appears in internal/ — drop the entry (%s)",
				deviceFenceTable, expr, deviceFenceCard)
		}
	}
}

// deviceOperand reports whether expr is a device value itself — rather than something computed from
// one — by the names this tree gives them.
func deviceOperand(expr ast.Expr) bool {
	if call, ok := expr.(*ast.CallExpr); ok && len(call.Args) == 1 {
		expr = call.Args[0] // uint64(stat.Dev), networkview.Count(stat.Dev)
	}
	var name string
	switch e := expr.(type) {
	case *ast.Ident:
		name = e.Name
	case *ast.SelectorExpr:
		name = e.Sel.Name
	default:
		return false
	}
	lower := strings.ToLower(name)
	return lower == "dev" || strings.HasSuffix(lower, "device")
}
