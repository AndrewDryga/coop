package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func ref(value string) *string { return &value }

func testSourceBinding(kind SourceKind) SourceBinding {
	binding := SourceBinding{
		Version: SourceBindingVersion, Kind: kind,
		Requested:      SourceSelector{Kind: kind},
		RemoteIdentity: "origin",
		DefaultRef:     "refs/heads/main", DefaultCommit: strings.Repeat("1", 40),
		SelectedRef: ref("refs/heads/main"), SelectedCommit: strings.Repeat("1", 40),
		BaseCommit: strings.Repeat("1", 40), AdmittedTree: strings.Repeat("7", 40),
		ResolvedAt: time.Unix(1700000000, 0).UTC(),
	}
	switch kind {
	case SourceBranch:
		binding.Requested.Name = "feature/payments"
		binding.SelectedRef = ref("refs/heads/feature/payments")
		binding.SelectedCommit = strings.Repeat("2", 40)
	case SourcePullRequest:
		binding.Requested.Number = 514
		binding.PullRequestNumber = 514
		binding.SelectedRef = ref("refs/pull/514/head")
		binding.SelectedCommit = strings.Repeat("3", 40)
	case SourceCommit:
		binding.Requested.SHA = strings.Repeat("4", 40)
		binding.SelectedRef = nil
		binding.SelectedCommit = strings.Repeat("4", 40)
	}
	return binding
}

func repositoryBackedCreate(id string, binding *SourceBinding) CreateSessionRequest {
	return CreateSessionRequest{
		ID: id, Target: "target", Policy: "policy", Repository: "/repo",
		Workspace: "/workspace/" + id, ForkName: "fork-" + id,
		BaseCommit: strings.Repeat("1", 40), Source: binding,
	}
}

// The binding is the caller's whole proof of which objects a session was admitted on, so the
// wire shape is frozen here rather than left to whatever the struct happens to marshal: an
// exact commit carries a NULL selected_ref (it has no advertised ref to name), a default
// selection repeats the default identity, and a pull request keeps its number beside the
// generic fields.
func TestSourceBindingJSONShapeIsFrozenPerKind(t *testing.T) {
	for _, testCase := range []struct {
		kind SourceKind
		want string
	}{
		{SourceDefault, `{"version":1,"kind":"default","requested":{"kind":"default"},"remote_identity":"origin","default_ref":"refs/heads/main","default_commit":"1111111111111111111111111111111111111111","selected_ref":"refs/heads/main","selected_commit":"1111111111111111111111111111111111111111","base_commit":"1111111111111111111111111111111111111111","admitted_tree":"7777777777777777777777777777777777777777","resolved_at":"2023-11-14T22:13:20Z"}`},
		{SourceBranch, `{"version":1,"kind":"branch","requested":{"kind":"branch","name":"feature/payments"},"remote_identity":"origin","default_ref":"refs/heads/main","default_commit":"1111111111111111111111111111111111111111","selected_ref":"refs/heads/feature/payments","selected_commit":"2222222222222222222222222222222222222222","base_commit":"1111111111111111111111111111111111111111","admitted_tree":"7777777777777777777777777777777777777777","resolved_at":"2023-11-14T22:13:20Z"}`},
		{SourcePullRequest, `{"version":1,"kind":"pull_request","requested":{"kind":"pull_request","number":514},"remote_identity":"origin","default_ref":"refs/heads/main","default_commit":"1111111111111111111111111111111111111111","selected_ref":"refs/pull/514/head","selected_commit":"3333333333333333333333333333333333333333","base_commit":"1111111111111111111111111111111111111111","admitted_tree":"7777777777777777777777777777777777777777","resolved_at":"2023-11-14T22:13:20Z","pull_request_number":514}`},
		{SourceCommit, `{"version":1,"kind":"commit","requested":{"kind":"commit","sha":"4444444444444444444444444444444444444444"},"remote_identity":"origin","default_ref":"refs/heads/main","default_commit":"1111111111111111111111111111111111111111","selected_ref":null,"selected_commit":"4444444444444444444444444444444444444444","base_commit":"1111111111111111111111111111111111111111","admitted_tree":"7777777777777777777777777777777777777777","resolved_at":"2023-11-14T22:13:20Z"}`},
	} {
		encoded, err := json.Marshal(testSourceBinding(testCase.kind))
		if err != nil {
			t.Fatal(err)
		}
		if string(encoded) != testCase.want {
			t.Fatalf("%s binding JSON =\n%s\nwant\n%s", testCase.kind, encoded, testCase.want)
		}
	}
}

