package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/twitchtv/twirp"
)

type pokerRoomClient interface {
	ListParticipants(context.Context, *livekit.ListParticipantsRequest) (*livekit.ListParticipantsResponse, error)
}

type pokerStore struct {
	mu     sync.Mutex
	tables map[string]*pokerTable
	routes *http.ServeMux
	client pokerRoomClient
}

var errPokerJSON = errors.New("bad json")

func newPokerStore(client pokerRoomClient) *pokerStore {
	ps := &pokerStore{tables: make(map[string]*pokerTable), routes: http.NewServeMux(), client: client}
	const base = "/api/rooms/{room}/poker"
	ps.routes.HandleFunc("GET "+base, ps.handle(nil))
	ps.routes.HandleFunc("POST "+base+"/seats", ps.handle(func(t *pokerTable, key string, r *http.Request, now time.Time) error {
		var req struct {
			Name     string `json:"name"`
			Seat     int    `json:"seat"`
			Identity string `json:"identity"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !identRe.MatchString(req.Identity) {
			return errPokerJSON
		}
		if err := t.sit(key, req.Seat, req.Name, now); err != nil {
			return err
		}
		t.players[req.Seat].identity = req.Identity
		return nil
	}))
	ps.routes.HandleFunc("DELETE "+base+"/seats/me", ps.move((*pokerTable).leave))
	ps.routes.HandleFunc("POST "+base+"/deal", ps.move((*pokerTable).startHand))
	ps.routes.HandleFunc("POST "+base+"/refill", ps.move((*pokerTable).refill))
	ps.routes.HandleFunc("POST "+base+"/fold", ps.move((*pokerTable).fold))
	ps.routes.HandleFunc("POST "+base+"/check", ps.move((*pokerTable).check))
	ps.routes.HandleFunc("POST "+base+"/call", ps.move((*pokerTable).call))
	ps.routes.HandleFunc("POST "+base+"/raise", ps.handle(func(t *pokerTable, key string, r *http.Request, now time.Time) error {
		var req struct {
			Amount int `json:"amount"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return errPokerJSON
		}
		return t.raise(key, req.Amount, now)
	}))
	return ps
}

func (ps *pokerStore) clearRoom(room string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	delete(ps.tables, room)
}

func (ps *pokerStore) move(move func(*pokerTable, string, time.Time) error) http.HandlerFunc {
	return ps.handle(func(t *pokerTable, key string, _ *http.Request, now time.Time) error {
		return move(t, key, now)
	})
}

func (ps *pokerStore) handle(move func(*pokerTable, string, *http.Request, time.Time) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-Poker-Player")
		if (key != "" && (len(key) < 16 || !identRe.MatchString(key))) || (move != nil && key == "") {
			http.Error(w, "invalid poker player key", http.StatusBadRequest)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 2048)
		room := r.PathValue("room")
		checkedAt := time.Now()
		var present map[string]bool
		if move == nil && ps.client != nil {
			ctx, cancel := context.WithTimeout(r.Context(), time.Second)
			res, err := ps.client.ListParticipants(ctx, &livekit.ListParticipantsRequest{Room: room})
			cancel()
			var rpcErr twirp.Error
			if err == nil || (errors.As(err, &rpcErr) && rpcErr.Code() == twirp.NotFound) {
				present = make(map[string]bool)
				if err == nil {
					for _, p := range res.GetParticipants() {
						if p.State != livekit.ParticipantInfo_DISCONNECTED {
							present[p.Identity] = true
						}
					}
				}
			}
		}
		ps.mu.Lock()
		defer ps.mu.Unlock()
		t := ps.tables[room]
		if t == nil {
			t = &pokerTable{dealer: -1, turn: -1, street: "waiting", minRaise: pokerBigBlind}
			ps.tables[room] = t
		}
		now := time.Now()
		if present != nil {
			for i, p := range t.players {
				if p != nil && !present[p.identity] && !p.seen.After(checkedAt) {
					t.removePlayer(i, now)
				}
			}
		}
		t.expire(now)
		if i := t.seat(key); i >= 0 {
			t.players[i].seen = now
		}
		if move != nil {
			if err := move(t, key, r, now); err != nil {
				status := http.StatusConflict
				if errors.Is(err, errPokerJSON) {
					status = http.StatusBadRequest
				}
				http.Error(w, err.Error(), status)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(t.view(key))
	}
}
