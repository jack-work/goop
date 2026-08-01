package loopapi

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jack-work/msauth"
)

// ---- hermetic seams: no network, no broker, no live token ----

// fakeTokens records the scope requested by each call site and returns a
// synthetic, non-secret JWT-shaped string.
type fakeTokens struct {
	scopes []string
	err    error
	token  string // overrides fakeJWT when set
}

func (f *fakeTokens) Token(_ context.Context, scope string) (string, error) {
	f.scopes = append(f.scopes, scope)
	if f.err != nil {
		return "", f.err
	}
	if f.token != "" {
		return f.token, nil
	}
	return fakeJWT, nil
}

// fakeJWT carries a upn claim so anchor-mailbox behavior is exercised. It is a
// hand-built unsigned string, not a credential.
var fakeJWT = "eyJhbGciOiJub25lIn0." +
	base64.RawURLEncoding.EncodeToString([]byte(`{"upn":"tester@example.invalid"}`)) +
	".notasignature"

// rewriteTransport sends every request to the test server while recording the
// URL and headers goop actually produced.
type rewriteTransport struct {
	target *url.URL
	seen   []*http.Request
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.seen = append(t.seen, req.Clone(req.Context()))
	routed := req.Clone(req.Context())
	routed.URL.Scheme = t.target.Scheme
	routed.URL.Host = t.target.Host
	routed.Host = ""
	return http.DefaultTransport.RoundTrip(routed)
}

func newTestClient(t *testing.T, handler http.Handler) (*Client, *fakeTokens, *rewriteTransport) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	transport := &rewriteTransport{target: target}
	tokens := &fakeTokens{}
	client := New(tokens)
	client.http = &http.Client{Transport: transport}
	return client, tokens, transport
}

const testHost = "microsoft.sharepoint-df.com"

func testWorkspace() Workspace {
	var w Workspace
	w.ID = "SPO_test"
	w.Title = "Test Loop"
	w.MfsInfo.PodID = base64.StdEncoding.EncodeToString(
		[]byte("ODSP|" + testHost + "|b!driveid|01ITEMID"))
	return w
}

// ---- scope selectors per call site ----

func TestSubstrateCallSitesRequestTheSubstrateScope(t *testing.T) {
	client, tokens, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"workspaces":[],"permissions":[]}`)
	}))
	ctx := context.Background()

	if _, err := client.Recent(ctx, 10); err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if _, err := client.DeltaSync(ctx); err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if _, err := client.Members(ctx, testWorkspace()); err != nil {
		t.Fatalf("Members: %v", err)
	}

	want := []string{SubstrateScope, SubstrateScope, SubstrateScope}
	if strings.Join(tokens.scopes, ",") != strings.Join(want, ",") {
		t.Errorf("scopes = %v, want %v", tokens.scopes, want)
	}
	if SubstrateScope != "https://substrate.office.com/.default" {
		t.Errorf("SubstrateScope = %q", SubstrateScope)
	}
}

func TestSharePointCallSitesRequestThePerHostScope(t *testing.T) {
	client, tokens, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/children") {
			fmt.Fprint(w, `{"value":[]}`)
			return
		}
		fmt.Fprint(w, `{"snapshot":true}`)
	}))
	ctx := context.Background()
	workspace := testWorkspace()

	if _, err := client.ListPages(ctx, workspace); err != nil {
		t.Fatalf("ListPages: %v", err)
	}
	if _, err := client.PageSnapshot(ctx, workspace, Page{ID: "01PAGE"}); err != nil {
		t.Fatalf("PageSnapshot: %v", err)
	}

	want := "https://" + testHost + "/.default"
	if len(tokens.scopes) != 2 {
		t.Fatalf("acquired %d tokens, want 2 (one per resource call site)", len(tokens.scopes))
	}
	for _, got := range tokens.scopes {
		if got != want {
			t.Errorf("scope = %q, want %q", got, want)
		}
	}
	if SharePointScope(testHost) != want {
		t.Errorf("SharePointScope(%q) = %q", testHost, SharePointScope(testHost))
	}
}

func TestTokenFailurePropagatesFromEveryCallSite(t *testing.T) {
	client, tokens, transport := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("no HTTP request should be made when acquisition fails")
		w.WriteHeader(500)
	}))
	tokens.err = fmt.Errorf("acquisition_failed: wam and azure-cli both failed")
	ctx := context.Background()
	workspace := testWorkspace()

	for name, call := range map[string]func() error{
		"Recent":       func() error { _, err := client.Recent(ctx, 5); return err },
		"Members":      func() error { _, err := client.Members(ctx, workspace); return err },
		"ListPages":    func() error { _, err := client.ListPages(ctx, workspace); return err },
		"PageSnapshot": func() error { _, err := client.PageSnapshot(ctx, workspace, Page{ID: "x"}); return err },
	} {
		if err := call(); err == nil || !strings.Contains(err.Error(), "acquisition_failed") {
			t.Errorf("%s: err = %v, want the acquisition failure", name, err)
		}
	}
	if len(transport.seen) != 0 {
		t.Errorf("made %d HTTP requests despite failed acquisition", len(transport.seen))
	}
}

// ---- tool-owned adapters that must survive the migration ----

func TestSubstrateRequestCarriesLoopHeaders(t *testing.T) {
	client, _, transport := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"workspaces":[]}`)
	}))
	if _, err := client.Recent(context.Background(), 10); err != nil {
		t.Fatalf("Recent: %v", err)
	}
	req := transport.seen[0]
	if got := req.URL.Host; got != "substrate.office.com" {
		t.Errorf("host = %q, want substrate.office.com", got)
	}
	for header, want := range map[string]string{
		"Origin":               "https://loop.cloud.microsoft",
		"X-Office-Application": "300",
		"X-Office-Platform":    "Web",
		"X-Anchormailbox":      "UPN:tester@example.invalid",
		"Authorization":        "Bearer " + fakeJWT,
	} {
		if got := req.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}

