package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jack-work/goop/auth"
	"github.com/jack-work/goop/loopapi"
	"github.com/jack-work/msauth"
)

// ---- fatal error rendering and the foundation's hint ----

// authFailure builds the error a user actually sees: goop's own *auth.Error
// wrapping a foundation *msauth.AuthError, so these tests exercise the same
// errors.As path the real error path uses.
func authFailure(attempts ...msauth.Attempt) error {
	return &auth.Error{
		Scope: loopapi.SubstrateScope,
		Auth: &msauth.AuthError{
			Code:     msauth.CodeAcquisitionFailed,
			Message:  "no configured credential source could satisfy the token request",
			Attempts: attempts,
		},
	}
}

// TestFatalPrintsTheFoundationHint pins the contract that goop tells a user
// what to do about a fault they cannot diagnose alone. The remedies are
// msauth's; goop must never map a code to text itself, so each want below is
// compared against msauth.Hint rather than hard-coded twice.
func TestFatalPrintsTheFoundationHint(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			"broker assembly mismatch",
			authFailure(msauth.Attempt{Source: "wam", Code: msauth.CodeBrokerAssemblyMismatch, Message: "PublicKeyToken=31bf3856ad364e35"}),
			"Run: msauth doctor",
		},
		{
			"cold azure cli",
			authFailure(msauth.Attempt{Source: "azure-cli", Code: msauth.CodeAzureCLIFailed, Message: "Azure CLI returned an empty token"}),
			"Run: az login",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			if code := fatal(&stderr, tc.err); code != 1 {
				t.Errorf("exit code = %d, want 1", code)
			}
			if got := msauth.Hint(tc.err); got != tc.want {
				t.Fatalf("the foundation's own hint = %q, want %q; this test is measuring the wrong thing", got, tc.want)
			}
			lines := strings.Split(strings.TrimSuffix(stderr.String(), "\n"), "\n")
			if lines[len(lines)-1] != tc.want {
				t.Errorf("last line = %q, want the hint %q\nfull output:\n%s", lines[len(lines)-1], tc.want, stderr.String())
			}
			if !strings.Contains(stderr.String(), string(msauth.CodeAcquisitionFailed)) {
				t.Errorf("the hint replaced the diagnostic instead of following it:\n%s", stderr.String())
			}
		})
	}
}

// TestFatalInventsNoHint is the assertion that stops a future edit from adding
// a default remedy. A failure whose codes imply no action must print the
// diagnostic and nothing else -- a hint that is wrong sends a signed-in user to
// re-run a login that was never the problem.
func TestFatalInventsNoHint(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"auth failure with no remedy", authFailure(msauth.Attempt{Source: "wam", Code: msauth.CodeWAMFailed, Message: "broker command failed"})},
		{"not an auth error at all", errors.New("context deadline exceeded")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := msauth.Hint(tc.err); got != "" {
				t.Fatalf("the foundation names a remedy %q for this case; the test is measuring the wrong thing", got)
			}
			var stderr bytes.Buffer
			fatal(&stderr, tc.err)
			want := fmt.Sprintf("\x1b[31merror:\x1b[0m %v\n", tc.err)
			if stderr.String() != want {
				t.Errorf("fatal added a line it could not justify:\n got %q\nwant %q", stderr.String(), want)
			}
		})
	}
}

// ---- whoami claim rendering ----

// fixedToken is a loopapi.TokenSource that hands back one synthetic,
// unsigned string. No broker, no network, no cache.
type fixedToken struct {
	tok    string
	scopes []string
}

func (f *fixedToken) Token(_ context.Context, scope string) (string, error) {
	f.scopes = append(f.scopes, scope)
	return f.tok, nil
}

// TestWhoamiRendersEveryClaimThroughTheFoundation fails if a private JWT
// decoder returns to goop. The payload is base64url WITH padding and carries
// no "upn", only "unique_name" -- the exact pair goop's deleted decoders got
// wrong, since they called base64.RawURLEncoding directly and would decode
// nothing at all here. The whole four-line output is compared, so a dropped or
// added line fails too.
func TestWhoamiRendersEveryClaimThroughTheFoundation(t *testing.T) {
	payload := []byte(`{"unique_name":"legacy@example.invalid","name":"Legacy Tester",` +
		`"tid":"72f988bf-86f1-41af-91ab-2d7cd011db47","appid":"d3590ed6-52b3-4102-aeff-aad2292ab01c",` +
		`"app_displayname":"Microsoft Office"}`)
	// JSON tolerates trailing whitespace, so pad the payload until base64url
	// encoding actually emits "=". The test must EXERCISE padding, not hope this
	// particular claim set happens to have a length that produces it.
	for len(payload)%3 == 0 {
		payload = append(payload, ' ')
	}
	padded := "eyJhbGciOiJub25lIn0." + base64.URLEncoding.EncodeToString(payload) + ".notasignature"
	if !strings.Contains(padded, "=") {
		t.Fatalf("payload encoded without padding; the test proves nothing: %s", padded)
	}

	source := &fixedToken{tok: padded}
	var stdout bytes.Buffer
	if err := cmdWhoami(context.Background(), source, &stdout); err != nil {
		t.Fatalf("cmdWhoami: %v", err)
	}

	want := "signed in as \x1b[1mlegacy@example.invalid\x1b[0m\n" +
		"  name:   Legacy Tester\n" +
		"  tenant: 72f988bf-86f1-41af-91ab-2d7cd011db47\n" +
		"  appid:  d3590ed6-52b3-4102-aeff-aad2292ab01c (Microsoft Office)\n"
	if stdout.String() != want {
		t.Errorf("whoami output =\n%q\nwant\n%q", stdout.String(), want)
	}
	if len(source.scopes) != 1 || source.scopes[0] != loopapi.SubstrateScope {
		t.Errorf("scopes = %v, want one Substrate scope", source.scopes)
	}
}

// A token that is not a decodable JWT must still print four lines with blank
// values, which is what the deleted map-based decoder did. Printing fewer
// lines, or panicking, would be a regression dressed as a cleanup.
func TestWhoamiKeepsItsShapeForANonJWT(t *testing.T) {
	var stdout bytes.Buffer
	if err := cmdWhoami(context.Background(), &fixedToken{tok: "not-a-jwt"}, &stdout); err != nil {
		t.Fatalf("cmdWhoami: %v", err)
	}
	want := "signed in as \x1b[1m\x1b[0m\n  name:   \n  tenant: \n  appid:   ()\n"
	if stdout.String() != want {
		t.Errorf("whoami output =\n%q\nwant\n%q", stdout.String(), want)
	}
}

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
	// A version that names no code is a failure, not a display quirk: it is
	// exactly the state in which nobody can tell a fixed foundation from a
	// vulnerable one.
	for _, forbidden := range []string{"", "(devel)", "unknown"} {
		if got.Msauth == forbidden {
			t.Errorf("msauth field = %q, which identifies no code", got.Msauth)
		}
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
}
