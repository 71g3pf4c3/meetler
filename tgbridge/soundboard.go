package main

// Транскодирует медиа из Telegram и загружает звук в API по SOUNDSVC_URL.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"time"
)

func (a *app) saveToSoundboard(ctx context.Context, palata, fileID, label string) error {
	if a.isProtected(palata) {
		return a.errProtected(palata)
	}
	dir, cleanup, err := tmpDir()
	if err != nil {
		return err
	}
	defer cleanup()

	src, err := a.download(ctx, fileID, dir)
	if err != nil {
		return err
	}
	// в ogg/opus 48k mono, обрезка до maxPlaySec — валидно для sniffAudio (OggS)
	ogg, err := toOgg(ctx, src, dir)
	if err != nil {
		return fmt.Errorf("transcode: %w", err)
	}
	return a.uploadSound(ctx, palata, ogg, label)
}

// uploadSound postит ogg-файл в api как multipart (file + label).
func (a *app) uploadSound(ctx context.Context, palata, oggPath, label string) error {
	f, err := os.Open(oggPath)
	if err != nil {
		return err
	}
	defer f.Close()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	// имя файла с .ogg — api берёт расширение из него для whitelist/sniff
	fw, err := mw.CreateFormFile("file", "tg.ogg")
	if err != nil {
		return err
	}
	if _, err := io.Copy(fw, f); err != nil {
		return err
	}
	if label != "" {
		_ = mw.WriteField("label", label)
	}
	_ = mw.Close()

	url := a.soundsvcURL + "/api/rooms/" + palata + "/sounds"
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(c, http.MethodPost, url, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	cookies, err := backendCookies(c, a.soundsvcURL, os.Getenv("APP_PASSWORD"))
	if err != nil {
		return err
	}
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("soundsvc unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("soundsvc %d: %s", resp.StatusCode, string(b))
	}
	return nil
}
