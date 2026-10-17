package forkspace

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

type constructorSource struct {
	path, pkg string
	file      *ast.File
	imports   map[string]string
}

// Track which formal argument feeds a local expression. This includes local
// aliases in forwarding closures, without treating a nested callback as its owner.
func constructorParameters(typ *ast.FuncType, body *ast.BlockStmt) map[string]int {
	params := map[string]int{}
	for _, field := range typ.Params.List {
		for _, name := range field.Names {
			params[name.Name] = len(params)
		}
	}
	for changed := true; changed; {
		changed = false
		ast.Inspect(body, func(node ast.Node) bool {
			if _, ok := node.(*ast.FuncLit); ok {
				return false
			}
			var left []ast.Expr
			var right []ast.Expr
			switch node := node.(type) {
			case *ast.AssignStmt:
				left, right = node.Lhs, node.Rhs
			case *ast.ValueSpec:
				for _, name := range node.Names {
					left = append(left, name)
				}
				right = node.Values
			}
			for i, expr := range right {
				if i >= len(left) {
					continue
				}
				name, ok := left[i].(*ast.Ident)
				if !ok {
					continue
				}
				if _, exists := params[name.Name]; exists {
					continue
				}
				if index, ok := constructorParameter(expr, params); ok {
					params[name.Name] = index
					changed = true
				}
			}
			return true
		})
	}
	return params
}

func constructorParameter(expr ast.Expr, params map[string]int) (int, bool) {
	index, found := 0, false
	ast.Inspect(expr, func(node ast.Node) bool {
		if _, ok := node.(*ast.FuncLit); ok {
			return false
		}
		if id, ok := node.(*ast.Ident); ok {
			if value, ok := params[id.Name]; ok {
				index, found = value, true
			}
		}
		return !found
	})
	return index, found
}

// Recover an explicitly declared receiver type without loading or executing the
// package. In particular, a data field named Run is not a runner method value.
func constructorReceiver(expr ast.Expr) ast.Expr {
	switch expr := expr.(type) {
	case *ast.CompositeLit:
		return expr.Type
	case *ast.UnaryExpr:
		return constructorReceiver(expr.X)
	case *ast.ParenExpr:
		return constructorReceiver(expr.X)
	case *ast.StarExpr:
		return expr.X
	case *ast.Ident:
		if expr.Obj == nil {
			return nil
		}
		switch decl := expr.Obj.Decl.(type) {
		case *ast.Field:
			if pointer, ok := decl.Type.(*ast.StarExpr); ok {
				return pointer.X
			}
			return decl.Type
		case *ast.ValueSpec:
			return decl.Type
		case *ast.AssignStmt:
			for i, left := range decl.Lhs {
				if id, ok := left.(*ast.Ident); ok && id.Name == expr.Name && i < len(decl.Rhs) {
					return constructorReceiver(decl.Rhs[i])
				}
			}
		}
	}
	return nil
}

