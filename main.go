// Command loop is a CLI for reading Microsoft Loop workspaces ("loops") and
// pages, reverse-engineered from the loop.cloud.microsoft web app.
//
//	loop list [--top N] [--all]        list your workspaces
//	loop search <query> [--pages]      search workspaces (and page names)
//	loop pages <workspace>             list pages in a workspace
//	loop read  <workspace> [page]      print a page's text content
//	loop members <workspace>           list who a workspace is shared with
//	loop whoami                        show the signed-in identity
//	loop version                       show build provenance (no auth)
//
// A <workspace> argument is a case-insensitive substring of the workspace
// title (or its id). Auth uses the Windows WAM broker with the Microsoft Office
// first-party client, falling back to the Azure CLI.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/jack-work/goop/auth"
	"github.com/jack-work/goop/daemon"
	"github.com/jack-work/goop/loopapi"
	"github.com/jack-work/msauth"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes one command and returns the process exit code: 0 on success or
// help, 1 on a runtime error, 2 on a usage error (missing or unknown command).
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		usage(stderr)
		return 2
	}
	ctx := context.Background()

	// Global flags (crude but dependency-free).
	verbose := false
	asJSON := false
	var rest []string
	for _, a := range args[1:] {
		switch a {
		case "-v", "--verbose":
			verbose = true
		case "--json":
			asJSON = true
		default:
			rest = append(rest, a)
		}
	}

	// version answers from build metadata alone. It is handled before the
	// token provider exists so it can never authenticate, read the token
	// cache, or fail for an auth reason.
	if args[0] == "version" || args[0] == "--version" {
		if err := cmdVersion(stdout, asJSON); err != nil {
			return fatal(stderr, err)
		}
		return 0
	}

	tokens, err := auth.New()
	if err != nil {
		return fatal(stderr, err)
	}
	tokens.Verbose = verbose
	client := loopapi.New(tokens)

	switch args[0] {
	case "list", "ls":
		err = cmdList(ctx, client, rest, asJSON)
	case "search", "find":
		err = cmdSearch(ctx, client, rest, asJSON)
	case "pages":
		err = cmdPages(ctx, client, rest, asJSON)
	case "read", "cat":
		err = cmdRead(ctx, client, rest, asJSON)
	case "members", "perms":
		err = cmdMembers(ctx, client, rest, asJSON)
	case "whoami":
		err = cmdWhoami(ctx, tokens, stdout)
	case "daemon":
		err = cmdDaemon(ctx)
	case "sync":
		err = cmdSync(ctx)
	case "cache-search", "cs":
		err = cmdCacheSearch(rest, asJSON)
	case "cache-read", "cr":
		err = cmdCacheRead(rest, asJSON)
	case "cache-stats":
		err = cmdCacheStats()
	case "-h", "--help", "help":
		usage(stderr)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
		usage(stderr)
		return 2
	}
	if err != nil {
		return fatal(stderr, err)
	}
	return 0
}

// fatal renders a terminal error and, when the foundation can name the one
// action that fixes it, that action on its own line. Which code implies which
// remedy is msauth's decision and only msauth's: a goop-local mapping would
// mean a hint corrected in the foundation reaches some tools and not others,
// which is the exact failure this convergence exists to end. A non-auth error,
// or an auth error whose codes imply no remedy, prints no extra line.
func fatal(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "\x1b[31merror:\x1b[0m %v\n", err)
	if hint := msauth.Hint(err); hint != "" {
		fmt.Fprintln(stderr, hint)
	}
	return 1
}

func usage(w io.Writer) {
	fmt.Fprint(w, `loop - read Microsoft Loop from the CLI

usage:
  loop list [--top N] [--all]      list your workspaces (loops)
  loop search <query> [--pages]    search workspaces by title (and page names)
  loop pages <workspace>           list pages in a workspace
  loop read  <workspace> [page]    print a page's text content
  loop members <workspace>         list who a workspace is shared with
  loop whoami                      show the signed-in identity
  loop version                     show build provenance (no auth)

  loop daemon                      run the sync daemon (foreground)
  loop sync                        run a one-shot sync now
  loop cache-search <query>        full-text search the local cache
  loop cache-read <ws> <page>      read a page from the local cache (instant)
  loop cache-stats                 show cache statistics

global flags:
  --json       emit JSON
  -v           verbose auth logging

<workspace> and [page] are case-insensitive substrings of the title/name.
config: ~/.goop/config.toml
`)
}

// ---- commands ----

