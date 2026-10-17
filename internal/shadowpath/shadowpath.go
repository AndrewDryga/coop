// Package shadowpath owns the repository visibility policy shared by every path that exports
// repository bytes into a sandbox or model prompt. A byte hidden in the primary checkout must not
// reappear through a synthesized home directory, preset prompt, build context, or sidecar bind.
package shadowpath

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/AndrewDryga/coop/internal/safefile"
	"golang.org/x/sys/unix"
)

var SecretGlobs = []string{
	".env", ".env.*", ".envrc", "*.secret", "*.secrets",
	"*.tfvars", "*.tfvars.json", "*.tfstate", "*.tfstate.*",
	"*.pem", "*.key", "*.p12", "*.pfx", "*.jks", "*.keystore", "*.p8", "*.ppk", "*.kdbx", "*.ovpn", "*.pkcs12",
	"id_rsa*", "id_ed25519*", "id_ecdsa*", "id_dsa*",
	".netrc", "_netrc", ".npmrc", ".yarnrc", ".yarnrc.yml", ".pypirc", ".git-credentials", ".htpasswd",
	".dockercfg", ".pgpass", ".my.cnf", ".s3cfg", ".boto", ".vault-token", "vault-token",
	"secrets", ".secrets", "credentials", ".aws", ".kube", ".ssh", ".gnupg", ".docker",
	"credentials.json", "service_account.json", "service-account.json", "*-sa.json", "client_secret*.json",
	"firebase-adminsdk*.json", "gha-creds-*.json", "auth.json", "secret.json", "secrets.json", "*.secret.json",
	"kubeconfig", "kubeconfig.yaml", "kubeconfig.yml", "database.yml", "credentials.y*ml", "secrets.y*ml",
}

var AllowGlobs = []string{
	"cacerts.pem", "cacert.pem", "ca-bundle.pem", "ca-bundle.crt", "ca-certificates.crt", "ca-cert.pem",
}

var allowTemplateGlobs = []string{"*.example", "*.sample", "*.template"}

var hardSecretGlobs = []string{
	"*.pem", "*.key", "*.p12", "*.pfx", "*.jks", "*.keystore", "*.p8", "*.ppk", "*.kdbx", "*.pkcs12",
	"id_rsa*", "id_ed25519*", "id_ecdsa*", "id_dsa*",
}

const (
	CoopIgnoreFile = ".coopignore"
	ignoreLimit    = 1 << 20
)

// UnreadableCoopignore is a .coopignore that is there but is not a regular file of at most 1 MiB
// that coop can read, so coop cannot tell what it hides. Everything that decides what a box sees
// refuses it rather than drop its rules. File is its path from the repository, or its full path in
// a folder above the repository; Reason follows "it".
type UnreadableCoopignore struct{ File, Reason string }

func (e UnreadableCoopignore) Error() string {
	return "coop cannot tell what " + e.File + " hides, because it " + e.Reason
}

type UserGlobs struct {
	Base []string
	Path []string
}

// Snapshot is an immutable view of every .coopignore relevant to one pinned repository subtree,
// those in the folders above the repository included. It exists for paths that re-export
// repository bytes outside the normally shadowed checkout: source bytes and their visibility rules
// must come from the same directory authority.
type Snapshot struct {
	enclosing []enclosingPolicy
	policies  map[string]UserGlobs
	bytes     int64
}

var ErrProtected = errors.New("path is protected by the repository secret policy")

const MaxSnapshotPolicyBytes = 16 << 20

// OpenTree opens rel through raw no-follow directory descriptors and freezes the root, ancestor,
// .coopignore files from those same pinned directories. Descendant policies are added only while
// traversing their actual pinned directory descriptors (Extend / ReadRegular), so swapping two
// real child directories cannot pair bytes from one with visibility rules from the other. The
// returned tree has an untouched read offset and remains valid after a repository path rename.
// repo is the path root was pinned from: the rules of the folders above it, outside the pinned
// tree, are read by that path (enclosingPolicies), and one that cannot be read fails the open.
func OpenTree(root *os.File, repo, rel string) (*os.File, *Snapshot, error) {
	parts, err := cleanParts(rel)
	if err != nil {
		return nil, nil, err
	}
	enclosing, totalBytes, err := enclosingPolicies(repo)
	if err != nil {
		return nil, nil, err
	}
	current, err := safefile.CloneDir(root)
	if err != nil {
		return nil, nil, err
	}
	policies := make(map[string]UserGlobs)
	dirRel := ""
	if err := snapshotPolicy(current, dirRel, policies, &totalBytes); err != nil {
		_ = current.Close()
		return nil, nil, err
	}
	for _, part := range parts {
		next, openErr := safefile.OpenDir(current, part)
		_ = current.Close()
		if openErr != nil {
			return nil, nil, openErr
		}
		current = next
		if dirRel == "" {
			dirRel = part
		} else {
			dirRel += "/" + part
		}
		if err := snapshotPolicy(current, dirRel, policies, &totalBytes); err != nil {
			_ = current.Close()
			return nil, nil, err
		}
	}
	return current, &Snapshot{enclosing: enclosing, policies: policies, bytes: totalBytes}, nil
}

