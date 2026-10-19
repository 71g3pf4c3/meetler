package main

// Звук приветствия палаты: проигрывается у всех, когда кто-то входит.
// Одна ячейка на палату: ROOMS_DIR/<room>/welcome.<ext>. Если кастомного нет —
// фронт играет дефолт (web/welcome.mp3). Лежит ВНЕ sounds/, чтобы не стать
// кнопкой аптечки. Переиспользует whitelist/sniff из sounds.go.

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (sb *soundboard) welcomePath(room string) (string, bool) {
	dir := filepath.Join(sb.root, room)
	for ext := range soundExt {
		p := filepath.Join(dir, "welcome"+ext)
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	return "", false
}

// GET /api/rooms/<room>/welcome -> кастомный звук приветствия (или 404 → дефолт)
func (sb *soundboard) welcomeServe(w http.ResponseWriter, r *http.Request, room string) {
	p, ok := sb.welcomePath(room)
	if !ok {
		http.Error(w, "no custom welcome", http.StatusNotFound)
		return
	}
	f, err := os.Open(p)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", soundExt[strings.ToLower(filepath.Ext(p))])
	w.Header().Set("Cache-Control", "no-cache") // можно переопределить — не кэшируем жёстко
	http.ServeContent(w, r, filepath.Base(p), time.Now(), f)
}

// POST /api/rooms/<room>/welcome (multipart: file) -> задать звук приветствия.
func (sb *soundboard) welcomeUpload(w http.ResponseWriter, r *http.Request, room string) {
	r.Body = http.MaxBytesReader(w, r.Body, maxSoundBytes+4096)
	if err := r.ParseMultipartForm(maxSoundBytes); err != nil {
		http.Error(w, "too large or bad form (max 2MiB)", http.StatusBadRequest)
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "no file", http.StatusBadRequest)
		return
	}
	defer file.Close()

	ext := strings.ToLower(filepath.Ext(hdr.Filename))
	if _, ok := soundExt[ext]; !ok {
		http.Error(w, "unsupported type (mp3/ogg/wav/m4a/webm/opus)", http.StatusBadRequest)
		return
	}
	head := make([]byte, 16)
	n, _ := io.ReadFull(file, head)
	head = head[:n]
	if !sniffAudio(head, ext) {
		http.Error(w, "file content is not valid audio for this extension", http.StatusBadRequest)
		return
	}

	sb.mu.Lock()
	defer sb.mu.Unlock()

	dir := filepath.Join(sb.root, room)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		http.Error(w, "store", http.StatusInternalServerError)
		return
	}
	// снести предыдущий welcome любого расширения — одна ячейка на палату
	for e := range soundExt {
		_ = os.Remove(filepath.Join(dir, "welcome"+e))
	}
	dst, err := os.Create(filepath.Join(dir, "welcome"+ext))
	if err != nil {
		http.Error(w, "store", http.StatusInternalServerError)
		return
	}
	lr := io.LimitReader(io.MultiReader(strings.NewReader(string(head)), file), maxSoundBytes)
	if _, err := io.Copy(dst, lr); err != nil {
		_ = dst.Close()
		http.Error(w, "write", http.StatusInternalServerError)
		return
	}
	_ = dst.Close()
	w.WriteHeader(http.StatusNoContent)
}

// DELETE /api/rooms/<room>/welcome -> вернуть дефолтный звук приветствия.
func (sb *soundboard) welcomeDelete(w http.ResponseWriter, _ *http.Request, room string) {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	dir := filepath.Join(sb.root, room)
	for e := range soundExt {
		_ = os.Remove(filepath.Join(dir, "welcome"+e))
	}
	w.WriteHeader(http.StatusNoContent)
}

func (sb *soundboard) welcomeDispatch(w http.ResponseWriter, r *http.Request, room string, rest []string) {
	if len(rest) != 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		sb.welcomeServe(w, r, room)
	case http.MethodPost:
		sb.welcomeUpload(w, r, room)
	case http.MethodDelete:
		sb.welcomeDelete(w, r, room)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
