package main

// Реестр палат в ROOMS_DIR и маршруты /api/rooms.
// Список объединяет каталоги палат и активные комнаты LiveKit.

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
)

type roomInfo struct {
	Name         string `json:"name"`
	Participants uint32 `json:"participants"`
	CreatedAt    int64  `json:"createdAt"` // unix seconds
}

type roomService struct {
	client      *lksdk.RoomServiceClient
	root        string // ROOMS_DIR
	db          *roomDB
	sb          *soundboard
	av          *avatarStore
	votes       *voteStore
	poker       *pokerStore
	tgbridgeURL string // база tgbridge HTTP (для "15с → TG"); "" = мост не настроен
	ok          bool   // БД открыта; иначе /api/rooms* → 503 (токены и кик работают)
	protected   map[string]bool
}

// parseProtectedRooms: csv → set валидных имён палат. Тихие (protected) палаты:
// всегда видны в списке, их нельзя закрыть (голосование/DELETE), tgbridge в
// них не публикует. Дефолт — "tishina".
func parseProtectedRooms(csv string) map[string]bool {
	out := map[string]bool{}
	for _, s := range strings.Split(csv, ",") {
		s = strings.TrimSpace(s)
		if s != "" && validRoom(s) {
			out[s] = true
		}
	}
	return out
}

func (rs *roomService) isProtected(room string) bool {
	return rs.protected[room]
}

func newRoomService(lkURL, apiKey, apiSecret, root, tgbridgeURL, protectedCSV string) *roomService {
	if root == "" {
		root = "/data/rooms"
	}
	client := lksdk.NewRoomServiceClient(lkURL, apiKey, apiSecret)
	// Если основной каталог недоступен, пробуем временное хранилище.
	if err := os.MkdirAll(root, 0o755); err != nil {
		log.Printf("WARNING: ROOMS_DIR %q unavailable: %v", root, err)
		fallback := filepath.Join(os.TempDir(), "meetler-rooms")
		if err2 := os.MkdirAll(fallback, 0o755); err2 == nil {
			log.Printf("WARNING: using EPHEMERAL fallback %q (палаты не переживут рестарт — почини права на %q)", fallback, root)
			root = fallback
		} else {
			log.Printf("ERROR: fallback %q тоже недоступен: %v — хранилище отключено", fallback, err2)
		}
	}
	rs := &roomService{
		client:      client,
		root:        root,
		tgbridgeURL: tgbridgeURL,
		protected:   parseProtectedRooms(protectedCSV),
	}
	// SQLite-хранилище: звуки/портреты/джинглы + миграция со старой файловой
	// структуры (ROOMS_DIR/<room>/{sounds,avatars}, джинглы в корне палаты).
	db, err := openRoomDB(filepath.Join(root, "meetler.db"))
	if err != nil {
		log.Printf("ERROR: open db в %q: %v — хранилище отключено", root, err)
		return rs
	}
	db.migrateFromFS(root)
	rs.db = db
	rs.sb = &soundboard{db: db}
	rs.av = &avatarStore{db: db}
	rs.ok = true
	rs.votes = newVoteStore(rs)
	rs.poker = newPokerStore(client)
	return rs
}

// dirWritable больше не используется (проверку записи делает openRoomDB):
// оставлена проверка доступности root в newRoomService через MkdirAll.

func (rs *roomService) roomDir(room string) string { return filepath.Join(rs.root, room) }

// ensureRoom регистрирует палату в реестре (БД). Best-effort: ошибка не
// мешает входу — палата просто останется неперсистентной до следующей попытки.
func (rs *roomService) ensureRoom(room string) {
	if !rs.ok {
		return
	}
	rs.db.roomRegister(room)
}

// identity участников: серверный anon-<hex> или tgbridge — безопасный набор
var identRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

type kickRequest struct {
	Room     string `json:"room"`
	Identity string `json:"identity"`
}