func (s *Snapshot) PolicyBytes() int64 { return s.bytes }

// Extend returns a new snapshot containing the policy read from this exact pinned descendant.
func (s *Snapshot) Extend(dirRel string, dir *os.File) (*Snapshot, error) {
	policies := make(map[string]UserGlobs, len(s.policies)+1)
	for rel, globs := range s.policies {
		policies[rel] = globs
	}
	dirRel = filepath.ToSlash(filepath.Clean(filepath.FromSlash(dirRel)))
	totalBytes := s.bytes
	if err := snapshotPolicy(dir, dirRel, policies, &totalBytes); err != nil {
		return nil, err
	}
	return &Snapshot{enclosing: s.enclosing, policies: policies, bytes: totalBytes}, nil
}

// ReadRegular walks a path from one pinned subtree root, loading each descendant directory's
// policy from the same descriptor used for the next lookup. It refuses a protected path before
// opening its file bytes.
func (s *Snapshot) ReadRegular(tree *os.File, treeRel, rel string, limit int64) ([]byte, error) {
	parts, err := cleanParts(rel)
	if err != nil || len(parts) == 0 {
		if err == nil {
			err = fmt.Errorf("path %q is not a file", rel)
		}
		return nil, err
	}
	current, err := safefile.CloneDir(tree)
	if err != nil {
		return nil, err
	}
	defer func() { _ = current.Close() }()
	policy := s
	currentRel := filepath.ToSlash(filepath.Clean(filepath.FromSlash(treeRel)))
	if currentRel == "." {
		currentRel = ""
	}
	for _, part := range parts[:len(parts)-1] {
		childRel := part
		if currentRel != "" {
			childRel = path.Join(currentRel, part)
		}
		if policy.Shadowed(childRel) {
			return nil, fmt.Errorf("%w: %s", ErrProtected, childRel)
		}
		next, openErr := safefile.OpenDir(current, part)
		if openErr != nil {
			return nil, openErr
		}
		policy, err = policy.Extend(childRel, next)
		if err != nil {
			_ = next.Close()
			return nil, err
		}
		_ = current.Close()
		current = next
		currentRel = childRel
	}
	fileRel := parts[len(parts)-1]
	if currentRel != "" {
		fileRel = path.Join(currentRel, fileRel)
	}
	if policy.Shadowed(fileRel) {
		return nil, fmt.Errorf("%w: %s", ErrProtected, fileRel)
	}
	return safefile.ReadRegular(current, parts[len(parts)-1], limit)
}

// Shadowed reports whether relSlash is hidden by the built-in policy or the frozen repository
// rules. Invalid or escaping paths fail closed.
func (s *Snapshot) Shadowed(relSlash string) bool {
	local := filepath.Clean(filepath.FromSlash(relSlash))
	if filepath.IsAbs(local) || !filepath.IsLocal(local) || local == "." {
		return true
	}
	relSlash = filepath.ToSlash(local)
	return shadowed(relSlash, s.enclosing, func(dirRel string) UserGlobs {
		return s.policies[dirRel]
	})
}

func snapshotPolicy(dir *os.File, dirRel string, policies map[string]UserGlobs, totalBytes *int64) error {
	data, err := readCoopignore(dir, path.Join(dirRel, CoopIgnoreFile))
	if errors.Is(err, fs.ErrNotExist) {
		policies[dirRel] = UserGlobs{}
		return nil
	}
	if err != nil {
		return err
	}
	*totalBytes += int64(len(data))
	if *totalBytes > MaxSnapshotPolicyBytes {
		return fmt.Errorf("repository policy snapshot exceeds %d bytes", MaxSnapshotPolicyBytes)
	}
	policies[dirRel] = ParseUserGlobs(data)
	return nil
}

