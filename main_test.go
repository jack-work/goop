package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jack-work/msauth"
)

// TestExitCodeContract pins the CLI contract: 0 for help, 1 for a runtime
// error, 2 for a usage error. Every case below is hermetic: it fails before any
// token acquisition or network call.
func TestExitCodeContract(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"help", []string{"help"}, 0},
		{"help short flag", []string{"-h"}, 0},
		{"help long flag", []string{"--help"}, 0},
		{"no command", nil, 2},
		{"unknown command", []string{"slurp"}, 2},
		{"read without arguments", []string{"read"}, 1},
		{"pages without arguments", []string{"pages"}, 1},
		{"search without arguments", []string{"search"}, 1},
		{"members without arguments", []string{"members"}, 1},
		{"cache-search without arguments", []string{"cache-search"}, 1},
		{"cache-read without arguments", []string{"cache-read", "only-one"}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := run(tc.args, &stdout, &stderr); got != tc.want {
				t.Errorf("run(%q) = %d, want %d (stderr: %s)", tc.args, got, tc.want, stderr.String())
			}
		})
	}
}

func TestHelpPrintsUsageToStderr(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("help exit code = %d, want 0", code)
	}
	if !strings.Contains(stderr.String(), "loop - read Microsoft Loop from the CLI") {
		t.Errorf("usage banner missing from stderr: %s", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("help wrote to stdout: %s", stdout.String())
	}
}

func TestUnknownCommandNamesTheCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"slurp"}, &stdout, &stderr); code != 2 {
		t.Fatalf("unknown command exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), `unknown command "slurp"`) {
		t.Errorf("stderr did not name the command: %s", stderr.String())
	}
}

func TestRuntimeErrorIsReportedOnStderr(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"read"}, &stdout, &stderr); code != 1 {
		t.Fatalf("runtime error exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "usage: loop read") {
		t.Errorf("stderr lacks the command usage hint: %s", stderr.String())
	}
}

// TestVersionReportsLinkedMsauthVersion pins the auditable-linkage contract:
// msauth is compiled into this binary, so the binary must be able to say which
// foundation it carries without authenticating. A build that cannot answer is
// indistinguishable from a stale one.
func TestVersionReportsLinkedMsauthVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"version", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("version --json exit code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	var got buildInfo
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("version --json emitted invalid JSON (%v): %s", err, stdout.String())
	}
	if got.Msauth == "" {
		t.Errorf("msauth field is empty: %s", stdout.String())
	}
	if got.Msauth != msauth.Version() {
		t.Errorf("msauth field = %q, want the linked module version %q", got.Msauth, msauth.Version())
	}
	if got.Tool != "goop" {
		t.Errorf("tool = %q, want goop", got.Tool)
	}
}

func TestVersionHumanFormNamesMsauthAndDoesNotAuthenticate(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("version exit code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"goop", "msauth", msauth.Version()} {
		if !strings.Contains(out, want) {
			t.Errorf("version output missing %q:\n%s", want, out)
		}
	}
	if stderr.Len() != 0 {
		t.Errorf("version wrote to stderr: %s", stderr.String())
	}
	if replace := versionInfo().MsauthReplace; replace != "" && !strings.Contains(out, replace) {
		t.Errorf("version output hides the filesystem replace %q:\n%s", replace, out)
	}
}
