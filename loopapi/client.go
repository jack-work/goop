// Package loopapi is a client for Microsoft Loop's undocumented web APIs,
// reverse-engineered from the loop.cloud.microsoft web app.
//
// Loop is a shell over two backends:
//
//   - Substrate (https://substrate.office.com) exposes discovery APIs under
//     /recommended/api/v1.1/loop/* that list a user's workspaces ("loops").
//   - SharePoint Embedded stores each workspace as a container (drive). Pages
//     are ".loop" files whose content is a Fluid Framework document, read from
//     the drive item's /opStream/snapshots/trees/latest endpoint.
//
// Auth is delegated per resource (Substrate, or the workspace's SharePoint
// host) through a TokenSource; goop backs that with the shared msauth
// foundation.
package loopapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/jack-work/msauth"
)

// SubstrateScope is the resource scope for Loop's Substrate discovery APIs.
const SubstrateScope = "https://substrate.office.com/.default"

// SharePointScope is the resource scope for one SharePoint Embedded host that
// stores a workspace's content.
func SharePointScope(host string) string { return "https://" + host + "/.default" }

// TokenSource acquires a bearer token for exactly one resource scope.
type TokenSource interface {
	Token(ctx context.Context, scope string) (string, error)
}

// Client talks to Substrate and SharePoint on behalf of the signed-in user.
type Client struct {
	auth TokenSource
	http *http.Client
	upn  string // anchor mailbox, resolved lazily
}

// New returns a Client using the given token source.
func New(auth TokenSource) *Client {
	return &Client{auth: auth, http: http.DefaultClient}
}

// Workspace is a Loop workspace ("a loop") as returned by the Substrate
// discovery APIs.
type Workspace struct {
	ID      string `json:"id"`    // stable "SPO_..." identifier
	Title   string `json:"title"` // display name
	MfsInfo struct {
		PodID string `json:"pod_id"`
	} `json:"mfs_info"`
	SharePointInfo struct {
		SiteURL string `json:"site_url"`
	} `json:"sharepoint_info"`
	CreationInfo struct {
		User struct {
			UPN         string `json:"upn"`
			DisplayName string `json:"display_name"`
		} `json:"user"`
		Timestamp string `json:"timestamp"`
	} `json:"creation_info"`
	UserRelationship struct {
		LastAccessDatetime string `json:"last_access_datetime"`
		LastActionDatetime string `json:"last_action_datetime"`
	} `json:"user_relationship"`
	IsPersonal bool     `json:"is_personal"`
	DataSource []string `json:"data_source"`
}

// Pod describes the SharePoint location of a workspace, decoded from PodID.
type Pod struct {
	Host    string // e.g. microsoft.sharepoint-df.com
	DriveID string // e.g. b!...
	ItemID  string // root item id
}

// Pod decodes the base64 pod_id ("ODSP|host|driveId|itemId").
func (w Workspace) Pod() (Pod, error) {
	raw, err := base64.StdEncoding.DecodeString(w.MfsInfo.PodID)
	if err != nil {
		return Pod{}, fmt.Errorf("decode pod_id: %w", err)
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) < 4 || parts[0] != "ODSP" {
		return Pod{}, fmt.Errorf("unexpected pod_id format: %q", string(raw))
	}
	return Pod{Host: parts[1], DriveID: parts[2], ItemID: parts[3]}, nil
}

// LastAccess returns the best available "recently touched" timestamp.
func (w Workspace) LastAccess() string {
	if w.UserRelationship.LastAccessDatetime != "" {
		return w.UserRelationship.LastAccessDatetime
	}
	return w.UserRelationship.LastActionDatetime
}

// Page is a page (.loop file) inside a workspace container.
type Page struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Modified string `json:"lastModifiedDateTime"`
	WebURL   string `json:"webUrl"`
	folder   bool
}

// Title is the page name without the .loop extension.
func (p Page) Title() string {
	return strings.TrimSuffix(strings.TrimSuffix(p.Name, ".loop"), ".url")
}

// ---- Substrate discovery ----

const substrateBase = "https://substrate.office.com/recommended/api/v1.1/loop"

type workspacesEnvelope struct {
	Workspaces []Workspace `json:"workspaces"`
}

// Recent returns the user's most recently used workspaces. The Substrate API
// caps top at 30; larger values return HTTP 400, so we clamp.
func (c *Client) Recent(ctx context.Context, top int) ([]Workspace, error) {
	if top <= 0 {
		top = 30
	}
	if top > 30 {
		top = 30
	}
	u := fmt.Sprintf("%s/recent?top=%d&settings=false&rs=en-us", substrateBase, top)
	var env workspacesEnvelope
	if err := c.substrateGet(ctx, u, &env); err != nil {
		return nil, err
	}
	return env.Workspaces, nil
}

// DeltaSync returns the full set of the user's workspaces (broader than Recent).
func (c *Client) DeltaSync(ctx context.Context) ([]Workspace, error) {
	u := substrateBase + "/deltasync?loopComponents=true&rs=en-us"
	var env workspacesEnvelope
	if err := c.substrateGet(ctx, u, &env); err != nil {
		return nil, err
	}
	return env.Workspaces, nil
}

// Permission is an ACL entry on a workspace.
type Permission struct {
	Role        string `json:"role"`
	UPN         string `json:"upn"`
	Email       string `json:"email_address"`
	DisplayName string `json:"display_name"`
	Type        string `json:"type"`
}

// Members returns the permission list for a workspace.
func (c *Client) Members(ctx context.Context, w Workspace) ([]Permission, error) {
	u := fmt.Sprintf("%s/workspaces/%s/permissions", substrateBase, encodePod(w.MfsInfo.PodID))
	var env struct {
		Permissions []Permission `json:"permissions"`
	}
	if err := c.substrateGet(ctx, u, &env); err != nil {
		return nil, err
	}
	return env.Permissions, nil
}

