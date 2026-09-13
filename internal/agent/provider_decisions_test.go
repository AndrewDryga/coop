package agent

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Parse rather than grep so documentation and multiline examples remain prose,
// while both quoted and raw-string provider selectors are caught.
func providerDecisionLiterals(path string, src any) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		return nil, err
	}
	var found []string
	ast.Inspect(file, func(node ast.Node) bool {
		lit, ok := node.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(lit.Value)
		if err == nil && Valid(value) {
			found = append(found, fset.Position(lit.Pos()).String()+": "+lit.Value)
		}
		return true
	})
	return found, nil
}

func TestProviderDecisionsStayInAdapters(t *testing.T) {
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch filepath.ToSlash(path) {
			case "../agent", "../cli/testdata/providerfixture", "../acpproxy/testdata/acpfixture":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		found, err := providerDecisionLiterals(path, nil)
		if err != nil {
			return err
		}
		for _, hit := range found {
			t.Errorf("provider decision belongs in its adapter: %s", hit)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestProviderDecisionGuardDetectsMutations(t *testing.T) {
	for _, name := range Names() {
		for _, body := range []string{
			`func f(s string) bool { return s == "` + name + `" }`,
			"var m = map[string]bool{`" + name + "`:true}",
			`func f(s string) { switch s { case "` + name + `": } }`,
		} {
			found, err := providerDecisionLiterals("mutation.go", "package fixture\n"+body)
			if err != nil || len(found) != 1 {
				t.Fatalf("guard missed %s: %v/%v", body, found, err)
			}
		}
	}
	prose := "package fixture\n// example: \"claude\"\nconst help = `Examples:\n[\"acp\", \"claude\"]`"
	if found, err := providerDecisionLiterals("prose.go", prose); err != nil || len(found) != 0 {
		t.Fatalf("prose rejected: %v/%v", found, err)
	}
	// A newly registered provider joins the guard without editing its pattern.
	registry["guard-fixture"] = providerDecisionTestAgent{Agent: registry[Default()]}
	t.Cleanup(func() { delete(registry, "guard-fixture") })
	if found, err := providerDecisionLiterals("new.go", `package fixture; var name = "guard-fixture"`); err != nil || len(found) != 1 {
		t.Fatalf("new provider missed: %v/%v", found, err)
	}
}

type providerDecisionTestAgent struct{ Agent }

func (providerDecisionTestAgent) Name() string { return "guard-fixture" }

// Keep the filesystem scanner's read failure explicit rather than silently skipping a source.
func TestProviderDecisionGuardReadFailure(t *testing.T) {
	if _, err := providerDecisionLiterals(filepath.Join(t.TempDir(), "missing.go"), nil); !os.IsNotExist(err) {
		t.Fatalf("read failure = %v", err)
	}
}