// enclosingPolicy is the .coopignore of a folder above a repository. Its rules reach the folder's
// whole subtree, so a path in the repository is matched as its path from that folder: prefix, the
// repository's own path from there ending in "/", then the path in the repository.
type enclosingPolicy struct {
	prefix string
	globs  UserGlobs
}

// enclosingPolicies reads the .coopignore of each folder above repo, stopping before the home
// folder or a folder above it. A box can make any folder it writes a repository by planting a .git
// there, and a command run from inside takes that folder for the project: without these rules,
// what a .coopignore above it hides there would mount, copy and build like any other file. A real
// submodule keeps them too. The walk does not stop at the repository around repo, since a box can
// move that .git away; it cannot change a .coopignore, which every box mounts read-only. The owner
// of the folder decides, not the file's: a .coopignore in a folder another user owns is skipped,
// since anyone can create /tmp/.coopignore or hard-link one of your files there so it looks like
// yours, while one saved with sudo in your own folder applies. Root's /tmp, /private and /Volumes
// therefore count for nothing. It returns the rules it read and their size, or the first
// .coopignore it cannot read as an UnreadableCoopignore naming its full path.
func enclosingPolicies(repo string) ([]enclosingPolicy, int64, error) {
	canonical, err := filepath.EvalSymlinks(repo)
	if err != nil {
		if canonical, err = filepath.Abs(repo); err != nil {
			return nil, 0, err
		}
	}
	var (
		policies []enclosingPolicy
		size     int64
	)
	for child, dir := canonical, filepath.Dir(canonical); dir != child; child, dir = dir, filepath.Dir(dir) {
		if _, above := HomeOrAbove(dir); above {
			break
		}
		file := filepath.Join(dir, CoopIgnoreFile)
		if _, err := os.Lstat(file); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if uid, known := ownerOf(dir); known && uid != os.Getuid() {
			continue
		}
		root, err := safefile.OpenRoot(dir)
		if err != nil {
			return nil, 0, unreadableCoopignore(nil, file, err)
		}
		data, err := readCoopignore(root, file)
		_ = root.Close()
		if errors.Is(err, fs.ErrNotExist) {
			continue // removed since the Lstat
		}
		if err != nil {
			return nil, 0, err
		}
		size += int64(len(data))
		if g := ParseUserGlobs(data); len(g.Base) > 0 || len(g.Path) > 0 {
			rel, _ := filepath.Rel(dir, canonical) // dir is above canonical, so this cannot fail
			policies = append(policies, enclosingPolicy{prefix: filepath.ToSlash(rel) + "/", globs: g})
		}
	}
	return policies, size, nil
}

// readCoopignore reads the .coopignore in dir; file is the path a refusal names it by. Nothing there
// is fs.ErrNotExist. Anything else that is not a regular file of at most ignoreLimit coop can read
// is an UnreadableCoopignore saying why.
func readCoopignore(dir *os.File, file string) ([]byte, error) {
	data, err := safefile.ReadRegular(dir, CoopIgnoreFile, ignoreLimit)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, unreadableCoopignore(dir, file, err)
	}
	return data, err
}

// unreadableCoopignore says why the .coopignore in dir could not be read, from what dir holds under
// that name; dir is nil when its folder could not be opened. Anything else, such as a file without
// read permission, is named by what made the read fail.
func unreadableCoopignore(dir *os.File, file string, err error) UnreadableCoopignore {
	for errors.Unwrap(err) != nil {
		err = errors.Unwrap(err)
	}
	reason := "cannot be read: " + err.Error()
	var st unix.Stat_t
	if dir != nil && unix.Fstatat(int(dir.Fd()), CoopIgnoreFile, &st, unix.AT_SYMLINK_NOFOLLOW) == nil {
		switch st.Mode & unix.S_IFMT {
		case unix.S_IFLNK:
			reason = "is a link"
		case unix.S_IFDIR:
			reason = "is a folder"
		case unix.S_IFREG:
			if st.Size > ignoreLimit {
				reason = "is larger than 1 MiB"
			}
		default:
			reason = "is not a regular file"
		}
	}
	return UnreadableCoopignore{File: file, Reason: reason}
}

// ownerOf is the user ID that owns path, by Lstat; known is false when that cannot be told. It is
// a variable so tests can stand in owners that only root could set up.
var ownerOf = func(path string) (uid int, known bool) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

