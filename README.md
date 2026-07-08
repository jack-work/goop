# goop — a Microsoft Loop CLI

`loop` gives you command-line **read** access to Microsoft Loop workspaces
("loops") and pages: list them, search them, see who they are shared with, and
print a page's text content. It was reverse-engineered from the
`loop.cloud.microsoft` web app (see [docs/REVERSE-ENGINEERING.md](docs/REVERSE-ENGINEERING.md)).

Before this, there was **no** programmatic access to your loops. Now there is.

```
loop list [--top N] [--all]        list your workspaces (most-recent first)
loop search <query> [--pages]      search workspaces by title (and page names)
loop pages <workspace>             list the pages in a workspace
loop read  <workspace> [page]      print a page's text content
loop members <workspace>           list who a workspace is shared with
loop whoami                        show the signed-in identity
```

`<workspace>` and `[page]` are case-insensitive substrings of the title/name.
Add `--json` to any command for machine-readable output, `-v` for auth logging.

## Examples

```pwsh
loop list --all
loop search orchard
loop pages "App Studio"
loop read "App Studio" "Onboarding"
loop members "App Studio"
loop read "App Studio" "AI Research" --json | ConvertFrom-Json
```

## How it works (short version)

Loop is a shell over two Microsoft backends:

| What | Where | Endpoint |
|------|-------|----------|
| Discover your loops | **Substrate** (`substrate.office.com`) | `/recommended/api/v1.1/loop/recent`, `/deltasync`, `/workspaces/{pod}/permissions` |
| Store loop content | **SharePoint Embedded** (`*.sharepoint*.com`) | `/_api/v2.0/drives/{drive}/…/children`, `/_api/v2.1/drives/{drive}/items/{item}/opStream/snapshots/trees/latest` |

Each workspace is a SharePoint Embedded **container** (a drive). Each page is a
`.loop` file, which is a **Fluid Framework** document. Page text is reconstructed
from the Fluid merge-tree (`segmentTexts`) inside the snapshot — see
[`loopapi/fluid.go`](loopapi/fluid.go).

### Auth

Tokens are acquired via the **Windows WAM broker** (MSAL.NET, driven by a small
PowerShell script) using the first-party **Microsoft Office** public client
(`d3590ed6-52b3-4102-aeff-aad2292ab01c`), which is broker-registered and trusted
by Substrate/SharePoint. It falls back to the **Azure CLI** (`az account
get-access-token`). Per-resource tokens are cached on disk until shortly before
expiry. See [`msauth/`](msauth) and
[RECOMMENDATIONS.md](RECOMMENDATIONS.md) for the proposal to promote `msauth`
into a shared library used by tomb/jacques/icy too.

Two headers matter for content reads:

- `Authorization: Bearer <SharePoint token for the workspace's host>`
- `X-CLP-Compliant-App: true` — asserts the client honors sensitivity-label /
  DLP policy. **Without it SharePoint returns `403 Insufficient permissions on
  file`.**

## Build / install

```pwsh
go build -o loop.exe .
# or
./install.ps1     # builds and copies loop.exe onto your PATH
```

Requirements: Go 1.24+, Windows with the WAM broker (or `az login`), and the
`Az.Accounts` PowerShell module (ships the MSAL assemblies the broker script
loads — the same dependency tomb uses).

## Limitations / not-yet

- **Read-focused.** No create/edit yet (write is a possible next step: Fluid ops
  through the delta service, or SharePoint file APIs for whole-file operations).
- Substrate `recent` caps at 30 items; `--all` merges `recent` + `deltasync` to
  widen coverage, but there is no full historical pagination yet.
- Fluid text extraction recovers paragraph/list text well; it does not (yet)
  render tables, embedded components, or comments as structured output.