func TestPageSnapshotRequiresCLPCompliantAppHeader(t *testing.T) {
	client, _, transport := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// SharePoint returns 403 for content reads without this assertion,
		// regardless of token validity.
		if r.Header.Get("X-CLP-Compliant-App") != "true" {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"error":"Insufficient permissions on file."}`)
			return
		}
		fmt.Fprint(w, `{"snapshot":"ok"}`)
	}))

	body, err := client.PageSnapshot(context.Background(), testWorkspace(), Page{ID: "01PAGE"})
	if err != nil {
		t.Fatalf("PageSnapshot: %v", err)
	}
	if string(body) != `{"snapshot":"ok"}` {
		t.Errorf("snapshot body = %q", body)
	}
	req := transport.seen[0]
	if got := req.Header.Get("X-CLP-Compliant-App"); got != "true" {
		t.Errorf("X-CLP-Compliant-App = %q, want true", got)
	}
	if !strings.HasSuffix(req.URL.Path, "/opStream/snapshots/trees/latest") {
		t.Errorf("snapshot path = %q", req.URL.Path)
	}
	if got := req.URL.Host; got != testHost {
		t.Errorf("host = %q, want the workspace pod host %q", got, testHost)
	}
}

func TestListPagesWalksLoopAppDataAndPaginates(t *testing.T) {
	var serverURL string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/root/children"):
			fmt.Fprint(w, `{"value":[{"id":"F1","name":"LoopAppData","folder":{}}]}`)
		case r.URL.Query().Get("page") == "2":
			fmt.Fprint(w, `{"value":[{"id":"P2","name":"Second.loop"}]}`)
		default:
			fmt.Fprintf(w, `{"value":[{"id":"P1","name":"First.loop"},{"id":"D1","name":"skip.docx"}],"@odata.nextLink":%q}`,
				serverURL+"/next?page=2")
		}
	})
	client, _, _ := newTestClient(t, handler)
	// The nextLink the service emits is absolute; the rewrite transport routes
	// it back to the same test server.
	serverURL = "https://" + testHost

	pages, err := client.ListPages(context.Background(), testWorkspace())
	if err != nil {
		t.Fatalf("ListPages: %v", err)
	}
	var names []string
	for _, p := range pages {
		names = append(names, p.Title())
	}
	if strings.Join(names, ",") != "First,Second" {
		t.Errorf("pages = %v, want the .loop files across both pages", names)
	}
}

// ---- redaction and truncation of quoted service bodies ----

const leakyBody = `{"error":"denied","access_token":"aaaaaaaaaaaaaaaa.bbbbbbbbbbbbbbbb.cccccccccccc",` +
	`"authorization":"Bearer aaaaaaaaaaaaaaaa.bbbbbbbbbbbbbbbb.cccccccccccc"}`

func TestDoJSONErrorRedactsAndTruncatesAt240(t *testing.T) {
	padding := strings.Repeat("x", 600)
	client, _, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, leakyBody+padding)
	}))

	_, err := client.Recent(context.Background(), 10)
	if err == nil {
		t.Fatal("Recent succeeded, want HTTP 400 error")
	}
	assertRedacted(t, err.Error())
	assertQuotedBodyLimit(t, err.Error(), "HTTP 400: ", 240)
}

func TestPageSnapshotErrorRedactsAndTruncatesAt200(t *testing.T) {
	padding := strings.Repeat("x", 600)
	client, _, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, leakyBody+padding)
	}))

	_, err := client.PageSnapshot(context.Background(), testWorkspace(), Page{ID: "01PAGE"})
	if err == nil {
		t.Fatal("PageSnapshot succeeded, want HTTP 403 error")
	}
	assertRedacted(t, err.Error())
	assertQuotedBodyLimit(t, err.Error(), "HTTP 403: ", 200)
}

func assertRedacted(t *testing.T, message string) {
	t.Helper()
	if strings.Contains(message, "aaaaaaaaaaaaaaaa.bbbbbbbbbbbbbbbb.cccccccccccc") {
		t.Errorf("error quoted credential material:\n%s", message)
	}
	if !strings.Contains(message, "[REDACTED]") {
		t.Errorf("error lacks a redaction marker:\n%s", message)
	}
}

func assertQuotedBodyLimit(t *testing.T, message, marker string, limit int) {
	t.Helper()
	index := strings.Index(message, marker)
	if index < 0 {
		t.Fatalf("error message lacks %q:\n%s", marker, message)
	}
	quoted := message[index+len(marker):]
	if !strings.HasSuffix(quoted, "…") {
		t.Errorf("long body was not truncated:\n%s", quoted)
	}
	if got := len(quoted) - len("…"); got != limit {
		t.Errorf("quoted body = %d bytes, want %d", got, limit)
	}
}

func TestSanitizeLeavesOrdinaryDiagnosticsIntact(t *testing.T) {
	const message = `{"error":{"code":"accessDenied","message":"Insufficient permissions on file."}}`
	if got := msauth.SanitizeDiagnostic(message); got != message {
		t.Errorf("the foundation sanitizer mangled a clean body:\n%s", got)
	}
}

// The next three tests fail if goop grows a private redactor or JWT decoder
// again. Each asserts behavior only the foundation's implementation has:
// goop's deleted jsonTokenPattern had no leading word boundary and therefore
// destroyed an assembly PublicKeyToken, and goop's deleted upnFromJWT called
// base64.RawURLEncoding directly and therefore returned nothing for a padded
// payload. A source-identical copy of either would fail here.

const assemblyBody = `{"error":"denied","id_token":"opaqueidtokenvalue",` +
	`"detail":"Assembly Microsoft.Identity.Client, PublicKeyToken=31bf3856ad364e35 not found"}`

func TestQuotedBodyRedactsOpaqueIDTokenAndKeepsAssemblyIdentity(t *testing.T) {
	client, _, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, assemblyBody)
	}))

	_, err := client.Recent(context.Background(), 10)
	if err == nil {
		t.Fatal("Recent succeeded, want HTTP 400 error")
	}
	message := err.Error()
	// The value is deliberately not JWT-shaped, so only the field-name pattern
	// can remove it.
	if strings.Contains(message, "opaqueidtokenvalue") {
		t.Errorf("an id_token value survived the diagnostic:\n%s", message)
	}
	// An assembly public key token is not a secret; it is the evidence a broker
	// diagnostic exists to carry.
	if !strings.Contains(message, "PublicKeyToken=31bf3856ad364e35") {
		t.Errorf("the assembly identity was destroyed:\n%s", message)
	}
}

func TestAnchorMailboxFallsBackToUniqueNameThroughAPaddedPayload(t *testing.T) {
	payload := []byte(`{"unique_name":"legacy@example.invalid","oid":"00000000-0000-0000-0000-000000000001"}`)
	padded := "eyJhbGciOiJub25lIn0." + base64.URLEncoding.EncodeToString(payload) + ".notasignature"
	if !strings.Contains(padded, "=") {
		t.Fatalf("payload encoded without padding; the test proves nothing: %s", padded)
	}

	client, tokens, transport := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"workspaces":[]}`)
	}))
	tokens.token = padded

	if _, err := client.Recent(context.Background(), 10); err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(transport.seen) != 1 {
		t.Fatalf("requests = %d, want 1", len(transport.seen))
	}
	if got := transport.seen[0].Header.Get("X-AnchorMailbox"); got != "UPN:legacy@example.invalid" {
		t.Errorf("anchor mailbox = %q, want the unique_name claim", got)
	}
}

func TestQuotedBodyNamesAnEmptyBodyRatherThanBlamingACredential(t *testing.T) {
	client, _, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))

	_, err := client.Recent(context.Background(), 10)
	if err == nil {
		t.Fatal("Recent succeeded, want HTTP 429 error")
	}
	if !strings.Contains(err.Error(), "(empty body)") {
		t.Errorf("empty body was not named:\n%s", err)
	}
	if strings.Contains(err.Error(), "credential source failed") {
		t.Errorf("an omitted response body was reported as a credential failure:\n%s", err)
	}
}

// ---- pod decoding ----

func TestPodDecodesHostDriveAndItem(t *testing.T) {
	pod, err := testWorkspace().Pod()
	if err != nil {
		t.Fatalf("Pod: %v", err)
	}
	if pod.Host != testHost || pod.DriveID != "b!driveid" || pod.ItemID != "01ITEMID" {
		t.Errorf("pod = %+v", pod)
	}
	var bad Workspace
	bad.MfsInfo.PodID = base64.StdEncoding.EncodeToString([]byte("NOTODSP|a|b|c"))
	if _, err := bad.Pod(); err == nil {
		t.Error("Pod accepted a non-ODSP pod id")
	}
}
