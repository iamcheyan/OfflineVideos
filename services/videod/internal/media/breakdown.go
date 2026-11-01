package media

import (
	"net/http"
	"os"
	"sort"
)

// storageBreakdown answers "what is using the space" with per-source and
// per-video totals.
type storageBreakdown struct {
	TotalBytes int64         `json:"total_bytes"`
	Orphans    int           `json:"orphans"`
	OrphanSize int64         `json:"orphan_bytes"`
	Watched    watchedUsage  `json:"watched"`
	Sources    []sourceUsage `json:"sources"`
	Videos     []videoUsage  `json:"videos"`
}

// watchedUsage totals the files whose source's watch-later list no longer
// contains them. That absence is the only portable "already watched" signal
// Bilibili and YouTube give us, so the app offers these as one deletable group.
type watchedUsage struct {
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
}

type sourceUsage struct {
	Account string `json:"account"`
	Files   int    `json:"files"`
	Bytes   int64  `json:"bytes"`
}

type videoUsage struct {
	VideoID string `json:"video_id"`
	Account string `json:"account"`
	Title   string `json:"title"`
	Bytes   int64  `json:"bytes"`
	Watched bool   `json:"watched"`
}

// breakdown lists real file sizes joined with the rows that own them.
//
// Sizes are read from disk rather than stored: a file can be replaced or
// truncated outside the app, and a report that disagrees with the filesystem
// would be misleading for a delete decision.
func (s *Service) breakdown(w http.ResponseWriter, r *http.Request) {
	out := storageBreakdown{Sources: []sourceUsage{}, Videos: []videoUsage{}}

	type row struct {
		videoID string
		account string
		title   string
		path    string
		since   string
	}
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT d.video_id, d.account_id, COALESCE(v.title, d.video_id), d.output_path,
		       COALESCE(m.since,'')
		FROM downloads d
		LEFT JOIN videos v ON v.id=d.video_id AND v.account_id=d.account_id
		LEFT JOIN remote_missing m ON m.video_id=d.video_id AND m.account_id=d.account_id
		WHERE d.state='completed' AND d.output_path<>''`)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "database_error", "unable to read downloads")
		return
	}
	var list []row
	for rows.Next() {
		var x row
		if rows.Scan(&x.videoID, &x.account, &x.title, &x.path, &x.since) == nil {
			list = append(list, x)
		}
	}
	rows.Close()

	byAccount := map[string]*sourceUsage{}
	var total int64
	for _, x := range list {
		info, err := os.Stat(x.path)
		if err != nil {
			continue
		}
		size := info.Size()
		total += size
		usage := byAccount[x.account]
		if usage == nil {
			usage = &sourceUsage{Account: x.account}
			byAccount[x.account] = usage
		}
		usage.Files++
		usage.Bytes += size
		out.Videos = append(out.Videos, videoUsage{
			VideoID: x.videoID,
			Account: x.account,
			Title:   x.title,
			Bytes:   size,
			Watched: x.since != "",
		})
		if x.since != "" {
			out.Watched.Files++
			out.Watched.Bytes += size
		}
	}

	for _, u := range byAccount {
		out.Sources = append(out.Sources, *u)
	}
	sort.Slice(out.Sources, func(i, j int) bool {
		return out.Sources[i].Bytes > out.Sources[j].Bytes
	})
	// Watched items first: they are the ones the user is most likely to clear.
	sort.Slice(out.Videos, func(i, j int) bool {
		if out.Videos[i].Watched != out.Videos[j].Watched {
			return out.Videos[i].Watched
		}
		return out.Videos[i].Bytes > out.Videos[j].Bytes
	})
	// The app draws a short list; sending every row would be wasteful. The
	// watched subset is small and carries its own totals, so the cap applies
	// only to the chart list.
	if len(out.Videos) > 50 {
		out.Videos = out.Videos[:50]
	}

	out.TotalBytes = total

	// Fold in files no row claims so the pie chart always adds up.
	known := map[string]bool{}
	for _, x := range list {
		known[x.path] = true
	}
	_ = walkFiles(s.cfg.DownloadDir, func(path string, size int64) {
		if known[path] {
			return
		}
		out.Orphans++
		out.OrphanSize += size
		out.TotalBytes += size
	})

	writeJSON(w, http.StatusOK, out)
}

// walkFiles calls fn for every regular file under root with its size.
func walkFiles(root string, fn func(path string, size int64)) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		fn(root+"/"+e.Name(), info.Size())
	}
	return nil
}
