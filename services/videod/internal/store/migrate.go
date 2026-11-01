package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// SchemaVersion is the migration number this build expects. Migration files are
// applied in order; each is recorded in schema_migrations so restarts are no-ops.
const SchemaVersion = 4

type migration struct {
	version int
	name    string
	stmts   string
}

// migrations must stay append-only: never edit an applied migration.
var migrations = []migration{
	{1, "core schema", `CREATE TABLE IF NOT EXISTS accounts(id TEXT PRIMARY KEY,name TEXT NOT NULL,base_url TEXT NOT NULL DEFAULT '',auth_type TEXT NOT NULL DEFAULT '',active INTEGER NOT NULL DEFAULT 0,created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS oauth_tokens(account_id TEXT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,encrypted_access_token BLOB,encrypted_refresh_token BLOB,encrypted_cookie BLOB,encrypted_credentials BLOB,token_expires_at TEXT,updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS channels(id TEXT PRIMARY KEY,account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,title TEXT NOT NULL,url TEXT NOT NULL,thumbnail TEXT NOT NULL DEFAULT '',description TEXT NOT NULL DEFAULT '',synced_at TEXT NOT NULL DEFAULT '',UNIQUE(account_id,url));
CREATE TABLE IF NOT EXISTS videos(id TEXT PRIMARY KEY,account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,channel_id TEXT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,title TEXT NOT NULL,url TEXT NOT NULL,thumbnail TEXT NOT NULL DEFAULT '',duration INTEGER NOT NULL DEFAULT 0,published_at TEXT NOT NULL,description TEXT NOT NULL DEFAULT '',UNIQUE(account_id,url));
CREATE INDEX IF NOT EXISTS videos_account_published ON videos(account_id,published_at DESC);
CREATE TABLE IF NOT EXISTS playlists(id TEXT PRIMARY KEY,account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,title TEXT NOT NULL,description TEXT NOT NULL DEFAULT '',synced_at TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS playlist_videos(playlist_id TEXT NOT NULL REFERENCES playlists(id) ON DELETE CASCADE,video_id TEXT NOT NULL REFERENCES videos(id) ON DELETE CASCADE,position INTEGER NOT NULL,PRIMARY KEY(playlist_id,video_id));
CREATE TABLE IF NOT EXISTS downloads(id INTEGER PRIMARY KEY,account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,video_id TEXT NOT NULL DEFAULT '',url TEXT NOT NULL,mode TEXT NOT NULL DEFAULT 'video',quality TEXT NOT NULL DEFAULT 'best',audio_bitrate TEXT NOT NULL DEFAULT '192',priority INTEGER NOT NULL DEFAULT 0,state TEXT NOT NULL DEFAULT 'queued',progress REAL NOT NULL DEFAULT 0,error TEXT NOT NULL DEFAULT '',output_path TEXT NOT NULL DEFAULT '',created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP);
CREATE INDEX IF NOT EXISTS downloads_account_state ON downloads(account_id,state,priority DESC,created_at);
CREATE TABLE IF NOT EXISTS watch_later(account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,video_id TEXT NOT NULL REFERENCES videos(id) ON DELETE CASCADE,added_at TEXT NOT NULL,PRIMARY KEY(account_id,video_id));
CREATE TABLE IF NOT EXISTS favorites(account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,video_id TEXT NOT NULL REFERENCES videos(id) ON DELETE CASCADE,added_at TEXT NOT NULL,PRIMARY KEY(account_id,video_id));
CREATE TABLE IF NOT EXISTS history(account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,video_id TEXT NOT NULL REFERENCES videos(id) ON DELETE CASCADE,watched_at TEXT NOT NULL,position REAL NOT NULL DEFAULT 0,duration REAL NOT NULL DEFAULT 0,PRIMARY KEY(account_id,video_id));
CREATE TABLE IF NOT EXISTS settings(account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,key TEXT NOT NULL,value TEXT NOT NULL,PRIMARY KEY(account_id,key));
CREATE TABLE IF NOT EXISTS media_settings(account_id TEXT NOT NULL DEFAULT 'demo',key TEXT NOT NULL,value TEXT NOT NULL,PRIMARY KEY(account_id,key));
CREATE VIRTUAL TABLE IF NOT EXISTS video_search USING fts5(video_id UNINDEXED,account_id UNINDEXED,title,description,channel_title);
CREATE TRIGGER IF NOT EXISTS videos_search_insert AFTER INSERT ON videos BEGIN INSERT INTO video_search(video_id,account_id,title,description,channel_title) SELECT NEW.id,NEW.account_id,NEW.title,NEW.description,title FROM channels WHERE id=NEW.channel_id; END;
CREATE TRIGGER IF NOT EXISTS videos_search_update AFTER UPDATE ON videos BEGIN DELETE FROM video_search WHERE video_id=OLD.id; INSERT INTO video_search(video_id,account_id,title,description,channel_title) SELECT NEW.id,NEW.account_id,NEW.title,NEW.description,title FROM channels WHERE id=NEW.channel_id; END;
CREATE TRIGGER IF NOT EXISTS videos_search_delete AFTER DELETE ON videos BEGIN DELETE FROM video_search WHERE video_id=OLD.id; END;`},
	{2, "history duration", `ALTER TABLE history ADD COLUMN duration REAL NOT NULL DEFAULT 0`},
	{3, "sync timestamps", `ALTER TABLE channels ADD COLUMN synced_at TEXT NOT NULL DEFAULT '';
ALTER TABLE playlists ADD COLUMN synced_at TEXT NOT NULL DEFAULT '';

ALTER TABLE settings ADD COLUMN updated_at TEXT NOT NULL DEFAULT '';`},
	// remote_missing records downloaded videos that the source's watch-later
	// list no longer contains. Both services drop an entry once it has been
	// watched, so the absence is the only signal available for "watched". The
	// row is metadata, not a delete instruction: the file stays until the user
	// asks for it to go.
	{4, "remote missing", `CREATE TABLE IF NOT EXISTS remote_missing(account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,video_id TEXT NOT NULL,since TEXT NOT NULL,PRIMARY KEY(account_id,video_id));`},
}

