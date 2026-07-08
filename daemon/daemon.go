// Package daemon implements the goop sync daemon: it periodically fetches
// Loop workspace contents and stores extracted page text in a local SQLite
// database for instant offline search and read.
package daemon

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jack-work/goop/loopapi"
	"github.com/jack-work/goop/msauth"
	_ "modernc.org/sqlite"
)

// Config holds daemon settings loaded from ~/.goop/config.toml.
type Config struct {
	Workspaces   []WorkspaceFilter
	SyncInterval time.Duration
	DBPath       string
}

type WorkspaceFilter struct {
	Title string
}

// DefaultConfig returns sensible defaults (sync all workspaces hourly).
func DefaultConfig() Config {
	return Config{
		SyncInterval: 1 * time.Hour,
		DBPath:       defaultDBPath(),
	}
}

func defaultDBPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".goop", "data", "loop.db")
}

// LoadConfig reads ~/.goop/config.toml (minimal TOML parsing).
func LoadConfig() Config {
	cfg := DefaultConfig()
	home, _ := os.UserHomeDir()
	paths := []string{
		filepath.Join(home, ".goop", "config.toml"),
		filepath.Join(home, ".config", "goop", "config.toml"),
	}
	var data []byte
	for _, p := range paths {
		if b, err := os.ReadFile(p); err == nil {
			data = b
			break
		}
	}
	if data == nil {
		return cfg
	}
	// Minimal line-by-line TOML parser for our simple schema.
	var current *WorkspaceFilter
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		if line == "[[workspace]]" {
			cfg.Workspaces = append(cfg.Workspaces, WorkspaceFilter{})
			current = &cfg.Workspaces[len(cfg.Workspaces)-1]
			continue
		}
		k, v := parseKV(line)
		switch k {
		case "title":
			if current != nil {
				current.Title = v
			}
		case "sync_interval":
			if d, err := time.ParseDuration(v); err == nil {
				cfg.SyncInterval = d
			}
		case "db_path":
			cfg.DBPath = v
		}
	}
	return cfg
}

func parseKV(line string) (string, string) {
	i := strings.IndexByte(line, '=')
	if i < 0 {
		return "", ""
	}
	k := strings.TrimSpace(line[:i])
	v := strings.TrimSpace(line[i+1:])
	v = strings.Trim(v, "\"")
	return k, v
}

// DB wraps the SQLite database with the schema and query methods.
type DB struct {
	db *sql.DB
}

// OpenDB opens (or creates) the cache database.
func OpenDB(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &DB{db: db}, nil
}