func cmdList(ctx context.Context, c *loopapi.Client, args []string, asJSON bool) error {
	top := 50
	all := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--top":
			if i+1 < len(args) {
				fmt.Sscanf(args[i+1], "%d", &top)
				i++
			}
		case "--all":
			all = true
		}
	}
	ws, err := loadWorkspaces(ctx, c, top, all)
	if err != nil {
		return err
	}
	if asJSON {
		return printJSON(ws)
	}
	printWorkspaceTable(ws)
	return nil
}

func cmdSearch(ctx context.Context, c *loopapi.Client, args []string, asJSON bool) error {
	pages := false
	var terms []string
	for _, a := range args {
		if a == "--pages" {
			pages = true
		} else {
			terms = append(terms, a)
		}
	}
	if len(terms) == 0 {
		return fmt.Errorf("usage: loop search <query> [--pages]")
	}
	q := strings.ToLower(strings.Join(terms, " "))

	ws, err := loadWorkspaces(ctx, c, 100, true)
	if err != nil {
		return err
	}
	var hits []loopapi.Workspace
	for _, w := range ws {
		if strings.Contains(strings.ToLower(w.Title), q) {
			hits = append(hits, w)
		}
	}

	type pageHit struct {
		Workspace string `json:"workspace"`
		Page      string `json:"page"`
	}
	var phits []pageHit
	if pages {
		for _, w := range ws {
			ps, err := c.ListPages(ctx, w)
			if err != nil {
				continue
			}
			for _, p := range ps {
				if strings.Contains(strings.ToLower(p.Title()), q) {
					phits = append(phits, pageHit{w.Title, p.Title()})
				}
			}
		}
	}

	if asJSON {
		return printJSON(map[string]any{"workspaces": hits, "pages": phits})
	}
	if len(hits) == 0 && len(phits) == 0 {
		fmt.Println("no matches")
		return nil
	}
	if len(hits) > 0 {
		fmt.Printf("\x1b[1mworkspaces\x1b[0m matching %q:\n", q)
		printWorkspaceTable(hits)
	}
	if len(phits) > 0 {
		fmt.Printf("\n\x1b[1mpages\x1b[0m matching %q:\n", q)
		for _, h := range phits {
			fmt.Printf("  %-40s  \x1b[2m(%s)\x1b[0m\n", h.Page, h.Workspace)
		}
	}
	return nil
}

func cmdPages(ctx context.Context, c *loopapi.Client, args []string, asJSON bool) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: loop pages <workspace>")
	}
	w, err := resolveWorkspace(ctx, c, strings.Join(args, " "))
	if err != nil {
		return err
	}
	ps, err := c.ListPages(ctx, *w)
	if err != nil {
		return err
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].Title() < ps[j].Title() })
	if asJSON {
		return printJSON(ps)
	}
	fmt.Printf("\x1b[1m%s\x1b[0m — %d page(s)\n", w.Title, len(ps))
	for _, p := range ps {
		fmt.Printf("  %-50s \x1b[2m%7s  %s\x1b[0m\n", p.Title(), humanSize(p.Size), shortDate(p.Modified))
	}
	return nil
}

func cmdRead(ctx context.Context, c *loopapi.Client, args []string, asJSON bool) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: loop read <workspace> [page]")
	}
	w, err := resolveWorkspace(ctx, c, args[0])
	if err != nil {
		return err
	}
	ps, err := c.ListPages(ctx, *w)
	if err != nil {
		return err
	}
	loopPages := make([]loopapi.Page, 0, len(ps))
	for _, p := range ps {
		if strings.HasSuffix(p.Name, ".loop") {
			loopPages = append(loopPages, p)
		}
	}

	var target *loopapi.Page
	if len(args) >= 2 {
		q := strings.ToLower(strings.Join(args[1:], " "))
		var matches []loopapi.Page
		for _, p := range loopPages {
			if strings.Contains(strings.ToLower(p.Title()), q) {
				matches = append(matches, p)
			}
		}
		if len(matches) == 0 {
			return fmt.Errorf("no page in %q matches %q", w.Title, q)
		}
		if len(matches) > 1 {
			fmt.Fprintf(os.Stderr, "multiple pages match %q:\n", q)
			for _, p := range matches {
				fmt.Fprintf(os.Stderr, "  - %s\n", p.Title())
			}
			return fmt.Errorf("be more specific")
		}
		target = &matches[0]
	} else {
		if len(loopPages) == 0 {
			return fmt.Errorf("workspace %q has no .loop pages", w.Title)
		}
		if len(loopPages) > 1 {
			fmt.Fprintf(os.Stderr, "%q has %d pages; specify one:\n", w.Title, len(loopPages))
			sort.Slice(loopPages, func(i, j int) bool { return loopPages[i].Title() < loopPages[j].Title() })
			for _, p := range loopPages {
				fmt.Fprintf(os.Stderr, "  - %s\n", p.Title())
			}
			return fmt.Errorf("be more specific")
		}
		target = &loopPages[0]
	}

	text, err := c.ReadPage(ctx, *w, *target)
	if err != nil {
		return err
	}
	if asJSON {
		return printJSON(map[string]string{
			"workspace": w.Title, "page": target.Title(), "text": text,
		})
	}
	fmt.Printf("\x1b[1m# %s\x1b[0m  \x1b[2m(%s)\x1b[0m\n\n", target.Title(), w.Title)
	fmt.Println(text)
	return nil
}

