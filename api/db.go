package main

// SQLite-хранилище палатного контента: звуковая аптечка, галерея портретов,
// джинглы welcome/farewell. Заменило файловое хранилище с sidecar-файлами
// (.meta/.likes) и отдельными файлами джинглов: одна БД, атомарные CRUD,
// бэкап = копия файла (или VACUUM INTO).
//
// Блобы живут в БД: лимиты загрузок малы (2 MiB звук, 4 MiB аватар),
// отдельная файловая система с её race-условиями не нужна.
//
// Драйвер modernc.org/sqlite — pure Go, без CGO.

import (
	"bytes"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS sounds (
	room       TEXT NOT NULL,
	id         TEXT NOT NULL, -- randHex() + ".ext", иммутабелен — используется как ETag
	ext        TEXT NOT NULL,
	label      TEXT NOT NULL DEFAULT '',
	data       BLOB NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (room, id)
);
CREATE TABLE IF NOT EXISTS avatars (
	room       TEXT NOT NULL,
	id         TEXT NOT NULL, -- randHex() + ".ext", иммутабелен — используется как ETag
	ext        TEXT NOT NULL,
	data       BLOB NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (room, id)
);
CREATE TABLE IF NOT EXISTS avatar_likes (
	room    TEXT NOT NULL,
	avatar  TEXT NOT NULL,
	voter   TEXT NOT NULL, -- анонимный client-id браузера (дедуп лайков)
	PRIMARY KEY (room, avatar, voter)
);
CREATE TABLE IF NOT EXISTS cues (
	room       TEXT NOT NULL,
	kind       TEXT NOT NULL, -- welcome | farewell
	ext        TEXT NOT NULL,
	data       BLOB NOT NULL,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (room, kind)
);
CREATE TABLE IF NOT EXISTS rooms (
	name       TEXT PRIMARY KEY,
	created_at INTEGER NOT NULL
);
`

// roomDB — обёртка над sql.DB для всех палатных хранилищ.
// Каждый метод — одна атомарная операция; транзакции не нужны.
// SetMaxOpenConns(1): sqlite — один писатель, при низком RPS сериализация
// через пул дешевле ретраев на SQLITE_BUSY.
type roomDB struct {
	db *sql.DB
}

// openRoomDB открывает (создаёт) БД по path и применяет схему.
func openRoomDB(path string) (*roomDB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &roomDB{db: db}, nil
}

func (r *roomDB) Close() error { return r.db.Close() }

// --- звуковая аптечка ---

func (r *roomDB) soundPut(room, id, ext, label string, data []byte) error {
	_, err := r.db.Exec(`INSERT INTO sounds (room, id, ext, label, data, created_at) VALUES (?,?,?,?,?,?)`,
		room, id, ext, label, data, time.Now().Unix())
	return err
}

// soundList: порядок стабилен по label — как в прежней файловой версии.
func (r *roomDB) soundList(room string) []soundMeta {
	rows, err := r.db.Query(`SELECT id, label FROM sounds WHERE room=? ORDER BY label, id`, room)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []soundMeta
	for rows.Next() {
		var m soundMeta
		if err := rows.Scan(&m.ID, &m.Label); err != nil {
			continue
		}
		m.URL = soundURL(room, m.ID)
		out = append(out, m)
	}
	return out
}

// soundGet возвращает ext, label, данные и время создания.
func (r *roomDB) soundGet(room, id string) (string, string, []byte, time.Time, bool) {
	var ext, label string
	var data []byte
	var at int64
	err := r.db.QueryRow(`SELECT ext, label, data, created_at FROM sounds WHERE room=? AND id=?`, room, id).
		Scan(&ext, &label, &data, &at)
	if err != nil {
		return "", "", nil, time.Time{}, false
	}
	return ext, label, data, time.Unix(at, 0), true
}

// soundDelete идемпотентна: нет записи — не ошибка.
func (r *roomDB) soundDelete(room, id string) error {
	_, err := r.db.Exec(`DELETE FROM sounds WHERE room=? AND id=?`, room, id)
	return err
}

func (r *roomDB) soundCount(room string) int {
	var n int
	if err := r.db.QueryRow(`SELECT count(*) FROM sounds WHERE room=?`, room).Scan(&n); err != nil {
		return 0
	}
	return n
}

// --- галерея портретов ---

func (r *roomDB) avatarPut(room, id, ext string, data []byte) error {
	_, err := r.db.Exec(`INSERT INTO avatars (room, id, ext, data, created_at) VALUES (?,?,?,?,?)`,
		room, id, ext, data, time.Now().Unix())
	return err
}

// avatarList: порядок стабилен по id — от него зависит детерминированный
// выбор аватарки хешем на клиентах (менять нельзя).
func (r *roomDB) avatarList(room string) []avatarMeta {
	rows, err := r.db.Query(`SELECT a.id, count(l.voter) FROM avatars a
		LEFT JOIN avatar_likes l ON l.room=a.room AND l.avatar=a.id
		WHERE a.room=? GROUP BY a.id ORDER BY a.id`, room)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make([]avatarMeta, 0, 8)
	for rows.Next() {
		var m avatarMeta
		if err := rows.Scan(&m.ID, &m.Likes); err != nil {
			continue
		}
		m.URL = avatarURL(room, m.ID)
		out = append(out, m)
	}
	return out
}

func (r *roomDB) avatarGet(room, id string) (string, []byte, time.Time, bool) {
	var ext string
	var data []byte
	var at int64
	err := r.db.QueryRow(`SELECT ext, data, created_at FROM avatars WHERE room=? AND id=?`, room, id).
		Scan(&ext, &data, &at)
	if err != nil {
		return "", nil, time.Time{}, false
	}
	return ext, data, time.Unix(at, 0), true
}

// avatarLikeToggle переключает лайк voter и возвращает новое состояние.
func (r *roomDB) avatarLikeToggle(room, id, voter string) (likes int, liked bool, ok bool) {
	if _, err := r.db.Exec(`INSERT INTO avatar_likes (room, avatar, voter) VALUES (?,?,?)`, room, id, voter); err == nil {
		liked = true
	} else {
		if _, err := r.db.Exec(`DELETE FROM avatar_likes WHERE room=? AND avatar=? AND voter=?`, room, id, voter); err != nil {
			return 0, false, false
		}
	}
	if err := r.db.QueryRow(`SELECT count(*) FROM avatar_likes WHERE room=? AND avatar=?`, room, id).Scan(&likes); err != nil {
		return 0, false, false
	}
	return likes, liked, true
}

// avatarDelete идемпотентна; лайки сносятся каскадно.
func (r *roomDB) avatarDelete(room, id string) error {
	if _, err := r.db.Exec(`DELETE FROM avatar_likes WHERE room=? AND avatar=?`, room, id); err != nil {
		return err
	}
	_, err := r.db.Exec(`DELETE FROM avatars WHERE room=? AND id=?`, room, id)
	return err
}

func (r *roomDB) avatarCount(room string) int {
	var n int
	if err := r.db.QueryRow(`SELECT count(*) FROM avatars WHERE room=?`, room).Scan(&n); err != nil {
		return 0
	}
	return n
}

// --- джинглы welcome/farewell ---

func (r *roomDB) cueGet(room, kind string) (string, []byte, bool) {
	var ext string
	var data []byte
	err := r.db.QueryRow(`SELECT ext, data FROM cues WHERE room=? AND kind=?`, room, kind).Scan(&ext, &data)
	return ext, data, err == nil
}

func (r *roomDB) cueSet(room, kind, ext string, data []byte) error {
	_, err := r.db.Exec(`INSERT INTO cues (room, kind, ext, data, updated_at) VALUES (?,?,?,?,?)
		ON CONFLICT (room, kind) DO UPDATE SET ext=excluded.ext, data=excluded.data, updated_at=excluded.updated_at`,
		room, kind, ext, data, time.Now().Unix())
	return err
}

func (r *roomDB) cueDelete(room, kind string) error {
	_, err := r.db.Exec(`DELETE FROM cues WHERE room=? AND kind=?`, room, kind)
	return err
}

// --- реестр палат ---

// roomRegister: INSERT OR IGNORE — палата попадает в реестр при первом
// обращении (токен, upload, вход).
func (r *roomDB) roomRegister(room string) {
	_, _ = r.db.Exec(`INSERT OR IGNORE INTO rooms (name, created_at) VALUES (?,?)`, room, time.Now().Unix())
}

// roomNames возвращает персистентный реестр палат.
func (r *roomDB) roomNames() map[string]int64 {
	rows, err := r.db.Query(`SELECT name, created_at FROM rooms`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var name string
		var at int64
		if err := rows.Scan(&name, &at); err == nil {
			out[name] = at
		}
	}
	return out
}

// roomDeleteCascade сносит всё содержимое палаты из БД (кроме реестра —
// его чистит отдельным запросом closeRoom).
func (r *roomDB) roomDeleteCascade(room string) {
	_, _ = r.db.Exec(`DELETE FROM avatar_likes WHERE room=?`, room)
	_, _ = r.db.Exec(`DELETE FROM avatars WHERE room=?`, room)
	_, _ = r.db.Exec(`DELETE FROM sounds WHERE room=?`, room)
	_, _ = r.db.Exec(`DELETE FROM cues WHERE room=?`, room)
}

func (r *roomDB) roomUnregister(room string) {
	_, _ = r.db.Exec(`DELETE FROM rooms WHERE name=?`, room)
}

// --- отдача блоба через HTTP ---

// serveBlob: http.ServeContent сохраняет range-запросы и If-None-Match по
// ETag = id (блобы иммутабельны: новая загрузка = новый id).
func serveBlob(w http.ResponseWriter, r *http.Request, id, contentType, cache string, data []byte, modTime time.Time) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", cache)
	w.Header().Set("ETag", `"`+id+`"`)
	http.ServeContent(w, r, id, modTime, bytes.NewReader(data))
}

// --- миграция с файлового хранилища (однократная, идемпотентная) ---
//
// До sqlite звуки/аватары/джинглы жили файлами в ROOMS_DIR/<room>/… с
// sidecar-файлами .meta/.likes. При старте сканируем старую структуру и
// переносим в БД. Файлы НЕ удаляются — после проверки их можно убрать руками.
// Повторный запуск безопасен: INSERT упирается в PRIMARY KEY.

func (r *roomDB) migrateFromFS(root string) {
	n := 0
	rooms, err := os.ReadDir(root)
	if err != nil {
		return // старой структуры нет — мигрировать нечего
	}
	for _, rd := range rooms {
		if !rd.IsDir() {
			continue
		}
		room := rd.Name()
		r.roomRegister(room) // каталог палаты = запись в реестре
		n += r.migrateSounds(filepath.Join(root, room, soundsSub), room)
		n += r.migrateAvatars(filepath.Join(root, room, avatarsSub), room)
		n += r.migrateCues(filepath.Join(root, room), room)
	}
	if n > 0 {
		log.Printf("migrate: перенесено %d записей из файлового хранилища в БД (старые файлы оставлены)", n)
	}
}

func (r *roomDB) migrateSounds(dir, room string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		name := e.Name()
		ext := strings.ToLower(filepath.Ext(name))
		if e.IsDir() || soundExt[ext] == "" {
			continue // .meta и прочий мусор отсеян по расширению
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		label := ""
		if b, err := os.ReadFile(filepath.Join(dir, name+".meta")); err == nil {
			label = strings.TrimSpace(string(b))
		}
		if err := r.soundPut(room, name, ext, label, data); err == nil {
			n++
		}
	}
	return n
}

func (r *roomDB) migrateAvatars(dir, room string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		name := e.Name()
		ext := strings.ToLower(filepath.Ext(name))
		if e.IsDir() || ext == ".likes" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if err := r.avatarPut(room, name, ext, data); err == nil {
			n++
		}
		// лайки хранились sidecar-файлом <name>.likes — по voter на строку
		if b, err := os.ReadFile(filepath.Join(dir, name+".likes")); err == nil {
			for _, voter := range strings.Split(string(b), "\n") {
				if voter = strings.TrimSpace(voter); voter != "" {
					_, _ = r.db.Exec(`INSERT OR IGNORE INTO avatar_likes (room, avatar, voter) VALUES (?,?,?)`, room, name, voter)
				}
			}
		}
	}
	return n
}

func (r *roomDB) migrateCues(dir, room string) int {
	n := 0
	for kind := range cueKinds {
		for ext := range soundExt {
			data, err := os.ReadFile(filepath.Join(dir, kind+ext))
			if err != nil {
				continue
			}
			if err := r.cueSet(room, kind, ext, data); err == nil {
				n++
			}
		}
	}
	return n
}
