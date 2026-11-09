package main

// SQLite-хранилище общего контента: звуковая аптечка, галерея портретов,
// джинглы welcome/farewell. Заменило файловое хранилище с sidecar-файлами
// (.meta) и отдельными файлами джинглов: одна БД, атомарные CRUD,
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
	"net/http"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS sounds (
	id         TEXT NOT NULL, -- randHex() + ".ext", иммутабелен — используется как ETag
	ext        TEXT NOT NULL,
	label      TEXT NOT NULL DEFAULT '',
	data       BLOB NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (id)
);
CREATE TABLE IF NOT EXISTS avatars (
	id         TEXT NOT NULL, -- randHex() + ".ext", иммутабелен — используется как ETag
	ext        TEXT NOT NULL,
	data       BLOB NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (id)
);
CREATE TABLE IF NOT EXISTS cues (
	kind       TEXT NOT NULL, -- welcome | farewell
	ext        TEXT NOT NULL,
	data       BLOB NOT NULL,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (kind)
);
CREATE TABLE IF NOT EXISTS rooms (
	name       TEXT PRIMARY KEY,
	created_at INTEGER NOT NULL
);
`

// roomDB хранит общий медиаконтент и отдельный реестр палат.
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
	// Breaking schema change: operators migrate old room-scoped media manually.
	var legacyColumns int
	if err := db.QueryRow(`SELECT count(*) FROM (
		SELECT name FROM pragma_table_info('sounds') UNION ALL
		SELECT name FROM pragma_table_info('avatars') UNION ALL
		SELECT name FROM pragma_table_info('cues')
	) WHERE name='room'`).Scan(&legacyColumns); err != nil {
		_ = db.Close()
		return nil, err
	}
	if legacyColumns > 0 {
		_ = db.Close()
		return nil, fmt.Errorf("room-scoped media schema: manual migration to global media required")
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &roomDB{db: db}, nil
}

func (r *roomDB) Close() error { return r.db.Close() }

// --- звуковая аптечка ---

func (r *roomDB) soundPut(id, ext, label string, data []byte) error {
	_, err := r.db.Exec(`INSERT INTO sounds (id, ext, label, data, created_at) VALUES (?,?,?,?,?)`,
		id, ext, label, data, time.Now().Unix())
	return err
}

// soundList: порядок стабилен по label — как в прежней файловой версии.
func (r *roomDB) soundList() []soundMeta {
	rows, err := r.db.Query(`SELECT id, label FROM sounds ORDER BY label, id`)
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
		m.URL = soundURL(m.ID)
		out = append(out, m)
	}
	return out
}

// soundGet возвращает ext, label, данные и время создания.
func (r *roomDB) soundGet(id string) (string, string, []byte, time.Time, bool) {
	var ext, label string
	var data []byte
	var at int64
	err := r.db.QueryRow(`SELECT ext, label, data, created_at FROM sounds WHERE id=?`, id).
		Scan(&ext, &label, &data, &at)
	if err != nil {
		return "", "", nil, time.Time{}, false
	}
	return ext, label, data, time.Unix(at, 0), true
}

// soundDelete идемпотентна: нет записи — не ошибка.
func (r *roomDB) soundDelete(id string) error {
	_, err := r.db.Exec(`DELETE FROM sounds WHERE id=?`, id)
	return err
}

func (r *roomDB) soundCount() int {
	var n int
	if err := r.db.QueryRow(`SELECT count(*) FROM sounds`).Scan(&n); err != nil {
		return 0
	}
	return n
}

// --- галерея портретов ---

func (r *roomDB) avatarPut(id, ext string, data []byte) error {
	_, err := r.db.Exec(`INSERT INTO avatars (id, ext, data, created_at) VALUES (?,?,?,?)`,
		id, ext, data, time.Now().Unix())
	return err
}

// avatarList возвращает общий список портретов по id.
func (r *roomDB) avatarList() []avatarMeta {
	rows, err := r.db.Query(`SELECT id FROM avatars ORDER BY id`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make([]avatarMeta, 0, 8)
	for rows.Next() {
		var m avatarMeta
		if err := rows.Scan(&m.ID); err != nil {
			continue
		}
		m.URL = avatarURL(m.ID)
		out = append(out, m)
	}
	return out
}

func (r *roomDB) avatarGet(id string) (string, []byte, time.Time, bool) {
	var ext string
	var data []byte
	var at int64
	err := r.db.QueryRow(`SELECT ext, data, created_at FROM avatars WHERE id=?`, id).
		Scan(&ext, &data, &at)
	if err != nil {
		return "", nil, time.Time{}, false
	}
	return ext, data, time.Unix(at, 0), true
}

// avatarDelete идемпотентна: нет записи — не ошибка.
func (r *roomDB) avatarDelete(id string) error {
	_, err := r.db.Exec(`DELETE FROM avatars WHERE id=?`, id)
	return err
}

func (r *roomDB) avatarCount() int {
	var n int
	if err := r.db.QueryRow(`SELECT count(*) FROM avatars`).Scan(&n); err != nil {
		return 0
	}
	return n
}

// --- джинглы welcome/farewell ---

func (r *roomDB) cueGet(kind string) (string, []byte, bool) {
	var ext string
	var data []byte
	err := r.db.QueryRow(`SELECT ext, data FROM cues WHERE kind=?`, kind).Scan(&ext, &data)
	return ext, data, err == nil
}

func (r *roomDB) cueSet(kind, ext string, data []byte) error {
	_, err := r.db.Exec(`INSERT INTO cues (kind, ext, data, updated_at) VALUES (?,?,?,?)
		ON CONFLICT (kind) DO UPDATE SET ext=excluded.ext, data=excluded.data, updated_at=excluded.updated_at`,
		kind, ext, data, time.Now().Unix())
	return err
}

func (r *roomDB) cueDelete(kind string) error {
	_, err := r.db.Exec(`DELETE FROM cues WHERE kind=?`, kind)
	return err
}

// --- реестр палат ---

// roomRegister: INSERT OR IGNORE — палата попадает в реестр при первом
// обращении (токен, вход, запрос к палате).
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