// The store is the final durable boundary: a binding whose derived ref, kind-specific evidence
// or object identities disagree with each other is refused before it can become a session a
// controller would later validate against.
func TestStoreRefusesEverySelfInconsistentSourceBinding(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "state"))
	defer store.Close()

	mutate := func(kind SourceKind, apply func(*SourceBinding)) *SourceBinding {
		binding := testSourceBinding(kind)
		apply(&binding)
		return &binding
	}
	cases := map[string]*SourceBinding{
		"wrong version":          mutate(SourceDefault, func(b *SourceBinding) { b.Version = 2 }),
		"unknown kind":           mutate(SourceDefault, func(b *SourceBinding) { b.Kind = "tag" }),
		"kind disagrees":         mutate(SourceBranch, func(b *SourceBinding) { b.Kind = SourceDefault }),
		"branch ref undreived":   mutate(SourceBranch, func(b *SourceBinding) { b.SelectedRef = ref("refs/heads/other") }),
		"pull ref underived":     mutate(SourcePullRequest, func(b *SourceBinding) { b.SelectedRef = ref("refs/pull/515/head") }),
		"pull number disagrees":  mutate(SourcePullRequest, func(b *SourceBinding) { b.PullRequestNumber = 515 }),
		"commit names a ref":     mutate(SourceCommit, func(b *SourceBinding) { b.SelectedRef = ref("refs/heads/main") }),
		"commit sha disagrees":   mutate(SourceCommit, func(b *SourceBinding) { b.SelectedCommit = strings.Repeat("5", 40) }),
		"default diverges":       mutate(SourceDefault, func(b *SourceBinding) { b.SelectedCommit = strings.Repeat("9", 40) }),
		"abbreviated commit":     mutate(SourceBranch, func(b *SourceBinding) { b.SelectedCommit = "0123456" }),
		"uppercase commit":       mutate(SourceBranch, func(b *SourceBinding) { b.SelectedCommit = strings.Repeat("A", 40) }),
		"missing remote":         mutate(SourceBranch, func(b *SourceBinding) { b.RemoteIdentity = "" }),
		"remote is a url":        mutate(SourceBranch, func(b *SourceBinding) { b.RemoteIdentity = "https://example.test/x.git" }),
		"missing base":           mutate(SourceBranch, func(b *SourceBinding) { b.BaseCommit = "" }),
		"unresolved timestamp":   mutate(SourceBranch, func(b *SourceBinding) { b.ResolvedAt = time.Time{} }),
		"malformed tree":         mutate(SourceBranch, func(b *SourceBinding) { b.AdmittedTree = "tree" }),
		"malformed request":      mutate(SourceBranch, func(b *SourceBinding) { b.Requested.Name = "" }),
		"request kind disagrees": mutate(SourceBranch, func(b *SourceBinding) { b.Requested = SourceSelector{Kind: SourceDefault} }),
	}
	for name, binding := range cases {
		_, err := store.CreateSession(ctx, "refuse-"+name, repositoryBackedCreate("s-"+strings.ReplaceAll(name, " ", "-"), binding))
		if CodeOf(err) != CodeInvalidRequest {
			t.Fatalf("%s error = %v, want invalid_request", name, err)
		}
	}
	// A workspace-free session binds a policy and nothing repository-shaped.
	_, err := store.CreateSession(ctx, "refuse-bare-source", CreateSessionRequest{
		ID: "bare-source", Target: "target", Policy: "policy", Mode: "bare",
		Source: mutate(SourceDefault, func(*SourceBinding) {}),
	})
	if CodeOf(err) != CodeInvalidRequest {
		t.Fatalf("bare session source error = %v, want invalid_request", err)
	}
}

// Every kind survives the durable round trip byte for byte, including the NULL selected ref an
// exact commit carries, because a controller re-validates the binding after a daemon restart.
func TestSourceBindingSurvivesReopenForEveryKind(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "state")
	store := openTestStore(t, root)
	for _, kind := range []SourceKind{SourceDefault, SourceBranch, SourcePullRequest, SourceCommit} {
		binding := testSourceBinding(kind)
		if _, err := store.CreateSession(ctx, "create-"+string(kind), repositoryBackedCreate(string(kind), &binding)); err != nil {
			t.Fatalf("create %s session: %v", kind, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openTestStore(t, root)
	defer store.Close()
	for _, kind := range []SourceKind{SourceDefault, SourceBranch, SourcePullRequest, SourceCommit} {
		sess, err := store.GetSession(ctx, string(kind))
		if err != nil {
			t.Fatalf("reopen %s session: %v", kind, err)
		}
		want := testSourceBinding(kind)
		got, _ := json.Marshal(sess.Source)
		expected, _ := json.Marshal(&want)
		if string(got) != string(expected) {
			t.Fatalf("reopened %s binding =\n%s\nwant\n%s", kind, got, expected)
		}
	}
}

// An idempotent create replay must be answered with the session it asked for, field for field.
// A different selector under the same key is a different request, not a retry.
func TestCreateReplayRefusesADifferentSourceBinding(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "state"))
	defer store.Close()
	branch := testSourceBinding(SourceBranch)
	req := repositoryBackedCreate("replayed", &branch)
	first, err := store.CreateSession(ctx, "same-key", req)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := store.CreateSession(ctx, "same-key", req)
	if err != nil || replayed.ID != first.ID {
		t.Fatalf("exact replay = %+v, err=%v", replayed, err)
	}
	other := testSourceBinding(SourceCommit)
	switched := repositoryBackedCreate("replayed", &other)
	if _, err := store.CreateSession(ctx, "same-key", switched); CodeOf(err) != CodeIdempotencyConflict {
		t.Fatalf("switched selector error = %v, want idempotency_conflict", err)
	}
}

