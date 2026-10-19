package main

// HTTP-слой tgbridge: приём "15с → TG" из api.
// api (за nginx) переливает WAV-микс палаты сюда; мы транскодим в
// ogg/opus (ffmpeg, как и всё остальное) и постим голосовым (SendVoice) во
// все Telegram-чаты, привязанные к этой палате (/palata <room>).

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"
)

// serveHTTP принимает аудио от API по заданному адресу.
func (a *app) serveHTTP(addr string) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/tg-voice", a.handleTGVoice)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	a.httpSrv = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
	}
	log.Printf("tgbridge http listening on %s", addr)
	go func() {
		if err := a.httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("tgbridge http: %v", err)
		}
	}()
	return a.httpSrv
}

// handleTGVoice: POST /tg-voice?room=<palata> (multipart: file=WAV).
// Транскодит в ogg/opus и шлёт голосовым во все чаты палаты.
func (a *app) handleTGVoice(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	room := r.URL.Query().Get("room")
	if !palataRe.MatchString(room) {
		http.Error(w, "bad room", http.StatusBadRequest)
		return
	}
	chats := a.store.chatsFor(room)
	if len(chats) == 0 {
		http.Error(w, "в этой палате нет привязанного Telegram-чата (сделай /palata "+room+" в чате)", http.StatusNotFound)
		return
	}

	if err := r.ParseMultipartForm(16 << 20); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "no file", http.StatusBadRequest)
		return
	}
	defer file.Close()

	dir, cleanup, err := tmpDir()
	if err != nil {
		http.Error(w, "tmp", http.StatusInternalServerError)
		return
	}
	defer cleanup()

	src := filepath.Join(dir, "mix.wav")
	out, err := os.Create(src)
	if err != nil {
		http.Error(w, "store", http.StatusInternalServerError)
		return
	}
	if _, err := io.Copy(out, file); err != nil {
		_ = out.Close()
		http.Error(w, "write", http.StatusInternalServerError)
		return
	}
	_ = out.Close()

	ctx, cancel := context.WithTimeout(r.Context(), ffTimeout)
	defer cancel()
	ogg, err := toOgg(ctx, src, dir) // уже режет до maxPlaySec, 48k mono opus
	if err != nil {
		http.Error(w, "transcode: "+err.Error(), http.StatusInternalServerError)
		return
	}

	sent := 0
	for _, chatID := range chats {
		// telego читает поток на каждый вызов — нужен свой reader на чат.
		// При сбое open не паникуем (раньше mustOpen→nil→panic рвал HTTP-сервер).
		fh, err := os.Open(ogg)
		if err != nil {
			log.Printf("open ogg room=%s: %v", room, err)
			continue
		}
		_, err = a.bot.SendVoice(ctx, &telego.SendVoiceParams{
			ChatID:  tu.ID(chatID),
			Voice:   tu.File(fh),
			Caption: "🎙 последние 15с из палаты " + room,
		})
		_ = fh.Close()
		if err != nil {
			log.Printf("SendVoice chat=%d room=%s: %v", chatID, room, err)
			continue
		}
		sent++
	}
	if sent == 0 {
		http.Error(w, "не удалось отправить ни в один чат", http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
