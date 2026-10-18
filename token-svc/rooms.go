package main

// GET /api/rooms — список открытых палат для главной страницы.
// Читается из LiveKit RoomService (twirp) теми же API key/secret, что и
// mint токенов. Публично отдаём только: имя, число участников, время создания.

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"time"

	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
)

type roomInfo struct {
	Name         string `json:"name"`
	Participants uint32 `json:"participants"`
	CreatedAt    int64  `json:"createdAt"` // unix seconds
}

type roomLister struct {
	client *lksdk.RoomServiceClient
}

func newRoomLister(lkURL, apiKey, apiSecret string) *roomLister {
	return &roomLister{
		client: lksdk.NewRoomServiceClient(lkURL, apiKey, apiSecret),
	}
}

func (rl *roomLister) handleRooms(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// живой запрос к SFU на каждый вызов: палаты живут минуты, кэш тут вреднее
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	res, err := rl.client.ListRooms(ctx, &livekit.ListRoomsRequest{})
	if err != nil {
		// наружу — общий 502 без деталей; в лог — реальную причину
		// (auth 401 / сеть / таймаут различаются, а клиенту всё равно)
		log.Printf("ListRooms: %v", err)
		http.Error(w, "livekit unreachable", http.StatusBadGateway)
		return
	}
	out := make([]roomInfo, 0, len(res.Rooms))
	for _, rm := range res.Rooms {
		out = append(out, roomInfo{
			Name:         rm.Name,
			Participants: rm.NumParticipants,
			CreatedAt:    rm.CreationTime,
		})
	}
	// популярные сверху, дальше по алфавиту — стабильный порядок для UI
	sort.Slice(out, func(i, j int) bool {
		if out[i].Participants != out[j].Participants {
			return out[i].Participants > out[j].Participants
		}
		return out[i].Name < out[j].Name
	})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}
