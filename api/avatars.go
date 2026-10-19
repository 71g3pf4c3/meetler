package main

// Портреты палат хранятся в ROOMS_DIR/<room>/avatars.

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
	maxAvatars     = 200     // потолок на палату (anti-flood)
	avatarsSub     = "avatars"
)

type avatarStore struct {
	root string // ROOMS_DIR; портреты палаты: root/<room>/avatars
	mu   sync.Mutex
}

type avatarMeta struct {
	ID    string `json:"id"`
	URL   string `json:"url"`   // /api/rooms/<room>/avatars/<id>
	Likes int    `json:"likes"` // число лайков (чем больше — тем реже выдаётся)
}

func newAvatarStore(root string) *avatarStore {
	if root == "" {
		root = "/data/rooms"
	}
	return &avatarStore{root: root}
}

func (as *avatarStore) dir(room string) string {
	return filepath.Join(as.root, room, avatarsSub)
}

func avatarURL(room, id string) string {
	return "/api/rooms/" + room + "/" + avatarsSub + "/" + id
}

// Лайки портрета хранятся рядом: <dir>/<id>.likes — по одному voter-cid на
// строку (дедуп на браузер). Чем больше лайков — тем реже портрет выдаётся
// (вес в раздаче клиент считает по этому числу).
func (as *avatarStore) likesPath(room, id string) string {
	return filepath.Join(as.dir(room), id+".likes")
}

func (as *avatarStore) readLikers(room, id string) map[string]bool {
	set := map[string]bool{}
	b, err := os.ReadFile(as.likesPath(room, id))
	if err != nil {
		return set
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v := strings.TrimSpace(line); v != "" {
			set[v] = true
		}
	}
	return set
}

func (as *avatarStore) writeLikers(room, id string, set map[string]bool) error {
	var sb strings.Builder
	for v := range set {
		sb.WriteString(v)
		sb.WriteByte('\n')
	}
	return os.WriteFile(as.likesPath(room, id), []byte(sb.String()), 0o644)
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

func (as *avatarStore) count(room string) int {
	entries, _ := os.ReadDir(as.dir(room))
	n := 0
	for _, e := range entries {
		if !e.IsDir() && avatarExt[strings.ToLower(filepath.Ext(e.Name()))] != "" {
			n++
		}
	}
	return n
}

// GET /api/rooms/<room>/avatars -> список портретов палаты.
// ПОРЯДОК СТАБИЛЕН (сортировка по id) — от него зависит детерминированный
// выбор аватарки хешем на клиентах. Несуществующая палата — пустой список.
func (as *avatarStore) list(w http.ResponseWriter, _ *http.Request, room string) {
	entries, err := os.ReadDir(as.dir(room))
	if err != nil {
		if os.IsNotExist(err) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("[]"))
			return
		}
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
		out = append(out, avatarMeta{ID: name, URL: avatarURL(room, name), Likes: len(as.readLikers(room, name))})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// POST /api/rooms/<room>/avatars (multipart: file) -> загрузка. Открыто для
// всех, но:
//   - размер <= maxAvatarBytes
//   - расширение из whitelist И magic bytes совпадают
//   - потолок maxAvatars файлов на палату
//
// Каталог палаты создаётся на лету: принести портрет можно и до входа.
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

	if as.count(room) >= maxAvatars {
		http.Error(w, "avatar pool full (limit reached)", http.StatusConflict)
		return
	}

	dir := as.dir(room)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		http.Error(w, "store", http.StatusInternalServerError)
		return
	}
	id := avatarRandName() + ext
	dst, err := os.Create(filepath.Join(dir, id))
	if err != nil {
		http.Error(w, "store", http.StatusInternalServerError)
		return
	}
	lr := io.LimitReader(io.MultiReader(strings.NewReader(string(head)), file), maxAvatarBytes)
	if _, err := io.Copy(dst, lr); err != nil {
		_ = dst.Close()
		_ = os.Remove(filepath.Join(dir, id))
		http.Error(w, "write", http.StatusInternalServerError)
		return
	}
	_ = dst.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(avatarMeta{ID: id, URL: avatarURL(room, id)})
}

// GET /api/rooms/<room>/avatars/<id> -> отдать картинку.
func (as *avatarStore) serve(w http.ResponseWriter, r *http.Request, room, id string) {
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
	f, err := os.Open(filepath.Join(as.dir(room), id))
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeContent(w, r, id, time.Now(), f)
}

// DELETE /api/rooms/<room>/avatars/<id> -> убрать картинку из галереи палаты.
// Намеренно без авторизации: что "лишнее" решает любой пациент.
// (id — серверный рандом, не пользовательская строка: безопасно)
func (as *avatarStore) remove(w http.ResponseWriter, _ *http.Request, room, id string) {
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
	if err := os.Remove(filepath.Join(as.dir(room), id)); err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "remove", http.StatusInternalServerError)
		return
	}
	_ = os.Remove(as.likesPath(room, id)) // лайки следом
	w.WriteHeader(http.StatusNoContent)
}

// delete удаляет файл портрета палаты. Внутренний метод, переиспользуется
// голосованием (votes.go). Нет файла → не ошибка (идемпотентно).
func (as *avatarStore) delete(room, id string) error {
	if id == "" || id != filepath.Base(id) {
		return os.ErrInvalid
	}
	if _, ok := avatarExt[strings.ToLower(filepath.Ext(id))]; !ok {
		return os.ErrInvalid
	}
	as.mu.Lock()
	defer as.mu.Unlock()
	err := os.Remove(filepath.Join(as.dir(room), id))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	_ = os.Remove(as.likesPath(room, id)) // лайки следом
	return nil
}

// POST /api/rooms/<room>/avatars/<id>/like {voter} -> переключить лайк.
// Возвращает {likes, liked}. Дедуп по voter (анонимный client-id браузера).
func (as *avatarStore) like(w http.ResponseWriter, r *http.Request, room, id string) {
	if id == "" || id != filepath.Base(id) {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if _, ok := avatarExt[strings.ToLower(filepath.Ext(id))]; !ok {
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
	as.mu.Lock()
	defer as.mu.Unlock()
	// портрет должен существовать
	if _, err := os.Stat(filepath.Join(as.dir(room), id)); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	set := as.readLikers(room, id)
	liked := !set[voter] // toggle
	if liked {
		set[voter] = true
	} else {
		delete(set, voter)
	}
	if err := as.writeLikers(room, id, set); err != nil {
		http.Error(w, "store", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"likes": len(set), "liked": liked})
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