// Constructor values cannot be exempted: reviewing their definition says nothing
// about executables supplied by later calls through an alias. Include command
// factories and generic runners, even when they return output instead of Cmd.
func forbiddenConstructorValues(root string) ([]string, error) {
	var sources []constructorSource
	err := walkGitSources(root, func(path string, data []byte) error {
		file, err := parser.ParseFile(token.NewFileSet(), path, data, 0)
		if err != nil {
			return err
		}
		source := constructorSource{path: path, pkg: "github.com/AndrewDryga/coop/" + filepath.ToSlash(filepath.Dir(path)), file: file, imports: map[string]string{}}
		for _, imp := range file.Imports {
			value, _ := strconv.Unquote(imp.Path.Value)
			name := filepath.Base(value)
			if imp.Name != nil {
				name = imp.Name.Name
			}
			// Dot imports are keyed by imported path, so multiple ones cannot overwrite.
			if name == "." {
				source.imports[value] = "."
			} else {
				source.imports[name] = value
			}
		}
		sources = append(sources, source)
		return nil
	})
	if err != nil {
		return nil, err
	}
	functionKey := func(source constructorSource, fn *ast.FuncDecl) string {
		if fn.Recv != nil {
			receiver := fn.Recv.List[0].Type
			if pointer, ok := receiver.(*ast.StarExpr); ok {
				receiver = pointer.X
			}
			if name, ok := receiver.(*ast.Ident); ok {
				return source.pkg + ".(" + name.Name + ")." + fn.Name.Name
			}
		}
		return source.pkg + "." + fn.Name.Name
	}
	executableParams := map[string]int{"os/exec.Command": 0, "os/exec.CommandContext": 1}
	methods := map[string]string{}
	wrappers := map[string]bool{"os/exec.Command": true, "os/exec.CommandContext": true}
	rawWrappers := map[string]bool{"os/exec.Command": true, "os/exec.CommandContext": true}
	key := func(source constructorSource, expr ast.Expr) string {
		switch expr := expr.(type) {
		case *ast.Ident:
			if expr.Obj != nil && expr.Obj.Kind != ast.Fun {
				return ""
			}
			if source.imports["os/exec"] == "." && (expr.Name == "Command" || expr.Name == "CommandContext") {
				return "os/exec." + expr.Name
			}
			for imported, alias := range source.imports {
				if alias == "." && wrappers[imported+"."+expr.Name] {
					return imported + "." + expr.Name
				}
			}
			return source.pkg + "." + expr.Name
		case *ast.SelectorExpr:
			if name, ok := constructorReceiver(expr.X).(*ast.Ident); ok {
				return source.pkg + ".(" + name.Name + ")." + expr.Sel.Name
			}
			if pkg, ok := expr.X.(*ast.Ident); ok && source.imports[pkg.Name] != "" {
				return source.imports[pkg.Name] + "." + expr.Sel.Name
			}
			// Method values are also constructors when a command factory has this name.
			if method := methods[expr.Sel.Name]; method != "" {
				return method
			}
			return ""
		}
		return ""
	}
	for _, source := range sources {
		for _, decl := range source.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			params := constructorParameters(fn.Type, fn.Body)
			factory := false
			raw := false
			if fn.Type.Results != nil {
				ast.Inspect(fn.Type.Results, func(node ast.Node) bool {
					if expr, ok := node.(ast.Expr); ok && key(source, expr) == "os/exec.Cmd" {
						factory = true
					}
					return true
				})
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				if _, ok := node.(*ast.FuncLit); ok {
					return false
				}
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				name := key(source, call.Fun)
				if name == "os/exec.Command" || name == "os/exec.CommandContext" {
					raw = true
				}
				index := 0
				if name == "os/exec.CommandContext" {
					index = 1
				}
				if (name == "os/exec.Command" || name == "os/exec.CommandContext") && len(call.Args) > index {
					if position, dependent := constructorParameter(call.Args[index], params); dependent {
						factory = true
						executableParams[functionKey(source, fn)] = position
					}
					if literal, ok := call.Args[index].(*ast.BasicLit); ok && literal.Kind == token.STRING {
						executable, _ := strconv.Unquote(literal.Value)
						base := strings.ToLower(filepath.Base(executable))
						if base == "git" || base == "git.exe" {
							for _, arg := range call.Args[index+1:] {
								if position, dependent := constructorParameter(arg, params); dependent {
									factory = true
									executableParams[functionKey(source, fn)] = position
								}
							}
						}
					}
				}
				return true
			})
			if factory {
				wrappers[functionKey(source, fn)] = true
				if raw && functionKey(source, fn) != "github.com/AndrewDryga/coop/internal/forkspace.TrustedGitCommand" {
					rawWrappers[functionKey(source, fn)] = true
				}
				if fn.Recv != nil {
					methods[fn.Name.Name] = functionKey(source, fn)
				}
			}
		}
	}
	// Forwarding wrappers may be in another file or package and need not mention exec.
	for changed := true; changed; {
		changed = false
		for _, source := range sources {
			for _, decl := range source.file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || functionKey(source, fn) == "github.com/AndrewDryga/coop/internal/forkspace.TrustedGitCommand" {
					continue
				}

				params := constructorParameters(fn.Type, fn.Body)
				values := map[string]bool{}
				depends := func(expr ast.Expr) bool {
					found := false
					ast.Inspect(expr, func(node ast.Node) bool {
						if _, ok := node.(*ast.FuncLit); ok {
							return false
						}
						if call, ok := node.(*ast.CallExpr); ok && rawWrappers[key(source, call.Fun)] {
							name := key(source, call.Fun)
							// Follow executable forwarding, not every application callback
							// that happens to return an error from a fixed tool.
							wrapper := functionKey(source, fn)
							if position, ok := executableParams[name]; ok && len(call.Args) > position {
								if index, dependent := constructorParameter(call.Args[position], params); dependent {
									if _, exists := executableParams[wrapper]; !exists {
										executableParams[wrapper] = index
										changed = true
									}
								} else {
									literal, ok := call.Args[position].(*ast.BasicLit)
									if !ok || literal.Kind != token.STRING {
										return false
									}
									value, _ := strconv.Unquote(literal.Value)
									base := strings.ToLower(filepath.Base(value))
									if base != "git" && base != "git.exe" {
										return false
									}
								}
							} else {
								return false
							}

							found = true
						}
						if id, ok := node.(*ast.Ident); ok && values[id.Name] {
							found = true
						}
						return true
					})
					return found
				}
				for progress := true; progress; {
					progress = false
					ast.Inspect(fn.Body, func(node ast.Node) bool {
						if _, ok := node.(*ast.FuncLit); ok {
							return false
						}
						var names []ast.Expr
						var right []ast.Expr
						switch node := node.(type) {
						case *ast.AssignStmt:
							names, right = node.Lhs, node.Rhs
						case *ast.ValueSpec:
							for _, name := range node.Names {
								names = append(names, name)
							}
							right = node.Values
						}
						for _, expr := range right {
							if depends(expr) {
								for _, name := range names {
									if id, ok := name.(*ast.Ident); ok && !values[id.Name] {
										values[id.Name] = true
										progress = true
									}
								}
							}
						}
						return true
					})
				}
				ast.Inspect(fn.Body, func(node ast.Node) bool {
					if _, ok := node.(*ast.FuncLit); ok {
						return false
					}
					ret, ok := node.(*ast.ReturnStmt)
					if !ok {
						return true
					}
					for _, expr := range ret.Results {
						if depends(expr) {
							wrapper := functionKey(source, fn)
							if !wrappers[wrapper] {
								wrappers[wrapper] = true
								changed = true
							}
							if !rawWrappers[wrapper] {
								rawWrappers[wrapper] = true
								changed = true
							}

						}
					}
					return true
				})
			}
		}
	}
	var forbidden []string
	for _, source := range sources {
		direct := map[ast.Node]bool{}
		ast.Inspect(source.file, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.FuncDecl:
				direct[node.Name] = true
			case *ast.SelectorExpr:
				direct[node.Sel] = true
			case *ast.CallExpr:
				direct[node.Fun] = true
			}
			return true
		})
		ast.Inspect(source.file, func(node ast.Node) bool {
			if expr, ok := node.(ast.Expr); ok && !direct[node] && wrappers[key(source, expr)] {
				forbidden = append(forbidden, source.path+": forbidden constructor function value "+key(source, expr))
			}
			if closure, ok := node.(*ast.FuncLit); ok && !direct[node] {
				params := constructorParameters(closure.Type, closure.Body)
				returnsCommand := false
				if closure.Type.Results != nil {
					ast.Inspect(closure.Type.Results, func(node ast.Node) bool {
						if expr, ok := node.(ast.Expr); ok && key(source, expr) == "os/exec.Cmd" {
							returnsCommand = true
						}
						return true
					})
				}
				ast.Inspect(closure.Body, func(child ast.Node) bool {
					call, ok := child.(*ast.CallExpr)
					if !ok {
						return true
					}
					name := key(source, call.Fun)
					safeBoundary := name == "github.com/AndrewDryga/coop/internal/forkspace.TrustedGitCommand" || name == "github.com/AndrewDryga/coop/internal/forkspace.GitCommand" || name == "github.com/AndrewDryga/coop/internal/forkspace.GitCommandWithEnv" || name == "github.com/AndrewDryga/coop/internal/forkspace.GitRefCommand"
					forbiddenCall := name == "os/exec.Command" || name == "os/exec.CommandContext" || returnsCommand && wrappers[name] && !safeBoundary
					if position, ok := executableParams[name]; ok && len(call.Args) > position {
						if _, dependent := constructorParameter(call.Args[position], params); dependent {
							forbiddenCall = true
						}
						if literal, ok := call.Args[position].(*ast.BasicLit); ok && literal.Kind == token.STRING {
							value, _ := strconv.Unquote(literal.Value)
							if filepath.Base(value) == "git" || filepath.Base(value) == "git.exe" {
								forbiddenCall = true
							}
						}
					}
					if forbiddenCall {
						forbidden = append(forbidden, source.path+": forbidden constructor function value in closure")
					}
					return true
				})
			}
			return true
		})
	}
	return forbidden, nil
}

