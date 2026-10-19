package main

// Персистентный реестр палат + routing всего, что живёт под /api/rooms.
//
// Реестр — это файловая система: палата существует <=> существует каталог
// ROOMS_DIR/<room>. Внутри палаты — её звуки и портреты:
//
//	ROOMS_DIR/<room>/sounds/<id>       звуковая аптечка палаты
//	ROOMS_DIR/<room>/avatars/<id>       галерея портретов палаты
//
// ФС и есть state: нет отдельного индекса, который можно рассинхронизировать.
// Палата регистрируется при первом входе (ensureRoom в handleToken) и живёт,
// пока кто-нибудь её не закроет (DELETE /api/rooms/<room> — каскад: DeleteRoom
// выписывает участников, RemoveAll сносит каталог со звуками и портретами).
//
// Список палат = реестр ∪ живые палаты SFU (созданные мимо /api/token,
// например tgbridge). Если SFU недоступен — показываем реестр без счётчика
// участников: список палат теперь важнее, чем живой статус.

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
	client *lksdk.RoomServiceClient
	root   string // ROOMS_DIR
	sb     *soundboard
	av     *avatarStore
	votes  *voteStore
	ok     bool // root доступен; иначе /api/rooms* → 503 (токены и кик работают)
}

func newRoomService(lkURL, apiKey, apiSecret, root string) *roomService {
	if root == "" {
		root = "/data/rooms"
	}
	client := lksdk.NewRoomServiceClient(lkURL, apiKey, apiSecret)
	// основной путь. Если не создаётся (права на volume/StateDirectory) —
	// логируем ТОЧНУЮ ошибку и пробуем fallback во временный каталог, чтобы
	// фича работала (эфемерно) вместо полного 503. Это чинит прод-симптом
	// "room storage disabled", когда /var/lib/meetler/rooms недоступен.
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
		client: client,
		root:   root,
		sb:     &soundboard{root: root},
		av:     &avatarStore{root: root},
	}
	rs.ok = dirWritable(root)
	rs.votes = newVoteStore(rs)
	return rs
}

// dirWritable: каталог существует и в него реально можно писать (а не только
// MkdirAll прошёл). Ловит кейсы root-owned volume под nonroot-процессом.
func dirWritable(dir string) bool {
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return false
	}
	probe := filepath.Join(dir, ".wtest")
	if err := os.WriteFile(probe, []byte("x"), 0o644); err != nil {
		log.Printf("WARNING: %q не доступен на запись: %v", dir, err)
		return false
	}
	_ = os.Remove(probe)
	return true
}

func (rs *roomService) roomDir(room string) string { return filepath.Join(rs.root, room) }

// ensureRoom регистрирует палату в реестре (создаёт каталог + подкаталоги
// звуков/портретов). Best-effort: ошибка не мешает входу — палата просто
// останется неперсистентной, до следующей попытки.
func (rs *roomService) ensureRoom(room string) {
	if !rs.ok {
		return
	}
	if err := os.MkdirAll(filepath.Join(rs.roomDir(room), soundsSub), 0o755); err != nil {
		log.Printf("ensureRoom(%s): %v", room, err)
	}
	if err := os.MkdirAll(filepath.Join(rs.roomDir(room), avatarsSub), 0o755); err != nil {
		log.Printf("ensureRoom(%s): %v", room, err)
	}
}

// identity участников: серверный anon-<hex> или tgbridge — безопасный набор
var identRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

type kickRequest struct {
	Room     string `json:"room"`
	Identity string `json:"identity"`
}

// POST /api/kick — выписать участника из палаты (LiveKit RemoveParticipant).
// ОТКРЫТЫЙ эндпоинт по дизайну продукта: анонимный вход, "кто кого выписывает"
// решает нажавший крестик. Клиентским токеном кикать нельзя (нет roomAdmin),
// поэтому сервер проксирует через API key/secret.
// Не зависит от хранилища: работает даже когда ROOMS_DIR недоступен.
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
	entries, err := os.ReadDir(rs.root)
	if err != nil {
		http.Error(w, "read dir", http.StatusInternalServerError)
		return
	}

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

	out := make([]roomInfo, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || !validRoom(name) {
			continue
		}
		lr := live[name]
		createdAt := time.Now().Unix()
		if fi, err := e.Info(); err == nil {
			createdAt = fi.ModTime().Unix()
		}
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
func (rs *roomService) deleteRoom(w http.ResponseWriter, _ *http.Request, room string) {
	rs.closeRoom(room)
	w.WriteHeader(http.StatusNoContent)
}

// closeRoom: каскадное закрытие палаты (без http). DeleteRoom выписывает всех
// (best-effort — если SFU недоступен, в палате и так никого нет), RemoveAll
// сносит каталог со звуками/портретами. Переиспользуется голосованием.
func (rs *roomService) closeRoom(room string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := rs.client.DeleteRoom(ctx, &livekit.DeleteRoomRequest{Room: room}); err != nil {
		log.Printf("DeleteRoom(%s): %v (удаляю из реестра в любом случае)", room, err)
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
	case "welcome":
		rs.sb.welcomeDispatch(w, r, room, parts[2:])
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}
