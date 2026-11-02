package media

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
	"time"
)

// NotificationSender sends a completion notice to the configured target.
type NotificationSender interface {
	Send(context.Context, string) error
}

// HermesTelegramSender delegates delivery to Hermes, which owns the bot credentials.
type HermesTelegramSender struct {
	Binary string
	Target string
}

func (s HermesTelegramSender) Send(ctx context.Context, text string) error {
	if strings.TrimSpace(s.Binary) == "" || strings.TrimSpace(s.Target) == "" {
		return fmt.Errorf("telegram notification is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.Binary, "send", "-t", s.Target, "--json", text)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("hermes send failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	var result struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		return fmt.Errorf("parse hermes send response: %w", err)
	}
	if !result.Success {
		return fmt.Errorf("hermes send reported unsuccessful delivery")
	}
	return nil
}

func completionMessage(account, title, videoID, sourceURL, outputPath string) string {
	if strings.TrimSpace(title) == "" {
		title = videoID
	}
	if strings.TrimSpace(sourceURL) == "" && videoID != "" {
		switch account {
		case "youtube":
			sourceURL = "https://www.youtube.com/watch?v=" + url.QueryEscape(videoID)
		case "bilibili":
			sourceURL = "https://www.bilibili.com/video/" + url.PathEscape(videoID)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "✅ OfflineVideos 下载完成\n平台：%s\n标题：%s", account, title)
	if sourceURL != "" {
		fmt.Fprintf(&b, "\n%s", sourceURL)
	}
	if outputPath != "" {
		fmt.Fprintf(&b, "\n文件：%s", outputPath)
	}
	return b.String()
}
