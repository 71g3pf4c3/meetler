package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// ffmpeg-транскод во временные файлы. Форматы заточены под lksdk.NewLocalFileTrack:
//   аудио -> .ogg (opus), видео -> .ivf (vp8) + отдельный .ogg (opus).

const ffTimeout = 2 * time.Minute

func run(ctx context.Context, name string, args ...string) error {
	c, cancel := context.WithTimeout(ctx, ffTimeout)
	defer cancel()
	cmd := exec.CommandContext(c, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, string(out))
	}
	return nil
}

// toOgg перекодирует любой аудио/контейнер в чистый ogg/opus 48k mono.
// Для TG voice (уже opus) это дешёвый remux+resample, не теряем качество зря.
func toOgg(ctx context.Context, src, dstDir string) (string, error) {
	dst := filepath.Join(dstDir, "audio.ogg")
	err := run(ctx, "ffmpeg", "-y", "-i", src,
		"-vn",
		"-ac", "1", "-ar", "48000",
		"-c:a", "libopus", "-b:a", "24k",
		"-f", "ogg", dst,
	)
	if err != nil {
		return "", err
	}
	return dst, nil
}

// toVideo перекодирует видео в .ivf (VP8) + .ogg (opus). Низкое разрешение/битрейт —
// приоритет "пролезет в палату", качество побоку.
func toVideo(ctx context.Context, src, dstDir string) (ivf, ogg string, err error) {
	ivf = filepath.Join(dstDir, "video.ivf")
	ogg = filepath.Join(dstDir, "video.ogg")

	// видео -> VP8 IVF
	if err = run(ctx, "ffmpeg", "-y", "-i", src,
		"-an",
		"-c:v", "libvpx", "-b:v", "500k",
		"-vf", "scale=-2:360", "-r", "25",
		"-deadline", "realtime", "-cpu-used", "8",
		"-f", "ivf", ivf,
	); err != nil {
		return "", "", fmt.Errorf("video->ivf: %w", err)
	}

	// аудио дорожка (если есть) -> ogg/opus. Если звука нет — пустой тихий трек.
	if err = run(ctx, "ffmpeg", "-y", "-i", src,
		"-vn", "-ac", "1", "-ar", "48000",
		"-c:a", "libopus", "-b:a", "24k",
		"-f", "ogg", ogg,
	); err != nil {
		// видео без звука — делаем тишину на длину видео
		if silErr := silentOgg(ctx, ivf, ogg); silErr != nil {
			return "", "", fmt.Errorf("video audio: %w", err)
		}
	}
	return ivf, ogg, nil
}

// silentOgg генерит тихий opus такой же длительности, как видео.
func silentOgg(ctx context.Context, refVideo, dst string) error {
	return run(ctx, "ffmpeg", "-y",
		"-f", "lavfi", "-i", "anullsrc=channel_layout=mono:sample_rate=48000",
		"-i", refVideo, "-shortest",
		"-c:a", "libopus", "-b:a", "16k", "-f", "ogg", dst,
	)
}

func tmpDir() (string, func(), error) {
	d, err := os.MkdirTemp("", "tgbridge-*")
	if err != nil {
		return "", nil, err
	}
	return d, func() { _ = os.RemoveAll(d) }, nil
}