// HomeOrAbove reports whether dir is the user's home folder or a folder above it, and names the
// home folder. box.FindRepo never picks one for a project, a trusted git view refuses one its
// walk climbed to (forkspace), and the rules a project takes from the folders above it stop below
// them. It compares folders, not spellings, so neither a case-insensitive disk nor a linked home
// lets one through. A HOME that is unset or relative falls back to the account's home folder.
func HomeOrAbove(dir string) (string, bool) {
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		u, err := user.Current()
		if err != nil || u.HomeDir == "" {
			return "", false
		}
		home = u.HomeDir
	}
	target, err := os.Stat(dir)
	if err != nil {
		return home, false
	}
	walk := home
	if real, err := filepath.EvalSymlinks(home); err == nil {
		walk = real
	}
	for ; ; walk = filepath.Dir(walk) {
		if info, err := os.Stat(walk); err == nil && os.SameFile(info, target) {
			return home, true
		}
		if filepath.Dir(walk) == walk {
			return home, false
		}
	}
}

func cleanParts(rel string) ([]string, error) {
	if rel == "" || rel == "." {
		return nil, nil
	}
	if filepath.IsAbs(rel) {
		return nil, fmt.Errorf("path %q is not local", rel)
	}
	clean := filepath.Clean(rel)
	if !filepath.IsLocal(clean) || clean == "." {
		return nil, fmt.Errorf("path %q is not local", rel)
	}
	parts := strings.Split(clean, string(filepath.Separator))
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, fmt.Errorf("path %q is not clean", rel)
		}
	}
	return parts, nil
}

func ParseUserGlobs(data []byte) UserGlobs {
	var g UserGlobs
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "./")
		line = strings.TrimPrefix(line, "/")
		line = strings.TrimSuffix(line, "/")
		if line == "" {
			continue
		}
		if strings.Contains(line, "/") {
			g.Path = append(g.Path, filepath.ToSlash(line))
			if !strings.ContainsAny(line, "*?[") {
				g.Base = append(g.Base, filepath.Base(line))
			}
		} else {
			g.Base = append(g.Base, line)
		}
	}
	return g
}

// CommentedEntries lists the .coopignore entries that carry a comment after the pattern, such as
// "prod.yml   # basename". As in .gitignore, only a line that starts with # is a comment, so each
// of these is one pattern, comment and all, which hides nothing its author meant to hide.
func CommentedEntries(data []byte) []string {
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if i := strings.Index(line, "#"); i > 0 && (line[i-1] == ' ' || line[i-1] == '\t') {
			out = append(out, line)
		}
	}
	return out
}

// RepoCommentedEntries is CommentedEntries for the repository's root .coopignore, the file the
// docs tell people to write; an unreadable or absent file has none.
func RepoCommentedEntries(repo string) []string {
	if repo == "" {
		return nil
	}
	root, err := safefile.OpenRoot(repo)
	if err != nil {
		return nil
	}
	defer root.Close()
	data, err := safefile.ReadRegular(root, CoopIgnoreFile, ignoreLimit)
	if err != nil {
		return nil
	}
	return CommentedEntries(data)
}

