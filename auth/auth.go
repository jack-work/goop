// Package auth adapts the shared Microsoft Entra foundation
// (github.com/jack-work/msauth) to goop's per-resource token needs.
//
// goop asks for exactly one resource per request: the Substrate discovery
// resource, or the SharePoint Embedded host that stores a workspace. Broker
// ordering, the Azure CLI fallback, the protected on-disk cache, refresh skew,
// and diagnostic sanitization all belong to the foundation; this package only
// selects the client, tenant, and policy, and renders foundation failures as
// goop-facing diagnostics.
package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jack-work/msauth"
)

// ClientName is the first-party Microsoft Office public client
// (d3590ed6-52b3-4102-aeff-aad2292ab01c), which Substrate and SharePoint
// Embedded both trust for delegated access.
const ClientName = "office"

// TenantID is the Microsoft corporate tenant.
const TenantID = "72f988bf-86f1-41af-91ab-2d7cd011db47"

// Policy is goop's acquisition policy: the Windows broker first, then the
// Azure CLI login.
const Policy = msauth.PolicyWAMFirst

// Provider acquires bearer tokens for goop's two resources.
type Provider struct {
	// Verbose logs non-secret acquisition metadata (scope, source, cache hit,
	// expiry) to the log writer.
	Verbose bool

	provider *msauth.Provider
	acquire  func(context.Context, msauth.TokenRequest) (msauth.TokenResult, error)
	log      io.Writer
}

// New returns a Provider bound to the Office client and the corporate tenant.
func New() (*Provider, error) {
	provider, err := msauth.NewForClient(ClientName)
	if err != nil {
		return nil, err
	}
	provider.TenantID = TenantID
	return &Provider{provider: provider, acquire: provider.Acquire, log: os.Stderr}, nil
}

// Token returns a bearer token for exactly one resource scope, for example
// "https://substrate.office.com/.default" or
// "https://microsoft.sharepoint-df.com/.default".
func (p *Provider) Token(ctx context.Context, scope string) (string, error) {
	result, err := p.acquire(ctx, msauth.TokenRequest{Scope: scope, Policy: Policy})
	if err != nil {
		return "", newError(scope, err)
	}
	p.logf("%s via %s (cached=%t, expires %s)", scope, result.Source, result.Cached,
		result.ExpiresAt.Local().Format(time.RFC3339))
	return result.AccessToken, nil
}

// Error reports a failed acquisition, naming every credential source that was
// tried together with the foundation's stable error codes. The wrapped
// *msauth.AuthError stays reachable through errors.As.
type Error struct {
	Scope string
	Auth  *msauth.AuthError
}

func newError(scope string, err error) error {
	var authErr *msauth.AuthError
	if !errors.As(err, &authErr) {
		return fmt.Errorf("acquire token for %s: %w", scope, err)
	}
	return &Error{Scope: scope, Auth: authErr}
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "acquire token for %s: %s: %s", e.Scope, e.Auth.Code, e.Auth.Message)
	for _, attempt := range e.Auth.Attempts {
		fmt.Fprintf(&b, "\n  %s [%s]: %s", attempt.Source, attempt.Code, attempt.Message)
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Auth }

func (p *Provider) logf(format string, args ...any) {
	if !p.Verbose || p.log == nil {
		return
	}
	fmt.Fprintf(p.log, "\x1b[38;5;241m○ "+format+"\x1b[0m\n", args...)
}
