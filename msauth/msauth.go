// Package msauth acquires Microsoft Entra (AAD) access tokens for CLI tools.
//
// It follows the pattern already used across this user's tools (tomb, jacques,
// icy): prefer the Windows WAM broker for a silent, Conditional-Access-friendly
// token on a managed device, and fall back to the Azure CLI (`az account
// get-access-token`) when the broker is unavailable.
//
// Tokens are cached on disk per (clientID, scope) and reused until shortly
// before expiry so the (slow) PowerShell broker call only runs when needed.
//
// This package is intentionally free of tool-specific logic so it can be
// promoted to a shared module (e.g. github.com/jack-work/msauth) and consumed
// by tomb/jacques/icy as well. See RECOMMENDATIONS.md.
package msauth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// MicrosoftOfficeClientID is the first-party "Microsoft Office" public client.
// It is registered for the Windows broker (native app redirect URI) and is
// broadly trusted by Substrate, SharePoint and Graph, which makes it a good
// default for acquiring delegated tokens silently on a managed device.
const MicrosoftOfficeClientID = "d3590ed6-52b3-4102-aeff-aad2292ab01c"

// MicrosoftTenantID is the Microsoft corporate tenant.
const MicrosoftTenantID = "72f988bf-86f1-41af-91ab-2d7cd011db47"

//go:generate true

// Provider acquires and caches tokens for a single (clientID, tenant).
type Provider struct {
	ClientID string
	TenantID string
	// ScriptPath is the path to wam-token.ps1. If empty, the embedded copy is
	// materialized to the user cache dir on first use.
	ScriptPath string
	// Verbose logs acquisition steps to stderr.
	Verbose bool

	mu    sync.Mutex
	cache map[string]cachedToken // key: scope
}

type cachedToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// New returns a Provider using the Microsoft Office client and corp tenant.
func New() *Provider {
	return &Provider{ClientID: MicrosoftOfficeClientID, TenantID: MicrosoftTenantID}
}

// Token returns a valid bearer token for the given scope, e.g.
// "https://substrate.office.com/.default". It uses an in-memory + on-disk
// cache, then the WAM broker, then the az CLI.
func (p *Provider) Token(ctx context.Context, scope string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cache == nil {
		p.cache = p.loadDiskCache()
	}
	if t, ok := p.cache[scope]; ok && time.Until(t.ExpiresAt) > 2*time.Minute {
		return t.Token, nil
	}

	tok, exp, err := p.acquireBroker(ctx, scope)
	if err != nil {
		p.logf("broker failed for %s: %v; falling back to az cli", scope, err)
		tok, exp, err = p.acquireAz(ctx, scope)
		if err != nil {
			return "", fmt.Errorf("acquire token for %s: broker and az both failed: %w", scope, err)
		}
	}

	p.cache[scope] = cachedToken{Token: tok, ExpiresAt: exp}
	p.saveDiskCache()
	return tok, nil
}

// --- WAM broker (PowerShell + MSAL.NET) ---

type psTokenResult struct {
	AccessToken string `json:"accessToken"`
	ExpiresOn   string `json:"expiresOn"`
}

func (p *Provider) acquireBroker(ctx context.Context, scope string) (string, time.Time, error) {
	script, err := p.ensureScript()
	if err != nil {
		return "", time.Time{}, err
	}
	p.logf("acquiring %s via WAM broker", scope)
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass",
		"-File", script,
		"-ClientId", p.ClientID,
		"-Scope", scope,
		"-TenantId", p.tenant())
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", time.Time{}, fmt.Errorf("%v: %s", err, strings.TrimSpace(stderr.String()))
	}
	out := strings.TrimSpace(stdout.String())
	if out == "" {
		return "", time.Time{}, fmt.Errorf("empty broker output: %s", strings.TrimSpace(stderr.String()))
	}
	var res psTokenResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		return "", time.Time{}, fmt.Errorf("parse broker output: %w", err)
	}
	if res.AccessToken == "" {
		return "", time.Time{}, fmt.Errorf("broker returned no token")
	}
	exp, _ := time.Parse(time.RFC3339, res.ExpiresOn)
	if exp.IsZero() {
		exp = time.Now().Add(50 * time.Minute)
	}
	return res.AccessToken, exp, nil
}

// --- az CLI fallback ---

type azTokenResult struct {
	AccessToken string `json:"accessToken"`
	ExpiresOn   string `json:"expiresOn"`
	Expires_on  int64  `json:"expires_on"`
}

func (p *Provider) acquireAz(ctx context.Context, scope string) (string, time.Time, error) {
	resource := strings.TrimSuffix(scope, "/.default")
	resource = strings.TrimSuffix(resource, "/user_impersonation")
	p.logf("acquiring %s via az cli (resource=%s)", scope, resource)
	cmd := exec.CommandContext(ctx, "az", "account", "get-access-token",
		"--resource", resource, "--output", "json")
	out, err := cmd.Output()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("az account get-access-token: %w (run 'az login')", err)
	}
	var res azTokenResult
	if err := json.Unmarshal(out, &res); err != nil {
		return "", time.Time{}, fmt.Errorf("parse az output: %w", err)
	}
	exp := time.Now().Add(50 * time.Minute)
	if res.Expires_on > 0 {
		exp = time.Unix(res.Expires_on, 0)
	}
	return res.AccessToken, exp, nil
}

// --- script + cache plumbing ---

func (p *Provider) tenant() string {
	if p.TenantID != "" {
		return p.TenantID
	}
	return MicrosoftTenantID
}

func (p *Provider) ensureScript() (string, error) {
	if p.ScriptPath != "" {
		if _, err := os.Stat(p.ScriptPath); err == nil {
			return p.ScriptPath, nil
		}
	}
	// Materialize the embedded script to the user cache dir.
	dir := cacheDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, "wam-token.ps1")
	if err := os.WriteFile(dst, []byte(embeddedScript), 0o644); err != nil {
		return "", err
	}
	p.ScriptPath = dst
	return dst, nil
}

func cacheDir() string {
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "goop")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".goop")
}

func (p *Provider) diskCachePath() string {
	sum := sha256.Sum256([]byte(p.ClientID + "|" + p.tenant()))
	name := "tokens-" + base64.RawURLEncoding.EncodeToString(sum[:6]) + ".json"
	return filepath.Join(cacheDir(), name)
}

func (p *Provider) loadDiskCache() map[string]cachedToken {
	m := map[string]cachedToken{}
	b, err := os.ReadFile(p.diskCachePath())
	if err != nil {
		return m
	}
	_ = json.Unmarshal(b, &m)
	return m
}

func (p *Provider) saveDiskCache() {
	_ = os.MkdirAll(cacheDir(), 0o755)
	b, _ := json.MarshalIndent(p.cache, "", "  ")
	_ = os.WriteFile(p.diskCachePath(), b, 0o600)
}

func (p *Provider) logf(format string, args ...any) {
	if p.Verbose {
		fmt.Fprintf(os.Stderr, "\x1b[38;5;241m○ "+format+"\x1b[0m\n", args...)
	}
}