func cmdMembers(ctx context.Context, c *loopapi.Client, args []string, asJSON bool) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: loop members <workspace>")
	}
	w, err := resolveWorkspace(ctx, c, strings.Join(args, " "))
	if err != nil {
		return err
	}
	perms, err := c.Members(ctx, *w)
	if err != nil {
		return err
	}
	if asJSON {
		return printJSON(perms)
	}
	fmt.Printf("\x1b[1m%s\x1b[0m — %d member(s)\n", w.Title, len(perms))
	for _, p := range perms {
		fmt.Printf("  %-10s %-40s %s\n", p.Role, p.UPN, p.DisplayName)
	}
	return nil
}

func cmdWhoami(ctx context.Context, tokens loopapi.TokenSource, stdout io.Writer) error {
	tok, err := tokens.Token(ctx, loopapi.SubstrateScope)
	if err != nil {
		return err
	}
	// The foundation owns JWT claim reading. A token that is not a decodable
	// JWT yields the zero Claims, so the fields print blank exactly as the
	// map-based decoder printed them for the same input.
	claims, _ := msauth.TokenClaims(tok)
	fmt.Fprintf(stdout, "signed in as \x1b[1m%s\x1b[0m\n", claims.UserPrincipalName)
	fmt.Fprintf(stdout, "  name:   %s\n", claims.Name)
	fmt.Fprintf(stdout, "  tenant: %s\n", claims.TenantID)
	fmt.Fprintf(stdout, "  appid:  %s (%s)\n", claims.AppID, claims.AppDisplayName)
	return nil
}

// ---- build provenance ----

// buildInfo names the code inside this binary: goop's own revision and the
// version of the shared auth foundation it was compiled against. msauth is
// linked as a library, so an installed artifact freezes whatever foundation
// existed at build time; a foundation fix does not reach it until it is
// rebuilt. Without this report, answering "does this artifact contain the
// fixed broker?" means archaeology on the file, so the report exists to make
// linkage a question anyone can ask the binary itself. The foundation version
// comes from msauth.Version() alone: one mechanism, owned by the foundation.
type buildInfo struct {
	Tool     string `json:"tool"`
	Revision string `json:"revision"`
	Modified bool   `json:"modified"`
	Msauth   string `json:"msauth"`
}

func versionInfo() buildInfo {
	info := buildInfo{Tool: "goop", Revision: "unknown", Msauth: msauth.Version()}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return info
	}
	for _, setting := range bi.Settings {
		switch setting.Key {
		case "vcs.revision":
			info.Revision = setting.Value
		case "vcs.modified":
			info.Modified = setting.Value == "true"
		}
	}
	return info
}

// cmdVersion prints build provenance. It performs no authentication and no
// network call.
func cmdVersion(stdout io.Writer, asJSON bool) error {
	info := versionInfo()
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(info)
	}
	revision := info.Revision
	if info.Modified {
		revision += " (modified)"
	}
	fmt.Fprintf(stdout, "goop   %s\nmsauth %s\n", revision, info.Msauth)
	return nil
}

// ---- workspace resolution / loading ----

func loadWorkspaces(ctx context.Context, c *loopapi.Client, top int, all bool) ([]loopapi.Workspace, error) {
	recent, err := c.Recent(ctx, top)
	if err != nil {
		return nil, err
	}
	if !all {
		return recent, nil
	}
	delta, err := c.DeltaSync(ctx)
	if err != nil {
		return recent, nil // recent is still useful
	}
	return dedupe(append(recent, delta...)), nil
}

