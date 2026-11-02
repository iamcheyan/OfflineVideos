package media

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const catalogName = "index.json"

var catalogMu sync.Mutex

type catalog struct {
	Version   int           `json:"version"`
	UpdatedAt string        `json:"updated_at"`
	Items     []catalogItem `json:"items"`
}
type catalogItem struct {
	ID                  string         `json:"id"`
	Source              string         `json:"source"`
	SourceURL           string         `json:"source_url"`
	Title               string         `json:"title"`
	Description         string         `json:"description,omitempty"`
	ThumbnailURL        string         `json:"thumbnail_url,omitempty"`
	Uploader            string         `json:"uploader,omitempty"`
	UploaderID          string         `json:"uploader_id,omitempty"`
	Channel             string         `json:"channel,omitempty"`
	ChannelID           string         `json:"channel_id,omitempty"`
	Duration            float64        `json:"duration_seconds,omitempty"`
	UploadDate          string         `json:"upload_date,omitempty"`
	ReleaseDate         string         `json:"release_date,omitempty"`
	Timestamp           float64        `json:"timestamp,omitempty"`
	ViewCount           int64          `json:"view_count,omitempty"`
	LikeCount           int64          `json:"like_count,omitempty"`
	LiveStatus          string         `json:"live_status,omitempty"`
	Availability        string         `json:"availability,omitempty"`
	Extractor           string         `json:"extractor,omitempty"`
	ExtractorKey        string         `json:"extractor_key,omitempty"`
	Format              string         `json:"format,omitempty"`
	FormatID            string         `json:"format_id,omitempty"`
	Ext                 string         `json:"ext,omitempty"`
	Width               int            `json:"width,omitempty"`
	Height              int            `json:"height,omitempty"`
	Filesize            int64          `json:"filesize,omitempty"`
	File                string         `json:"file"`
	ThumbnailFile       string         `json:"thumbnail_file,omitempty"`
	ThumbnailDownloaded bool           `json:"thumbnail_downloaded"`
	DownloadedAt        string         `json:"downloaded_at"`
	Metadata            map[string]any `json:"metadata,omitempty"`
}

