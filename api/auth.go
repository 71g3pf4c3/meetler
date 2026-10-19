package main

// APP_PASSWORD включает проверку cookie для /api/*, кроме /api/auth.
// Cookie подписан HMAC-SHA256; сервер не хранит сессии.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

const (
	authCookie = "meetler_auth"
	authMsg    = "meetler-auth-v1"
	authTTL    = 30 * 24 * time.Hour // ввёл пароль — месяц не спрашиваем
)

type authGate struct {
	password string // пусто → гейт выключен
	token    string // ожидаемое значение cookie
}

func newAuthGate(password string) *authGate {
	g := &authGate{password: password}
	if password != "" {
		mac := hmac.New(sha256.New, []byte(password))
		mac.Write([]byte(authMsg))
		g.token = hex.EncodeToString(mac.Sum(nil))
	}
	return g
}

func (g *authGate) enabled() bool { return g.password != "" }

// valid проверяет cookie запроса (constant-time).
func (g *authGate) valid(r *http.Request) bool {
	if !g.enabled() {
		return true
	}
	c, err := r.Cookie(authCookie)
	if err != nil {
		return false
	}
	return hmac.Equal([]byte(c.Value), []byte(g.token))
}

type authRequest struct {
	Password string `json:"password"`
}

// POST /api/auth {password} → ставит cookie при верном пароле.
// GET  /api/auth           → 200 если уже авторизован, иначе 401.
func (g *authGate) handle(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if g.valid(r) {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	case http.MethodPost:
		if !g.enabled() {
			// гейт выключен — считаем вход успешным, cookie не нужен
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var req authRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		// constant-time сравнение пароля
		if subtleEqual(req.Password, g.password) {
			http.SetCookie(w, &http.Cookie{
				Name:     authCookie,
				Value:    g.token,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteLaxMode,
				MaxAge:   int(authTTL.Seconds()),
			})
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Error(w, "wrong password", http.StatusUnauthorized)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// subtleEqual — constant-time сравнение строк (HMAC-обёрткой, чтобы не зависеть
// от длины преждевременно).
func subtleEqual(a, b string) bool {
	ha := sha256.Sum256([]byte(a))
	hb := sha256.Sum256([]byte(b))
	return hmac.Equal(ha[:], hb[:])
}

// wrap оборачивает хендлер гейтом: всё под /api/* (кроме /api/auth и /healthz)
// требует валидный cookie. Статику гейтит фронт (оверлей-сплэш), а реальная
// защита — тут: без пароля нельзя получить токен → нельзя войти в палату.
func (g *authGate) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/api/auth" || p == "/healthz" || !strings.HasPrefix(p, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		if !g.valid(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
