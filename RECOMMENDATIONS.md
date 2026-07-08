# Recommendation: a shared `msauth` library for CLI tools

## The problem

Four tools in `~/dev` each solve "get me a Microsoft token on this managed
device" slightly differently:

| Tool | Language | How it gets a token |
|------|----------|---------------------|
| **tomb** | Go | WAM broker via `scripts/wam-token.ps1` (Teams client `1fec8e78…`), **fallback** to `az` CLI (`azidentity.NewAzureCLICredential`) |
| **jacques** | Go | `az account get-access-token --resource …` only |
| **icy** | Python | (its own auth path) |
| **goop / loop** (new) | Go | WAM broker via `msauth/wam-token.ps1` (Office client `d3590ed6…`), **fallback** to `az` CLI |

tomb and goop are now **the same pattern with different constants**. That
duplication is the opportunity.

Why the broker matters here specifically: device-code flow does **not** work for
`@microsoft.com` accounts (Conditional Access requires an Intune-managed
device). The WAM broker runs *on* the managed device and satisfies CA silently.
`az` CLI works too but only exposes `user_impersonation`, which some first-party
resources (e.g. Substrate's Loop API) reject — so the broker path with a
first-party client is not just a nicety, it is required for some resources.

## The proposal

Promote `goop/msauth` to a standalone module, e.g.
**`github.com/jack-work/msauth`**, and have tomb (and any new Go tool) depend on
it. Its surface is already small and tool-agnostic:

```go
type Provider struct {
    ClientID string   // default: msauth.MicrosoftOfficeClientID
    TenantID string   // default: msauth.MicrosoftTenantID
    Verbose  bool
}

func New() *Provider
func (p *Provider) Token(ctx context.Context, scope string) (string, error)
```

Behavior: in-memory + on-disk cache per `(clientID, scope)` → **WAM broker** →
**az CLI** fallback. The broker PowerShell script is embedded (`go:embed`) and
materialized to the user cache dir on first use, so consumers ship nothing
extra.

### What each tool would change

- **tomb**: delete `auth.go`'s `getAADTokenWAM` / `getAADTokenAzCLI` and
  `scripts/wam-token.ps1`; replace with
  `msauth.Provider{ClientID: teamsClientID}.Token(ctx, "https://api.spaces.skype.com/.default")`.
  Its Skype-token exchange stays tool-specific.
- **jacques**: swap `azGetToken` for `msauth`. It gains the broker path for free
  (useful when `az` is logged out but the user is signed into Windows), while
  keeping `az` as the fallback it already relies on.
- **goop**: already uses it.
- **icy** (Python): out of scope for a Go module, but could shell out to a tiny
  `msauth token --scope …` CLI built from the same package for a single source
  of truth on client IDs / broker quirks.

### Design notes worth keeping

1. **Client-id registry.** Centralize the known-good first-party public clients
   and which resources they unlock (Office `d3590ed6…` = broad Substrate/SPO;
   Teams `1fec8e78…` = Skype/Substrate; Azure CLI `04b07795…` = ARM/generic).
   This is exactly the knowledge that was expensive to rediscover here.
2. **Per-resource tokens, not one token.** Loop needs Substrate *and* a
   per-host SharePoint token *and* optionally Graph. The `Token(ctx, scope)`
   shape handles that; a single-token API would not.
3. **Cache keyed by `(clientID, tenant, scope)`** with a small expiry skew
   (this impl: refresh when <2 min remain). Disk cache at `os.UserConfigDir()`.
4. **Broker script robustness.** Discover the newest installed `Az.Accounts`
   version rather than hard-coding `5.3.0` (tomb hard-codes it; `msauth`
   globs for the latest).
5. **Fallback ordering is deliberate:** broker first (silent, CA-friendly, gets
   first-party scopes), then `az` (works headless/SSH, but limited scopes).

## Suggested next steps

1. Extract `msauth` to its own repo/module; tag `v0.1.0`.
2. Migrate tomb to it (smallest diff, highest confidence — same pattern).
3. Add a `msauth.Provider.Clients` registry + a `msauth` debug CLI
   (`msauth token --client office --scope https://substrate.office.com/.default`).
4. Opportunistically move jacques over; keep `az`-only behavior available via
   `Provider{ClientID: ""}` → az-only mode if desired.
