package main

// Глобальный пул аватарок: загрузка/листинг пользовательских картинок.
// Аватарка участника = детерминированный хеш его sid по стабильному списку
// (клиентская логика) — все зрители видят одну и ту же картинку без сигналинга.
// Хранилище как у soundboard: AVATARS_DIR (по умолчанию /data/avatars).

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	maxAvatarBytes = 4 << 20 // 4 MiB на картинку (фото с телефона)
	maxAvatars     = 200     // потолок пула (anti-flood)
)

type avatarStore struct {
	dir string
	mu  sync.Mutex // сериализуем запись
}

type avatarMeta struct {
	ID  string `json:"id"`
	URL string `json:"url"` // /api/avatars/<id>
}

func newAvatarStore(dir string) (*avatarStore, error) {
	if dir == "" {
		dir = "/data/avatars"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &avatarStore{dir: dir}, nil
}

// разрешённые расширения -> content-type
var avatarExt = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".gif":  "image/gif",
	".webp": "image/webp",
}

func avatarRandName() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// sniffImage проверяет содержимое по magic bytes (расширению не доверяем).
func sniffImage(head []byte, ext string) bool {
	has := func(sig string) bool { return len(head) >= len(sig) && string(head[:len(sig)]) == sig }
	switch ext {
	case ".jpg", ".jpeg":
		return len(head) >= 3 && head[0] == 0xFF && head[1] == 0xD8 && head[2] == 0xFF
	case ".png":
		return has("\x89PNG\r\n\x1a\n")
	case ".gif":
		return has("GIF87a") || has("GIF89a")
	case ".webp":
		return has("RIFF") && len(head) >= 12 && string(head[8:12]) == "WEBP"
	}
	return false
}

func (as *avatarStore) count() int {
	entries, _ := os.ReadDir(as.dir)
	n := 0
	for _, e := range entries {
		if !e.IsDir() && avatarExt[strings.ToLower(filepath.Ext(e.Name()))] != "" {
			n++
		}
	}
	return n
}

// GET /api/avatars -> список. ПОРЯДОК СТАБИЛЕН (сортировка по id) — от него
// зависит детерминированный выбор аватарки хешем на клиентах.
func (as *avatarStore) list(w http.ResponseWriter, _ *http.Request) {
	entries, err := os.ReadDir(as.dir)
	if err != nil {
		http.Error(w, "read dir", http.StatusInternalServerError)
		return
	}
	out := make([]avatarMeta, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if _, ok := avatarExt[strings.ToLower(filepath.Ext(name))]; !ok {
			continue
		}
		out = append(out, avatarMeta{ID: name, URL: "/api/avatars/" + name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// POST /api/avatars (multipart: file) -> загрузка. Открыто для всех, но:
//   - размер <= maxAvatarBytes
//   - расширение из whitelist И magic bytes совпадают
//   - потолок maxAvatars файлов
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
	if _, ok := avatarExt[ext]; !ok {
		http.Error(w, "unsupported type (jpg/png/gif/webp)", http.StatusBadRequest)
		return
	}

	head := make([]byte, 16)
	n, _ := io.ReadFull(file, head)
	head = head[:n]
	if !sniffImage(head, ext) {
		http.Error(w, "file content is not a valid image for this extension", http.StatusBadRequest)
		return
	}

	as.mu.Lock()
	defer as.mu.Unlock()

	if as.count() >= maxAvatars {
		http.Error(w, "avatar pool full (limit reached)", http.StatusConflict)
		return
	}

	id := avatarRandName() + ext
	dst, err := os.Create(filepath.Join(as.dir, id))
	if err != nil {
		http.Error(w, "store", http.StatusInternalServerError)
		return
	}
	lr := io.LimitReader(io.MultiReader(strings.NewReader(string(head)), file), maxAvatarBytes)
	if _, err := io.Copy(dst, lr); err != nil {
		_ = dst.Close()
		_ = os.Remove(filepath.Join(as.dir, id))
		http.Error(w, "write", http.StatusInternalServerError)
		return
	}
	_ = dst.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(avatarMeta{ID: id, URL: "/api/avatars/" + id})
}

// GET /api/avatars/<id> -> отдать картинку.
func (as *avatarStore) serve(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/avatars/")
	// path traversal: только базовое имя с известным расширением
	if id == "" || id != filepath.Base(id) {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	ct, ok := avatarExt[strings.ToLower(filepath.Ext(id))]
	if !ok {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	p := filepath.Join(as.dir, id)
	f, err := os.Open(p)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeContent(w, r, id, time.Now(), f)
}

// DELETE /api/avatars/<id> -> убрать картинку из пула.
// Намеренно без авторизации: что "лишнее" решает любой пациент.
// (id — серверный рандом, не пользовательская строка: безопасно)
func (as *avatarStore) remove(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/avatars/")
	if id == "" || id != filepath.Base(id) {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if _, ok := avatarExt[strings.ToLower(filepath.Ext(id))]; !ok {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	as.mu.Lock()
	defer as.mu.Unlock()
	if err := os.Remove(filepath.Join(as.dir, id)); err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "remove", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (as *avatarStore) routes(mux *http.ServeMux) {
	mux.HandleFunc("/api/avatars", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			as.list(w, r)
		case http.MethodPost:
			as.upload(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/avatars/", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			as.serve(w, r)
		case http.MethodDelete:
			as.remove(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}
