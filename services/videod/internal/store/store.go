package store

import (
	"context"
	"database/sql"
	"fmt"
	_ "modernc.org/sqlite"
	"time"
)

func Open(path string) (*sql.DB, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	_, _ = db.Exec("PRAGMA busy_timeout = 5000; PRAGMA journal_mode = WAL;")
	if err = Migrate(context.Background(), db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}
func Seed(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO accounts(id,name,base_url,auth_type,active,created_at) VALUES('demo','Demo account','', 'demo',1,?)`, now); err != nil {
		return err
	}
	for i := range 100 {
		n := i + 1
		cid := fmt.Sprintf("channel-%03d", n)
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO channels(id,account_id,title,url) VALUES(?,?,?,?)`, cid, "demo", fmt.Sprintf("Demo Channel %03d", n), "https://example.test/channel/"+cid); err != nil {
			return err
		}
	}
	for i := range 1000 {
		n := i + 1
		vid := fmt.Sprintf("video-%04d", n)
		ch := fmt.Sprintf("channel-%03d", (n-1)%100+1)
		title := fmt.Sprintf("Demo Video %04d", n)
		date := time.Date(2024, 1, 1+n%365, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO videos(id,account_id,channel_id,title,url,duration,published_at,description) VALUES(?,?,?,?,?,?,?,?)`, vid, "demo", ch, title, "https://example.test/watch/"+vid, 120+n%600, date, fmt.Sprintf("Description for %s", title)); err != nil {
			return err
		}
	}
	for i := range 20 {
		n := i + 1
		pid := fmt.Sprintf("playlist-%02d", n)
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO playlists(id,account_id,title,description) VALUES(?,?,?,?)`, pid, "demo", fmt.Sprintf("Demo Playlist %02d", n), "Seeded playlist"); err != nil {
			return err
		}
		for j := range 10 {
			vid := fmt.Sprintf("video-%04d", (n-1)*10+j+1)
			if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO playlist_videos(playlist_id,video_id,position) VALUES(?,?,?)`, pid, vid, j); err != nil {
				return err
			}
		}
	}
	for i := range 50 {
		vid := fmt.Sprintf("video-%04d", i+1)
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO watch_later(account_id,video_id,added_at) VALUES('demo',?,?)`, vid, now); err != nil {
			return err
		}
		if i < 25 {
			if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO history(account_id,video_id,watched_at,position) VALUES('demo',?,?,30)`, vid, now); err != nil {
				return err
			}
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO settings(account_id,key,value) VALUES('demo','theme','dark')`)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// SeedDownloads inserts deterministic demo queue fixtures after media tables exist.
func SeedDownloads(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i := range 50 {
		vid := fmt.Sprintf("video-%04d", i+1)
		state := []string{"queued", "downloading", "paused", "completed", "failed", "cancelled"}[i%6]
		if i < 8 {
			state = "completed"
		}
		progress := 0.0
		if state == "completed" {
			progress = 100
		}
		var exists int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM downloads WHERE account_id='demo' AND video_id=?`, vid).Scan(&exists); err != nil {
			return err
		}
		if exists > 0 {
			continue
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO downloads(account_id,video_id,url,mode,quality,audio_bitrate,priority,state,progress,output_path) VALUES('demo',?,?,?,?,?,?,?,?,?)`, vid, "https://example.test/watch/"+vid, "video", "720p", "192", 50-i, state, progress, ""); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type AccountStore struct{ DB *sql.DB }

func (s AccountStore) List(ctx context.Context) ([]map[string]any, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,name,base_url,auth_type,active,created_at FROM accounts ORDER BY created_at,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, name, url, auth, created string
		var active int
		if err := rows.Scan(&id, &name, &url, &auth, &active, &created); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"id": id, "name": name, "base_url": url, "auth_type": auth, "active": active != 0, "created_at": created})
	}
	return out, rows.Err()
}
func (s AccountStore) Activate(ctx context.Context, id string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	if err = tx.QueryRowContext(ctx, `SELECT 1 FROM accounts WHERE id=?`, id).Scan(&exists); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE accounts SET active=(id=?)`, id); err != nil {
		return err
	}
	return tx.Commit()
}
func ActiveAccount(ctx context.Context, db *sql.DB) (string, error) {
	var id string
	err := db.QueryRowContext(ctx, `SELECT id FROM accounts WHERE active=1 ORDER BY id LIMIT 1`).Scan(&id)
	return id, err
}

// EnsureAccount creates an active local account when none exists.
//
// Downloads, library rows and credential records all carry an account_id with a
// foreign key, so a database without any active account rejects writes. The demo
// seed normally provides one; when seeding is disabled (POCKET_YOUTUBE_SEED=0)
// this supplies the minimal account needed to use the service.
func EnsureAccount(ctx context.Context, db *sql.DB, id string) error {
	if id == "" {
		id = "local"
	}
	var exists int
	err := db.QueryRowContext(ctx, `SELECT 1 FROM accounts WHERE id=?`, id).Scan(&exists)
	if err == nil {
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = db.ExecContext(ctx,
		`INSERT OR IGNORE INTO accounts(id,name,base_url,auth_type,active,created_at) VALUES(?,?,?,?,1,?)`,
		id, "Local account", "", "local", now)
	return err
}
