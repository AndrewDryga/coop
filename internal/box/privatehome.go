package box

import (
	"io"
	"os"
	"path/filepath"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
)

// privateHomes builds a per-box scratch home for every in-scope agent whose CLI keeps
// single-writer state in its home (Agent.SharedHomePaths — codex ≥0.144's sqlite databases):
// two boxes bind-mounting the same account's home make the second crash at startup ("failed
// to initialize sqlite state runtime"), and sqlite over the shared macOS bind mount risks
// corruption. Instead of sharing the whole dir, each box gets a fresh scratch home SEEDED
// with the profile's small top-level files (config, identity — never *.sqlite*), and only
// the durable paths the agent names stay bound from the profile: the credential, the session
// rollouts, installed user content. Parallel boxes on one account then never contend.
//
// Returns agent → scratch dir, plus the dirs for the caller's cleanup (removed when the run
// ends — the state is per-box by design). Best-effort: a seeding failure falls back to the
// plain shared mount for that agent (yesterday's behavior) rather than failing the run.
func privateHomes(cfg *config.Config, spec RunSpec) (map[string]string, []string) {
	if !spec.Homes {
		return nil, nil
	}
	homes := map[string]string{}
	var tmpDirs []string
	for _, name := range credentialScope(cfg, spec) {
		ag, ok := agents.Get(name)
		if !ok || len(ag.SharedHomePaths()) == 0 {
			continue
		}
		scratch, err := os.MkdirTemp("", "coop-home-"+name+"-")
		if err != nil {
			continue
		}
		if err := seedPrivateHome(cfg.AgentDir(name), scratch, ag.SharedHomePaths()); err != nil {
			os.RemoveAll(scratch)
			continue
		}
		homes[name] = scratch
		tmpDirs = append(tmpDirs, scratch)
	}
	return homes, tmpDirs
}

// seedPrivateHome copies the profile's top-level REGULAR files into the scratch home —
// config.toml, version.json, installation_id, … — so the boxed CLI doesn't re-run first-run
// setup. Skipped: every sqlite family file (*.sqlite, -wal, -shm — the per-box state this
// exists to isolate) and the shared paths (they're bind-mounted from the profile instead).
// Dirs are never copied: the durable ones are in shared, the rest (cache/, .tmp/, log/) are
// ephemera an agent rebuilds.
func seedPrivateHome(profile, scratch string, shared []string) error {
	skip := map[string]bool{}
	for _, s := range shared {
		skip[strings.TrimSuffix(s, "/")] = true
	}
	entries, err := os.ReadDir(profile)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.Type().IsRegular() || skip[e.Name()] || strings.Contains(e.Name(), ".sqlite") {
			continue
		}
		if err := copyFile(filepath.Join(profile, e.Name()), filepath.Join(scratch, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, fi.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// privateHomeMountArgs renders one agent's private-home mounts: the scratch dir at the
// agent's home, then each shared path bound from the profile — auth.json as a single-file
// bind (codex rewrites it in place, so the shared inode carries token refreshes to every
// box and the host), dirs (trailing-slash entries) created host-side first so the binds
// can't fail on a fresh profile, missing FILES skipped (an API-key login has no auth.json).
// skipDirs names shared dirs another mount already covers at the same target (the ACP
// lead's sessions overlay) — a duplicate -v target is a docker error, not a shadow.
func privateHomeMountArgs(profile, scratch, homeInBox, agent string, shared []string, skipDirs map[string]bool) []string {
	args := []string{"-v", scratch + ":" + homeInBox + "/." + agent}
	for _, entry := range shared {
		rel, isDir := strings.CutSuffix(entry, "/")
		if skipDirs[rel] {
			continue
		}
		host := filepath.Join(profile, rel)
		if _, err := os.Stat(host); err != nil {
			if !isDir {
				continue // a missing shared file has nothing to bind
			}
			if mkErr := os.MkdirAll(host, 0o700); mkErr != nil {
				continue
			}
		}
		args = append(args, "-v", host+":"+homeInBox+"/."+agent+"/"+rel)
	}
	return args
}
