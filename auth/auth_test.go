package auth

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jack-work/msauth"
)

// stub replaces the foundation acquisition seam so tests never touch the
// broker, the Azure CLI, or the protected token cache.
func stub(t *testing.T, result msauth.TokenResult, err error) (*Provider, *[]msauth.TokenRequest) {
	t.Helper()
	var seen []msauth.TokenRequest
	p := &Provider{
		acquire: func(_ context.Context, request msauth.TokenRequest) (msauth.TokenResult, error) {
			seen = append(seen, request)
			return result, err
		},
	}
	return p, &seen
}

func TestNewBindsOfficeClientAndCorporateTenant(t *testing.T) {
	p, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.provider.ClientID != msauth.OfficeClientID {
		t.Errorf("client id = %q, want the Office client %q", p.provider.ClientID, msauth.OfficeClientID)
	}
	if p.provider.TenantID != TenantID {
		t.Errorf("tenant = %q, want %q", p.provider.TenantID, TenantID)
	}
	if ClientName != "office" {
		t.Errorf("client name = %q, want office", ClientName)
	}
}

func TestTokenRequestsOneScopeWithWAMFirst(t *testing.T) {
	p, seen := stub(t, msauth.TokenResult{AccessToken: "fake-token", Source: "wam"}, nil)
	got, err := p.Token(context.Background(), "https://substrate.office.com/.default")
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if got != "fake-token" {
		t.Errorf("token = %q, want the acquired token", got)
	}
	if len(*seen) != 1 {
		t.Fatalf("acquire called %d times, want 1", len(*seen))
	}
	request := (*seen)[0]
	if request.Scope != "https://substrate.office.com/.default" {
		t.Errorf("scope = %q", request.Scope)
	}
	if request.Audience != "" {
		t.Errorf("audience = %q, want empty (goop always sends an explicit scope)", request.Audience)
	}
	if request.Policy != msauth.PolicyWAMFirst {
		t.Errorf("policy = %q, want %q", request.Policy, msauth.PolicyWAMFirst)
	}
	if request.ForceRefresh {
		t.Error("ForceRefresh = true, want false")
	}
}

func TestTokenErrorNamesBothCredentialSources(t *testing.T) {
	failure := &msauth.AuthError{
		Code:    msauth.CodeAcquisitionFailed,
		Message: "no configured credential source could satisfy the token request",
		Attempts: []msauth.Attempt{
			{Source: "wam", Code: msauth.CodeWAMFailed, Message: "broker command failed"},
			{Source: "azure-cli", Code: msauth.CodeAzureCLIFailed, Message: "Azure CLI returned an empty token"},
		},
	}
	p, _ := stub(t, msauth.TokenResult{}, failure)

	_, err := p.Token(context.Background(), "https://microsoft.sharepoint-df.com/.default")
	if err == nil {
		t.Fatal("Token succeeded, want failure")
	}
	message := err.Error()
	for _, want := range []string{
		"https://microsoft.sharepoint-df.com/.default",
		string(msauth.CodeAcquisitionFailed),
		"wam", string(msauth.CodeWAMFailed), "broker command failed",
		"azure-cli", string(msauth.CodeAzureCLIFailed), "Azure CLI returned an empty token",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("error message missing %q:\n%s", want, message)
		}
	}

	var authErr *msauth.AuthError
	if !errors.As(err, &authErr) {
		t.Fatal("errors.As did not reach *msauth.AuthError")
	}
	if authErr.Code != msauth.CodeAcquisitionFailed || len(authErr.Attempts) != 2 {
		t.Errorf("unwrapped error = %+v", authErr)
	}
}

// TestTokenErrorUsesTheFoundationLayout fails if goop builds the diagnostic
// itself again: the hand-rolled renderer printed "scope: code: message" with no
// brackets around the top-level code, so every tool on the foundation printed a
// different shape for the same failure.
func TestTokenErrorUsesTheFoundationLayout(t *testing.T) {
	failure := &msauth.AuthError{
		Code:    msauth.CodeAcquisitionFailed,
		Message: "no configured credential source could satisfy the token request",
		Attempts: []msauth.Attempt{
			{Source: "wam", Code: msauth.CodeBrokerAssemblyMismatch, Message: "PublicKeyToken=31bf3856ad364e35"},
		},
	}
	p, _ := stub(t, msauth.TokenResult{}, failure)

	_, err := p.Token(context.Background(), "https://substrate.office.com/.default")
	if err == nil {
		t.Fatal("Token succeeded, want failure")
	}
	want := msauth.FormatError("acquire token for https://substrate.office.com/.default", failure)
	if err.Error() != want {
		t.Errorf("rendering diverged from the foundation:\n got: %s\nwant: %s", err, want)
	}
	const literal = "acquire token for https://substrate.office.com/.default [acquisition_failed]: " +
		"no configured credential source could satisfy the token request\n" +
		"  wam [broker_assembly_mismatch]: PublicKeyToken=31bf3856ad364e35"
	if err.Error() != literal {
		t.Errorf("layout = %q, want %q", err.Error(), literal)
	}
}

func TestTokenWrapsNonFoundationError(t *testing.T) {
	sentinel := errors.New("context deadline exceeded")
	p, _ := stub(t, msauth.TokenResult{}, sentinel)

	_, err := p.Token(context.Background(), "https://substrate.office.com/.default")
	if !errors.Is(err, sentinel) {
		t.Fatalf("errors.Is lost the cause: %v", err)
	}
	if !strings.Contains(err.Error(), "https://substrate.office.com/.default") {
		t.Errorf("error message lacks the scope: %v", err)
	}
}

func TestVerboseLoggingRevealsSourceButNotToken(t *testing.T) {
	p, _ := stub(t, msauth.TokenResult{
		AccessToken: "header.payload.signature",
		Source:      "azure-cli",
		Cached:      true,
		ExpiresAt:   time.Unix(1900000000, 0),
	}, nil)
	var log bytes.Buffer
	p.Verbose = true
	p.log = &log

	if _, err := p.Token(context.Background(), "https://substrate.office.com/.default"); err != nil {
		t.Fatalf("Token: %v", err)
	}
	if strings.Contains(log.String(), "header.payload.signature") {
		t.Fatal("verbose log leaked the access token")
	}
	for _, want := range []string{"azure-cli", "cached=true", "https://substrate.office.com/.default"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("verbose log missing %q: %s", want, log.String())
		}
	}
}

func TestVerboseOffIsSilent(t *testing.T) {
	p, _ := stub(t, msauth.TokenResult{AccessToken: "fake-token", Source: "wam"}, nil)
	var log bytes.Buffer
	p.log = &log
	if _, err := p.Token(context.Background(), "https://substrate.office.com/.default"); err != nil {
		t.Fatalf("Token: %v", err)
	}
	if log.Len() != 0 {
		t.Errorf("quiet mode wrote %q", log.String())
	}
}
