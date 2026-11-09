package main

// Общие портреты: CRUD через roomDB (SQLite).

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const (
	maxAvatarBytes = 4 << 20 // 4 MiB на картинку (фото с телефона)
	maxAvatars     = 200     // общий потолок (anti-flood)
	avatarsSub     = "avatars"
)

type avatarStore struct {
	db *roomDB
}

type avatarMeta struct {
	ID  string `json:"id"`
	URL string `json:"url"` // /api/avatars/<id>
}

func avatarURL(id string) string {
	return "/api/" + avatarsSub + "/" + id
}

// GET /api/avatars -> общий список портретов для всех палат.
func (as *avatarStore) list(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(as.db.avatarList())
}

// POST /api/avatars (multipart: file) -> загрузка. Открыто для
// всех, но:
//   - размер <= maxAvatarBytes
//   - потолок maxAvatars файлов в общем пуле
func (as *avatarStore) upload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxAvatarBytes+4096)
	if err := r.ParseMultipartForm(maxAvatarBytes); err != nil {
		http.Error(w, "too large or bad form (max 4MiB)", http.StatusBadRequest)
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "no file", http.StatusBadRequest)
		return
	}
	defer file.Close()

	ext := strings.ToLower(filepath.Ext(hdr.Filename))

	if as.db.avatarCount() >= maxAvatars {
		http.Error(w, "avatar pool full (limit reached)", http.StatusConflict)
		return
	}
	data, err := io.ReadAll(io.LimitReader(file, maxAvatarBytes))
	if err != nil {
		http.Error(w, "read", http.StatusInternalServerError)
		return
	}
	id := randHex() + ext
	if err := as.db.avatarPut(id, ext, data); err != nil {
		http.Error(w, "store", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(avatarMeta{ID: id, URL: avatarURL(id)})
}

// GET /api/avatars/<id> -> отдать картинку.
func (as *avatarStore) serve(w http.ResponseWriter, r *http.Request, id string) {
	if id == "" || id != filepath.Base(id) {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	_, data, at, ok := as.db.avatarGet(id)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	serveBlob(w, r, id, http.DetectContentType(data), "public, max-age=31536000, immutable", data, at)
}

// DELETE /api/avatars/<id> -> убрать картинку из общей галереи.
// Намеренно без авторизации: что "лишнее" решает любой пациент.
// (id — серверный рандом, не пользовательская строка: безопасно)
func (as *avatarStore) remove(w http.ResponseWriter, _ *http.Request, id string) {
	if id == "" || id != filepath.Base(id) {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := as.db.avatarDelete(id); err != nil {
		http.Error(w, "store", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// delete удаляет общий портрет. Внутренний метод, переиспользуется
// голосованием (votes.go). Нет записи → не ошибка (идемпотентно).
func (as *avatarStore) delete(id string) error {
	if id == "" || id != filepath.Base(id) {
		return os.ErrInvalid
	}
	return as.db.avatarDelete(id)
}

// dispatch роутит /api/avatars[/<id>] по методу.
func (as *avatarStore) dispatch(w http.ResponseWriter, r *http.Request, rest []string) {
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			as.list(w, r)
		case http.MethodPost:
			as.upload(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	if len(rest) == 1 {
		id := rest[0]
		switch r.Method {
		case http.MethodGet:
			as.serve(w, r, id)
		case http.MethodDelete:
			as.remove(w, r, id)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	http.Error(w, "not found", http.StatusNotFound)
}
