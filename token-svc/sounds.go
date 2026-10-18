package main

// Звуковая аптечка ПАЛАТЫ: загрузка/листинг/удаление звуков.
// Файлы живут в ROOMS_DIR/<room>/sounds — у каждой палаты своя аптечка,
// при закрытии палаты удаляются каскадом вместе с ней.
// Каждый звук на фронте становится кнопкой (как ГОЙДА).

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
	maxSoundBytes = 2 << 20 // 2 MiB на файл — короткие семплы для soundboard
	maxSounds     = 200     // потолок на палату (anti-flood)
	soundsSub     = "sounds"
)

type soundboard struct {
	root string // ROOMS_DIR; звуки палаты: root/<room>/sounds
	mu   sync.Mutex
}

type soundMeta struct {
	ID    string `json:"id"`    // имя файла в хранилище (rand + ext)
	Label string `json:"label"` // отображаемое имя кнопки
	URL   string `json:"url"`   // /api/rooms/<room>/sounds/<id>
}

func newSoundboard(root string) *soundboard {
	if root == "" {
		root = "/data/rooms"
	}
	return &soundboard{root: root}
}

func (sb *soundboard) dir(room string) string {
	return filepath.Join(sb.root, room, soundsSub)
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

func sanitizeLabel(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 32 {
		s = s[:32]
	}
	s = strings.Map(func(r rune) rune {
		if r < 0x20 {
			return -1
		}
		return r
	}, s)
	if s == "" {
		s = "звук"
	}
	return s
}

func randName() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// GET /api/rooms/<room>/sounds -> список звуков палаты.
// Label берём из .meta-файла (label\n), если есть.
// Несуществующая палата (её ещё никто не открывал) — пустой список, не ошибка.
func (sb *soundboard) list(w http.ResponseWriter, _ *http.Request, room string) {
	entries, err := os.ReadDir(sb.dir(room))
	if err != nil {
		if os.IsNotExist(err) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("[]"))
			return
		}
		http.Error(w, "read dir", http.StatusInternalServerError)
		return
	}
	var out []soundMeta
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		ext := strings.ToLower(filepath.Ext(name))
		if _, ok := soundExt[ext]; !ok {
			continue // пропускаем .meta и прочее
		}
		label := name
		if b, err := os.ReadFile(filepath.Join(sb.dir(room), name+".meta")); err == nil {
			label = strings.TrimSpace(string(b))
		}
		out = append(out, soundMeta{ID: name, Label: label, URL: soundURL(room, name)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
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

func (sb *soundboard) count(room string) int {
	entries, _ := os.ReadDir(sb.dir(room))
	n := 0
	for _, e := range entries {
		if !e.IsDir() && soundExt[strings.ToLower(filepath.Ext(e.Name()))] != "" {
			n++
		}
	}
	return n
}

// POST /api/rooms/<room>/sounds (multipart: file, label) -> загрузка.
// Открыто для всех, но:
//   - размер <= maxSoundBytes
//   - расширение из whitelist И magic bytes совпадают
//   - потолок maxSounds файлов на палату (anti-flood)
//
// Каталог палаты создаётся на лету: залить звук можно и до первого входа.
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

	// sniff: читаем голову, проверяем сигнатуру, потом пишем голову+остаток
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

	sb.mu.Lock()
	defer sb.mu.Unlock()

	if sb.count(room) >= maxSounds {
		http.Error(w, "soundboard full (limit reached)", http.StatusConflict)
		return
	}

	dir := sb.dir(room)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		http.Error(w, "store", http.StatusInternalServerError)
		return
	}
	id := randName() + ext
	dst, err := os.Create(filepath.Join(dir, id))
	if err != nil {
		http.Error(w, "store", http.StatusInternalServerError)
		return
	}
	// голова + остаток, но не больше maxSoundBytes
	lr := io.LimitReader(io.MultiReader(strings.NewReader(string(head)), file), maxSoundBytes)
	if _, err := io.Copy(dst, lr); err != nil {
		_ = dst.Close()
		_ = os.Remove(filepath.Join(dir, id))
		http.Error(w, "write", http.StatusInternalServerError)
		return
	}
	_ = dst.Close()
	_ = os.WriteFile(filepath.Join(dir, id+".meta"), []byte(label), 0o644)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(soundMeta{ID: id, Label: label, URL: soundURL(room, id)})
}

// GET /api/rooms/<room>/sounds/<id> -> отдать файл звука.
func (sb *soundboard) serve(w http.ResponseWriter, r *http.Request, room, id string) {
	// защита от path traversal: только базовое имя, известное расширение
	if id == "" || id != filepath.Base(id) {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	ext := strings.ToLower(filepath.Ext(id))
	ct, ok := soundExt[ext]
	if !ok {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	f, err := os.Open(filepath.Join(sb.dir(room), id))
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeContent(w, r, id, time.Now(), f)
}

// DELETE /api/rooms/<room>/sounds/<id> -> удалить звук из аптечки палаты.
// Без авторизации: что "лишнее" решает любой пациент (как в галерее портретов).
// id — серверный рандом, не пользовательская строка.
func (sb *soundboard) remove(w http.ResponseWriter, _ *http.Request, room, id string) {
	if id == "" || id != filepath.Base(id) {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if _, ok := soundExt[strings.ToLower(filepath.Ext(id))]; !ok {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	sb.mu.Lock()
	defer sb.mu.Unlock()
	if err := os.Remove(filepath.Join(sb.dir(room), id)); err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "remove", http.StatusInternalServerError)
		return
	}
	_ = os.Remove(filepath.Join(sb.dir(room), id+".meta")) // метаданные следом
	w.WriteHeader(http.StatusNoContent)
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
