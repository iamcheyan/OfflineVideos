package media

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"kanade/videod/internal/store"
)

func TestAudioFallbackExtractsMP3RegardlessOfJobCompletionOrder(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is required for the audio fallback integration test")
	}
	for _, failedFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "audio_failed_before_video", false: "video_completed_before_audio_failure"}[failedFirst], func(t *testing.T) {
			root := t.TempDir()
			videoPath := filepath.Join(root, "fixture.mp4")
			command := exec.Command(ffmpeg, "-nostdin", "-y", "-f", "lavfi", "-i", "color=c=black:s=64x64:d=1", "-f", "lavfi", "-i", "sine=frequency=440:duration=1", "-c:v", "mpeg4", "-c:a", "aac", "-shortest", videoPath)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("create fixture video: %v: %s", err, output)
			}

			db, err := store.Open(filepath.Join(root, "db.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := store.EnsureAccount(context.Background(), db, "youtube"); err != nil {
				t.Fatal(err)
			}
			svc, err := New(db, Config{DownloadDir: root, FFmpegPath: ffmpeg})
			if err != nil {
				t.Fatal(err)
			}
			defer svc.Close()

			videoResult, err := db.Exec(`INSERT INTO downloads(account_id,video_id,url,mode,state,output_path) VALUES('youtube','fixture','https://example.test/fixture','video','completed',?)`, videoPath)
			if err != nil {
				t.Fatal(err)
			}
			videoID, err := videoResult.LastInsertId()
			if err != nil {
				t.Fatal(err)
			}
			audioResult, err := db.Exec(`INSERT INTO downloads(account_id,video_id,url,mode,audio_bitrate,state,error) VALUES('youtube','fixture','https://example.test/fixture','audio','128','failed','source audio failed')`)
			if err != nil {
				t.Fatal(err)
			}
			audioID, err := audioResult.LastInsertId()
			if err != nil {
				t.Fatal(err)
			}
			audio := &job{ID: audioID, AccountID: "youtube", VideoID: "fixture", URL: "https://example.test/fixture", Mode: "audio", AudioBitrate: "128", State: "failed", Error: "source audio failed"}
			if failedFirst {
				svc.jobs[audio.ID] = audio
				svc.extractAudioFallback(audio)
			} else {
				video := &job{ID: 2, AccountID: "youtube", VideoID: "fixture", Mode: "video", State: "completed", OutputPath: videoPath}
				video.ID = videoID
				svc.jobs[video.ID] = video
				svc.extractAudioFallback(video)
				audio = svc.jobs[audio.ID]
			}

			var state, outputPath string
			if err := db.QueryRow(`SELECT state,output_path FROM downloads WHERE id=?`, audioID).Scan(&state, &outputPath); err != nil {
				t.Fatal(err)
			}
			if state != "completed" || filepath.Ext(outputPath) != ".mp3" {
				t.Fatalf("audio fallback state/path = %q/%q", state, outputPath)
			}
			info, err := os.Stat(outputPath)
			if err != nil || info.Size() == 0 {
				t.Fatalf("extracted MP3 missing or empty: %v", err)
			}
		})
	}
}
