package scaffold

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/project"
)

// The scaffolded project.yaml selects filtered access explicitly — the one active key — and
// documents the rest of box:/gate: as comments, so it parses via project.Load as a filtered
// project that asks for nothing a human would have to approve.
func TestScaffoldedProjectParses(t *testing.T) {
	repo := t.TempDir()
	if _, err := WriteProject(repo, nil); err != nil {
		t.Fatal(err)
	}
	p, err := project.Load(repo)
	if err != nil {
		t.Fatalf("scaffolded project.yaml must parse: %v", err)
	}
	if p.Box.Egress != "filtered" || len(p.Box.EgressRules) != 0 || p.Gate != "" {
		t.Errorf("scaffold must load as filtered with no rules and nothing else, got %+v", p)
	}
	data, _ := os.ReadFile(filepath.Join(repo, filepath.FromSlash(project.File)))
	for _, want := range []string{"box:", "env:", "PGHOST:", "  egress: filtered\n", "egress_rules:", "# offline:", "gate:", "gate_sources:"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("scaffold missing %q", want)
		}
	}
	// "none" is an internal spelling; the file a human edits never shows it.
	if strings.Contains(string(data), "none") {
		t.Errorf("scaffold spells offline as none:\n%s", data)
	}
	// Re-running init keeps the file a human may have edited since.
	edited := []byte("box:\n  egress: open\n")
	if err := os.WriteFile(filepath.Join(repo, filepath.FromSlash(project.File)), edited, 0o644); err != nil {
		t.Fatal(err)
	}
	if wrote, err := WriteProject(repo, nil, "postgres"); err != nil || wrote {
		t.Fatalf("re-init rewrote an existing project.yaml: wrote=%v err=%v", wrote, err)
	}
	if data, _ := os.ReadFile(filepath.Join(repo, filepath.FromSlash(project.File))); string(data) != string(edited) {
		t.Errorf("re-init changed the project file:\n%s", data)
	}
}

func TestScaffoldedProjectRequestsSelectedServices(t *testing.T) {
	repo := t.TempDir()
	if _, err := WriteProject(repo, nil, "postgres", "redis"); err != nil {
		t.Fatal(err)
	}
	p, err := project.Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"db": 5432, "redis": 6379}
	if len(p.Box.EgressRules) != len(want) {
		t.Fatalf("service rules = %+v, want db and redis", p.Box.EgressRules)
	}
	for _, rule := range p.Box.EgressRules {
		port, ok := want[rule.To.Service]
		if !ok || rule.Protocol != "tcp" || len(rule.Ports) != 1 || rule.Ports[0] != port {
			t.Errorf("unexpected generated service rule: %+v", rule)
		}
		delete(want, rule.To.Service)
	}
	if len(want) != 0 {
		t.Errorf("missing generated service rules: %v", want)
	}
}