func migrate(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS workspaces (
			id         TEXT PRIMARY KEY,
			title      TEXT NOT NULL,
			pod_id     TEXT,
			site_url   TEXT,
			synced_at  TEXT
		);
		CREATE TABLE IF NOT EXISTS pages (
			id           TEXT PRIMARY KEY,
			workspace_id TEXT NOT NULL REFERENCES workspaces(id),
			name         TEXT NOT NULL,
			title        TEXT NOT NULL,
			size         INTEGER,
			modified     TEXT,
			web_url      TEXT,
			text_content TEXT,
			synced_at    TEXT
		);
		CREATE INDEX IF NOT EXISTS idx_pages_workspace ON pages(workspace_id);
		CREATE INDEX IF NOT EXISTS idx_pages_title ON pages(title);
		CREATE VIRTUAL TABLE IF NOT EXISTS pages_fts USING fts5(
			title, text_content, content=pages, content_rowid=rowid
		);
		-- Triggers to keep FTS in sync
		CREATE TRIGGER IF NOT EXISTS pages_ai AFTER INSERT ON pages BEGIN
			INSERT INTO pages_fts(rowid, title, text_content)
			VALUES (new.rowid, new.title, new.text_content);
		END;
		CREATE TRIGGER IF NOT EXISTS pages_ad AFTER DELETE ON pages BEGIN
			INSERT INTO pages_fts(pages_fts, rowid, title, text_content)
			VALUES ('delete', old.rowid, old.title, old.text_content);
		END;
		CREATE TRIGGER IF NOT EXISTS pages_au AFTER UPDATE ON pages BEGIN
			INSERT INTO pages_fts(pages_fts, rowid, title, text_content)
			VALUES ('delete', old.rowid, old.title, old.text_content);
			INSERT INTO pages_fts(rowid, title, text_content)
			VALUES (new.rowid, new.title, new.text_content);
		END;
	`)
	return err
}

func (d *DB) Close() error { return d.db.Close() }

// UpsertWorkspace inserts or updates a workspace record.
func (d *DB) UpsertWorkspace(w loopapi.Workspace) error {
	_, err := d.db.Exec(`INSERT INTO workspaces (id, title, pod_id, site_url, synced_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET title=excluded.title, pod_id=excluded.pod_id,
			site_url=excluded.site_url, synced_at=excluded.synced_at`,
		w.ID, w.Title, w.MfsInfo.PodID, w.SharePointInfo.SiteURL, time.Now().UTC().Format(time.RFC3339))
	return err
}

// UpsertPage inserts or updates a page record (including text content).
func (d *DB) UpsertPage(workspaceID string, p loopapi.Page, text string) error {
	_, err := d.db.Exec(`INSERT INTO pages (id, workspace_id, name, title, size, modified, web_url, text_content, synced_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, title=excluded.title,
			size=excluded.size, modified=excluded.modified, web_url=excluded.web_url,
			text_content=excluded.text_content, synced_at=excluded.synced_at`,
		p.ID, workspaceID, p.Name, p.Title(), p.Size, p.Modified, p.WebURL, text, time.Now().UTC().Format(time.RFC3339))
	return err
}

// PageNeedsSync returns true if the page is not cached or its modified date differs.
func (d *DB) PageNeedsSync(pageID, modified string) bool {
	var cached string
	err := d.db.QueryRow("SELECT modified FROM pages WHERE id = ?", pageID).Scan(&cached)
	if err != nil {
		return true
	}
	return cached != modified
}

// Search performs full-text search across cached pages.
func (d *DB) Search(query string) ([]SearchResult, error) {
	rows, err := d.db.Query(`
		SELECT p.title, p.name, w.title, p.modified,
			snippet(pages_fts, 1, '>>>', '<<<', '...', 40) as snip
		FROM pages_fts
		JOIN pages p ON p.rowid = pages_fts.rowid
		JOIN workspaces w ON w.id = p.workspace_id
		WHERE pages_fts MATCH ?
		ORDER BY rank
		LIMIT 30`, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(&r.PageTitle, &r.PageName, &r.WorkspaceTitle, &r.Modified, &r.Snippet); err != nil {
			continue
		}
		results = append(results, r)
	}
	return results, nil
}

type SearchResult struct {
	PageTitle      string
	PageName       string
	WorkspaceTitle string
	Modified       string
	Snippet        string
}

// ReadCached returns cached text for a page by title substring match.
func (d *DB) ReadCached(workspace, page string) (string, error) {
	q := "%" + page + "%"
	wq := "%" + workspace + "%"
	var text string
	err := d.db.QueryRow(`
		SELECT p.text_content FROM pages p
		JOIN workspaces w ON w.id = p.workspace_id
		WHERE w.title LIKE ? AND p.title LIKE ?
		LIMIT 1`, wq, q).Scan(&text)
	if err != nil {
		return "", fmt.Errorf("page not found in cache: %w", err)
	}
	return text, nil
}

// Stats returns cache statistics.
func (d *DB) Stats() (workspaces, pages int, totalChars int64, lastSync string, err error) {
	d.db.QueryRow("SELECT COUNT(*) FROM workspaces").Scan(&workspaces)
	d.db.QueryRow("SELECT COUNT(*) FROM pages").Scan(&pages)
	d.db.QueryRow("SELECT COALESCE(SUM(LENGTH(text_content)),0) FROM pages").Scan(&totalChars)
	d.db.QueryRow("SELECT COALESCE(MAX(synced_at),'never') FROM pages").Scan(&lastSync)
	return
}

// --- Sync engine ---

// Sync performs a full sync of configured workspaces.
func Sync(ctx context.Context, cfg Config, db *DB) error {
	auth := msauth.New()
	auth.Verbose = true
	client := loopapi.New(auth)

	allWS, err := client.Recent(ctx, 30)
	if err != nil {
		return fmt.Errorf("fetch recent: %w", err)
	}
	delta, err := client.DeltaSync(ctx)
	if err == nil {
		allWS = dedupeWS(append(allWS, delta...))
	}

	// Filter to configured workspaces (if any specified).
	workspaces := filterWorkspaces(allWS, cfg.Workspaces)
	slog.Info("sync starting", "workspaces", len(workspaces))

	for _, w := range workspaces {
		if err := db.UpsertWorkspace(w); err != nil {
			slog.Error("upsert workspace", "title", w.Title, "error", err)
			continue
		}
		if err := syncWorkspace(ctx, client, db, w); err != nil {
			slog.Error("sync workspace failed", "title", w.Title, "error", err)
		}
	}

	ws, pg, chars, last, _ := db.Stats()
	slog.Info("sync complete", "workspaces", ws, "pages", pg, "chars", chars, "last_sync", last)
	return nil
}

func syncWorkspace(ctx context.Context, client *loopapi.Client, db *DB, w loopapi.Workspace) error {
	slog.Info("syncing workspace", "title", w.Title)
	pages, err := client.ListPages(ctx, w)
	if err != nil {
		return err
	}

	synced, skipped, failed := 0, 0, 0
	for _, p := range pages {
		if !strings.HasSuffix(p.Name, ".loop") {
			continue
		}
		// Skip if already cached with same modified date.
		if !db.PageNeedsSync(p.ID, p.Modified) {
			skipped++
			continue
		}
		text, err := client.ReadPage(ctx, w, p)
		if err != nil {
			slog.Warn("read page failed", "page", p.Name, "error", err)
			failed++
			// Still upsert with empty text so we don't retry immediately.
			_ = db.UpsertPage(w.ID, p, "")
			continue
		}
		if err := db.UpsertPage(w.ID, p, text); err != nil {
			slog.Error("upsert page", "page", p.Name, "error", err)
			failed++
			continue
		}
		synced++
	}
	slog.Info("workspace done", "title", w.Title, "synced", synced, "skipped", skipped, "failed", failed)
	return nil
}

func filterWorkspaces(all []loopapi.Workspace, filters []WorkspaceFilter) []loopapi.Workspace {
	if len(filters) == 0 {
		return all
	}
	var out []loopapi.Workspace
	for _, w := range all {
		for _, f := range filters {
			if strings.EqualFold(strings.TrimSpace(w.Title), strings.TrimSpace(f.Title)) {
				out = append(out, w)
				break
			}
		}
	}
	return out
}

func dedupeWS(ws []loopapi.Workspace) []loopapi.Workspace {
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

// Run starts the daemon loop: sync immediately, then every cfg.SyncInterval.
func Run(ctx context.Context, cfg Config) error {
	db, err := OpenDB(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	slog.Info("goop daemon starting",
		"db", cfg.DBPath,
		"interval", cfg.SyncInterval,
		"workspaces", len(cfg.Workspaces))

	// Initial sync.
	if err := Sync(ctx, cfg, db); err != nil {
		slog.Error("initial sync failed", "error", err)
	}

	ticker := time.NewTicker(cfg.SyncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("daemon shutting down")
			return nil
		case <-ticker.C:
			if err := Sync(ctx, cfg, db); err != nil {
				slog.Error("sync failed", "error", err)
			}
		}
	}
}
