// tgbridge — Telegram-бот, транслирующий пересланные голосовые/видео/аудио и
// озвученный текст в палату meetler (LiveKit) как headless-participant.
//
// Палата определяется (по убыванию приоритета):
//  1. явно в сообщении/подписи:  "#6" или "/palata 6"
//  2. сохранённая для этого юзера (командой /palata 6)
//  3. дефолтная (DEFAULT_PALATA)
//
// Триггеры:
//   - voice / audio / video / video_note  -> транскод ffmpeg -> публикация
//   - текст с упоминанием бота (@bot) ИЛИ с "#<палата>"  -> TTS (piper) -> публикация
//
// ГРУППЫ: по умолчанию у бота включён privacy mode (@BotFather) — рядовой
// участник-бот получает только команды, упоминания и реплаи. Голосовые/видео
// без упоминания до него НЕ доставляются вовсе (код их не увидит). Решение:
// сделать бота админом группы ИЛИ выключить privacy mode у @BotFather.
// При добавлении в группу бот сам пишет эту подсказку (см. chatMember).

package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/mymmrac/telego"
)

// groupAnonymousBotID — служебный "пользователь", от которого приходят
// сообщения анонимных админов группы (скрытых за маской группы).
const groupAnonymousBotID = 1087968824

// Лимиты длительности (сек):
//
//	maxUploadSec — то, что принимаем от юзера (voice/video/... из Telegram,
//	               длительность известна из метаданных сообщения, без probe).
//	maxPlaySec   — жёсткий потолок проигрывания в палате: всё, что длиннее,
//	               обрезается на транскоде (ffmpeg -t), включая TTS.
const (
	maxUploadSec = 10
	maxPlaySec   = 15
)

type config struct {
	botToken   string
	lkURL      string
	lkKey      string
	lkSecret   string
	defPalata  string
	piperBin   string
	piperModel string
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("missing env %s", k)
	}
	return v
}

func loadConfig() config {
	return config{
		botToken:   mustEnv("TELEGRAM_BOT_TOKEN"),
		lkURL:      env("LIVEKIT_URL", "ws://127.0.0.1:7880"),
		lkKey:      mustEnv("LIVEKIT_API_KEY"),
		lkSecret:   mustEnv("LIVEKIT_API_SECRET"),
		defPalata:  env("DEFAULT_PALATA", "telegram"),
		piperBin:   os.Getenv("PIPER_BIN"),
		piperModel: os.Getenv("PIPER_MODEL"),
	}
}

// --- per-user выбранная палата ---
type palataStore struct {
	mu sync.Mutex
	m  map[int64]string
}

func newPalataStore() *palataStore { return &palataStore{m: map[int64]string{}} }
func (s *palataStore) get(uid int64, def string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.m[uid]; ok {
		return v
	}
	return def
}
func (s *palataStore) set(uid int64, v string) {
	s.mu.Lock()
	s.m[uid] = v
	s.mu.Unlock()
}

// валидный номер палаты = имя комнаты livekit (как в token-svc)
var palataRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// извлечь палату из текста/подписи: "#6" или "/palata 6" / "/room 6"
// (в группах команда приходит с @суффиксом: "/palata@bot 6" — терпим).
var hashRe = regexp.MustCompile(`#([a-zA-Z0-9_-]{1,64})`)
var cmdRe = regexp.MustCompile(`(?i)^/(?:palata|room)(?:@[A-Za-z0-9_]+)?\s+([a-zA-Z0-9_-]{1,64})`)

// parseCommand разбирает "/cmd@addressee arg..." (Telegram в группах шлёт
// команды с @суффиксом бота). addressee — кому адресована команда ("", если всем).
var cmdParseRe = regexp.MustCompile(`^/([a-zA-Z0-9_]+)(?:@([A-Za-z0-9_]+))?(?:\s+(.*))?$`)

func parseCommand(text string) (cmd, addressee, arg string, ok bool) {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "/") {
		return "", "", "", false
	}
	mm := cmdParseRe.FindStringSubmatch(t)
	if mm == nil {
		return "", "", "", false
	}
	return mm[1], mm[2], strings.TrimSpace(mm[3]), true
}

