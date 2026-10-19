package main

// Проксирование "15с → TG": браузер шлёт WAV-микс палаты сюда, token-svc
// переливает его в tgbridge (TGBRIDGE_URL), который транскодит в ogg/opus и
// постит голосовым в Telegram-чат(ы), привязанные к этой палате.
//
// Почему через token-svc, а не напрямую: tgbridge в host-network и не торчит
// наружу через Caddy; браузер его не видит. token-svc уже за Caddy (/api/*),
// поэтому он — естественная точка проксирования. Если TGBRIDGE_URL не задан
// (мост не поднят) — 503, фронт покажет "в TG не ушло".

import (
	"io"
	"net/http"
	"time"
)

const maxTGVoiceBytes = 8 << 20 // 15с WAV 48k mono ≈ 1.4 МБ; запас на всякий

// handleTGVoice: POST /api/rooms/<room>/tg-voice (multipart или raw) → tgbridge.
func (rs *roomService) handleTGVoice(w http.ResponseWriter, r *http.Request, room string, rest []string) {
	if len(rest) != 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if rs.tgbridgeURL == "" {
		http.Error(w, "telegram bridge not configured", http.StatusServiceUnavailable)
		return
	}
	// просто перельём тело запроса (multipart form с полем file) в tgbridge как есть
	body := http.MaxBytesReader(w, r.Body, maxTGVoiceBytes)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		rs.tgbridgeURL+"/tg-voice?room="+room, body)
	if err != nil {
		http.Error(w, "proxy build", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", r.Header.Get("Content-Type"))
	cl := &http.Client{Timeout: 60 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		http.Error(w, "bridge unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
