package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGlobalMediaSurvivesRoomClosure(t *testing.T) {
	root := t.TempDir()
	legacyFile := filepath.Join(root, "ward-a", "sounds", "legacy.ogg")
	if err := os.MkdirAll(filepath.Dir(legacyFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyFile, []byte("OggSlegacy"), 0o644); err != nil {
		t.Fatal(err)
	}
	livekit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer livekit.Close()
	rs := newRoomService(livekit.URL, "key", "secret", root, "")
	if !rs.ok {
		t.Fatal("storage unavailable")
	}
	defer rs.db.Close()
	if rs.db.soundCount() != 0 {
		t.Fatal("legacy files were automatically imported")
	}
	rs.ensureRoom("ward-a")
	rs.ensureRoom("ward-b")

	request := func(method, path string, body io.Reader, contentType string, status int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, body)
		r.Header.Set("Content-Type", contentType)
		w := httptest.NewRecorder()
		if strings.HasPrefix(path, "/api/rooms/") {
			rs.handleRoomScoped(w, r)
		} else {
			rs.handleMedia(w, r)
		}
		if w.Code != status {
			t.Fatalf("%s %s: got %d, want %d: %s", method, path, w.Code, status, w.Body)
		}
		return w
	}
	upload := func(path, filename, data string, status int) *httptest.ResponseRecorder {
		t.Helper()
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		file, err := form.CreateFormFile("file", filename)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(file, data); err != nil {
			t.Fatal(err)
		}
		if err := form.Close(); err != nil {
			t.Fatal(err)
		}
		return request(http.MethodPost, path, &body, form.FormDataContentType(), status)
	}
	var sound soundMeta
	if err := json.Unmarshal(upload("/api/sounds", "clip.ogg", "OggSclip", http.StatusCreated).Body.Bytes(), &sound); err != nil {
		t.Fatal(err)
	}
	var avatar avatarMeta
	if err := json.Unmarshal(upload("/api/avatars", "portrait.png", "portrait", http.StatusCreated).Body.Bytes(), &avatar); err != nil {
		t.Fatal(err)
	}
	if sound.URL != "/api/sounds/"+sound.ID || avatar.URL != "/api/avatars/"+avatar.ID {
		t.Fatalf("room-dependent URLs: %q, %q", sound.URL, avatar.URL)
	}
	for _, cue := range []string{"welcome", "farewell"} {
		upload("/api/"+cue, "cue.ogg", "OggS"+cue, http.StatusNoContent)
	}
	for _, expectedLikes := range []int{1, 0, 1} {
		response := request(http.MethodPost, avatar.URL+"/like", strings.NewReader(`{"voter":"same-browser"}`), "application/json", http.StatusOK)
		var result struct {
			Likes int `json:"likes"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Likes != expectedLikes {
			t.Fatalf("likes: got %d, want %d", result.Likes, expectedLikes)
		}
	}
	request(http.MethodDelete, "/api/rooms/ward-a", nil, "", http.StatusNoContent)
	if rooms := rs.db.roomNames(); rooms["ward-a"] != 0 || rooms["ward-b"] == 0 {
		t.Fatalf("unexpected room registry: %v", rooms)
	}
	if _, err := os.Stat(legacyFile); err != nil {
		t.Fatalf("room closure removed legacy files: %v", err)
	}
	for path, data := range map[string]string{
		sound.URL: "OggSclip", avatar.URL: "portrait",
		"/api/welcome": "OggSwelcome", "/api/farewell": "OggSfarewell",
	} {
		if got := request(http.MethodGet, path, nil, "", http.StatusOK).Body.String(); got != data {
			t.Fatalf("%s: got %q, want %q", path, got, data)
		}
	}
	var avatars []avatarMeta
	if err := json.Unmarshal(request(http.MethodGet, "/api/avatars", nil, "", http.StatusOK).Body.Bytes(), &avatars); err != nil {
		t.Fatal(err)
	}
	if len(avatars) != 1 || avatars[0].Likes != 1 {
		t.Fatalf("shared portraits/likes lost: %v", avatars)
	}
	var sounds []soundMeta
	if err := json.Unmarshal(request(http.MethodGet, "/api/sounds", nil, "", http.StatusOK).Body.Bytes(), &sounds); err != nil {
		t.Fatal(err)
	}
	if len(sounds) != 1 || sounds[0].ID != sound.ID {
		t.Fatalf("shared sound lost: %v", sounds)
	}
	for _, path := range []string{"sounds", "avatars", "welcome", "farewell"} {
		request(http.MethodGet, "/api/rooms/ward-b/"+path, nil, "", http.StatusNotFound)
	}
	// A deletion vote in either room operates on the same shared file.
	rs.votes.execute("sound", "ward-b", sound.ID)
	rs.votes.execute("avatar", "ward-b", avatar.ID)
	request(http.MethodGet, sound.URL, nil, "", http.StatusNotFound)
	request(http.MethodGet, avatar.URL, nil, "", http.StatusNotFound)
	var likes int
	if err := rs.db.db.QueryRow(`SELECT count(*) FROM avatar_likes`).Scan(&likes); err != nil || likes != 0 {
		t.Fatalf("deleted portrait left likes: %d, %v", likes, err)
	}
	request(http.MethodDelete, "/api/welcome", nil, "", http.StatusNoContent)
	request(http.MethodGet, "/api/welcome", nil, "", http.StatusNotFound)
	request(http.MethodGet, "/api/farewell", nil, "", http.StatusOK)
}

func TestLegacyMediaSchemaRequiresManualMigration(t *testing.T) {
	for _, table := range []string{"sounds", "avatars", "avatar_likes", "cues"} {
		t.Run(table, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "meetler.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(`CREATE TABLE ` + table + ` (room TEXT, data BLOB); INSERT INTO ` + table + ` VALUES ('ward-a', 'preserve me')`); err != nil {
				t.Fatal(err)
			}
			opened, err := openRoomDB(path)
			if err == nil {
				_ = opened.Close()
				t.Fatal("legacy schema accepted")
			}
			if !strings.Contains(err.Error(), "manual migration") {
				t.Fatalf("missing migration instruction: %v", err)
			}
			var data string
			if err := db.QueryRow(`SELECT data FROM ` + table + ` WHERE room='ward-a'`).Scan(&data); err != nil || data != "preserve me" {
				t.Fatalf("legacy data changed: %q, %v", data, err)
			}
			var tables int
			if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table'`).Scan(&tables); err != nil || tables != 1 {
				t.Fatalf("legacy schema changed: %d tables, %v", tables, err)
			}
		})
	}
}