func resolveWorkspace(ctx context.Context, c *loopapi.Client, query string) (*loopapi.Workspace, error) {
	ws, err := loadWorkspaces(ctx, c, 100, true)
	if err != nil {
		return nil, err
	}
	q := strings.ToLower(strings.TrimSpace(query))
	var matches []loopapi.Workspace
	for _, w := range ws {
		if strings.EqualFold(w.ID, query) || strings.Contains(strings.ToLower(w.Title), q) {
			matches = append(matches, w)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no workspace matches %q (try 'loop list --all')", query)
	case 1:
		return &matches[0], nil
	default:
		// Prefer an exact (case-insensitive) title match if present.
		for i := range matches {
			if strings.EqualFold(matches[i].Title, query) {
				return &matches[i], nil
			}
		}
		fmt.Fprintf(os.Stderr, "%d workspaces match %q:\n", len(matches), query)
		for _, m := range matches {
			fmt.Fprintf(os.Stderr, "  - %s\n", m.Title)
		}
		return nil, fmt.Errorf("be more specific")
	}
}

func dedupe(ws []loopapi.Workspace) []loopapi.Workspace {
	seen := map[string]bool{}
	var out []loopapi.Workspace
	for _, w := range ws {
		if w.ID == "" || seen[w.ID] {
			continue
		}
		seen[w.ID] = true
		out = append(out, w)
	}
	return out
}

// ---- rendering ----

func printWorkspaceTable(ws []loopapi.Workspace) {
	for _, w := range ws {
		host := hostOf(w.SharePointInfo.SiteURL)
		fmt.Printf("  %-44s \x1b[2m%-10s %s\x1b[0m\n",
			truncateStr(w.Title, 44), shortDate(w.LastAccess()), host)
	}
	fmt.Printf("\x1b[2m%d workspace(s)\x1b[0m\n", len(ws))
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func hostOf(u string) string {
	u = strings.TrimPrefix(u, "https://")
	if i := strings.IndexByte(u, '/'); i >= 0 {
		return u[:i]
	}
	return u
}

func shortDate(s string) string {
	if s == "" {
		return ""
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Local().Format("2006-01-02")
	}
	if len(s) >= 10 {
		return s[:10]
	}
	return s
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fM", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0fK", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

func truncateStr(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// ---- daemon / cache commands ----

func cmdDaemon(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	cfg := daemon.LoadConfig()
	return daemon.Run(ctx, cfg)
}

func cmdSync(ctx context.Context) error {
	cfg := daemon.LoadConfig()
	db, err := daemon.OpenDB(cfg.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()
	return daemon.Sync(ctx, cfg, db)
}

func cmdCacheSearch(args []string, asJSON bool) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: loop cache-search <query>")
	}
	cfg := daemon.LoadConfig()
	db, err := daemon.OpenDB(cfg.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()
	results, err := db.Search(strings.Join(args, " "))
	if err != nil {
		return err
	}
	if asJSON {
		return printJSON(results)
	}
	if len(results) == 0 {
		fmt.Println("no results")
		return nil
	}
	for _, r := range results {
		fmt.Printf("  \x1b[1m%s\x1b[0m  \x1b[2m(%s, %s)\x1b[0m\n", r.PageTitle, r.WorkspaceTitle, shortDate(r.Modified))
		if r.Snippet != "" {
			fmt.Printf("    %s\n", r.Snippet)
		}
	}
	return nil
}

func cmdCacheRead(args []string, asJSON bool) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: loop cache-read <workspace> <page>")
	}
	cfg := daemon.LoadConfig()
	db, err := daemon.OpenDB(cfg.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()
	text, err := db.ReadCached(args[0], strings.Join(args[1:], " "))
	if err != nil {
		return err
	}
	if asJSON {
		return printJSON(map[string]string{"text": text})
	}
	fmt.Println(text)
	return nil
}

func cmdCacheStats() error {
	cfg := daemon.LoadConfig()
	db, err := daemon.OpenDB(cfg.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()
	ws, pg, chars, lastSync, _ := db.Stats()
	fmt.Printf("cache: %s\n", cfg.DBPath)
	fmt.Printf("  workspaces: %d\n", ws)
	fmt.Printf("  pages:      %d\n", pg)
	fmt.Printf("  text:       %d chars (%.1f KB)\n", chars, float64(chars)/1024)
	fmt.Printf("  last sync:  %s\n", lastSync)
	return nil
}