// Rule 10: migrate a persisted pull-request binding into the generic shape ONLY where the exact
// old values prove it. A v22 row's own columns plus its immutable freshness receipts state the
// remote, the default ref and head, the derived PR ref and head, and the merge base — so the
// migrated binding is derived, never guessed. Its admitted tree is absent because no pre-selector
// row ever recorded one.
func TestMigrationDerivesGenericSourceBindingFromProvenPullRequestRows(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, databaseName)
	buildLegacyDatabase(t, path, 22)

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defaultCommit, pullHead := strings.Repeat("a", 40), strings.Repeat("b", 40)
	mergeBase := strings.Repeat("c", 40)
	receipts := func(pull bool) string {
		list := []RepositoryFreshnessReceipt{{
			Version: 2, Name: "primary", RequestedRevision: "refs/heads/main",
			ResolvedRevision: defaultCommit, WorkspaceBaseRevision: mergeBase,
			FetchedAt: time.Unix(1700000000, 0).UTC(), RemoteIdentity: "origin",
			StaleBaseStatus: "current", StaleBaseRevision: defaultCommit,
		}}
		if pull {
			list = append(list, RepositoryFreshnessReceipt{
				Version: 2, Name: "pull_request", RequestedRevision: "refs/pull/514/head",
				ResolvedRevision: pullHead, FetchedAt: time.Unix(1700000100, 0).UTC(),
				RemoteIdentity: "origin", StaleBaseStatus: "current", StaleBaseRevision: pullHead,
			})
		}
		encoded, _ := json.Marshal(list)
		return string(encoded)
	}
	insert := func(id, freshness string, number int, pullRef, head, base string) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO sessions
			(id, target, revision, state, activity, max_turns, max_queued_turns, max_queued_bytes,
			 created_at, updated_at, companions, policy, repository, workspace, fork_name, base_commit,
			 repository_freshness, pull_request_number, pull_request_ref, pull_request_head_commit)
			VALUES (?, ?, 1, 'open', 'parked', 100, 20, 1048576, 1000, 1000, '[]', 'responder', '/repo', ?, ?, ?, ?, ?, ?, ?)`,
			id, "codex:legacy", "/work/"+id, "fork-"+id, base, freshness, number, pullRef, head,
		); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	insert("proven-pull", receipts(true), 514, "refs/pull/514/head", pullHead, mergeBase)
	// No receipts at all: nothing proves the remote identity or the default head, so this row
	// stays a historical already-bound session rather than acquiring a synthesized binding.
	insert("unproven-pull", "", 515, "refs/pull/515/head", pullHead, mergeBase)
	// Receipts that disagree with the columns prove nothing either.
	insert("mismatched-pull", receipts(true), 516, "refs/pull/516/head", pullHead, mergeBase)
	// An ordinary default-branch session was never labeled, so it must not be labeled now.
	insert("legacy-default", receipts(false), 0, "", "", defaultCommit)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store := openTestStore(t, root)
	defer store.Close()

	proven, err := store.GetSession(ctx, "proven-pull")
	if err != nil {
		t.Fatal(err)
	}
	if proven.Source == nil {
		t.Fatal("proven pull-request row did not migrate to a generic source binding")
	}
	want := SourceBinding{
		Version: SourceBindingVersion, Kind: SourcePullRequest,
		Requested:      SourceSelector{Kind: SourcePullRequest, Number: 514, ExpectedHeadCommit: pullHead},
		RemoteIdentity: "origin", DefaultRef: "refs/heads/main", DefaultCommit: defaultCommit,
		SelectedRef: ref("refs/pull/514/head"), SelectedCommit: pullHead, BaseCommit: mergeBase,
		ResolvedAt:        time.Unix(1700000100, 0).UTC(),
		PullRequestNumber: 514, PullRequestExpectedHead: pullHead,
	}
	got, _ := json.Marshal(proven.Source)
	expected, _ := json.Marshal(&want)
	if string(got) != string(expected) {
		t.Fatalf("migrated binding =\n%s\nwant\n%s", got, expected)
	}

	for _, id := range []string{"unproven-pull", "mismatched-pull", "legacy-default"} {
		sess, err := store.GetSession(ctx, id)
		if err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if sess.Source != nil {
			t.Fatalf("%s acquired a synthesized binding %+v", id, sess.Source)
		}
		if sess.BaseCommit == "" || sess.Workspace == "" {
			t.Fatalf("%s lost its historical bindings: %+v", id, sess)
		}
	}
	// The historical pull-request identity is preserved on disk even where it could not be
	// proven into the generic shape; removing the runtime field never deletes the record.
	var number int
	var pullRef, head string
	if err := store.db.QueryRow(
		`SELECT pull_request_number, pull_request_ref, pull_request_head_commit FROM sessions WHERE id = ?`,
		"unproven-pull",
	).Scan(&number, &pullRef, &head); err != nil {
		t.Fatal(err)
	}
	if number != 515 || pullRef != "refs/pull/515/head" || head != pullHead {
		t.Fatalf("historical pull columns = %d/%q/%q", number, pullRef, head)
	}
}

// A malformed union member never reaches resolution. The shape check is transport neutral and
// runs no subprocess, so the store can repeat it as its own final boundary.
func TestSourceSelectorShapeRejectsEveryMalformedUnionMember(t *testing.T) {
	for name, selector := range map[string]SourceSelector{
		"empty kind":            {},
		"unknown kind":          {Kind: "tag", Name: "v1"},
		"default with a name":   {Kind: SourceDefault, Name: "main"},
		"branch without a name": {Kind: SourceBranch},
		"branch with a number":  {Kind: SourceBranch, Name: "main", Number: 1},
		"branch option flag":    {Kind: SourceBranch, Name: "--upload-pack=touch"},
		"branch newline":        {Kind: SourceBranch, Name: "main\nrefs/heads/other"},
		"branch too long":       {Kind: SourceBranch, Name: strings.Repeat("x", MaxSourceBranchBytes+1)},
		"pull request zero":     {Kind: SourcePullRequest, Number: 0},
		"pull request negative": {Kind: SourcePullRequest, Number: -3},
		"pull request unbound":  {Kind: SourcePullRequest, Number: MaxSourcePullRequestNumber + 1},
		"pull request sha":      {Kind: SourcePullRequest, Number: 1, SHA: strings.Repeat("a", 40)},
		"pull request evidence": {Kind: SourcePullRequest, Number: 1, ExpectedHeadCommit: "abc"},
		"commit abbreviated":    {Kind: SourceCommit, SHA: "0123456"},
		"commit uppercase":      {Kind: SourceCommit, SHA: strings.ToUpper(strings.Repeat("a", 40))},
		"commit symbolic":       {Kind: SourceCommit, SHA: "HEAD"},
		"commit with evidence":  {Kind: SourceCommit, SHA: strings.Repeat("a", 40), ExpectedHeadCommit: strings.Repeat("b", 40)},
	} {
		if err := ValidateSourceSelectorShape(selector); CodeOf(err) != CodeInvalidRequest {
			t.Fatalf("%s error = %v, want invalid_request", name, err)
		}
	}
	for name, selector := range map[string]SourceSelector{
		"default":            {Kind: SourceDefault},
		"branch":             {Kind: SourceBranch, Name: "feature/payments"},
		"pull request":       {Kind: SourcePullRequest, Number: 514},
		"pull request proof": {Kind: SourcePullRequest, Number: 514, ExpectedHeadCommit: strings.Repeat("a", 40)},
		"commit sha1":        {Kind: SourceCommit, SHA: strings.Repeat("a", 40)},
		"commit sha256":      {Kind: SourceCommit, SHA: strings.Repeat("a", 64)},
	} {
		if err := ValidateSourceSelectorShape(selector); err != nil {
			t.Fatalf("%s error = %v, want accepted", name, err)
		}
	}
}

func TestSourceSelectorJSONShapeIsFrozen(t *testing.T) {
	for _, testCase := range []struct {
		selector SourceSelector
		want     string
	}{
		{SourceSelector{Kind: SourceDefault}, `{"kind":"default"}`},
		{SourceSelector{Kind: SourceBranch, Name: "feature/payments"}, `{"kind":"branch","name":"feature/payments"}`},
		{SourceSelector{Kind: SourcePullRequest, Number: 514}, `{"kind":"pull_request","number":514}`},
		{SourceSelector{Kind: SourceCommit, SHA: strings.Repeat("0", 40)}, fmt.Sprintf(`{"kind":"commit","sha":%q}`, strings.Repeat("0", 40))},
	} {
		encoded, err := json.Marshal(testCase.selector)
		if err != nil {
			t.Fatal(err)
		}
		if string(encoded) != testCase.want {
			t.Fatalf("selector JSON = %s, want %s", encoded, testCase.want)
		}
	}
}