// Services added to an existing filtered project get the same rules a fresh init writes, so a box
// can reach them after one coop approve. The template's commented example becomes the list, and
// every other line, comments included, stays where it was.
func TestAddServiceRulesFillsTheTemplate(t *testing.T) {
	repo := t.TempDir()
	if _, err := WriteProject(repo, nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(repo, filepath.FromSlash(project.File))
	before, _ := os.ReadFile(path)
	got, err := AddServiceRules(repo, []string{"redis", "postgres"})
	if err != nil {
		t.Fatal(err)
	}
	want := []ServiceRule{{"db", 5432}, {"redis", 6379}}
	if !slices.Equal(got.Added, want) || len(got.Unplaced) != 0 {
		t.Fatalf("AddServiceRules = %+v, want %v added", got, want)
	}
	after, _ := os.ReadFile(path)
	fresh := t.TempDir()
	if _, err := WriteProject(fresh, nil, "postgres", "redis"); err != nil {
		t.Fatal(err)
	}
	// The edited file is exactly the one a fresh `coop init --services postgres,redis` writes.
	if written, _ := os.ReadFile(filepath.Join(fresh, filepath.FromSlash(project.File))); string(after) != string(written) {
		t.Errorf("edited project.yaml differs from a fresh one:\n%s\nwant:\n%s", after, written)
	}
	if string(after) == string(before) {
		t.Fatal("the edit changed nothing")
	}
	// Asking again adds nothing and leaves the file alone.
	if again, err := AddServiceRules(repo, []string{"postgres", "redis"}); err != nil || len(again.Added)+len(again.Unplaced) != 0 {
		t.Errorf("second AddServiceRules = %+v, %v; want nothing to do", again, err)
	}
	if again, _ := os.ReadFile(path); string(again) != string(after) {
		t.Errorf("a no-op edit rewrote the file:\n%s", again)
	}
}

func TestAddServiceRulesJoinsAnExistingList(t *testing.T) {
	for _, tc := range []struct {
		name, file, want string
		services         []string
		added            []ServiceRule
	}{
		{"indented list with a trailing comment",
			"box:\n  egress: filtered\n  egress_rules:\n    - to: {domain: \"docs.example.com\"}\n      protocol: tls\n      ports: [443]\n\n  # Resource limits.\n  memory: 4g\n",
			"box:\n  egress: filtered\n  egress_rules:\n    - to: {domain: \"docs.example.com\"}\n      protocol: tls\n      ports: [443]\n    - to: {service: \"db\"}\n      protocol: tcp\n      ports: [5432]\n\n  # Resource limits.\n  memory: 4g\n",
			[]string{"postgres"}, []ServiceRule{{"db", 5432}}},
		{"compact list",
			"box:\n  egress: filtered\n  egress_rules:\n  - to: {domain: \"docs.example.com\"}\n    protocol: tls\n    ports: [443]\n",
			"box:\n  egress: filtered\n  egress_rules:\n  - to: {domain: \"docs.example.com\"}\n    protocol: tls\n    ports: [443]\n  - to: {service: \"db\"}\n    protocol: tcp\n    ports: [5432]\n",
			[]string{"postgres"}, []ServiceRule{{"db", 5432}}},
		// The person's own db rule stands, port and all: only redis is missing.
		{"a service the project already asks for",
			"box:\n  egress: filtered\n  egress_rules:\n    - to: {service: db}\n      protocol: tcp\n      ports: [6432]\n",
			"box:\n  egress: filtered\n  egress_rules:\n    - to: {service: db}\n      protocol: tcp\n      ports: [6432]\n    - to: {service: \"redis\"}\n      protocol: tcp\n      ports: [6379]\n",
			[]string{"postgres", "redis"}, []ServiceRule{{"redis", 6379}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := writeProjectFixture(t, tc.file)
			got, err := AddServiceRules(repo, tc.services)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got.Added, tc.added) || len(got.Unplaced) != 0 {
				t.Fatalf("AddServiceRules = %+v, want %v added", got, tc.added)
			}
			if after, _ := os.ReadFile(filepath.Join(repo, filepath.FromSlash(project.File))); string(after) != tc.want {
				t.Errorf("project.yaml =\n%s\nwant:\n%s", after, tc.want)
			}
			if _, err := project.Load(repo); err != nil {
				t.Errorf("the edited project.yaml does not load: %v", err)
			}
		})
	}
}

// Only a filtered project asks for service rules, and a layout coop cannot edit safely is left
// exactly as it was: the rules come back for the caller to show instead.
func TestAddServiceRulesLeavesOtherFilesAlone(t *testing.T) {
	for _, tc := range []struct {
		name, file string
		unplaced   bool
	}{
		{"open", "box:\n  egress: open\n", false},
		{"offline", "box:\n  egress: offline\n", false},
		{"no network setting", "subprojects: [api]\n", false},
		{"a flow-style list", "box:\n  egress: filtered\n  egress_rules: []\n", true},
		{"a flow-style box", "box: {egress: filtered}\n", true},
		{"an edited example", "box:\n  egress: filtered\n  # egress_rules:\n  #   - to: {domain: \"api.example.com\"}\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := writeProjectFixture(t, tc.file)
			got, err := AddServiceRules(repo, []string{"postgres"})
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Added) != 0 || (len(got.Unplaced) != 0) != tc.unplaced {
				t.Errorf("AddServiceRules = %+v, want unplaced=%v and nothing added", got, tc.unplaced)
			}
			if tc.unplaced && !slices.Equal(got.Unplaced, []ServiceRule{{"db", 5432}}) {
				t.Errorf("unplaced rules = %v, want db:5432", got.Unplaced)
			}
			if after, _ := os.ReadFile(filepath.Join(repo, filepath.FromSlash(project.File))); string(after) != tc.file {
				t.Errorf("a refused edit changed project.yaml:\n%s", after)
			}
		})
	}
	// No project.yaml asks for nothing, and none is created.
	repo := t.TempDir()
	if got, err := AddServiceRules(repo, []string{"postgres"}); err != nil || len(got.Added)+len(got.Unplaced) != 0 {
		t.Errorf("AddServiceRules without project.yaml = %+v, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(repo, filepath.FromSlash(project.File))); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("AddServiceRules created project.yaml: %v", err)
	}
}

func writeProjectFixture(t *testing.T, body string) string {
	t.Helper()
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, filepath.FromSlash(project.File)), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}