// encodePod percent-encodes the base64 special characters in a pod_id so it is
// safe as a single URL path segment.
func encodePod(pod string) string {
	r := strings.NewReplacer("+", "%2B", "/", "%2F", "=", "%3D")
	return r.Replace(pod)
}

func (c *Client) substrateGet(ctx context.Context, u string, out any) error {
	tok, err := c.auth.Token(ctx, SubstrateScope)
	if err != nil {
		return err
	}
	if c.upn == "" {
		// The foundation owns JWT claim reading; goop only decides that the
		// sign-in name is what Substrate's anchor mailbox header wants.
		if claims, ok := msauth.TokenClaims(tok); ok {
			c.upn = claims.UserPrincipalName
		}
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", "https://loop.cloud.microsoft")
	req.Header.Set("X-Office-Application", "300")
	req.Header.Set("X-Office-Platform", "Web")
	if c.upn != "" {
		req.Header.Set("X-AnchorMailbox", "UPN:"+c.upn)
	}
	return c.doJSON(req, out)
}

// ---- SharePoint Embedded content ----

// ListPages returns the pages (.loop / .url files) in a workspace container.
func (c *Client) ListPages(ctx context.Context, w Workspace) ([]Page, error) {
	pod, err := w.Pod()
	if err != nil {
		return nil, err
	}
	tok, err := c.auth.Token(ctx, SharePointScope(pod.Host))
	if err != nil {
		return nil, err
	}
	// Find the LoopAppData folder under the drive root.
	root, err := c.driveChildren(ctx, pod.Host, tok, pod.DriveID, "root")
	if err != nil {
		return nil, err
	}
	var appData *Page
	for i := range root {
		if root[i].folder && root[i].Name == "LoopAppData" {
			appData = &root[i]
			break
		}
	}
	if appData == nil {
		// Some containers hold pages at the root directly.
		return filterPages(root), nil
	}
	kids, err := c.driveChildren(ctx, pod.Host, tok, pod.DriveID, "items/"+appData.ID)
	if err != nil {
		return nil, err
	}
	return filterPages(kids), nil
}

func filterPages(items []Page) []Page {
	var out []Page
	for _, it := range items {
		if it.folder {
			continue
		}
		if strings.HasSuffix(it.Name, ".loop") || strings.HasSuffix(it.Name, ".url") {
			out = append(out, it)
		}
	}
	return out
}

func (c *Client) driveChildren(ctx context.Context, host, tok, driveID, itemPath string) ([]Page, error) {
	u := fmt.Sprintf("https://%s/_api/v2.0/drives/%s/%s/children?select=name,id,folder,size,webUrl,lastModifiedDateTime&$top=200",
		host, driveID, itemPath)
	var out []Page
	for u != "" {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/json")
		var env struct {
			Value    []rawItem `json:"value"`
			NextLink string    `json:"@odata.nextLink"`
		}
		if err := c.doJSON(req, &env); err != nil {
			return nil, err
		}
		for _, it := range env.Value {
			out = append(out, Page{
				ID: it.ID, Name: it.Name, Size: it.Size,
				Modified: it.Modified, WebURL: it.WebURL,
				folder: it.Folder != nil,
			})
		}
		u = env.NextLink
	}
	return out, nil
}

type rawItem struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Size     int64           `json:"size"`
	Modified string          `json:"lastModifiedDateTime"`
	WebURL   string          `json:"webUrl"`
	Folder   json.RawMessage `json:"folder"`
}

// ReadPage fetches a page's Fluid snapshot and returns the extracted text.
func (c *Client) ReadPage(ctx context.Context, w Workspace, page Page) (string, error) {
	snap, err := c.PageSnapshot(ctx, w, page)
	if err != nil {
		return "", err
	}
	return ExtractText(snap), nil
}

// PageSnapshot returns the raw Fluid ODSP snapshot bytes for a page.
func (c *Client) PageSnapshot(ctx context.Context, w Workspace, page Page) ([]byte, error) {
	pod, err := w.Pod()
	if err != nil {
		return nil, err
	}
	tok, err := c.auth.Token(ctx, SharePointScope(pod.Host))
	if err != nil {
		return nil, err
	}
	u := fmt.Sprintf("https://%s/_api/v2.1/drives/%s/items/%s/opStream/snapshots/trees/latest",
		pod.Host, pod.DriveID, page.ID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json, application/ms-fluid; v=1.0")
	// Required: asserts the client honors sensitivity-label / DLP policy. Without
	// it SharePoint returns 403 "Insufficient permissions on file."
	req.Header.Set("X-CLP-Compliant-App", "true")
	req.Header.Set("prefer", "manualredirect")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("read page: HTTP %d: %s", resp.StatusCode, quoteBody(body, 200))
	}
	return body, nil
}

// ---- helpers ----

func (c *Client) doJSON(req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s %s: HTTP %d: %s", req.Method, req.URL.Path, resp.StatusCode, quoteBody(body, 240))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// quoteBody renders a service response body for quoting in an error.
// Sanitizing happens before truncation so a clipped token can never survive in
// a diagnostic; msauth.SanitizeDiagnostic bounds at 500 and goop then clips to
// the caller's tighter limit.
//
// An empty body is reported as such rather than passed through, because the
// foundation renders empty input as "credential source failed without
// diagnostics" -- true for the credential diagnostics it was written for, and a
// false accusation for an HTTP status whose body the service simply omitted.
func quoteBody(body []byte, limit int) string {
	if len(strings.TrimSpace(string(body))) == 0 {
		return "(empty body)"
	}
	return truncate(msauth.SanitizeDiagnostic(string(body)), limit)
}