func (s *Service) updateCatalog(j *job, raw map[string]any, outputPath string) error {
	if raw == nil {
		raw = map[string]any{}
	}
	abs, err := filepath.Abs(outputPath)
	if err != nil || !withinDirectory(s.cfg.DownloadDir, abs) {
		return errors.New("download output is outside configured directory")
	}
	if _, err = os.Stat(abs); err != nil {
		return err
	}
	root, err := filepath.Abs(s.cfg.DownloadDir)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return err
	}
	thumbPath := strings.TrimSuffix(abs, filepath.Ext(abs)) + ".jpg"
	thumbRel := ""
	thumbOK := false
	if _, e := os.Stat(thumbPath); e == nil && withinDirectory(root, thumbPath) {
		if r, x := filepath.Rel(root, thumbPath); x == nil {
			thumbRel = filepath.ToSlash(r)
			thumbOK = true
		}
	}
	id := stringValue(raw["id"])
	if id == "" {
		id = j.VideoID
	}
	title := stringValue(raw["title"])
	if title == "" {
		title = id
	}
	item := catalogItem{ID: id, Source: j.AccountID, SourceURL: nonEmpty(stringValue(raw["webpage_url"]), j.URL), Title: title, Description: stringValue(raw["description"]), ThumbnailURL: stringValue(raw["thumbnail"]), Uploader: nonEmpty(stringValue(raw["uploader"]), stringValue(raw["channel"])), UploaderID: stringValue(raw["uploader_id"]), Channel: stringValue(raw["channel"]), ChannelID: stringValue(raw["channel_id"]), Duration: numberValue(raw["duration"]), UploadDate: stringValue(raw["upload_date"]), ReleaseDate: stringValue(raw["release_date"]), Timestamp: numberValue(raw["timestamp"]), ViewCount: int64Value(raw["view_count"]), LikeCount: int64Value(raw["like_count"]), LiveStatus: stringValue(raw["live_status"]), Availability: stringValue(raw["availability"]), Extractor: stringValue(raw["extractor"]), ExtractorKey: stringValue(raw["extractor_key"]), Format: stringValue(raw["format"]), FormatID: stringValue(raw["format_id"]), Ext: stringValue(raw["ext"]), Width: int(numberValue(raw["width"])), Height: int(numberValue(raw["height"])), Filesize: int64Value(raw["filesize"]), File: filepath.ToSlash(rel), ThumbnailFile: thumbRel, ThumbnailDownloaded: thumbOK, DownloadedAt: time.Now().UTC().Format(time.RFC3339Nano), Metadata: raw}
	catalogMu.Lock()
	defer catalogMu.Unlock()
	indexPath := filepath.Join(root, catalogName)
	data, err := os.ReadFile(indexPath)
	idx := catalog{Version: 1, Items: []catalogItem{}}
	if err == nil {
		if err = json.Unmarshal(data, &idx); err != nil {
			return err
		}
		if idx.Version == 0 {
			idx.Version = 1
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	found := false
	for i := range idx.Items {
		if idx.Items[i].Source == item.Source && idx.Items[i].ID == item.ID {
			idx.Items[i] = item
			found = true
			break
		}
	}
	if !found {
		idx.Items = append(idx.Items, item)
	}
	idx.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	encoded, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(root, ".index-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err = tmp.Write(append(encoded, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, indexPath)
}

// RebuildCatalogFromDB refreshes each completed download's catalog metadata from SQLite.
func (s *Service) RebuildCatalogFromDB() error {
	rows, err := s.db.Query(`SELECT d.id,d.account_id,d.video_id,d.url,d.output_path,COALESCE(v.title,''),COALESCE(v.description,''),COALESCE(v.thumbnail,''),COALESCE(v.duration,0),COALESCE(v.published_at,''),COALESCE(c.title,''),COALESCE(c.id,'') FROM downloads d LEFT JOIN videos v ON v.id=d.video_id AND v.account_id=d.account_id LEFT JOIN channels c ON c.id=v.channel_id AND c.account_id=v.account_id WHERE d.state='completed' AND d.output_path<>'' AND d.mode<>'audio' ORDER BY d.created_at,d.id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var j job
		var path, title, description, thumbnail, published, channel, channelID string
		var duration int
		if err := rows.Scan(&j.ID, &j.AccountID, &j.VideoID, &j.URL, &path, &title, &description, &thumbnail, &duration, &published, &channel, &channelID); err != nil {
			return err
		}
		meta := map[string]any{"id": j.VideoID, "title": title, "description": description, "thumbnail": thumbnail, "duration": duration, "upload_date": strings.ReplaceAll(strings.Split(published, "T")[0], "-", ""), "channel": channel, "channel_id": channelID, "webpage_url": j.URL}
		if !withinDirectory(s.cfg.DownloadDir, path) {
			continue
		}
		if err := s.updateCatalog(&j, meta, path); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	indexPath := filepath.Join(s.cfg.DownloadDir, catalogName)
	if _, err := os.Stat(indexPath); os.IsNotExist(err) {
		return s.writeEmptyCatalog()
	} else if err != nil {
		return err
	}
	return nil
}

func (s *Service) writeEmptyCatalog() error {
	root, err := filepath.Abs(s.cfg.DownloadDir)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(root, 0755); err != nil {
		return err
	}
	indexPath := filepath.Join(root, catalogName)
	encoded, err := json.MarshalIndent(catalog{Version: 1, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano), Items: []catalogItem{}}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(root, ".index-*.json")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err = tmp.Write(append(encoded, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, indexPath)
}
func withinDirectory(root, path string) bool {
	r, e := filepath.Abs(root)
	if e != nil {
		return false
	}
	rel, e := filepath.Rel(r, path)
	return e == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}
func stringValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
func numberValue(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	default:
		return 0
	}
}
func int64Value(v any) int64 { return int64(numberValue(v)) }
func nonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
