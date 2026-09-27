package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"strconv"
)

// A comparison is only honest if it can tell apart the three things that change independently: the
// WORKLOAD (what the model is asked to do and how it is graded), the evaluated CONFIGURATION (the
// system under test, including the loop config and the Coop build), and the OBSERVED conditions (the
// machine, the models that actually answered, timing). Two runs with the same workload fingerprint
// and different configuration fingerprints are the point of a comparison; two with different
// WORKLOAD fingerprints must never be merged into one score. This file computes the workload
// fingerprint from the suite's SEMANTIC fields — so a comment or whitespace edit does not refuse a
// pooled score, but any change to an instruction, a budget, a verifier or the runner does. The
// configuration and observed fingerprints are frozen at their own milestones.
type Fingerprint string

func (f Fingerprint) Short() string {
	if len(f) > 12 {
		return string(f[:12])
	}
	return string(f)
}

// hasher accumulates length-prefixed fields so that no two distinct field sets can collide by
// running together — "ab"+"c" and "a"+"bc" hash differently because each field carries its length.
type hasher struct{ h hash.Hash }

func newHasher() *hasher { return &hasher{h: sha256.New()} }

func (w *hasher) text(label, value string) *hasher { return w.bytes(label, []byte(value)) }

func (w *hasher) bytes(label string, value []byte) *hasher {
	// label\0len(value)\0value — the label keeps two fields with the same bytes but different
	// meaning (an instruction vs a verifier path) from being interchangeable, and the length keeps
	// two adjacent fields from running together.
	w.h.Write([]byte(label))
	w.h.Write([]byte{0})
	w.h.Write([]byte(strconv.Itoa(len(value))))
	w.h.Write([]byte{0})
	w.h.Write(value)
	return w
}

func (w *hasher) sum() Fingerprint { return Fingerprint(hex.EncodeToString(w.h.Sum(nil))) }

// WorkloadFingerprint identifies the workload/protocol dimension from the suite's parsed fields: the
// runner, version and each case's identity, budget, verifier and inputs. loop_config is deliberately
// NOT here — it is part of the evaluated configuration (comparing two loop recipes is comparing two
// configurations, not two workloads), and it folds into the configuration fingerprint at its
// milestone. StageSuite supplies ContentDigest from the exact copied fixture and verifier trees;
// a later source edit cannot change a trial while keeping the comparison key.
func WorkloadFingerprint(s *Suite) Fingerprint {
	w := newHasher()
	w.text("schema", "eval.workload.v2")
	w.text("version", strconv.Itoa(s.Version))
	w.text("runner", string(s.Runner))
	w.text("content", string(s.ContentDigest))
	for _, c := range s.Cases {
		w.text("case.id", c.ID)
		w.text("case.timeout", c.Timeout.String())
		w.text("case.verifier", c.Verifier)
		w.text("case.instruction", c.Instruction)
		w.text("case.files", c.Files)
		w.text("case.fixture", c.Fixture)
		w.text("case.tasks", c.Tasks)
	}
	return w.sum()
}
