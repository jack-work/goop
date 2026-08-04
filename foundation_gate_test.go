package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/jack-work/msauth"
)

// ---- delivery gate ----
//
// goop links the auth foundation as a Go library, which means the binary a user
// runs contains a FROZEN copy of it. A foundation fix reaches goop only when
// somebody rebuilds and reinstalls, and nothing ever checked that anybody had.
// The foundation's convergence was declared five times over six days and
// reached exactly one of its five clients: the one that execs msauth.exe rather
// than linking it.
//
// This is goop's half of the check. The judgement itself belongs to msauth --
// reading build metadata, ordering revisions, deciding what a "+dirty" stamp
// means, wording the remedy -- because four clients writing their own would
// answer one question four ways, which is exactly how four private -X stamp
// variables came to exist. goop contributes two things: the floor in
// .msauth-floor, and the path of the artifact it actually ships.
//
// THE ARTIFACT IS READ, NOT RUN. Asking a binary its version by executing it is
// the wrong instrument twice: it can trigger a real credential acquisition, and
// a build too broken to start is the one whose provenance you most need.

// installedArtifact names the binary goop ships. goop's artifact lives in the
// repository root rather than on PATH, so the default is derived from the test's
// working directory and no committed path names a user or a machine.
func installedArtifact() string {
	if override := os.Getenv("GOOP_ARTIFACT"); override != "" {
		return override
	}
	name := "loop"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(".", name)
}

func TestInstalledArtifactCarriesTheRequiredFoundation(t *testing.T) {
	artifact := installedArtifact()
	if _, err := os.Stat(artifact); err != nil {
		// A SKIP, not a pass: on a fresh clone nothing is installed yet, and
		// "there is no artifact" is a different answer from "the artifact is
		// current". Note the asymmetry -- an artifact that EXISTS is judged
		// strictly below, including for being unreadable, so a path typo in an
		// override still fails rather than quietly skipping.
		t.Skipf("no artifact at %s: build one first (go build -o %s .), then this gate can judge it", artifact, artifact)
	}

	gate, err := msauth.LoadGate(".", artifact)
	if err != nil {
		t.Fatalf("delivery gate: %v", err)
	}
	report, err := gate.Audit()
	if err != nil {
		t.Fatalf("delivery gate: %v", err)
	}
	if !report.OK {
		t.Fatal(gate.Explain(report))
	}
	t.Logf("%s carries foundation %s (floor %s)", artifact, report.Artifacts[0].FoundationVersion, gate.Floor)
}

// TestGateRejectsAPreConvergenceArtifact is the negative control. A gate that
// has never been observed failing is indistinguishable from a gate that cannot
// fail, and the gates this estate proposed before this one all passed on a
// binary that was five foundation commits stale.
//
// The stale artifact is supplied by the environment because a path to one names
// a machine, and this repository is public.
func TestGateRejectsAPreConvergenceArtifact(t *testing.T) {
	stale := os.Getenv("GOOP_GATE_STALE_ARTIFACT")
	if stale == "" {
		t.Skip("set GOOP_GATE_STALE_ARTIFACT to a known-old goop binary to exercise the gate's failing path")
	}
	gate, err := msauth.LoadGate(".", stale)
	if err != nil {
		t.Fatalf("delivery gate: %v", err)
	}
	report, err := gate.Audit()
	if err != nil {
		t.Fatalf("delivery gate: %v", err)
	}
	if report.OK {
		t.Fatalf("the gate passed a known pre-convergence artifact (%s): it cannot detect what it exists to detect", stale)
	}
	if explanation := gate.Explain(report); explanation == "" {
		t.Error("a failing gate must explain itself; an unexplained failure gets rebuilt by guesswork")
	} else {
		t.Logf("gate correctly rejected %s:\n%s", stale, explanation)
	}
}
