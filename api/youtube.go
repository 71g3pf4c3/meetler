package main

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"regexp"
	"sync"
	"time"
)

type youtubeState struct {
	VideoID  string  `json:"videoId"`
	Playing  bool    `json:"playing"`
	Position float64 `json:"position"`
	Revision uint64  `json:"revision"`
	updated  time.Time
}

type youtubeStore struct {
	mu       sync.Mutex
	rooms    map[string]youtubeState
	routes   *http.ServeMux
	revision uint64
}

type youtubeRequest struct {
	VideoID  string   `json:"videoId"`
	Position *float64 `json:"position"`
}

var youtubeID = regexp.MustCompile(`^[a-zA-Z0-9_-]{11}$`)

func newYouTubeStore() *youtubeStore {
	ys := &youtubeStore{
		rooms: make(map[string]youtubeState), routes: http.NewServeMux(),
		revision: uint64(time.Now().UnixMicro()),
	}
	const base = "/api/rooms/{room}/youtube"
	ys.routes.HandleFunc("GET "+base, ys.handle(nil))
	ys.routes.HandleFunc("PUT "+base+"/video", ys.handle(func(s *youtubeState, req youtubeRequest) error {
		if !youtubeID.MatchString(req.VideoID) {
			return errors.New("invalid YouTube video ID")
		}
		s.VideoID, s.Position, s.Playing = req.VideoID, 0, true
		return nil
	}))
	ys.routes.HandleFunc("DELETE "+base+"/video", ys.handle(func(s *youtubeState, _ youtubeRequest) error {
		s.VideoID, s.Position, s.Playing = "", 0, false
		return nil
	}))
	for _, action := range []string{"play", "pause", "seek"} {
		ys.routes.HandleFunc("POST "+base+"/"+action, ys.handle(func(s *youtubeState, req youtubeRequest) error {
			if req.Position == nil ||
				math.IsNaN(*req.Position) || math.IsInf(*req.Position, 0) || *req.Position < 0 {
				return errors.New("invalid playback position")
			}
			s.Position = *req.Position
			if action != "seek" {
				s.Playing = action == "play"
			}
			return nil
		}))
	}
	return ys
}

func (ys *youtubeStore) clearRoom(room string) {
	ys.mu.Lock()
	defer ys.mu.Unlock()
	delete(ys.rooms, room)
	ys.revision++
}

func (ys *youtubeStore) handle(change func(*youtubeState, youtubeRequest) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req youtubeRequest
		if r.Method == http.MethodPut || r.Method == http.MethodPost {
			if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req) != nil {
				http.Error(w, "bad json", http.StatusBadRequest)
				return
			}
		}
		room := r.PathValue("room")
		ys.mu.Lock()
		s := ys.rooms[room]
		if s.Revision == 0 {
			s.Revision = ys.revision
		}
		now := time.Now()
		if s.Playing {
			s.Position += now.Sub(s.updated).Seconds()
		}
		if change != nil {
			if s.VideoID == "" && r.Method == http.MethodPost {
				ys.mu.Unlock()
				http.Error(w, "no video selected", http.StatusConflict)
				return
			}
			if err := change(&s, req); err != nil {
				ys.mu.Unlock()
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			ys.revision++
			s.Revision = ys.revision
			s.updated = now
			ys.rooms[room] = s
		}
		ys.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(s)
	}
}
