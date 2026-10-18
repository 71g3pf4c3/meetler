package main

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// TTS через piper (локальный, оффлайн). piper читает текст из stdin и пишет WAV в файл,
// затем конвертим в ogg/opus под publisher.
type TTS struct {
	piperBin  string // путь к бинарю piper
	modelPath string // .onnx голосовая модель
}

func NewTTS(bin, model string) *TTS { return &TTS{piperBin: bin, modelPath: model} }

func (t *TTS) enabled() bool { return t.piperBin != "" && t.modelPath != "" }

// Synthesize превращает текст в .ogg (opus) во временной директории.
func (t *TTS) Synthesize(ctx context.Context, text, dstDir string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("empty text")
	}
	if len(text) > 1000 {
		text = text[:1000] // не даём забить TTS простынёй
	}
	wav := filepath.Join(dstDir, "tts.wav")

	c, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(c, t.piperBin,
		"--model", t.modelPath,
		"--output_file", wav,
	)
	cmd.Stdin = strings.NewReader(text)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("piper: %w: %s", err, string(out))
	}
	// WAV -> ogg/opus
	return toOgg(ctx, wav, dstDir)
}
