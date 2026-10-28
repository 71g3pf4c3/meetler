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
		return target != "" && target == filepath.Base(target)
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
	Voted     bool `json:"voted"`
}

type voteSnapshot struct {
	Counts map[string]int  `json:"counts"`
	Voted  map[string]bool `json:"voted"`
}

// thresholdFor: tgbridge — служебный участник (мост), а не пациент: его
// выписывает ПЕРВЫЙ голос. Живого человека — только коллегиально.
func thresholdFor(kind, target string) int {
	if kind == "kick" && target == "tgbridge" {
		return 1
	}
	return voteThreshold
}

// POST /api/rooms/<room>/votes — добавить или отозвать свой голос;
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
	// тихую (protected) палату закрыть голосованием нельзя
	if req.Kind == "room" && vs.rs.isProtected(req.Target) {
		http.Error(w, "тихая палата не закрывается", http.StatusForbidden)
		return
	}
	voter := strings.TrimSpace(req.Voter)
	if l := len(voter); l < 1 || l > 64 {
		http.Error(w, "bad voter", http.StatusBadRequest)
		return
	}

	k := voteKey{room, req.Kind, req.Target}
	th := thresholdFor(req.Kind, req.Target)

	vs.mu.Lock()
	vs.countLive(k)
	if vs.v[k] == nil {
		vs.v[k] = map[string]time.Time{}
	}
	_, voted := vs.v[k][voter]
	if voted {
		delete(vs.v[k], voter)
	} else {
		vs.v[k][voter] = time.Now()
	}
	voted = !voted
	n := len(vs.v[k])
	done := n >= th
	if done || n == 0 {
		delete(vs.v, k)
		voted = false
	}
	vs.mu.Unlock()

	if done {
		vs.execute(req.Kind, room, req.Target)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(voteResponse{Count: n, Threshold: th, Done: done, Voted: voted})
}

// GET /api/rooms/<room>/votes — текущие счётчики всех активных голосований
// палаты. С ?voter= возвращает также состояние собственного голоса.
func (vs *voteStore) list(w http.ResponseWriter, r *http.Request, room string) {
	out := map[string]int{}
	voter := strings.TrimSpace(r.URL.Query().Get("voter"))
	voted := map[string]bool{}
	vs.mu.Lock()
	for k := range vs.v {
		if k.room != room {
			continue
		}
		if n := vs.countLive(k); n > 0 {
			key := k.kind + ":" + k.target
			out[key] = n
			if _, ok := vs.v[k][voter]; ok {
				voted[key] = true
			}
		}
	}
	vs.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if voter != "" {
		_ = json.NewEncoder(w).Encode(voteSnapshot{Counts: out, Voted: voted})
		return
	}
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