// POST /api/kick удаляет участника через LiveKit RoomService.
func (rs *roomService) handleKick(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req kickRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if !validRoom(req.Room) || !identRe.MatchString(req.Identity) {
		http.Error(w, "invalid room or identity", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if _, err := rs.client.RemoveParticipant(ctx, &livekit.RoomParticipantIdentity{
		Room: req.Room, Identity: req.Identity,
	}); err != nil {
		log.Printf("RemoveParticipant(%s/%s): %v", req.Room, req.Identity, err)
		http.Error(w, "kick failed", http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GET /api/rooms — все палаты: персистентный реестр ∪ живые комнаты SFU.
func (rs *roomService) handleRooms(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !rs.ok {
		http.Error(w, "room storage disabled", http.StatusServiceUnavailable)
		return
	}
	registry := rs.db.roomNames()

	// живой статус — best-effort: SFU лёг → показываем реестр с нулями
	type liveRoom struct {
		participants uint32
		createdAt    int64
	}
	live := map[string]liveRoom{}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if res, err := rs.client.ListRooms(ctx, &livekit.ListRoomsRequest{}); err == nil {
		for _, rm := range res.Rooms {
			live[rm.Name] = liveRoom{participants: rm.NumParticipants, createdAt: rm.CreationTime}
		}
	} else {
		log.Printf("ListRooms: %v (показываю реестр без живого счётчика)", err)
	}

	out := make([]roomInfo, 0, len(registry))
	for name, createdAt := range registry {
		if !validRoom(name) {
			continue
		}
		lr := live[name]
		out = append(out, roomInfo{
			Name: name, Participants: lr.participants, CreatedAt: createdAt,
		})
		delete(live, name) // отмечена из реестра
	}
	// живые палаты вне реестра (созданы мимо /api/token, напр. tgbridge)
	for name, lr := range live {
		if validRoom(name) {
			out = append(out, roomInfo{Name: name, Participants: lr.participants, CreatedAt: lr.createdAt})
		}
	}
	// тихие (protected) палаты — персистентные: всегда в списке, даже если
	// каталога нет и в ней никто не сидит (появится при первом входе)
	for name := range rs.protected {
		found := false
		for i := range out {
			if out[i].Name == name {
				found = true
				break
			}
		}
		if !found {
			out = append(out, roomInfo{Name: name, Participants: 0, CreatedAt: 0})
		}
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

// deleteRoom: DELETE /api/rooms/<room> — закрыть палату.
// Каскад: DeleteRoom выписывает всех участников (best-effort — если SFU
// недоступен, в палате и так никого нет), RemoveAll сносит каталог палаты
// со ВСЕМИ её звуками и портретами. Идемпотентно: несуществующая палата → 204.
// Тихие (protected) палаты закрыть нельзя.
func (rs *roomService) deleteRoom(w http.ResponseWriter, _ *http.Request, room string) {
	if rs.isProtected(room) {
		http.Error(w, "тихая палата не закрывается", http.StatusForbidden)
		return
	}
	rs.closeRoom(room)
	w.WriteHeader(http.StatusNoContent)
}

// closeRoom: каскадное закрытие палаты (без http). DeleteRoom выписывает всех
// (best-effort — если SFU недоступен, в палате и так никого нет), содержимое
// палаты сносится из БД + каталог старого файлового хранилища (если остался
// после миграции). Переиспользуется голосованием.
func (rs *roomService) closeRoom(room string) {
	if rs.poker != nil {
		rs.poker.clearRoom(room)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := rs.client.DeleteRoom(ctx, &livekit.DeleteRoomRequest{Room: room}); err != nil {
		log.Printf("DeleteRoom(%s): %v (удаляю из реестра в любом случае)", room, err)
	}
	if rs.db != nil {
		rs.db.roomDeleteCascade(room)
		rs.db.roomUnregister(room)
	}
	if err := os.RemoveAll(rs.roomDir(room)); err != nil {
		log.Printf("closeRoom RemoveAll(%s): %v", room, err)
	}
}

// handleRoomScoped: всё под /api/rooms/<room>/...
//
//	DELETE /api/rooms/<room>                     — закрыть палату (каскад)
//	GET|POST  /api/rooms/<room>/sounds           — листинг/заливка звуков
//	GET|DELETE /api/rooms/<room>/sounds/<id>     — отдать/удалить звук
//	GET|POST  /api/rooms/<room>/avatars           — листинг/заливка портретов
//	GET|DELETE /api/rooms/<room>/avatars/<id>     — отдать/удалить портрет
func (rs *roomService) handleRoomScoped(w http.ResponseWriter, r *http.Request) {
	if !rs.ok {
		http.Error(w, "room storage disabled", http.StatusServiceUnavailable)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/rooms/")
	parts := strings.Split(rest, "/")
	if len(parts) == 0 || !validRoom(parts[0]) {
		http.Error(w, "bad room", http.StatusBadRequest)
		return
	}
	// любое обращение к палате регистрирует её в реестре (как раньше —
	// созданием каталога: залить звук можно и до первого входа)
	rs.ensureRoom(parts[0])
	room := parts[0]
	if len(parts) == 1 {
		if r.Method != http.MethodDelete {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		rs.deleteRoom(w, r, room)
		return
	}
	switch parts[1] {
	case soundsSub:
		rs.sb.dispatch(w, r, room, parts[2:])
	case avatarsSub:
		rs.av.dispatch(w, r, room, parts[2:])
	case "votes":
		rs.votes.dispatch(w, r, room, parts[2:])
	case "welcome", "farewell":
		rs.sb.cueDispatch(w, r, room, parts[1], parts[2:])
	case "tg-voice":
		rs.handleTGVoice(w, r, room, parts[2:])
	case "poker":
		rs.poker.routes.ServeHTTP(w, r)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}
