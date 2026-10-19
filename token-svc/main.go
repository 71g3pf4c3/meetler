// token-svc выдаёт анонимные short-lived room-JWT для входа в LiveKit.
//
// Анонимность: нет аккаунтов, нет IdP, нет email. identity генерируется случайно
// (anon-<rand>), display name — произвольная строка от клиента. Токен ограничен
// одной комнатой и имеет TTL. Сервер не персистит ничего кроме метрик LiveKit.
//
// Безопасность: сервис доступен только через Caddy (/api/token). Он не принимает
// готовый identity от клиента — identity всегда серверный рандом, чтобы клиент не мог
// выдать себя за другого участника.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
)

type config struct {
	apiKey    string
	apiSecret string
	ttl       time.Duration
	roomMax   uint32
	listen    string
	rooms     *roomService // персистентный реестр палат (может быть nil в тестах)
}

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("missing required env %s", k)
	}
	return v
}

func loadConfig() config {
	ttl, err := time.ParseDuration(getEnv("TOKEN_TTL", "6h"))
	if err != nil {
		log.Fatalf("bad TOKEN_TTL: %v", err)
	}
	maxP := uint32(30)
	if s := os.Getenv("ROOM_MAX_PARTICIPANTS"); s != "" {
		n, err := strconv.ParseUint(s, 10, 32)
		if err != nil {
			log.Fatalf("bad ROOM_MAX_PARTICIPANTS: %v", err)
		}
		maxP = uint32(n)
	}
	return config{
		apiKey:    mustEnv("LIVEKIT_API_KEY"),
		apiSecret: mustEnv("LIVEKIT_API_SECRET"),
		ttl:       ttl,
		roomMax:   maxP,
		listen:    getEnv("LISTEN_ADDR", ":8080"),
	}
}

func getEnv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

type tokenRequest struct {
	Room string `json:"room"`
	Name string `json:"name"` // display name, необязательно
}

type tokenResponse struct {
	Token    string `json:"token"`
	Identity string `json:"identity"`
	Room     string `json:"room"`
}

// validRoom: пускаем только безопасный набор символов, чтобы имя комнаты не ломало
// маршрутизацию и не использовалось для инъекций в метаданные/логи.
func validRoom(s string) bool {
	if len(s) < 1 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_'
		if !ok {
			return false
		}
	}
	return true
}

func randID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// rand.Read из crypto/rand на практике не падает; если упал — это фатально.
		log.Fatalf("crypto/rand failed: %v", err)
	}
	return "anon-" + hex.EncodeToString(b)
}

func sanitizeName(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 40 {
		s = s[:40]
	}
	// вырезаем управляющие символы, остальное оставляем (имя — UTF-8 произвольное)
	return strings.Map(func(r rune) rune {
		if r < 0x20 {
			return -1
		}
		return r
	}, s)
}

func (c config) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req tokenRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	req.Room = strings.TrimSpace(req.Room)
	if !validRoom(req.Room) {
		http.Error(w, "invalid room: use [a-zA-Z0-9_-], 1-64 chars", http.StatusBadRequest)
		return
	}

	identity := randID()
	name := sanitizeName(req.Name)
	if name == "" {
		name = "Гость"
	}

	canUpdateOwn := true
	at := auth.NewAccessToken(c.apiKey, c.apiSecret).
		SetIdentity(identity).
		SetName(name).
		SetValidFor(c.ttl).
		SetVideoGrant(&auth.VideoGrant{
			RoomJoin:             true,
			Room:                 req.Room,
			CanPublish:           boolPtr(true),
			CanSubscribe:         boolPtr(true),
			CanPublishData:       boolPtr(true),
			CanUpdateOwnMetadata: &canUpdateOwn,
		})

	// enforce лимит участников: токен несёт RoomConfiguration, который livekit
	// применяет при auto-create комнаты. Без этого roomMax был мёртвой настройкой.
	if c.roomMax > 0 {
		at.SetRoomConfig(&livekit.RoomConfiguration{
			Name:            req.Room,
			MaxParticipants: c.roomMax,
		})
	}

	jwt, err := at.ToJWT()
	if err != nil {
		log.Printf("token mint failed: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// регистрируем палату в персистентном реестре (best-effort: ошибка
	// хранилища не должна мешать входу — палата останется неперсистентной)
	if c.rooms != nil {
		c.rooms.ensureRoom(req.Room)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(tokenResponse{
		Token:    jwt,
		Identity: identity,
		Room:     req.Room,
	})
}

func boolPtr(b bool) *bool { return &b }

func (c config) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func main() {
	cfg := loadConfig()

	// палаты: персистентный реестр (FS) + per-room звуки/портреты + кик.
	// Если ROOMS_DIR не создаётся (нет прав) — НЕ роняем сервис: токены и
	// кик работают, /api/rooms* деградируют в 503.
	// ВАЖНО: до mux.HandleFunc("/api/token", cfg.handleToken): handleToken —
	// метод на ЗНАЧЕНИИ config, регистрация копирует cfg; поля, присвоенные
	// после регистрации, копия не увидит (ловили на ensureRoom == noop).
	roomsSvc := newRoomService(
		getEnv("LIVEKIT_URL", "ws://127.0.0.1:7880"),
		cfg.apiKey, cfg.apiSecret,
		getEnv("ROOMS_DIR", "/data/rooms"),
		os.Getenv("TGBRIDGE_URL"), // напр. http://host.docker.internal:8090; "" = мост не настроен
	)
	cfg.rooms = roomsSvc
	if !roomsSvc.ok {
		log.Printf("WARNING: room storage disabled: %s unavailable (token issuing and kick still work)", roomsSvc.root)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/token", cfg.handleToken)
	mux.HandleFunc("/healthz", cfg.handleHealth)
	mux.HandleFunc("/api/rooms", roomsSvc.handleRooms)       // GET: список палат
	mux.HandleFunc("/api/rooms/", roomsSvc.handleRoomScoped) // DELETE палаты + звуки/портреты
	mux.HandleFunc("/api/kick", roomsSvc.handleKick)         // выписать участника (кто угодно)

	// центральная аутентификация: один общий пароль (APP_PASSWORD). Если пуст —
	// гейт выключен, сайт открыт. /api/auth и /healthz — всегда доступны.
	gate := newAuthGate(os.Getenv("APP_PASSWORD"))
	mux.HandleFunc("/api/auth", gate.handle)
	var handler http.Handler = gate.wrap(mux)

	srv := &http.Server{
		Addr:              cfg.listen,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		// ReadTimeout побольше: upload звука (до 5MiB) по медленному аплинку.
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
	storage := "OFF"
	if roomsSvc.ok {
		storage = roomsSvc.root
	}
	log.Printf("token-svc listening on %s (ttl=%s, room_max=%d, rooms=%s)",
		cfg.listen, cfg.ttl, cfg.roomMax, storage)
	if gate.enabled() {
		log.Printf("central auth: ON (APP_PASSWORD set)")
	} else {
		log.Printf("central auth: OFF (APP_PASSWORD empty — site open)")
	}
	log.Fatal(srv.ListenAndServe())
}
