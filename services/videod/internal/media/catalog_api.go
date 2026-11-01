package media

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
)

func (s *Service) catalogEndpoint(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		if err := s.RebuildCatalogFromDB(); err != nil {
			apiError(w, 500, "catalog_rebuild_failed", "unable to rebuild media catalog")
			return
		}
		writeJSON(w, 200, map[string]any{"status": "ok"})
		return
	}
	if r.Method != http.MethodGet {
		apiError(w, 405, "method_not_allowed", "method not allowed")
		return
	}
	data, err := os.ReadFile(filepath.Join(s.cfg.DownloadDir, catalogName))
	if os.IsNotExist(err) {
		writeJSON(w, 200, catalog{Version: 1, Items: []catalogItem{}})
		return
	}
	if err != nil {
		apiError(w, 500, "catalog_read_failed", "unable to read media catalog")
		return
	}
	var value catalog
	if err = json.Unmarshal(data, &value); err != nil {
		apiError(w, 500, "catalog_invalid", "media catalog is invalid")
		return
	}
	writeJSON(w, 200, value)
}