func walkGitSources(root string, visit func(string, []byte) error) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if strings.HasPrefix(entry.Name(), ".") && path != root || entry.Name() == "testdata" || entry.Name() == "testutil" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		return visit(filepath.ToSlash(relative), data)
	})
}

// Run the actual guard in a copy of the production tree, without adding an
// exception. Nothing in these probes runs: the child test only parses source.
func TestHostGitBoundaryRejectsAliasTreeMutation(t *testing.T) {
	root := t.TempDir()
	if err := walkGitSources(filepath.Join("..", ".."), func(path string, data []byte) error {
		dest := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
			return err
		}
		return os.WriteFile(dest, data, 0600)
	}); err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, probe := range []struct{ name, source string }{
		{"control", ""},
		{"raw alias", `package runtime; import "os/exec"; func probe() { run := exec.Command; _ = run("git", "status") }`},
		{"reviewed wrapper alias", `package runtime; var escaped = contextCommand`},
		{"forwarding wrapper", `package runtime; import "context"; import "os/exec"; func forward(ctx context.Context, name string, args ...string) *exec.Cmd { return contextCommand(ctx,name,args...) }; var escaped = forward`},
		{"chained runner", `package runtime; import "context"; func forward(ctx context.Context, name string,args ...string)([]byte,error){ return contextCommand(ctx,name,args...).Output() }; var escaped = forward`},
		{"forwarding method", `package runtime; import "context"; type launcher struct{}; func (launcher) invoke(ctx context.Context,name string,args ...string)([]byte,error){return contextCommand(ctx,name,args...).Output()}; var escaped=launcher{}.invoke`},
		{"chained forwarding method", `package runtime; import "context"; type launcher struct{}; func (b launcher) invoke(ctx context.Context,name string,args ...string)([]byte,error){return contextCommand(ctx,name,args...).Output()}; func(b launcher) forward(ctx context.Context,name string,args ...string)([]byte,error){return b.invoke(ctx,name,args...)}; var escaped=launcher{}.forward`},
		{"assigned runner", `package runtime; import "context"; func forward(ctx context.Context, name string,args ...string)([]byte,error){ cmd:=contextCommand(ctx,name,args...); out,err:=cmd.Output(); return out,err }; var escaped = forward`},
		{"imported factory", `package runtime; import . "github.com/AndrewDryga/coop/internal/forkspace"; var escaped = GitCommand`},
		{"closure local alias", `package runtime; import "context"; var escaped = func(name string, args ...string)([]byte,error){ binary:=name; return contextCommand(context.Background(),binary,args...).Output() }`},
		{"closure forwarding factory", `package runtime; import "context"; import "os/exec"; func forward(name string,args ...string)*exec.Cmd{return contextCommand(context.Background(),name,args...)}; var escaped=func(name string,args ...string)([]byte,error){return forward(name,args...).Output()}`},
		{"closure", `package runtime; import "os/exec"; var escaped = func(name string, args ...string) *exec.Cmd { return exec.Command(name,args...) }`},
	} {
		t.Run(probe.name, func(t *testing.T) {
			path := filepath.Join(root, "internal/runtime/constructor_probe.go")
			if probe.source != "" {
				if err := os.WriteFile(path, []byte(probe.source), 0600); err != nil {
					t.Fatal(err)
				}
				defer os.Remove(path)
			}
			cmd := exec.Command(binary, "-test.run=^TestHostGitCommandsUseSharedBoundary$", "-test.count=1")
			cmd.Env = append(os.Environ(), "COOP_TEST_GIT_BOUNDARY_ROOT="+root)
			out, err := cmd.CombinedOutput()
			if probe.source == "" {
				if err != nil {
					t.Fatalf("unmodified copy: %v\n%s", err, out)
				}
				t.Logf("unmodified tree: guard exited %d", cmd.ProcessState.ExitCode())
				return
			}
			if err == nil || !strings.Contains(string(out), "forbidden constructor function value") {
				t.Fatalf("alias guard did not fail: %v\n%s", err, out)
			}
			t.Logf("%s: guard exited %d as required", probe.name, cmd.ProcessState.ExitCode())
		})
	}
	t.Run("release raw wrapper", func(t *testing.T) {
		path := filepath.Join(root, "tools/releasenotes/main.go")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		mutated := strings.Replace(string(data), "import (", "import (\n\"os/exec\"", 1)
		mutated = strings.Replace(mutated, "out, err := forkspace.ObserveGit(ctx, repo, args...)", "cmd := exec.CommandContext(ctx, \"git\", args...); out, err := cmd.Output()", 1)
		if mutated == string(data) {
			t.Fatal("release mutation did not apply")
		}
		if err := os.WriteFile(path, []byte(mutated), 0600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(binary, "-test.run=^TestHostGitCommandsUseSharedBoundary$", "-test.count=1")
		cmd.Env = append(os.Environ(), "COOP_TEST_GIT_BOUNDARY_ROOT="+root)
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "unreviewed Git constructor: \"tools/releasenotes/main.go:git:exec.CommandContext") {
			t.Fatalf("raw release wrapper escaped: %v\n%s", err, out)
		}
		t.Logf("restored raw release wrapper: guard exited %d as required", cmd.ProcessState.ExitCode())
	})

}

func TestHostGitConstructorValueCannotBeAllowlisted(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "alias.go")
	for _, body := range []string{
		`func f(){ run := exec.Command; _ = run("git", "status") }`,
		`func f(){ run := exec.CommandContext; _ = run(ctx, "git", "status") }`,
		`func forward(name string,args ...string) *exec.Cmd { return exec.Command(name,args...) }; var run = forward`,
	} {
		if err := os.WriteFile(path, []byte("package p; import \"os/exec\"; "+body), 0600); err != nil {
			t.Fatal(err)
		}
		violations, err := forbiddenConstructorValues(root)
		if err != nil || len(violations) == 0 {
			t.Fatalf("function value was accepted: %v %v", violations, err)
		}
	}
}
