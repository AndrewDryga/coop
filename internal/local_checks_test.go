package internal_test

import (
	"bufio"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// These are the foundations of explicit Make scopes, not a runtime test selector. Include
// every tag and test import, root/tool consumers and fixtures attributed to their owner.
var localScopeRoots = map[string][]string{
	"agent": {"./internal/agent"}, "runtime": {"./internal/runtime"},
	"sessions": {"./internal/session", "./internal/sessionsvc"},
	"tasks":    {"./internal/tasks"}, "connector": {"./internal/workerconnector"},
	"cli": {"./internal/cli"},
}

func TestLocalCheckScopesCoverReverseConsumers(t *testing.T) {
	graph, err := localConsumerGraph("..")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string][]string{}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "LOCAL_") {
			continue
		}
		name, packages, ok := strings.Cut(line, " := ")
		if !ok {
			t.Fatalf("explicit local package declaration must use :=: %s", line)
		}
		declared[strings.TrimPrefix(name, "LOCAL_")] = strings.Fields(packages)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	for scope, roots := range localScopeRoots {
		if !slices.Contains(declared[scope], "./internal") {
			t.Errorf("LOCAL_%s must include its structural/scope guard package ./internal", scope)
		}
		if problems := localScopeProblems(graph, roots, declared[scope]); len(problems) != 0 {
			t.Errorf("LOCAL_%s misses owners/reverse consumers: %v; expand its reviewed list or use broad checks", scope, problems)
		}
		for _, pkg := range declared[scope] {
			files, err := filepath.Glob(filepath.Join("..", pkg, "*.go"))
			if err != nil || len(files) == 0 {
				t.Errorf("LOCAL_%s declares package without source %s: %v", scope, pkg, err)
			}
		}
	}
}

func localScopeProblems(graph map[string][]string, roots, declared []string) []string {
	needed := map[string]bool{}
	for _, root := range roots {
		needed[root] = true
	}
	for changed := true; changed; {
		changed = false
		for consumer, deps := range graph {
			if needed[consumer] {
				continue
			}
			for _, dep := range deps {
				if needed[dep] {
					needed[consumer], changed = true, true
					break
				}
			}
		}
	}
	var problems []string
	for pkg := range needed {
		if !slices.Contains(declared, pkg) {
			problems = append(problems, pkg)
		}
	}
	for _, pkg := range declared {
		if _, ok := graph[pkg]; !ok {
			problems = append(problems, "missing package "+pkg)
		}
	}
	slices.Sort(problems)
	return problems
}

func TestLocalCheckScopeRejectsMissedReverseEdges(t *testing.T) {
	graph := map[string][]string{"./internal/a": nil, "./internal/b": {"./internal/a"}, ".": {"./internal/b"}}
	if got := localScopeProblems(graph, []string{"./internal/a"}, []string{"./internal/a", "./internal/b", "."}); len(got) != 0 {
		t.Fatal(got)
	}
	graph["./tools/new"] = []string{"./internal/b"}
	if got := localScopeProblems(graph, []string{"./internal/a"}, []string{"./internal/a", "./internal/b", "."}); !slices.Equal(got, []string{"./tools/new"}) {
		t.Fatalf("new indirect consumer escaped: %v", got)
	}
	if got := localScopeProblems(graph, []string{"./internal/a"}, []string{"./internal/a", "./internal/gone"}); !slices.Contains(got, "missing package ./internal/gone") {
		t.Fatalf("deleted owner escaped: %v", got)
	}
}

func TestLocalConsumerGraphIncludesTagsTestsToolsAndFixtureOwners(t *testing.T) {
	root := t.TempDir()
	for path, data := range map[string]string{
		"internal/leaf/leaf.go":                         "package leaf\n",
		"internal/consumer/consumer.go":                 "package consumer\n",
		"internal/consumer/consumer_test.go":            "//go:build windows\n\npackage consumer\nimport _ \"github.com/AndrewDryga/coop/internal/leaf\"\n",
		"tools/generator/main.go":                       "package main\nimport _ \"github.com/AndrewDryga/coop/internal/consumer\"\n",
		"main.go":                                       "package main\nimport _ \"github.com/AndrewDryga/coop/internal/cli\"\n",
		"internal/cli/cli.go":                           "package cli\n",
		"internal/acpproxy/proxy.go":                    "package acpproxy\n",
		"internal/acpproxy/testdata/acpfixture/main.go": "package main\n",
		"internal/cli/testdata/providerfixture/main.go": "package main\n",
		"internal/box/testdata/initprobe/main.go":       "package main\n",
		"internal/box/testdata/mcpprobe/main.go":        "package main\nimport _ \"github.com/AndrewDryga/coop/internal/leaf\"\n",
	} {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	graph, err := localConsumerGraph(root)
	if err != nil {
		t.Fatal(err)
	}
	for owner, dep := range map[string]string{
		"./internal/consumer": "./internal/leaf", "./tools/generator": "./internal/consumer",
		"./internal/box": "./internal/leaf", "./internal/acpproxy": ".",
		".": "./internal/cli", "./internal/box/testdata/mcpprobe": "./internal/cli",
	} {
		if !slices.Contains(graph[owner], dep) {
			t.Errorf("missed tag/test/fixture/dynamic edge %s -> %s: %v", owner, dep, graph[owner])
		}
	}
	if err := os.Remove(filepath.Join(root, "internal/box/testdata/mcpprobe/main.go")); err != nil {
		t.Fatal(err)
	}
	if _, err := localConsumerGraph(root); err == nil || !strings.Contains(err.Error(), "mcpprobe") {
		t.Fatalf("removed fixture was invented by the ownership table: %v", err)
	}
}

func localConsumerGraph(root string) (map[string][]string, error) {
	// ACP's scripted harness builds and executes the root CLI program. An import-only graph
	// cannot discover that dynamic consumer; keep its observed ownership explicit.
	graph := map[string][]string{"./internal/acpproxy": {"."}}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (strings.HasPrefix(d.Name(), ".") || strings.HasPrefix(d.Name(), "_")) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		owner := filepath.ToSlash(rel)
		if prefix, _, ok := strings.Cut(owner, "/testdata/"); ok {
			owner = prefix
		}
		if owner != "." {
			owner = "./" + owner
		}
		if graph[owner] == nil {
			graph[owner] = []string{}
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range file.Imports {
			name, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			if dep, ok := strings.CutPrefix(name, "github.com/AndrewDryga/coop/"); ok {
				graph[owner] = append(graph[owner], "./"+dep)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// These programs are built by real subprocess callers, rather than imported. Keep them
	// in the reviewed scope alongside their owner, including packages with no internal imports.
	for fixture, owner := range map[string]string{
		"./internal/acpproxy/testdata/acpfixture": "./internal/acpproxy",
		"./internal/cli/testdata/providerfixture": "./internal/cli",
		"./internal/box/testdata/initprobe":       "./internal/box",
		"./internal/box/testdata/mcpprobe":        "./internal/box",
	} {
		files, err := filepath.Glob(filepath.Join(root, fixture, "*.go"))
		if err != nil || len(files) == 0 {
			return nil, fmt.Errorf("compiled fixture %s has no source: %v", fixture, err)
		}
		graph[fixture] = []string{owner}
	}
	graph["./internal/box/testdata/mcpprobe"] = append(graph["./internal/box/testdata/mcpprobe"], "./internal/cli")
	return graph, err
}
