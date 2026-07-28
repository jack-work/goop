# goop

**A CLI that oozes into Microsoft Loop and slurps out your pages as text.**

Loop is designed for *highly collaborative human interaction* -- colorful cursors, real-time co-editing, emoji reactions. Goop treats it like a flat-file database. We regret nothing.

```
loop list                          # what loops do I have?
loop pages "App Studio"            # what's in there?
loop read "App Studio" "Onboarding" # give me the text
loop cache-search "orchard api"    # full-text search, instant
loop members "App Studio"          # who's in here?
```

## Why

There is no official Loop API. The community has been [begging for one since 2024](https://learn.microsoft.com/en-us/answers/questions/4427976/looking-for-info-on-api-access-to-content-in-loop). Microsoft has a pre-alpha internal CLI that approximately nobody can use. Meanwhile your team's entire knowledge base lives in Loop and your agents can't read it.

Goop fixes that by reverse-engineering the Loop web app's actual API calls (Substrate discovery + SharePoint Embedded content + Fluid Framework snapshot parsing). Full writeup: [`docs/REVERSE-ENGINEERING.md`](docs/REVERSE-ENGINEERING.md).

## What you get

| Command | What it does | Speed |
|---------|-------------|-------|
| `loop list [--all]` | Your workspaces, most-recent first | ~2s |
| `loop search <q> [--pages]` | Match workspace/page titles | ~2s (or 12s with --pages) |
| `loop pages <workspace>` | Pages in a workspace | ~2s |
| `loop read <ws> [page]` | Full page text content | ~3.5s |
| `loop members <workspace>` | Permissions/sharing list | ~3s |
| `loop whoami` | Signed-in identity | ~1s |
| `loop daemon` | Background sync to SQLite | runs forever |
| `loop sync` | One-shot sync | ~30s per workspace |
| `loop cache-search <q>` | FTS5 full-text search | **~60ms** |
| `loop cache-read <ws> <page>` | Read from local cache | **~60ms** |
| `loop cache-stats` | Cache health | instant |

Every command accepts `--json` for machine consumption, `-v` for auth logging.

## Install

```pwsh
git clone https://github.com/jack-work/goop.git
cd goop
go build -o loop.exe .
# or:
./install.ps1   # builds + copies to ~/.goop/bin + adds to PATH
```

**Requirements:** Go 1.24+, Windows (WAM broker auth), `Az.Accounts` PowerShell module installed.

## Configuration

```toml
# ~/.goop/config.toml
[[workspace]]
title = "App Studio"

[[workspace]]
title = "My Other Loop"

sync_interval = "1h"
# db_path = "C:/Users/you/.goop/data/loop.db"   # default shown
```

## How it works

Loop stores content across two backends. Goop talks to both:

1. **Substrate** (`substrate.office.com`) -- workspace discovery, permissions, recent items
2. **SharePoint Embedded** (`*.sharepoint*.com`) -- page enumeration + Fluid snapshot read

Pages are `.loop` files = Fluid Framework documents. Goop fetches the ODSP snapshot, walks the merge-tree `segmentTexts` arrays, and reconstructs readable text. Tables and embedded components are partially supported (text nodes extracted, structure not yet preserved).

### Auth

Tokens come from the shared [`msauth`](https://github.com/jack-work/msauth) foundation: the **Windows WAM broker** first, using the Microsoft Office first-party client (`d3590ed6-...`) in the corporate tenant, then `az account get-access-token`. The foundation owns the DPAPI-protected per-user token cache, the two-minute refresh skew, and sanitized diagnostics with stable error codes. The broker satisfies Conditional Access on managed devices where device-code flow fails.

goop asks for exactly one resource per request: `https://substrate.office.com/.default` for discovery, and `https://<workspace sharepoint host>/.default` for content.

One critical header for content reads: **`X-CLP-Compliant-App: true`** -- without it, SharePoint returns 403 regardless of token validity.

### The `auth` adapter

[`auth/`](auth) is a thin adapter over the shared `msauth` module: it selects the client, tenant, and `wam-first` policy, and renders foundation failures so every credential source that was tried is named with its stable error code. There is no goop-local token cache and no sidecar PowerShell script.

## Daemon mode

```pwsh
loop daemon          # foreground (Ctrl+C to stop)
loop sync            # one-shot, then exit
```

Or register with [angl](https://github.com/jack-work/angl) for supervised background operation:

```pwsh
angl register goop-sync --interval 0 --charge "Loop sync daemon" -- loop.exe daemon
angl start goop-sync
```

The daemon syncs configured workspaces hourly, only re-fetching pages whose `lastModifiedDateTime` changed. Incremental syncs take ~3s when nothing changed vs ~30s for a full workspace flush.

## Performance

| Operation | Live (network) | Cached (SQLite) | Speedup |
|-----------|---------------|-----------------|---------|
| Page read | 3,899ms | 61ms | **55x** |
| Search | 2,613ms | 62ms | **38x** |

## Limitations

- **Read-only.** Write access (append/edit) is a future goal.
- **Text extraction is ~90% coverage.** Pages using only tables/matrices or embedded components may extract partially or empty.
- Substrate `recent` caps at 30 items; `--all` merges with `deltasync` for broader coverage.
- Windows-only for now (WAM broker dependency). Linux/Mac would need an alternative auth path.

## See also

- [`docs/REVERSE-ENGINEERING.md`](docs/REVERSE-ENGINEERING.md) -- full technical writeup of Loop's undocumented APIs
