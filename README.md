OfflineVideos downloads videos from authenticated YouTube and Bilibili Watch Later lists.

## Download catalog

Every successful download adds or updates an item in `index.json` in the configured download directory. `yt-dlp` supplies the metadata and the thumbnail during the download; `videod` writes the catalog before marking the job completed. Updates use an atomic replacement so readers never observe a partially written JSON document.

The catalog has a version, update timestamp, and `items` array. Each item includes source (`youtube` or `bilibili`), video ID and URL, title, description, thumbnail URL and local thumbnail path when available, uploader/channel identifiers, duration, upload/release dates, view/like counts, availability/live state, extractor and format details, relative media path, download timestamp, and the raw `yt-dlp` metadata object.

`GET /api/catalog` returns the catalog. `POST /api/catalog` rebuilds/refreshes it from completed download records; the systemd timer calls this endpoint hourly. New successful downloads update the index immediately.

## Runtime

The Go service uses `yt-dlp`, `ffmpeg`, and SQLite. See [`services/videod/README.md`](services/videod/README.md) for setup, cookie handling, systemd installation, and configuration.
