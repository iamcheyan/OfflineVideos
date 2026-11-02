# OfflineVideos

Go service that syncs and downloads YouTube and Bilibili Watch Later videos. It keeps a persistent SQLite queue, imports browser cookie headers into restricted-permission yt-dlp jars, and downloads media and thumbnails through yt-dlp.

## Download catalog

Each successful download writes or updates `index.json` in `OFFLINEVIDEOS_DOWNLOAD_DIR` before the job is marked completed. The downloader obtains metadata and a JPG thumbnail during the same yt-dlp run.

The JSON document has `version`, `updated_at`, and `items`. Catalog items include source, media ID, source URL, title, description, thumbnail URL, relative media and thumbnail paths, uploader/channel identifiers, duration, upload/release dates, timestamp, view and like counts, availability/live state, extractor and format details, dimensions, file size, download time, and raw yt-dlp metadata.

`GET /api/catalog` returns the catalog. `POST /api/catalog` rebuilds it from completed rows whose files are under the current download root. The hourly systemd timer calls this endpoint. Legacy rows that point outside the current download root are skipped.

## Requirements

- Go 1.23 to build
- `yt-dlp` and `ffmpeg`
- `curl` for the catalog timer

## Build

```sh
make build
```

The binary is `videod`. HTTP listens on `127.0.0.1:8732` by default.

## Configuration

Set these variables in a private systemd EnvironmentFile:

- `OFFLINEVIDEOS_DATA_DIR` — SQLite database, credential key, and cookie jars
- `OFFLINEVIDEOS_DOWNLOAD_DIR` — media and `index.json` directory
- `OFFLINEVIDEOS_LISTEN` — HTTP listen address; keep on loopback
- `OFFLINEVIDEOS_YTDLP` — yt-dlp executable
- `OFFLINEVIDEOS_MPV` — mpv path for optional playback
- `OFFLINEVIDEOS_CONCURRENCY` — download workers, 1–8; default 2
- `OFFLINEVIDEOS_AUTO_DOWNLOAD` — auto-queue newly synced items; default `on`
- `OFFLINEVIDEOS_SYNC_INTERVAL_MIN` — Watch Later sync interval; default 60, `0` disables it

The service syncs both Watch Later lists at startup and on each interval. When auto-download is enabled, each item is kept as a video download plus a companion MP3 at the configured audio bitrate. Existing video-only cache entries are backfilled with audio on the next successful list sync; completed or in-flight audio jobs are not duplicated. The source audio stream and video download run as separate jobs; if the source audio job fails, the service extracts an MP3 from the completed local video with ffmpeg. MP3 files carry ID3 title and artist/album-artist tags using the video title and channel/uploader name, so music players show readable track details even though the stored filename uses the source ID.

## Cookie import

POST JSON to `/api/auth/cookies` on the loopback listener:

```json
{"source":"youtube","cookie":"NAME=value; ..."}
```

Use `bilibili` as the source for Bilibili. Never put cookie values in source control, shell history, or logs.

## Systemd

The repository includes generic templates:

- `offlinevideos.service` — downloader, enabled at boot
- `offlinevideos-index.service` — rebuilds catalog through the local API
- `offlinevideos-index.timer` — invokes catalog rebuild hourly
- `offlinevideos.env.example` — configuration shape only; replace paths for the host and store the actual file outside the repository with mode `0600`

Create a runtime account, install `videod` and the unit files, provide a private EnvironmentFile, and add a local drop-in for the host's NAS mount dependency. The timer must run as an account that can read the data directory and write the download directory.

Do not commit host usernames, home directories, NAS mount paths, cookies, databases, or downloaded media.
