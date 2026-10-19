package main

// Приветствие и прощание палаты: ROOMS_DIR/<room>/<kind>.<ext>.
// При отсутствии файла клиент использует web/welcome.mp3 или web/farewell.mp3.

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// cueKinds — допустимые имена джинглов (защита от произвольного имени файла).
var cueKinds = map[string]bool{"welcome": true, "farewell": true}

func (sb *soundboard) cuePath(room, kind string) (string, bool) {
	dir := filepath.Join(sb.root, room)
	for ext := range soundExt {
		p := filepath.Join(dir, kind+ext)
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	return "", false
}

// GET /api/rooms/<room>/<kind> -> кастомный джингл (или 404 → фронт играет дефолт)
func (sb *soundboard) cueServe(w http.ResponseWriter, r *http.Request, room, kind string) {
	p, ok := sb.cuePath(room, kind)
	if !ok {
		http.Error(w, "no custom "+kind, http.StatusNotFound)
		return
	}
	f, err := os.Open(p)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", soundExt[strings.ToLower(filepath.Ext(p))])
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, filepath.Base(p), time.Now(), f)
}

// POST /api/rooms/<room>/<kind> (multipart: file) -> задать джингл.
func (sb *soundboard) cueUpload(w http.ResponseWriter, r *http.Request, room, kind string) {
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
	// снести предыдущий джингл этого вида любого расширения — одна ячейка
	for e := range soundExt {
		_ = os.Remove(filepath.Join(dir, kind+e))
	}
	dst, err := os.Create(filepath.Join(dir, kind+ext))
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

// DELETE /api/rooms/<room>/<kind> -> вернуть дефолтный джингл.
func (sb *soundboard) cueDelete(w http.ResponseWriter, _ *http.Request, room, kind string) {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	dir := filepath.Join(sb.root, room)
	for e := range soundExt {
		_ = os.Remove(filepath.Join(dir, kind+e))
	}
	w.WriteHeader(http.StatusNoContent)
}

// cueDispatch обслуживает и welcome, и farewell (kind задаёт роутер).
func (sb *soundboard) cueDispatch(w http.ResponseWriter, r *http.Request, room, kind string, rest []string) {
	if len(rest) != 0 || !cueKinds[kind] {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		sb.cueServe(w, r, room, kind)
	case http.MethodPost:
		sb.cueUpload(w, r, room, kind)
	case http.MethodDelete:
		sb.cueDelete(w, r, room, kind)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
