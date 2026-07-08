# Reverse-engineering Loop authentication and APIs

Source material: a HAR capture of a `loop.cloud.microsoft` session
(`loop.zip` → `loop.cloud.microsoft.har`). The capture was **sanitized** —
`Authorization` and `Cookie` request headers were stripped — so tokens had to be
recovered indirectly and the auth model reconstructed from the request
structure plus empirical testing.

## 1. Topology

427 requests across these hosts (API-relevant ones):

| Host | Role |
|------|------|
| `substrate.office.com` | Loop **discovery**: list workspaces, permissions, presence, activity |
| `microsoft.sharepoint-df.com` / `microsoft.sharepoint.com` | **content**: SharePoint Embedded containers holding Fluid `.loop` files |
| `graph.microsoft.com` | user profiles, photos, sensitivity labels |
| `editor.svc.cloud.microsoft` | NL editor config (not needed for read) |
| `augloop.office.com` | augmentation loop / copilot (not needed for read) |
| `res.cdn.office.net`, `hubblecontent…` | static assets, icons |

**Model:** a "loop" (workspace) is a SharePoint Embedded container = a *drive*.
Its pages are `.loop` files = **Fluid Framework** documents. Substrate is a thin
index over the user's containers.

## 2. Recovering the tokens

No `Authorization` headers survived in the HAR. But the SharePoint content
requests use SharePoint's **`ump=1` (unified metadata protocol)** CORS-bypass:
the real request is tunneled inside a `multipart/form-data` **body**, and the
bearer token rides in that body — which the HAR *did* keep:

```
--<boundary>
Authorization: Bearer eyJ0eXAiOiJKV1Q…
X-HTTP-Method-Override: GET
prefer: manualredirect
X-CLP-Compliant-App: true
_post: 1
--<boundary>--
```

Decoding those JWTs revealed the **Loop app registration**:

- `appid` = **`a187e399-0c36-4b98-8f04-1edc167a0996`** ("Microsoft Loop App")
- SharePoint content token: `aud = 00000003-0000-0ff1-ce00-000000000000`
  (SharePoint Online), `scp = Container.Selected Files.ReadWrite.All`

## 3. The auth flow that actually works from a CLI

The Loop web app is a browser SPA using MSAL.js; its client id
(`a187e399…`) is **not** registered for the native WAM broker
(`AcquireTokenSilent` fails with *"Invalid redirect uri … ms-appx-web://…"`).

Empirically testing broker-registered first-party public clients against the
Loop APIs:

| Client | `/loop/recent` | Notes |
|--------|----------------|-------|
| Azure CLI `04b07795…` | **401** invalid_token | only gets `user_impersonation`; Substrate rejects |
| Azure PowerShell `1950a258…` | **401** | same |
| **Microsoft Office `d3590ed6-52b3-4102-aeff-aad2292ab01c`** | **200** | broker-registered, rich Substrate scopes |
| Teams `1fec8e78…` | 200 | also works |

→ **Use the Microsoft Office client via the WAM broker.** It yields Substrate
tokens (`aud=https://substrate.office.com`) with scopes including
`Files.ReadWrite.All`, `Sites.Read.All.Sdp`, `SubstrateSearch-Internal.ReadWrite`,
and delegated SharePoint tokens (`aud=00000003-0000-0ff1-ce00-000000000000`,
`scp=user_impersonation`) good enough to read container content.

Resources needed:

- `https://substrate.office.com/.default` — discovery
- `https://<workspace-host>/.default` — content (per host: `microsoft.sharepoint.com`,
  `microsoft.sharepoint-df.com`, `microsoft-my.sharepoint-df.com`, …)
- `https://graph.microsoft.com/.default` — profiles (optional)

## 4. Discovery APIs (Substrate)

All GET, `Authorization: Bearer <substrate token>`, plus
`Origin: https://loop.cloud.microsoft` and `X-AnchorMailbox: UPN:<upn>`.

- `GET /recommended/api/v1.1/loop/recent?top=N&rs=en-us` — recent workspaces
  **and** recent individual pages. `top` is capped at **30** (higher ⇒ HTTP 400).
- `GET /recommended/api/v1.1/loop/deltasync?loopComponents=true&rs=en-us` — the
  broader working set.
- `GET /recommended/api/v1.1/loop/workspaces/{podId}/permissions` — ACL (owners,
  managers, members). `{podId}` is the base64 `pod_id`; percent-encode `+ / =`.

Workspace identity: each item has an `id` (`SPO_<base64>`) and
`mfs_info.pod_id`. The `pod_id` decodes to:

```
ODSP|<host>|<driveId>|<itemId>
e.g. ODSP|microsoft.sharepoint-df.com|b!_vY0…|01EP5HFF…
```

That gives the SharePoint host + drive id needed for content.

## 5. Content APIs (SharePoint Embedded)

Token: `https://<host>/.default`.

- **List pages:** `GET /_api/v2.0/drives/{driveId}/root/children` → find the
  `LoopAppData` folder → `GET …/items/{folderId}/children` → `.loop` / `.url`
  files. (Paginate via `@odata.nextLink`.)
- **Read a page:**
  `GET /_api/v2.1/drives/{driveId}/items/{itemId}/opStream/snapshots/trees/latest`
  with `Accept: application/ms-fluid; v=1.0` **and `X-CLP-Compliant-App: true`**.
  Returns `application/ms-fluid`. (Plain `/content` returns 0 bytes — `.loop`
  files have no stream; content lives in the Fluid op stream / snapshot.)

The browser wraps this GET in the `ump=1` multipart tunnel to dodge CORS; a
native client just sends a normal GET with the two headers above.

## 6. Fluid snapshot → text

The `application/ms-fluid` snapshot is a length-prefixed tree. Page rich text is
stored as a Fluid **merge tree** (SharedString); the snapshot embeds JSON chunks:

```json
{"segmentTexts":["AI Rese",{"text":"arch","props":{…}},
                 {"marker":{"refType":1},"props":{"nodeType":"Paragraph"}}],
 "totalLengthChars":12, …}
```

Extraction = find every JSON object containing `"segmentTexts"`, walk the array
in order, append string runs (`"…"` and `{"text":"…"}`), and emit a newline on
each block `marker` (Paragraph / ListItem / heading). Implemented in
[`loopapi/fluid.go`](../loopapi/fluid.go); it cleanly recovers page bodies.

Data stores seen in a page container: `LoopCanvasComponentSingleton`,
`LoopPageHeaderComponentSingleton`, `LoopPageTitleSingleton`, package
`@fluidx/loop-page-container`.
