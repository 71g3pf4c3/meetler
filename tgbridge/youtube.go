package main

// YouTube → палата через yt-dlp. Кидаешь боту ссылку на YouTube — качаем
// ПЕРВЫЕ maxPlaySec секунд в низком качестве, транскодим (toVideo) и льём в
// палату как видео (VP8/IVF + Opus/OGG).
//
// Безопасность (server-side fetch по ссылке от пользователя = SSRF-поверхность):
//   - строгий whitelist хостов youtube.com / youtu.be / m.youtube.com;
//   - только http(s);
//   - yt-dlp ограничен секцией 0-maxPlaySec и низким форматом (трафик/время).

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// грубый матчер URL в тексте
var urlRe = regexp.MustCompile(`https?://[^\s]+`)

// допустимые хосты (и поддомены www./m./music.)
var ytHosts = map[string]bool{
	"youtube.com":       true,
	"www.youtube.com":   true,
	"m.youtube.com":     true,
	"music.youtube.com": true,
	"youtu.be":          true,
}

// extractYouTubeURL ищет в тексте первую валидную YouTube-ссылку. "" если нет.
func extractYouTubeURL(text string) string {
	for _, raw := range urlRe.FindAllString(text, -1) {
		raw = strings.TrimRight(raw, ".,!?)")
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			continue
		}
		if ytHosts[strings.ToLower(u.Host)] {
			return raw
		}
	}
	return ""
}

const ytDLTimeout = 2 * time.Minute

// downloadYouTube качает первые maxPlaySec секунд ролика в низком качестве во
// временный файл и возвращает путь. yt-dlp сам выбирает контейнер (.mp4/.webm).
func downloadYouTube(ctx context.Context, link, dir string) (string, error) {
	if extractYouTubeURL(link) == "" {
		return "", fmt.Errorf("не YouTube-ссылка")
	}
	c, cancel := context.WithTimeout(ctx, ytDLTimeout)
	defer cancel()

	outTmpl := filepath.Join(dir, "yt.%(ext)s")
	// -f: низкое разрешение (≤360p) — приоритет "пролезет", качество побоку.
	// --download-sections: качаем только первые maxPlaySec секунд (ключ к
	//   экономии — не тянем весь ролик). Требует ffmpeg (есть в образе).
	// --no-playlist: ссылка на плейлист → только один ролик.
	cmd := exec.CommandContext(c, "yt-dlp",
		"--no-playlist",
		"--quiet", "--no-warnings",
		"-f", "bv*[height<=360]+ba/b[height<=360]/b",
		"--download-sections", fmt.Sprintf("*0-%d", maxPlaySec),
		"--force-keyframes-at-cuts",
		"-o", outTmpl,
		link,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("yt-dlp: %w: %s", err, string(out))
	}
	// найти скачанный файл (расширение заранее неизвестно)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "yt.") && !e.IsDir() {
			return filepath.Join(dir, e.Name()), nil
		}
	}
	return "", fmt.Errorf("yt-dlp: файл не найден после скачивания")
}

// streamYouTube: ссылка → скачать → toVideo → PublishVideo в палату.
func (a *app) streamYouTube(ctx context.Context, palata, link string) error {
	// очередь: YouTube тоже под лимитом (yt-dlp + ffmpeg тяжёлые, не плодим параллель)
	release, ok := a.acquireMedia()
	if !ok {
		return errQueueFull
	}
	defer release()

	dir, cleanup, err := tmpDir()
	if err != nil {
		return err
	}
	defer cleanup()
	src, err := downloadYouTube(ctx, link, dir)
	if err != nil {
		return err
	}
	ivf, ogg, err := toVideo(ctx, src, dir)
	if err != nil {
		return err
	}
	return a.pub.PublishVideo(palata, ivf, ogg)
}
