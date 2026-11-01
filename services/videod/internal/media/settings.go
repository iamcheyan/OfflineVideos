package media

import (
	"net/http"
	"strconv"
	"strings"
)

// settingDefaults are the account-level preferences the application understands.
var settingDefaults = map[string]string{
	"theme": "dark", "language": "en", "profile": "desktop", "scale": "100",
	"download_dir": "", "library_dir": "", "subtitle_language": "off",
	"default_quality": "720p", "storage_limit_gb": "20", "concurrency": "2",
	"source_youtube": "on", "source_bilibili": "on", "auto_download": "on",
	"source_sync": "on", "sync_interval_min": "0",
}

func (s *Service) settings(w http.ResponseWriter, r *http.Request) {
	account, err := resolveAccount(r, s.db)
	if err != nil {
		apiError(w, 500, "account_unavailable", "active account unavailable")
		return
	}
	if r.Method == http.MethodGet {
		rows, err := s.db.QueryContext(r.Context(), `SELECT key,value FROM settings WHERE account_id=? UNION ALL SELECT key,value FROM media_settings WHERE account_id=?`, account, account)
		if err != nil {
			apiError(w, 500, "database_error", "unable to read settings")
			return
		}
		defer rows.Close()
		m := map[string]string{}
		for k, v := range settingDefaults {
			m[k] = v
		}
		for rows.Next() {
			var k, v string
			if err := rows.Scan(&k, &v); err != nil {
				apiError(w, 500, "database_error", "unable to read settings")
				return
			}
			m[k] = v
		}
		if m["download_dir"] == "" {
			m["download_dir"] = s.cfg.DownloadDir
		}
		if m["library_dir"] == "" {
			m["library_dir"] = s.cfg.DownloadDir
		}
		writeJSON(w, 200, m)
		return
	}
	if r.Method != http.MethodPut {
		apiError(w, 405, "method_not_allowed", "method not allowed")
		return
	}
	var values map[string]string
	if decodeJSON(r, &values) != nil {
		apiError(w, 400, "invalid_request", "settings must be a string map")
		return
	}
	for k, v := range values {
		switch k {
		case "theme":
			if !oneOf(v, "light", "dark", "amoled", "system") {
				apiError(w, 400, "invalid_setting", "unsupported theme")
				return
			}
		case "language":
			if !oneOf(v, "en", "zh-CN", "ja") {
				apiError(w, 400, "invalid_setting", "unsupported language")
				return
			}
		case "profile":
			if !oneOf(v, "desktop", "tv", "handheld", "dap", "compact") {
				apiError(w, 400, "invalid_setting", "unsupported profile")
				return
			}
		case "scale":
			if n, e := strconv.Atoi(v); e != nil || n < 100 || n > 300 {
				apiError(w, 400, "invalid_setting", "scale must be 100-300")
				return
			}
		case "default_quality":
			if !oneOf(v, "360p", "720p", "1080p", "1440p", "2160p") {
				apiError(w, 400, "invalid_setting", "unsupported quality")
				return
			}
		case "subtitle_language":
			if !oneOf(v, "off", "en", "zh-Hans", "ja") {
				apiError(w, 400, "invalid_setting", "unsupported subtitle preference")
				return
			}
		case "concurrency":
			if n, e := strconv.Atoi(v); e != nil || n < 1 || n > 8 {
				apiError(w, 400, "invalid_setting", "concurrency must be 1-8")
				return
			}
		case "storage_limit_gb":
			if n, e := strconv.Atoi(v); e != nil || n < 1 {
				apiError(w, 400, "invalid_setting", "storage limit must be positive")
				return
			}
		case "auto_download", "source_sync", "source_youtube", "source_bilibili":
			if !oneOf(v, "on", "off") {
				apiError(w, 400, "invalid_setting", k+" must be on or off")
				return
			}
		case "sync_interval_min":
			if n, e := strconv.Atoi(v); e != nil || n < 0 || n > 1440 {
				apiError(w, 400, "invalid_setting", "sync interval must be 0-1440 minutes")
				return
			}
		case "download_dir", "library_dir":
			if strings.Contains(v, "..") {
				apiError(w, 400, "invalid_setting", "directory must not escape the data directory")
				return
			}
		default:
			apiError(w, 400, "invalid_setting", "unknown setting key")
			return
		}
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		apiError(w, 500, "database_error", "unable to update settings")
		return
	}
	defer tx.Rollback()
	for k, v := range values {
		if _, err = tx.ExecContext(r.Context(), `INSERT INTO settings(account_id,key,value) VALUES(?,?,?) ON CONFLICT(account_id,key) DO UPDATE SET value=excluded.value`, account, k, v); err != nil {
			apiError(w, 500, "database_error", "unable to update settings")
			return
		}
	}
	if err = tx.Commit(); err != nil {
		apiError(w, 500, "database_error", "unable to update settings")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
