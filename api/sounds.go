package main

// Звуковая аптечка палат: CRUD через roomDB (SQLite).

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const (
	maxSoundBytes = 2 << 20 // 2 MiB на файл — короткие семплы для soundboard
	maxSounds     = 200     // потолок на палату (anti-flood)
	soundsSub     = "sounds"
)

type soundboard struct {
	db *roomDB
}

type soundMeta struct {
	ID    string `json:"id"`    // идентификатор в хранилище (rand + ext)
	Label string `json:"label"` // отображаемое имя кнопки
	URL   string `json:"url"`   // /api/rooms/<room>/sounds/<id>
}

func soundURL(room, id string) string {
	return "/api/rooms/" + room + "/" + soundsSub + "/" + id
}

// разрешённые расширения -> content-type
var soundExt = map[string]string{
	".mp3":  "audio/mpeg",
	".ogg":  "audio/ogg",
	".oga":  "audio/ogg",
	".wav":  "audio/wav",
	".m4a":  "audio/mp4",
	".webm": "audio/webm",
	".opus": "audio/opus",
}

// sanitizeLabel → общий sanitizeText (main.go): обрезка 32 + def "звук".
func sanitizeLabel(s string) string { return sanitizeText(s, 32, "звук") }

// GET /api/rooms/<room>/sounds -> список звуков палаты.
// Несуществующая палата (её ещё никто не открывал) — пустой список, не ошибка.
func (sb *soundboard) list(w http.ResponseWriter, _ *http.Request, room string) {
	out := sb.db.soundList(room)
	if out == nil {
		out = []soundMeta{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// sniffAudio проверяет, что содержимое ДЕЙСТВИТЕЛЬНО аудио указанного типа —
// по magic bytes, а не по расширению. Публичная заливка: расширению не доверяем.
func sniffAudio(head []byte, ext string) bool {
	has := func(sig string) bool { return len(head) >= len(sig) && string(head[:len(sig)]) == sig }
	switch ext {
	case ".mp3":
		return has("ID3") || (len(head) >= 2 && head[0] == 0xFF && (head[1]&0xE0) == 0xE0)
	case ".ogg", ".oga", ".opus":
		return has("OggS")
	case ".wav":
		return has("RIFF") && len(head) >= 12 && string(head[8:12]) == "WAVE"
	case ".webm":
		return len(head) >= 4 && head[0] == 0x1A && head[1] == 0x45 && head[2] == 0xDF && head[3] == 0xA3
	case ".m4a":
		return len(head) >= 12 && string(head[4:8]) == "ftyp"
	}
	return false
}

// POST /api/rooms/<room>/sounds (multipart: file, label) -> загрузка.
// Открыто для всех, но:
//   - размер <= maxSoundBytes
//   - расширение из whitelist И magic bytes совпадают
//   - потолок maxSounds файлов на палату (anti-flood)
func (sb *soundboard) upload(w http.ResponseWriter, r *http.Request, room string) {
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

	// sniff: читаем голову, проверяем сигнатуру, потом читаем всё
	head := make([]byte, 16)
	n, _ := io.ReadFull(file, head)
	head = head[:n]
	if !sniffAudio(head, ext) {
		http.Error(w, "file content is not valid audio for this extension", http.StatusBadRequest)
		return
	}

	label := sanitizeLabel(r.FormValue("label"))
	if label == "звук" {
		label = sanitizeLabel(strings.TrimSuffix(hdr.Filename, ext))
	}

	if sb.db.soundCount(room) >= maxSounds {
		http.Error(w, "soundboard full (limit reached)", http.StatusConflict)
		return
	}
	// head уже снят для sniff — доклеиваем его к остатку потока
	data, err := io.ReadAll(io.LimitReader(io.MultiReader(bytes.NewReader(head), file), maxSoundBytes))
	if err != nil {
		http.Error(w, "read", http.StatusInternalServerError)
		return
	}
	id := randHex() + ext
	if err := sb.db.soundPut(room, id, ext, label, data); err != nil {
		http.Error(w, "store", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(soundMeta{ID: id, Label: label, URL: soundURL(room, id)})
}

// GET /api/rooms/<room>/sounds/<id> -> отдать звук.
func (sb *soundboard) serve(w http.ResponseWriter, r *http.Request, room, id string) {
	// защита от path traversal: только базовое имя, известное расширение
	if id == "" || id != filepath.Base(id) {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	ext, _, data, at, ok := sb.db.soundGet(room, id)
	ct, known := soundExt[strings.ToLower(ext)]
	if !ok || !known {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	serveBlob(w, r, id, ct, "public, max-age=31536000, immutable", data, at)
}

// DELETE /api/rooms/<room>/sounds/<id> -> удалить звук из аптечки палаты.
// Без авторизации: что "лишнее" решает любой пациент (как в галерее портретов).
// id — серверный рандом, не пользовательская строка.
func (sb *soundboard) remove(w http.ResponseWriter, _ *http.Request, room, id string) {
	if id == "" || id != filepath.Base(id) || soundExt[strings.ToLower(filepath.Ext(id))] == "" {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := sb.db.soundDelete(room, id); err != nil {
		http.Error(w, "store", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// delete удаляет звук палаты. Внутренний метод, переиспользуется голосованием
// (votes.go). Нет записи → не ошибка (идемпотентно).
func (sb *soundboard) delete(room, id string) error {
	if id == "" || id != filepath.Base(id) || soundExt[strings.ToLower(filepath.Ext(id))] == "" {
		return os.ErrInvalid
	}
	return sb.db.soundDelete(room, id)
}

// dispatch роутит /api/rooms/<room>/sounds[/<id>] по методу.
func (sb *soundboard) dispatch(w http.ResponseWriter, r *http.Request, room string, rest []string) {
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			sb.list(w, r, room)
		case http.MethodPost:
			sb.upload(w, r, room)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	if len(rest) == 1 {
		id := rest[0]
		switch r.Method {
		case http.MethodGet:
			sb.serve(w, r, room, id)
		case http.MethodDelete:
			sb.remove(w, r, room, id)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	http.Error(w, "not found", http.StatusNotFound)
}
