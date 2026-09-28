package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/eval"
	"github.com/AndrewDryga/coop/internal/preset"
)

// freezeConfiguration captures the exact system a configuration evaluates, so a run recorded now can
// be compared with one recorded after the preset, loop recipe or Coop build changes. It reads the
// content (preset.yaml plus every prompt, the already-read loop.yaml) and the running executable's
// identity here, in the CLI, and hands eval the bytes — eval stays a leaf. It reads nothing a
// candidate could have written and launches nothing.
func (a *app) freezeConfiguration(c eval.Configuration, loopConfig []byte, build eval.BuildIdentity) (eval.FrozenConfig, error) {
	frozen := eval.FrozenConfig{Kind: c.Kind, Label: c.Label, LoopConfig: loopConfig, Build: build}
	if c.Kind == eval.ConfigPreset {
		content, err := a.freezePresetContent(c.Label)
		if err != nil {
			return eval.FrozenConfig{}, err
		}
		frozen.Content = content
	}
	return frozen, nil
}

// freezePresetContent serializes a preset's resolved content deterministically: its preset.yaml
// bytes, then the lead prompt and each role prompt in declaration order. Editing the preset.yaml OR
// any prompt file it loads changes these bytes, which is exactly what an old-vs-new comparison of a
// same-named preset must notice.
func (a *app) freezePresetContent(name string) ([]byte, error) {
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return nil, err
	}
	globalDir := a.cfg.GlobalPresetsDir()
	loaded, err := preset.Load(repo, globalDir, name)
	if err != nil {
		return nil, err
	}
	// loaded.Dir is the folder Load parsed, so the manifest bytes are guaranteed to come from the
	// same file (not a re-resolution that could pick a differently-shadowed preset).
	manifest, err := os.ReadFile(filepath.Join(loaded.Dir, "preset.yaml"))
	if err != nil {
		return nil, fmt.Errorf("read preset.yaml: %w", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "preset.yaml\x00%s\x00", manifest)
	fmt.Fprintf(&b, "lead-prompt\x00%s\x00%s\x00", loaded.LeadPromptPath, loaded.LeadPromptText)
	for _, role := range loaded.Roles {
		fmt.Fprintf(&b, "role\x00%s\x00%s\x00%s\x00", role.Name, role.PromptPath, role.PromptText)
	}
	return []byte(b.String()), nil
}

// evalBuildIdentity is the running coop executable's identity: its version string, its commit and
// dirtiness (from the version stamp AND the embedded VCS build info, so a plain `go build` — which
// stamps `+dirty`, not the Makefile's `-dirty` — is still marked), and a digest of the binary so two
// builds of one version still differ. The digest is left empty only if the executable cannot be read
// (Version and Dirty still stand); it is never faked.
func (a *app) evalBuildIdentity() eval.BuildIdentity {
	version := resolveVersion()
	id := eval.BuildIdentity{Version: version, Dirty: strings.Contains(version, "dirty")}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				id.Revision = s.Value
			case "vcs.modified":
				id.Dirty = id.Dirty || s.Value == "true"
			}
		}
	}
	if exe, err := os.Executable(); err == nil {
		if digest, derr := fileDigest(exe); derr == nil {
			id.Digest = digest
		}
	}
	return id
}

// fileDigest is the sha256 of a file, hex-encoded with the sha256: prefix — the same shape coop uses
// for image ids, so a reader recognizes it.
func fileDigest(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