func (a *app) helpText() string {
	return "meetler — мост в палату 🏥\n\n" +
		"• Голосовое / аудио / видео / кружок → проиграю всем в палате (до " +
		fmt.Sprint(maxUploadSec) + " сек).\n" +
		"• Текст с @упоминанием меня ИЛИ (в личке) с #палатой → озвучу (piper TTS).\n" +
		"• Палата: #6 или /palata 6 в сообщении/подписи; запомнить свою: /palata 6;\n" +
		"  посмотреть текущую: /palata; по умолчанию: " + a.cfg.defPalata + ".\n\n" +
		"В группах я реагирую только на адресованные мне сообщения: @упоминание,\n" +
		"реплай на моё сообщение или #палата в подписи к медиа. Чтобы я вообще\n" +
		"видел медиа, я должен быть админом группы или с выключенным privacy mode\n" +
		"(@BotFather → /setprivacy → Disable).\n\n" +
		"Всё проигрываемое — не длиннее " + fmt.Sprint(maxPlaySec) + " сек.\n" +
		"/help — эта справка."
}

func palataFromText(s string) string {
	if m := cmdRe.FindStringSubmatch(strings.TrimSpace(s)); m != nil {
		return m[1]
	}
	if m := hashRe.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

type app struct {
	cfg   config
	bot   *telego.Bot
	me    string // username бота в нижнем регистре, без @
	meID  int64  // ID бота (фильтр my_chat_member)
	pub   *Publisher
	tts   *TTS
	store *palataStore
}

func main() {
	cfg := loadConfig()

	bot, err := telego.NewBot(cfg.botToken, telego.WithDefaultLogger(false, true))
	if err != nil {
		log.Fatalf("bot init: %v", err)
	}
	ctx := context.Background()
	me, err := bot.GetMe(ctx)
	if err != nil {
		log.Fatalf("getMe: %v", err)
	}

	// меню команд в UI Telegram (кнопка "/" рядом с полем ввода).
	// Best-effort: неудача не критична.
	_ = bot.SetMyCommands(ctx, &telego.SetMyCommandsParams{
		Commands: []telego.BotCommand{
			{Command: "help", Description: "Справка: медиа и TTS в палату"},
			{Command: "palata", Description: "Показать или задать палату: /palata 6"},
		},
	})

	a := &app{
		cfg:   cfg,
		bot:   bot,
		me:    strings.ToLower(me.Username),
		meID:  me.ID,
		pub:   NewPublisher(cfg.lkURL, cfg.lkKey, cfg.lkSecret, 60*time.Second),
		tts:   NewTTS(cfg.piperBin, cfg.piperModel),
		store: newPalataStore(),
	}
	log.Printf("tgbridge up as @%s, default palata=%s, tts=%v", a.me, cfg.defPalata, a.tts.enabled())

	// allowed_updates Telegram запоминает между вызовами getUpdates (sticky):
	// явный список страхует от "залипшей" настройки с прошлого запуска/вебхука.
	updates, err := bot.UpdatesViaLongPolling(ctx, &telego.GetUpdatesParams{
		AllowedUpdates: []string{"message", "channel_post", "my_chat_member"},
	})
	if err != nil {
		log.Fatalf("long polling: %v", err)
	}
	for u := range updates {
		switch {
		case u.Message != nil:
			go a.handle(ctx, u.Message)
		case u.ChannelPost != nil: // посты в каналах — те же медиа-триггеры
			go a.handle(ctx, u.ChannelPost)
		case u.MyChatMember != nil: // добавление/повышение бота в группе
			go a.chatMember(ctx, u.MyChatMember)
		}
	}
}

// chatMember реагирует на добавление бота в группу/канал и на выдачу прав.
// Именно тут ловим главный источник "в группе медиа не работает".
func (a *app) chatMember(ctx context.Context, cm *telego.ChatMemberUpdated) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("chatMember panic: %v", r)
		}
	}()
	if cm.NewChatMember.MemberUser().ID != a.meID {
		return // изменение касается не нашего бота (иных и не присылают, но всё же)
	}
	if cm.Chat.Type != telego.ChatTypeGroup && cm.Chat.Type != telego.ChatTypeSupergroup {
		return
	}
	switch cm.NewChatMember.MemberStatus() {
	case telego.MemberStatusMember:
		// добавили рядовым участником: privacy mode режет все медиа без упоминания
		a.reply(ctx, cm.Chat.ID,
			"Я в группе, но рядовой бот видит только команды, упоминания и реплаи — "+
				"голосовые/видео до меня не дойдут. Сделайте меня админом группы ИЛИ выключите "+
				"privacy mode: @BotFather → /setprivacy → выбрать бота → Disable.")
	case telego.MemberStatusAdministrator, telego.MemberStatusCreator:
		a.reply(ctx, cm.Chat.ID,
			"✓ теперь я вижу все сообщения группы, включая медиа. Кидайте голосовые/видео/аудио — "+
				"проиграю в палате (палата: /palata 6 или #6 в подписи).")
	}
}

