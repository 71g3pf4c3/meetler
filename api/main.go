// API: аутентификация, JWT для LiveKit, палаты, медиа и голосования.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
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
	rooms     *roomService // реестр палат
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

// randHex: криптостойкая hex-строка (id файлов, identity). Единственная
// реализация — раньше randID/randName/avatarRandName дублировали её трижды.
// crypto/rand на практике не падает; если упал — фатально (иначе коллизия
// id → перезапись чужого файла).
func randHex() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		log.Fatalf("crypto/rand failed: %v", err)
	}
	return hex.EncodeToString(b)
}

// sanitizeText: обрезка до max байт + вырез управляющих символов; пустой
// результат → def. Единственная реализация — раньше sanitizeName и
// sanitizeLabel дублировали её с разными константами.
func sanitizeText(s string, max int, def string) string {
	s = strings.TrimSpace(s)
	if len(s) > max {
		s = s[:max]
	}
	s = strings.Map(func(r rune) rune {
		if r < 0x20 {
			return -1
		}
		return r
	}, s)
	if s == "" {
		return def
	}
	return s
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

	identity := "anon-" + randHex()
	name := sanitizeText(req.Name, 40, "Гость")

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

	// Передаём лимит участников в конфигурации создаваемой комнаты.
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

	// Заполняем cfg.rooms до регистрации handleToken: метод копирует config.
	roomsSvc := newRoomService(
		getEnv("LIVEKIT_URL", "ws://127.0.0.1:7880"),
		cfg.apiKey, cfg.apiSecret,
		getEnv("ROOMS_DIR", "/data/rooms"),
		os.Getenv("TGBRIDGE_URL"), // пусто — мост не настроен
		getEnv("PROTECTED_ROOMS", "tishina"), // тихие палаты: без бота, не закрываются
	)
	cfg.rooms = roomsSvc
	if !roomsSvc.ok {
		log.Printf("WARNING: room storage disabled: %s unavailable (token issuing and kick still work)", roomsSvc.root)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/token", cfg.handleToken)
	mux.HandleFunc("/healthz", cfg.handleHealth)
	mux.HandleFunc("/api/rooms", roomsSvc.handleRooms)       // GET: список палат
	mux.HandleFunc("/api/rooms/", roomsSvc.handleRoomScoped) // маршруты отдельной палаты
	mux.HandleFunc("/api/kick", roomsSvc.handleKick)         // кик участника

	// центральная аутентификация: один общий пароль (APP_PASSWORD). Если пуст —
	// гейт выключен, сайт открыт. /api/auth и /healthz — всегда доступны.
	gate := newAuthGate(os.Getenv("APP_PASSWORD"))
	mux.HandleFunc("/api/auth", gate.handle)
	var handler http.Handler = gate.wrap(mux)

	srv := &http.Server{
		Addr:              cfg.listen,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		// Время на загрузку медиа.
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
	storage := "OFF"
	if roomsSvc.ok {
		storage = roomsSvc.root
	}
	log.Printf("api listening on %s (ttl=%s, room_max=%d, rooms=%s)",
		cfg.listen, cfg.ttl, cfg.roomMax, storage)
	if gate.enabled() {
		log.Printf("central auth: ON (APP_PASSWORD set)")
	} else {
		log.Printf("central auth: OFF (APP_PASSWORD empty — site open)")
	}

	// graceful shutdown: SIGINT/SIGTERM → влёте запросы получают 5с
	// дописаться (раньше ListenAndServe рвал их посреди upload/загрузки).
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-errCh:
		log.Fatalf("listen: %v", err)
	case s := <-sig:
		log.Printf("api: got %s, shutting down…", s)
	}
	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	if err := srv.Shutdown(sctx); err != nil {
		log.Printf("api shutdown: %v", err)
	}
	log.Printf("api: stopped")
}