// NewDecider returns the one visibility predicate for a repository-relative slash path. A launch
// asks it about every path in the repository, so it remembers each directory's verdict, ancestors
// included: a path then costs its own check plus one lookup. Re-deciding every ancestor for every
// path took 4 s in a checkout whose task folders held 41,000 files (2026-10-04). Its own check is
// shadowedHere's, made cheap: the built-in patterns are compiled once (see globSet), and only the
// directories above the path whose .coopignore has rules are consulted, instead of every level
// (0.4 s of a 0.6 s walk over 40,000 paths on 2026-10-06). The answers are shadowed's, which stays
// the reference (TestDeciderMatchesShadowed).
//
// A .coopignore that is there but cannot be read, in the repository or a folder above it, leaves
// the paths it covers undecided: for those the predicate returns its UnreadableCoopignore, as
// OpenTree does, and reports them hidden, so even a caller that cannot refuse hides them.
func NewDecider(repo string) func(string) (bool, error) {
	canonical, err := filepath.EvalSymlinks(repo)
	if err != nil {
		canonical, _ = filepath.Abs(repo)
	}
	enclosing, _, unread := enclosingPolicies(canonical)
	// The directories at and above dirRel whose .coopignore has rules, the folders above the
	// repository first. Almost every chain is empty; a directory without rules can never match, so
	// skipping it changes nothing. A folder above the repository matches its path rules against
	// prefix plus the path; one inside it, against the path below dir. A chain that meets a
	// .coopignore it cannot read carries that error instead.
	type policy struct {
		dir, prefix string
		globs       UserGlobs
	}
	type chain struct {
		policies []policy
		err      error
	}
	chains := map[string]chain{}
	var policies func(string) ([]policy, error)
	policies = func(dirRel string) ([]policy, error) {
		if c, ok := chains[dirRel]; ok {
			return c.policies, c.err
		}
		var c chain
		if dirRel != "" {
			c.policies, c.err = policies(parentSlash(dirRel))
		} else {
			for _, p := range enclosing {
				c.policies = append(c.policies, policy{prefix: p.prefix, globs: p.globs})
			}
			c.err = unread
		}
		if c.err == nil {
			var data []byte
			data, c.err = readRepoPolicy(canonical, dirRel)
			if g := ParseUserGlobs(data); len(g.Base) > 0 || len(g.Path) > 0 {
				c.policies = append(c.policies[:len(c.policies):len(c.policies)], policy{dir: dirRel, globs: g}) // never extend a shared chain
			}
		}
		chains[dirRel] = c
		return c.policies, c.err
	}
	secret, allow := compileGlobs(SecretGlobs), compileGlobs(AllowGlobs)
	template, hard := compileGlobs(allowTemplateGlobs), compileGlobs(hardSecretGlobs)
	here := func(relSlash string) (bool, error) { // shadowedHere, with the same answers
		name := relSlash[strings.LastIndexByte(relSlash, '/')+1:]
		lname := strings.ToLower(name)
		if secret.matches(lname) && !(allow.matches(lname) || template.matches(lname) && !hard.matches(lname)) {
			return true, nil
		}
		chain, err := policies(parentSlash(relSlash))
		if err != nil {
			return true, err
		}
		for _, p := range chain {
			remaining := relSlash
			if p.dir != "" {
				remaining = relSlash[len(p.dir)+1:]
			}
			if MatchesAny(name, p.globs.Base) || MatchesPath(p.prefix+remaining, p.globs.Path) {
				return true, nil
			}
		}
		return false, nil
	}
	type verdict struct {
		hidden bool
		err    error
	}
	dirs := map[string]verdict{"": {}} // the repository root is never hidden
	var dirShadowed func(string) (bool, error)
	dirShadowed = func(dirRel string) (bool, error) {
		v, ok := dirs[dirRel]
		if !ok {
			if v.hidden, v.err = dirShadowed(parentSlash(dirRel)); !v.hidden {
				v.hidden, v.err = here(dirRel)
			}
			dirs[dirRel] = v
		}
		return v.hidden, v.err
	}
	return func(relSlash string) (bool, error) {
		relSlash = cleanSlash(relSlash)
		if hidden, err := dirShadowed(parentSlash(relSlash)); hidden {
			return true, err
		}
		return here(relSlash)
	}
}

// readRepoPolicy reads the .coopignore of the folder dirRel in the repository at root through
// descriptors that never follow a link. A folder reached through a link or a file has none of its
// own: the walks that ask about paths never enter one, and the folder the link reaches is judged
// by its own path.
func readRepoPolicy(root, dirRel string) ([]byte, error) {
	file := path.Join(dirRel, CoopIgnoreFile)
	// Almost no directory has a .coopignore, and one lstat says so; the safe read below opens the
	// root and every component. Only "nothing there" skips it: anything else, a link or an error
	// included, gets the safe read, which alone decides what counts.
	if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(file))); errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return nil, nil
	}
	dir, err := safefile.OpenRoot(root)
	if err == nil && dirRel != "" {
		var sub *os.File
		sub, err = safefile.OpenDir(dir, filepath.FromSlash(dirRel))
		_ = dir.Close()
		if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOTDIR) {
			return nil, nil
		}
		dir = sub
	}
	if err != nil {
		return nil, unreadableCoopignore(nil, file, err)
	}
	defer dir.Close()
	data, err := readCoopignore(dir, file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil // removed since the lstat
	}
	return data, err
}

// globSet is MatchesAny over a fixed list of patterns, for a caller that asks about many names.
// A name can match a pattern only if it starts with the text before the pattern's first wildcard
// and ends with the text after its last one, and almost no name passes both string comparisons;
// filepath.Match still decides every name that does. Matching 40,000 names against SecretGlobs
// with filepath.Match alone took 0.31 s (2026-10-06).
type globSet []compiledGlob

type compiledGlob struct {
	pattern, prefix, suffix string
}