func (a *app) reply(ctx context.Context, chatID int64, text string) {
	_, _ = a.bot.SendMessage(ctx, &telego.SendMessageParams{
		ChatID: telego.ChatID{ID: chatID}, Text: text,
	})
}

func (a *app) handle(ctx context.Context, m *telego.Message) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("handle panic: %v", r)
		}
	}()

	uid := int64(0)
	if m.From != nil {
		uid = m.From.ID
	}
	// anti-loop в группах: чужие боты не триггерят медиа/TTS. Исключение —
	// GroupAnonymousBot: через него пишут анонимные админы группы.
	if m.From != nil && m.From.IsBot && m.From.ID != groupAnonymousBotID {
		return
	}
	caption := m.Caption
	text := m.Text

	// контекст чата: группа и "адресованность" боту
	isGroup := m.Chat.Type == telego.ChatTypeGroup || m.Chat.Type == telego.ChatTypeSupergroup
	replyToBot := m.ReplyToMessage != nil && m.ReplyToMessage.From != nil &&
		m.ReplyToMessage.From.ID == a.meID
	mentioned := a.me != "" && strings.Contains(strings.ToLower(text+" "+caption), "@"+a.me)

	// --- команды (только чистый текст, без медиа) ---
	if text != "" && m.Voice == nil && m.Audio == nil && m.Video == nil && m.VideoNote == nil {
		if cmd, addressee, arg, ok := parseCommand(text); ok {
			if addressee != "" && addressee != a.me {
				return // команда адресована другому боту — молча пропускаем
			}
			switch strings.ToLower(cmd) {
			case "start", "help":
				a.reply(ctx, m.Chat.ID, a.helpText())
				return
			case "palata", "room":
				if arg == "" {
					a.reply(ctx, m.Chat.ID, "Текущая палата: "+a.store.get(uid, a.cfg.defPalata))
					return
				}
				if !palataRe.MatchString(arg) {
					a.reply(ctx, m.Chat.ID, "Неверный номер палаты: только a-z, 0-9, -, _ (до 64 символов).")
					return
				}
				a.store.set(uid, arg)
				a.reply(ctx, m.Chat.ID, "Палата запомнена: "+arg)
				return
			}
			// неизвестная команда: в личке подскажем, в группе молчим (шумно)
			if !isGroup {
				a.reply(ctx, m.Chat.ID, "Не знаю такой команды. /help — справка.")
				return
			}
		}
	}

	// --- палата: явная в тексте/подписи -> сохранённая -> дефолт ---
	palata := palataFromText(caption)
	if palata == "" {
		palata = palataFromText(text)
	}
	if palata == "" {
		palata = a.store.get(uid, a.cfg.defPalata)
	}
	if !palataRe.MatchString(palata) {
		a.reply(ctx, m.Chat.ID, "Неверный номер палаты.")
		return
	}

	// --- медиа ---
	var fileID string
	var dur int
	switch {
	case m.Voice != nil:
		fileID = m.Voice.FileID
		dur = m.Voice.Duration
	case m.Audio != nil:
		fileID = m.Audio.FileID
		dur = m.Audio.Duration
	case m.VideoNote != nil:
		fileID = m.VideoNote.FileID
		dur = m.VideoNote.Duration
	case m.Video != nil:
		fileID = m.Video.FileID
		dur = m.Video.Duration
	}

	if fileID != "" {
		// групповой этикет: играем только АДРЕСОВАННОЕ боту медиа —
		// @упоминание, реплай на сообщение бота или явная #палата в подписи.
		// Иначе (при выключенном privacy) бот hijack'ает каждое голосовое чата.
		if isGroup && !mentioned && !replyToBot && palataFromText(caption) == "" {
			return
		}
		if dur > maxUploadSec {
			a.reply(ctx, m.Chat.ID, fmt.Sprintf(
				"Слишком длинная запись: %d сек (максимум %d). Обрежь и пришли короче.",
				dur, maxUploadSec))
			return
		}
		isVideo := m.VideoNote != nil || m.Video != nil
		if err := a.streamFile(ctx, palata, fileID, isVideo); err != nil {
			log.Printf("streamFile: %v", err)
			a.reply(ctx, m.Chat.ID, "Не удалось проиграть: "+err.Error())
			return
		}
		a.reply(ctx, m.Chat.ID, "✓ проиграно в палате "+palata)
		return
	}

	// --- текст: озвучить, если упомянут бот (@) ИЛИ (в личке) есть #палата.
	// В группах #хэштеги — обычное дело, сами по себе TTS не триггерят.
	if text != "" {
		hasHash := hashRe.MatchString(text)
		if mentioned || replyToBot || (!isGroup && hasHash) {
			if !a.tts.enabled() {
				a.reply(ctx, m.Chat.ID, "TTS выключен (нет piper).")
				return
			}
			spoken := a.cleanText(text)
			if err := a.streamTTS(ctx, palata, spoken); err != nil {
				log.Printf("streamTTS: %v", err)
				a.reply(ctx, m.Chat.ID, "TTS ошибка: "+err.Error())
				return
			}
			a.reply(ctx, m.Chat.ID, "✓ озвучено в палате "+palata)
		}
	}
}

