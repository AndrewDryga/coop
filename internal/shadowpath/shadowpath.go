// Package shadowpath owns the repository visibility policy shared by every path that exports
// repository bytes into a sandbox or model prompt. A byte hidden in the primary checkout must not
// reappear through a synthesized home directory, preset prompt, build context, or sidecar bind.
package shadowpath

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/AndrewDryga/coop/internal/safefile"
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

type UserGlobs struct {
	Base []string
	Path []string
}

// Snapshot is an immutable view of every .coopignore relevant to one pinned repository subtree.
// It exists for paths that re-export repository bytes outside the normally shadowed checkout:
// source bytes and their visibility rules must come from the same directory authority.
type Snapshot struct {
	policies map[string]UserGlobs
	bytes    int64
}

var ErrProtected = errors.New("path is protected by the repository secret policy")

const MaxSnapshotPolicyBytes = 16 << 20

// OpenTree opens rel through raw no-follow directory descriptors and freezes the root, ancestor,
// .coopignore files from those same pinned directories. Descendant policies are added only while
// traversing their actual pinned directory descriptors (Extend / ReadRegular), so swapping two
// real child directories cannot pair bytes from one with visibility rules from the other. The
// returned tree has an untouched read offset and remains valid after a repository path rename.
func OpenTree(root *os.File, rel string) (*os.File, *Snapshot, error) {
	parts, err := cleanParts(rel)
	if err != nil {
		return nil, nil, err
	}
	current, err := safefile.CloneDir(root)
	if err != nil {
		return nil, nil, err
	}
	policies := make(map[string]UserGlobs)
	var totalBytes int64
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
	return current, &Snapshot{policies: policies, bytes: totalBytes}, nil
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
	return &Snapshot{policies: policies, bytes: totalBytes}, nil
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
	return shadowed(relSlash, func(dirRel string) UserGlobs {
		return s.policies[dirRel]
	})
}

func snapshotPolicy(dir *os.File, dirRel string, policies map[string]UserGlobs, totalBytes *int64) error {
	data, err := safefile.ReadRegular(dir, CoopIgnoreFile, ignoreLimit)
	if errors.Is(err, os.ErrNotExist) {
		policies[dirRel] = UserGlobs{}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read repository policy in %q: %w", dirRel, err)
	}
	*totalBytes += int64(len(data))
	if *totalBytes > MaxSnapshotPolicyBytes {
		return fmt.Errorf("repository policy snapshot exceeds %d bytes", MaxSnapshotPolicyBytes)
	}
	policies[dirRel] = ParseUserGlobs(data)
	return nil
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
// path took 4 s in a checkout whose task folders held 41,000 files (2026-10-04). The answers are
// shadowed's, which stays the reference (TestDeciderMatchesShadowed).
func NewDecider(repo string) func(string) bool {
	canonical, err := filepath.EvalSymlinks(repo)
	if err != nil {
		canonical, _ = filepath.Abs(repo)
	}
	cache := map[string]UserGlobs{}
	loadDir := func(dirRel string) UserGlobs {
		if g, ok := cache[dirRel]; ok {
			return g
		}
		var g UserGlobs
		rel := CoopIgnoreFile
		if dirRel != "" {
			rel = filepath.Join(filepath.FromSlash(dirRel), CoopIgnoreFile)
		}
		// Almost no directory has a .coopignore, and one lstat says so; the safe read below opens
		// the root and every component. Only "nothing there" skips it: anything else, a link or an
		// error included, gets the safe read, which alone decides what counts.
		if _, statErr := os.Lstat(filepath.Join(canonical, rel)); errors.Is(statErr, fs.ErrNotExist) {
			cache[dirRel] = g
			return g
		}
		root, openErr := safefile.OpenRoot(canonical)
		if openErr == nil {
			if data, readErr := safefile.ReadRegular(root, rel, ignoreLimit); readErr == nil {
				g = ParseUserGlobs(data)
			}
			_ = root.Close()
		}
		cache[dirRel] = g
		return g
	}
	dirs := map[string]bool{"": false} // the repository root is never hidden
	var dirShadowed func(string) bool
	dirShadowed = func(dirRel string) bool {
		if hidden, ok := dirs[dirRel]; ok {
			return hidden
		}
		hidden := dirShadowed(parentSlash(dirRel)) || shadowedHere(dirRel, loadDir)
		dirs[dirRel] = hidden
		return hidden
	}
	return func(relSlash string) bool {
		relSlash = cleanSlash(relSlash)
		return dirShadowed(parentSlash(relSlash)) || shadowedHere(relSlash, loadDir)
	}
}

// parentSlash is the directory holding a cleaned slash path, "" at the top.
func parentSlash(relSlash string) string {
	if i := strings.LastIndexByte(relSlash, '/'); i >= 0 {
		return relSlash[:i]
	}
	return ""
}

// shadowed hides a path when it, or any directory above it, is hidden on its own.
func shadowed(relSlash string, loadDir func(string) UserGlobs) bool {
	relSlash = cleanSlash(relSlash)
	for i := 0; i < len(relSlash); i++ {
		if relSlash[i] == '/' && shadowedHere(relSlash[:i], loadDir) {
			return true
		}
	}
	return shadowedHere(relSlash, loadDir)
}

// shadowedHere is one path's own verdict, its ancestors aside: a secret name no allow rule
// rescues, or a .coopignore rule on the way down to it.
func shadowedHere(relSlash string, loadDir func(string) UserGlobs) bool {
	name := relSlash
	if i := strings.LastIndexByte(relSlash, '/'); i >= 0 {
		name = relSlash[i+1:]
	}
	lname := strings.ToLower(name)
	allowed := MatchesAny(lname, AllowGlobs) ||
		(MatchesAny(lname, allowTemplateGlobs) && !MatchesAny(lname, hardSecretGlobs))
	return MatchesAny(lname, SecretGlobs) && !allowed || shadowedByCoopignore(relSlash, loadDir)
}

func cleanSlash(relSlash string) string {
	return filepath.ToSlash(filepath.Clean(filepath.FromSlash(relSlash)))
}

func shadowedByCoopignore(relSlash string, loadDir func(string) UserGlobs) bool {
	base := relSlash
	if i := strings.LastIndexByte(relSlash, '/'); i >= 0 {
		base = relSlash[i+1:]
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