// wildcards are the bytes filepath.Match treats specially, escapes and classes included; the
// literal text is what lies outside them.
const wildcards = `*?[]\`

func compileGlobs(globs []string) globSet {
	set := make(globSet, len(globs))
	for i, glob := range globs {
		prefix, suffix := glob, ""
		if first := strings.IndexAny(glob, wildcards); first >= 0 {
			prefix, suffix = glob[:first], glob[strings.LastIndexAny(glob, wildcards)+1:]
		}
		set[i] = compiledGlob{pattern: glob, prefix: prefix, suffix: suffix}
	}
	return set
}

func (set globSet) matches(name string) bool {
	for _, glob := range set {
		if !strings.HasPrefix(name, glob.prefix) || !strings.HasSuffix(name, glob.suffix) {
			continue
		}
		if ok, _ := filepath.Match(glob.pattern, name); ok {
			return true
		}
	}
	return false
}

// parentSlash is the directory holding a cleaned slash path, "" at the top.
func parentSlash(relSlash string) string {
	if i := strings.LastIndexByte(relSlash, '/'); i >= 0 {
		return relSlash[:i]
	}
	return ""
}

// shadowed hides a path when it, or any directory above it, is hidden on its own.
func shadowed(relSlash string, enclosing []enclosingPolicy, loadDir func(string) UserGlobs) bool {
	relSlash = cleanSlash(relSlash)
	for i := 0; i < len(relSlash); i++ {
		if relSlash[i] == '/' && shadowedHere(relSlash[:i], enclosing, loadDir) {
			return true
		}
	}
	return shadowedHere(relSlash, enclosing, loadDir)
}

// shadowedHere is one path's own verdict, its ancestors aside: a secret name no allow rule
// rescues, or a .coopignore rule on the way down to it, from the folders above the repository on.
func shadowedHere(relSlash string, enclosing []enclosingPolicy, loadDir func(string) UserGlobs) bool {
	name := relSlash
	if i := strings.LastIndexByte(relSlash, '/'); i >= 0 {
		name = relSlash[i+1:]
	}
	return secretName(name) || shadowedByCoopignore(relSlash, enclosing, loadDir)
}

// secretName is the verdict on one file or folder name by itself: a secret glob no allow rule
// rescues.
func secretName(name string) bool {
	lname := strings.ToLower(name)
	allowed := MatchesAny(lname, AllowGlobs) ||
		(MatchesAny(lname, allowTemplateGlobs) && !MatchesAny(lname, hardSecretGlobs))
	return MatchesAny(lname, SecretGlobs) && !allowed
}

func cleanSlash(relSlash string) string {
	return filepath.ToSlash(filepath.Clean(filepath.FromSlash(relSlash)))
}

func shadowedByCoopignore(relSlash string, enclosing []enclosingPolicy, loadDir func(string) UserGlobs) bool {
	base := relSlash
	if i := strings.LastIndexByte(relSlash, '/'); i >= 0 {
		base = relSlash[i+1:]
	}
	for _, p := range enclosing {
		if MatchesAny(base, p.globs.Base) || MatchesPath(p.prefix+relSlash, p.globs.Path) {
			return true
		}
	}
	dir, remaining := "", relSlash
	for {
		g := loadDir(dir)
		if MatchesAny(base, g.Base) || MatchesPath(remaining, g.Path) {
			return true
		}
		i := strings.IndexByte(remaining, '/')
		if i < 0 {
			return false
		}
		if dir == "" {
			dir = remaining[:i]
		} else {
			dir += "/" + remaining[:i]
		}
		remaining = remaining[i+1:]
	}
}

// BuiltinSecret reports whether the file at relSlash, or a folder above it, is a secret by its name
// alone, as `secrets/` or `.aws/` is: what the decider checks for each before any .coopignore. A scan
// that cannot read a .coopignore still names such a file instead of only the .coopignore.
func BuiltinSecret(relSlash string) bool {
	for name := range strings.SplitSeq(cleanSlash(relSlash), "/") {
		if secretName(name) {
			return true
		}
	}
	return false
}

func MatchesAny(name string, globs []string) bool {
	for _, glob := range globs {
		if ok, _ := filepath.Match(glob, name); ok {
			return true
		}
	}
	return false
}

func MatchesPath(relSlash string, globs []string) bool {
	for _, glob := range globs {
		if ok, _ := filepath.Match(glob, relSlash); ok {
			return true
		}
	}
	return false
}