// Migrate applies every pending migration in a transaction and records its version.
func Migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations(version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
		return err
	}
	var current int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&current); err != nil {
		return err
	}
	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for _, stmt := range splitStatements(m.stmts) {
			if _, err = tx.ExecContext(ctx, stmt); err != nil {
				// Columns added out of band (older builds) are already present.
				if strings.Contains(err.Error(), "duplicate column name") {
					continue
				}
				tx.Rollback()
				return fmt.Errorf("migration %d (%s): %w", m.version, m.name, err)
			}
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,name,applied_at) VALUES(?,?,?)`, m.version, m.name, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			tx.Rollback()
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// splitStatements splits a migration body on statement terminators while keeping
// trigger bodies (which contain internal semicolons) intact.
func splitStatements(body string) []string {
	var out []string
	var b strings.Builder
	inTrigger := false
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(lower, "create trigger") {
			inTrigger = true
		}
		if inTrigger {
			if strings.HasSuffix(trimmed, "end;") {
				out = append(out, strings.TrimSpace(b.String()))
				b.Reset()
				inTrigger = false
			}
			continue
		}
		if strings.HasSuffix(trimmed, ";") {
			out = append(out, strings.TrimSpace(b.String()))
			b.Reset()
		}
	}
	if rest := strings.TrimSpace(b.String()); rest != "" {
		out = append(out, rest)
	}
	return out
}

// SchemaState reports the applied version and records for diagnostics.
func SchemaState(ctx context.Context, db *sql.DB) (int, []map[string]any, error) {
	var current int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&current); err != nil {
		return 0, nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT version,name,applied_at FROM schema_migrations ORDER BY version`)
	if err != nil {
		return current, nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var version int
		var name, applied string
		if err := rows.Scan(&version, &name, &applied); err != nil {
			return current, nil, err
		}
		out = append(out, map[string]any{"version": version, "name": name, "applied_at": applied})
	}
	return current, out, rows.Err()
}
