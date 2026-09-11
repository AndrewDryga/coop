package cli

import (
	"testing"
)

// The approved `coop doctor` reports. Rendering is a pure function of what the probes measured,
// so these run the REAL header, sections, rows and verdict against recorded evidence — no
// container runtime, and no fixture that could quietly disagree with the arithmetic.

// doctorEvidence is one run's measurements: the verdict per check id, plus the few values the
// host interprets rather than the probe.
type doctorEvidence struct {
	runtime    string
	image      string
	baseImage  string
	usingReal  bool
	hardened   bool
	pids       string // the cgroup value the box reported
	pidsConfig string // what configuration asked for
	verdicts   map[string]string
	sandboxErr string // the sandbox probe produced nothing; this is why
	taskErr    string // the task-channel probe produced nothing; this is why
	orphans    []string
	unattached []string
	surveyErr  string
}

// allPassed is the evidence of a healthy run on the shared image: every check ran and held.
func allPassed() doctorEvidence {
	verdicts := map[string]string{}
	for _, def := range doctorSecretChecks {
		verdicts[def.id] = "PASS"
	}
	for _, def := range doctorHostChecks {
		verdicts[def.id] = "PASS"
	}
	for _, def := range doctorCredentialChecks {
		verdicts[def.id] = "PASS"
	}
	for _, def := range doctorCloneChecks {
		verdicts[def.id] = "PASS"
	}
	verdicts["UID"] = "1000"
	verdicts["CAPS"] = "0000000000000000"
	verdicts["HOME"] = "writable"
	return doctorEvidence{
		runtime: "docker", image: "coop-box", baseImage: "coop-box",
		usingReal: true, hardened: true, pids: "4096", pidsConfig: "4096",
		verdicts: verdicts,
	}
}

// render replays the evidence through the same header, sections and verdict a real run uses.
func (e doctorEvidence) render(t *testing.T) string {
	t.Helper()
	return captureStderr(t, func() {
		doctorHeader(e.runtime, e.image, e.baseImage, e.usingReal)
		report := &doctorReport{}
		secrets, host, offline, tasks, credentials, clone := report.doctorSections()
		if e.sandboxErr != "" {
			secrets.probeFailed("Could not run the sandbox checks", e.sandboxErr, len(doctorSecretChecks))
			host.unrun("Host access and privileges could not be checked", len(doctorHostChecks)+3)
		} else {
			for _, def := range doctorSecretChecks {
				secrets.record(def, e.verdicts[def.id] == "PASS")
			}
			for _, def := range doctorHostChecks {
				host.record(def, e.verdicts[def.id] == "PASS")
			}
			doctorCheckUID(host, e.verdicts["UID"], e.usingReal)
			doctorCheckCaps(host, e.verdicts["CAPS"], e.hardened)
			doctorCheckPids(host, e.pids, e.hardened, e.pidsConfig)
		}
		offline.pass("Offline mode leaves only the loopback interface")
		switch {
		case e.taskErr != "":
			tasks.probeFailed("Could not run the task-channel checks", e.taskErr, doctorTaskChecks)
		case !e.usingReal:
			tasks.skip("Task channel not checked", "", doctorTaskChecks)
		default:
			tasks.pass("The task channel exposes only its 8 task tools")
			tasks.pass("Calls outside the task tools are refused")
			tasks.pass("Changes to a task held by another process are refused")
			tasks.pass("The assigned task can be updated")
		}
		for _, def := range doctorCredentialChecks {
			credentials.record(def, e.verdicts[def.id] == "PASS")
		}
		doctorCheckHome(credentials, e.verdicts["HOME"], e.usingReal)
		for _, def := range doctorCloneChecks {
			clone.record(def, e.verdicts[def.id] == "PASS")
		}
		e.survey(report)
		report.print()
	})
}

// survey adds the read-only abandoned-box findings, which sit outside the check tally.
func (e doctorEvidence) survey(report *doctorReport) {
	switch {
	case e.surveyErr != "":
		report.section("Running boxes").add(doctorRow{outcome: doctorNote,
			label: "Could not check for abandoned boxes", reason: e.surveyErr})
	case len(e.orphans) > 0:
		report.section("Abandoned boxes").add(doctorRow{outcome: doctorNote,
			label:   "2 boxes have no running supervisor",
			details: e.orphans,
			footer:  "They will be cleaned up when Coop next starts work or builds an image."})
	case len(e.unattached) > 0:
		report.section("Running boxes").add(doctorRow{outcome: doctorNote,
			label:   "Could not identify the supervisor for 1 box",
			details: e.unattached,
			footer:  "Check that the box is no longer needed before removing it manually."})
	}
}

func TestApprovedDoctorReports(t *testing.T) {
	alpine := func() doctorEvidence {
		e := allPassed()
		e.image, e.usingReal = "alpine", false
		delete(e.verdicts, "HOME")
		return e
	}
	cases := []struct {
		fixture  string
		evidence func() doctorEvidence
	}{
		{"18a-doctor-all-passed", allPassed},
		{"18b-doctor-alpine-fallback", alpine},
		{"18c-doctor-limits-not-applied", func() doctorEvidence {
			e := allPassed()
			e.runtime, e.hardened = "container", false
			return e
		}},
		{"18d-doctor-process-limit-disabled", func() doctorEvidence {
			e := allPassed()
			e.pidsConfig, e.pids = "0", ""
			return e
		}},
		{"18e-doctor-process-limit-unreadable", func() doctorEvidence {
			e := allPassed()
			e.pids = ""
			return e
		}},
		{"18f-doctor-root-user-fails", func() doctorEvidence {
			e := allPassed()
			e.image = "coop-atlas"
			e.verdicts["UID"] = "0"
			return e
		}},
		{"18g-doctor-abandoned-boxes", func() doctorEvidence {
			e := allPassed()
			e.orphans = []string{"42c96f112acd", "978b8129d20a"}
			return e
		}},
		{"18h-doctor-unattributed-box", func() doctorEvidence {
			e := allPassed()
			e.unattached = []string{"42c96f112acd"}
			return e
		}},
		{"18i-doctor-orphan-survey-failed", func() doctorEvidence {
			e := allPassed()
			e.surveyErr = "Docker did not respond before the check timed out."
			return e
		}},
		{"18j-doctor-task-probe-failed", func() doctorEvidence {
			e := allPassed()
			e.taskErr = "The task channel did not respond."
			return e
		}},
		{"18k-doctor-sandbox-probe-failed", func() doctorEvidence {
			e := allPassed()
			e.sandboxErr = "The container could not start: permission denied."
			return e
		}},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			assertApprovedOutput(t, tc.fixture, tc.evidence().render(t))
		})
	}
}

// The two refusals that stop a run before any check: no runtime to check on, and no temporary
// project to check against.
func TestApprovedDoctorRefusals(t *testing.T) {
	t.Run("18l-doctor-no-runtime", func(t *testing.T) {
		out := captureStderr(t, func() {
			failBlock("Could not check the Coop box", "Docker is unavailable.",
				"Start Docker, then run coop doctor again.")
		})
		assertApprovedOutput(t, "18l-doctor-no-runtime", out)
	})
	t.Run("18m-doctor-fixture-failed", func(t *testing.T) {
		out := captureStderr(t, func() {
			failBlock("Could not prepare the isolation checks",
				"Could not create a temporary project: permission denied.",
				"Fix the temporary-directory permissions, then run coop doctor again.")
		})
		assertApprovedOutput(t, "18m-doctor-fixture-failed", out)
	})
}
