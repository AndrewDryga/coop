package eval

import "strconv"

// The evaluated-configuration dimension of a comparison: the system under test, frozen so that two
// runs of the same suite can be told apart by what actually changed. "Frozen" means the exact
// content, not a name or a path — an edited-but-same-named preset must compare old vs new, and two
// development builds of the same version must differ. The CLI reads the content (a preset's
// preset.yaml and prompts, the loop.yaml, the running executable) and hands the bytes here; this
// package hashes them, so eval stays a leaf that imports no preset, loop or runtime machinery.
//
// The spec's configuration dimension also lists the provider client versions, resolved account
// ladders and non-secret settings. Those are knowable only once a trial is prepared (the locked
// client image is resolved, the ladder is expanded), so they fold into this fingerprint at the
// preparation milestone; what is frozen at plan time is what is knowable then.

// BuildIdentity is the exact Coop executable a configuration was evaluated with. Version alone is not
// enough — two dirty builds carry the same version string — so the executable digest and the dirty
// flag are part of it. A change to any of these is a real change to the system under test.
type BuildIdentity struct {
	Version  string // e.g. v9.0.0-391-g5017eb89 (or -dirty)
	Revision string // the commit, when known
	Digest   string // sha256 of the executable, when computable
	Dirty    bool   // built from an uncommitted tree
}

func (b BuildIdentity) hash(w *hasher) {
	w.text("build.version", b.Version)
	w.text("build.revision", b.Revision)
	w.text("build.digest", b.Digest)
	w.text("build.dirty", strconv.FormatBool(b.Dirty))
}

func (b BuildIdentity) String() string {
	s := b.Version
	if s == "" {
		s = "unknown"
	}
	if b.Dirty {
		s += " (dirty)"
	}
	return s
}

// FrozenConfig is one evaluated configuration with its content and build captured. Content is the
// preset's frozen bytes (preset.yaml plus every prompt it loads) — nil for a bare target, whose
// label is its whole identity. LoopConfig is the loop.yaml bytes for a loop suite (the recipe under
// test), nil otherwise. Two runs whose FrozenConfig fingerprints differ are the point of comparing;
// two whose fingerprints match are the same system, so their scores pool.
type FrozenConfig struct {
	Kind       ConfigKind
	Label      string
	Content    []byte
	LoopConfig []byte
	Build      BuildIdentity
}

// Fingerprint identifies this configuration: its kind and label, the exact frozen content and loop
// recipe, and the Coop build. It uses the same length-prefixed hashing as the workload fingerprint,
// so no two distinct configurations collide.
func (c FrozenConfig) Fingerprint() Fingerprint {
	w := newHasher()
	w.text("schema", "eval.config.v1")
	w.text("kind", string(c.Kind))
	w.text("label", c.Label)
	w.bytes("content", c.Content)
	w.bytes("loop_config", c.LoopConfig)
	c.Build.hash(w)
	return w.sum()
}
