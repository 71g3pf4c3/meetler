package main

// Приветствие и прощание палаты (джинглы): cues-таблица roomDB.
// При отсутствии кастомного джингла клиент использует web/welcome.mp3
// или web/farewell.mp3.

import (
	"bytes"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

// cueKinds — допустимые имена джинглов (защита от произвольного kind).
var cueKinds = map[string]bool{"welcome": true, "farewell": true}

// GET /api/rooms/<room>/<kind> -> кастомный джингл (или 404 → фронт играет дефолт)
func (sb *soundboard) cueServe(w http.ResponseWriter, r *http.Request, room, kind string) {
	ext, data, ok := sb.db.cueGet(room, kind)
	if !ok {
		http.Error(w, "no custom "+kind, http.StatusNotFound)
		return
	}
	// кэш: одна «ячейка» на вид — перезагрузка с новым содержимым должна
	// доехать до клиентов, поэтому no-cache (как в прежней версии)
	w.Header().Set("Content-Type", soundExt[ext])
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, kind+ext, time.Time{}, bytes.NewReader(data))
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
	data, err := io.ReadAll(io.LimitReader(io.MultiReader(bytes.NewReader(head), file), maxSoundBytes))
	if err != nil {
		http.Error(w, "read", http.StatusInternalServerError)
		return
	}
	if err := sb.db.cueSet(room, kind, ext, data); err != nil {
		http.Error(w, "store", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DELETE /api/rooms/<room>/<kind> -> вернуть дефолтный джингл.
func (sb *soundboard) cueDelete(w http.ResponseWriter, _ *http.Request, room, kind string) {
	if err := sb.db.cueDelete(room, kind); err != nil {
		http.Error(w, "store", http.StatusInternalServerError)
		return
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
	case http.MethodGet, http.MethodHead:
		sb.cueServe(w, r, room, kind)
	case http.MethodPost:
		sb.cueUpload(w, r, room, kind)
	case http.MethodDelete:
		sb.cueDelete(w, r, room, kind)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
