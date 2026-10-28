package main

// Портреты палат: CRUD через roomDB (SQLite).

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
	maxAvatars     = 200     // потолок на палату (anti-flood)
	avatarsSub     = "avatars"
)

type avatarStore struct {
	db *roomDB
}

type avatarMeta struct {
	ID    string `json:"id"`
	URL   string `json:"url"`   // /api/rooms/<room>/avatars/<id>
	Likes int    `json:"likes"` // число лайков (чем больше — тем реже выдаётся)
}

func avatarURL(room, id string) string {
	return "/api/rooms/" + room + "/" + avatarsSub + "/" + id
}

// GET /api/rooms/<room>/avatars -> список портретов палаты.
// ПОРЯДОК СТАБИЛЕН (сортировка по id) — от него зависит детерминированный
// выбор аватарки хешем на клиентах. Несуществующая палата — пустой список.
func (as *avatarStore) list(w http.ResponseWriter, _ *http.Request, room string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(as.db.avatarList(room))
}

// POST /api/rooms/<room>/avatars (multipart: file) -> загрузка. Открыто для
// всех, но:
//   - размер <= maxAvatarBytes
//   - потолок maxAvatars файлов на палату
func (as *avatarStore) upload(w http.ResponseWriter, r *http.Request, room string) {
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

	if as.db.avatarCount(room) >= maxAvatars {
		http.Error(w, "avatar pool full (limit reached)", http.StatusConflict)
		return
	}
	data, err := io.ReadAll(io.LimitReader(file, maxAvatarBytes))
	if err != nil {
		http.Error(w, "read", http.StatusInternalServerError)
		return
	}
	id := randHex() + ext
	if err := as.db.avatarPut(room, id, ext, data); err != nil {
		http.Error(w, "store", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(avatarMeta{ID: id, URL: avatarURL(room, id)})
}

// GET /api/rooms/<room>/avatars/<id> -> отдать картинку.
func (as *avatarStore) serve(w http.ResponseWriter, r *http.Request, room, id string) {
	if id == "" || id != filepath.Base(id) {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	_, data, at, ok := as.db.avatarGet(room, id)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	serveBlob(w, r, id, http.DetectContentType(data), "public, max-age=31536000, immutable", data, at)
}

// DELETE /api/rooms/<room>/avatars/<id> -> убрать картинку из галереи палаты.
// Намеренно без авторизации: что "лишнее" решает любой пациент.
// (id — серверный рандом, не пользовательская строка: безопасно)
func (as *avatarStore) remove(w http.ResponseWriter, _ *http.Request, room, id string) {
	if id == "" || id != filepath.Base(id) {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := as.db.avatarDelete(room, id); err != nil {
		http.Error(w, "store", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// delete удаляет портрет палаты (+лайки). Внутренний метод, переиспользуется
// голосованием (votes.go). Нет записи → не ошибка (идемпотентно).
func (as *avatarStore) delete(room, id string) error {
	if id == "" || id != filepath.Base(id) {
		return os.ErrInvalid
	}
	return as.db.avatarDelete(room, id)
}

// POST /api/rooms/<room>/avatars/<id>/like {voter} -> переключить лайк.
// Возвращает {likes, liked}. Дедуп по voter (анонимный client-id браузера).
func (as *avatarStore) like(w http.ResponseWriter, r *http.Request, room, id string) {
	if id == "" || id != filepath.Base(id) {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	var req struct {
		Voter string `json:"voter"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	voter := strings.TrimSpace(req.Voter)
	if l := len(voter); l < 1 || l > 64 {
		http.Error(w, "bad voter", http.StatusBadRequest)
		return
	}
	likes, liked, ok := as.db.avatarLikeToggle(room, id, voter)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"likes": likes, "liked": liked})
}

// dispatch роутит /api/rooms/<room>/avatars[/<id>[/like]] по методу.
func (as *avatarStore) dispatch(w http.ResponseWriter, r *http.Request, room string, rest []string) {
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			as.list(w, r, room)
		case http.MethodPost:
			as.upload(w, r, room)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	if len(rest) == 1 {
		id := rest[0]
		switch r.Method {
		case http.MethodGet:
			as.serve(w, r, room, id)
		case http.MethodDelete:
			as.remove(w, r, room, id)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	if len(rest) == 2 && rest[1] == "like" {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		as.like(w, r, room, rest[0])
		return
	}
	http.Error(w, "not found", http.StatusNotFound)
}