// cleanText убирает @упоминание бота и #палату из озвучиваемого текста.
func (a *app) cleanText(s string) string {
	if a.me != "" {
		s = regexp.MustCompile(`(?i)@`+regexp.QuoteMeta(a.me)).ReplaceAllString(s, "")
	}
	s = hashRe.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

func (a *app) streamFile(ctx context.Context, palata, fileID string, isVideo bool) error {
	dir, cleanup, err := tmpDir()
	if err != nil {
		return err
	}
	defer cleanup()

	src, err := a.download(ctx, fileID, dir)
	if err != nil {
		return err
	}
	if isVideo {
		ivf, ogg, err := toVideo(ctx, src, dir)
		if err != nil {
			return err
		}
		return a.pub.PublishVideo(palata, ivf, ogg)
	}
	ogg, err := toOgg(ctx, src, dir)
	if err != nil {
		return err
	}
	return a.pub.PublishAudio(palata, ogg)
}

func (a *app) streamTTS(ctx context.Context, palata, text string) error {
	dir, cleanup, err := tmpDir()
	if err != nil {
		return err
	}
	defer cleanup()
	ogg, err := a.tts.Synthesize(ctx, text, dir)
	if err != nil {
		return err
	}
	return a.pub.PublishAudio(palata, ogg)
}

// download тянет файл Telegram в dir, возвращает путь.
func (a *app) download(ctx context.Context, fileID, dir string) (string, error) {
	f, err := a.bot.GetFile(ctx, &telego.GetFileParams{FileID: fileID})
	if err != nil {
		return "", fmt.Errorf("getFile: %w", err)
	}
	url := a.bot.FileDownloadURL(f.FilePath)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download status %d", resp.StatusCode)
	}
	name := filepath.Base(f.FilePath)
	if name == "" || name == "." {
		name = "input"
	}
	dst := filepath.Join(dir, name)
	out, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	defer out.Close()
	if _, err := io.Copy(out, resp.Body); err != nil {
		return "", err
	}
	return dst, nil
}
