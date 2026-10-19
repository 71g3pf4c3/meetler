package main

// Голосования за кик и удаление звука, портрета или палаты.
// Голоса хранятся в памяти и различаются по переданному клиентом voter-id.

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"
)

const (
	voteThreshold = 3               // число различных voter-id для действия
	voteTTL       = 5 * time.Minute // протухание голосов (не копим мусор)
)

type voteKey struct {
	room, kind, target string
}

type voteStore struct {
	rs *roomService
	mu sync.Mutex
	// room/kind/target -> voter -> время голоса
	v map[voteKey]map[string]time.Time
}

func newVoteStore(rs *roomService) *voteStore {
	return &voteStore{rs: rs, v: map[voteKey]map[string]time.Time{}}
}

var voteKinds = map[string]bool{"kick": true, "sound": true, "avatar": true, "room": true}

// validTarget: для kick — identity участника, для sound/avatar — имя файла,
// для room — имя самой палаты (закрытие палаты).
func validTarget(kind, target string) bool {
	switch kind {
	case "kick":
		return identRe.MatchString(target)
	case "sound":
		return target == filepath.Base(target) && soundExt[strings.ToLower(filepath.Ext(target))] != ""
	case "avatar":
		return target == filepath.Base(target) && avatarExt[strings.ToLower(filepath.Ext(target))] != ""
	case "room":
		return validRoom(target)
	}
	return false
}

// countLive: число непротухших голосов под ключом (чистит протухшие).
func (vs *voteStore) countLive(k voteKey) int {
	m := vs.v[k]
	if m == nil {
		return 0
	}
	now := time.Now()
	for voter, t := range m {
		if now.Sub(t) > voteTTL {
			delete(m, voter)
		}
	}
	if len(m) == 0 {
		delete(vs.v, k)
	}
	return len(m)
}

type voteRequest struct {
	Kind   string `json:"kind"`
	Target string `json:"target"`
	Voter  string `json:"voter"`
}

type voteResponse struct {
	Count     int  `json:"count"`
	Threshold int  `json:"threshold"`
	Done      bool `json:"done"`
}

// POST /api/rooms/<room>/votes — отдать голос. Возвращает текущий счётчик;
// при достижении порога исполняет действие на сервере и сбрасывает голоса.
func (vs *voteStore) cast(w http.ResponseWriter, r *http.Request, room string) {
	var req voteRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if !voteKinds[req.Kind] || !validTarget(req.Kind, req.Target) {
		http.Error(w, "bad kind/target", http.StatusBadRequest)
		return
	}
	voter := strings.TrimSpace(req.Voter)
	if l := len(voter); l < 1 || l > 64 {
		http.Error(w, "bad voter", http.StatusBadRequest)
		return
	}

	k := voteKey{room, req.Kind, req.Target}

	vs.mu.Lock()
	if vs.v[k] == nil {
		vs.v[k] = map[string]time.Time{}
	}
	vs.v[k][voter] = time.Now()
	n := vs.countLive(k)
	done := n >= voteThreshold
	if done {
		delete(vs.v, k) // порог взят — сбрасываем, чтобы не исполнить дважды
	}
	vs.mu.Unlock()

	if done {
		vs.execute(req.Kind, room, req.Target)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(voteResponse{Count: n, Threshold: voteThreshold, Done: done})
}

// GET /api/rooms/<room>/votes — текущие счётчики всех активных голосований
// палаты: map "kind:target" -> count. Для раскраски кнопок у всех клиентов.
func (vs *voteStore) list(w http.ResponseWriter, _ *http.Request, room string) {
	out := map[string]int{}
	vs.mu.Lock()
	for k := range vs.v {
		if k.room != room {
			continue
		}
		if n := vs.countLive(k); n > 0 {
			out[k.kind+":"+k.target] = n
		}
	}
	vs.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}

// execute исполняет действие после набора порога. Идемпотентно по смыслу:
// повторный кик/удаление уже-удалённого — просто залогируется.
func (vs *voteStore) execute(kind, room, target string) {
	switch kind {
	case "kick":
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := vs.rs.client.RemoveParticipant(ctx, &livekit.RoomParticipantIdentity{
			Room: room, Identity: target,
		}); err != nil {
			log.Printf("vote kick %s/%s: %v", room, target, err)
		}
	case "sound":
		if err := vs.rs.sb.delete(room, target); err != nil {
			log.Printf("vote sound-delete %s/%s: %v", room, target, err)
		}
	case "avatar":
		if err := vs.rs.av.delete(room, target); err != nil {
			log.Printf("vote avatar-delete %s/%s: %v", room, target, err)
		}
	case "room":
		// закрытие палаты: сносим все её голосования и каскадно удаляем
		vs.clearRoom(room)
		vs.rs.closeRoom(room)
	}
}

// clearRoom выкидывает все голосования палаты (её больше нет).
func (vs *voteStore) clearRoom(room string) {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	for k := range vs.v {
		if k.room == room {
			delete(vs.v, k)
		}
	}
}

// dispatch: /api/rooms/<room>/votes (GET счётчики, POST голос).
func (vs *voteStore) dispatch(w http.ResponseWriter, r *http.Request, room string, rest []string) {
	if len(rest) != 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		vs.list(w, r, room)
	case http.MethodPost:
		vs.cast(w, r, room)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
