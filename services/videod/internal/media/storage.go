package media

import (
	"net/http"
	"os"
	"path/filepath"
)

// storage reports disk usage of the download directory.
//
// The sizes come from walking the directory rather than summing the database:
// files can be deleted outside the app, and a plan that trusts stale rows would
// promise space that is not there.
type storageStats struct {
	DownloadDir string `json:"download_dir"`
	Files       int    `json:"files"`
	Bytes       int64  `json:"bytes"`
	Orphans     int    `json:"orphans"`
	OrphanBytes int64  `json:"orphan_bytes"`
}

func (s *Service) storage(w http.ResponseWriter, r *http.Request) {
	root := s.cfg.DownloadDir
	stats := storageStats{DownloadDir: root}

	known := map[string]bool{}
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT output_path FROM downloads WHERE state='completed' AND output_path<>''`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var p string
			if rows.Scan(&p) == nil && p != "" {
				if abs, e := filepath.Abs(p); e == nil {
					known[abs] = true
				}
			}
		}
	}

	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, e := d.Info()
		if e != nil {
			return nil
		}
		stats.Files++
		stats.Bytes += info.Size()
		abs, _ := filepath.Abs(path)
		if !known[abs] {
			stats.Orphans++
			stats.OrphanBytes += info.Size()
		}
		return nil
	})

	writeJSON(w, http.StatusOK, stats)
}

// clearDownloads removes completed downloads.
//
// scope selects what goes: "all" deletes every completed job and its file,
// "orphans" deletes only files with no completed download row for them, and
// "watched" deletes the files the source's watch-later list no longer contains.
// Orphans is the safest form and watched is the next one: both are narrower than
// "all", which is what makes them offerable as one tap in a settings screen.
func (s *Service) clearDownloads(w http.ResponseWriter, r *http.Request) {
	scope := r.URL.Query().Get("scope")
	if scope == "" {
		scope = "orphans"
	}
	if scope != "all" && scope != "orphans" && scope != "watched" {
		apiError(w, http.StatusBadRequest, "invalid_scope", "scope must be all, orphans or watched")
		return
	}

	removed, freed := 0, int64(0)

	if scope == "watched" {
		rows, err := s.db.QueryContext(r.Context(), `
			SELECT d.id, d.output_path
			FROM downloads d
			JOIN remote_missing m ON m.video_id=d.video_id AND m.account_id=d.account_id
			WHERE d.state='completed'`)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "database_error", "unable to read downloads")
			return
		}
		type job struct {
			id   int64
			path string
		}
		var jobs []job
		for rows.Next() {
			var j job
			if rows.Scan(&j.id, &j.path) == nil {
				jobs = append(jobs, j)
			}
		}
		rows.Close()
		for _, j := range jobs {
			if j.path != "" {
				if info, e := os.Stat(j.path); e == nil && os.Remove(j.path) == nil {
					removed++
					freed += info.Size()
				}
			}
			_, _ = s.db.ExecContext(r.Context(), `DELETE FROM downloads WHERE id=?`, j.id)
		}
	} else if scope == "orphans" {
		known := map[string]bool{}
		rows, err := s.db.QueryContext(r.Context(),
			`SELECT output_path FROM downloads WHERE state='completed' AND output_path<>''`)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "database_error", "unable to read downloads")
			return
		}
		for rows.Next() {
			var p string
			if rows.Scan(&p) == nil && p != "" {
				if abs, e := filepath.Abs(p); e == nil {
					known[abs] = true
				}
			}
		}
		rows.Close()

		_ = filepath.WalkDir(s.cfg.DownloadDir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			abs, _ := filepath.Abs(path)
			if known[abs] {
				return nil
			}
			if info, e := d.Info(); e == nil {
				if os.Remove(path) == nil {
					removed++
					freed += info.Size()
				}
			}
			return nil
		})
	} else {
		rows, err := s.db.QueryContext(r.Context(),
			`SELECT id, output_path FROM downloads WHERE state='completed'`)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "database_error", "unable to read downloads")
			return
		}
		type job struct {
			id   int64
			path string
		}
		var jobs []job
		for rows.Next() {
			var j job
			if rows.Scan(&j.id, &j.path) == nil {
				jobs = append(jobs, j)
			}
		}
		rows.Close()

		for _, j := range jobs {
			if j.path != "" {
				if info, e := os.Stat(j.path); e == nil {
					if os.Remove(j.path) == nil {
						removed++
						freed += info.Size()
					}
				}
			}
			_, _ = s.db.ExecContext(r.Context(), `DELETE FROM downloads WHERE id=?`, j.id)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":        "ok",
		"scope":         scope,
		"files_removed": removed,
		"bytes_freed":   freed,
	})
}
