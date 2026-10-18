package main

// Глобальный soundboard: загрузка/листинг пользовательских звуков.
// Файлы хранятся на диске (SOUNDS_DIR, по умолчанию /data/sounds), общие для всех
// палат и пользователей. Каждый звук на фронте становится кнопкой (как ГОЙДА).

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
	maxSounds     = 200     // потолок на весь глобальный soundboard (anti-flood)
	soundsSub     = "sounds"
)

type soundboard struct {
	dir string
	mu  sync.Mutex // сериализуем запись метаданных
}

type soundMeta struct {
	ID    string `json:"id"`    // имя файла в хранилище (rand + ext)
	Label string `json:"label"` // отображаемое имя кнопки
	URL   string `json:"url"`   // путь для проигрывания: /api/sounds/<id>
}

func newSoundboard(dir string) (*soundboard, error) {
	if dir == "" {
		dir = "/data/" + soundsSub
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &soundboard{dir: dir}, nil
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

// GET /api/sounds -> список звуков. Label берём из .meta-файла (label\n), если есть.
func (sb *soundboard) list(w http.ResponseWriter, _ *http.Request) {
	entries, err := os.ReadDir(sb.dir)
	if err != nil {
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
		if b, err := os.ReadFile(filepath.Join(sb.dir, name+".meta")); err == nil {
			label = strings.TrimSpace(string(b))
		}
		out = append(out, soundMeta{ID: name, Label: label, URL: "/api/sounds/" + name})
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

func (sb *soundboard) count() int {
	entries, _ := os.ReadDir(sb.dir)
	n := 0
	for _, e := range entries {
		if !e.IsDir() && soundExt[strings.ToLower(filepath.Ext(e.Name()))] != "" {
			n++
		}
	}
	return n
}

// POST /api/sounds (multipart: file, label) -> загрузка. Открыто для всех, но:
//   - размер <= maxSoundBytes
//   - расширение из whitelist И magic bytes совпадают
//   - общий потолок maxSounds файлов (anti-flood)
func (sb *soundboard) upload(w http.ResponseWriter, r *http.Request) {
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

	if sb.count() >= maxSounds {
		http.Error(w, "soundboard full (limit reached)", http.StatusConflict)
		return
	}

	id := randName() + ext
	dst, err := os.Create(filepath.Join(sb.dir, id))
	if err != nil {
		http.Error(w, "store", http.StatusInternalServerError)
		return
	}
	// голова + остаток, но не больше maxSoundBytes
	lr := io.LimitReader(io.MultiReader(strings.NewReader(string(head)), file), maxSoundBytes)
	if _, err := io.Copy(dst, lr); err != nil {
		_ = dst.Close()
		_ = os.Remove(filepath.Join(sb.dir, id))
		http.Error(w, "write", http.StatusInternalServerError)
		return
	}
	_ = dst.Close()
	_ = os.WriteFile(filepath.Join(sb.dir, id+".meta"), []byte(label), 0o644)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(soundMeta{ID: id, Label: label, URL: "/api/sounds/" + id})
}

// GET /api/sounds/<id> -> отдать файл звука.
func (sb *soundboard) serve(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/sounds/")
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
	p := filepath.Join(sb.dir, id)
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

// DELETE /api/sounds/<id> -> удалить звук из аптечки. Без авторизации:
// что "лишнее" решает любой пациент (как в галерее портретов).
// id — серверный рандом, не пользовательская строка.
func (sb *soundboard) remove(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/sounds/")
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
	if err := os.Remove(filepath.Join(sb.dir, id)); err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "remove", http.StatusInternalServerError)
		return
	}
	_ = os.Remove(filepath.Join(sb.dir, id+".meta")) // метаданные следом
	w.WriteHeader(http.StatusNoContent)
}

// routes регистрирует хендлеры soundboard на mux.
func (sb *soundboard) routes(mux *http.ServeMux) {
	mux.HandleFunc("/api/sounds", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			sb.list(w, r)
		case http.MethodPost:
			sb.upload(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/sounds/", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			sb.serve(w, r)
		case http.MethodDelete:
			sb.remove(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}
