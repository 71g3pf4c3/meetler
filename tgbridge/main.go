// Telegram-мост: медиа, YouTube, TTS, аптечка звуков и аудио из палат.
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

// Лимит длительности (сек): всё, что длиннее, НЕ отклоняется, а принудительно
// обрезается на транскоде (ffmpeg -t maxPlaySec). Любое медиа проходит — просто
// в палату попадут первые maxPlaySec секунд.
const (
	maxPlaySec = 20
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

// --- per-chat выбранная палата + обратный индекс палата→чаты ---
// Обратный индекс нужен для "15с → TG": по имени палаты находим чаты, которые
// на неё подписаны (командой /palata), и шлём туда голосовое.
type palataStore struct {
	mu  sync.Mutex
	m   map[int64]string              // chatID -> палата
	rev map[string]map[int64]struct{} // палата -> set(chatID)
}

func newPalataStore() *palataStore {
	return &palataStore{m: map[int64]string{}, rev: map[string]map[int64]struct{}{}}
}
func (s *palataStore) get(key int64, def string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.m[key]; ok {
		return v
	}
	return def
}
func (s *palataStore) set(key int64, v string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.m[key]; ok && s.rev[old] != nil {
		delete(s.rev[old], key) // снять старую привязку
	}
	s.m[key] = v
	if s.rev[v] == nil {
		s.rev[v] = map[int64]struct{}{}
	}
	s.rev[v][key] = struct{}{}
}

// chatsFor возвращает чаты, привязанные к палате командой /palata.
func (s *palataStore) chatsFor(palata string) []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []int64
	for id := range s.rev[palata] {
		out = append(out, id)
	}
	return out
}

// валидный номер палаты = имя комнаты livekit (как в api)
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
		"• Голосовое / аудио / видео / кружок → проиграю всем в палате\n" +
		"  (длинное обрежу до " + fmt.Sprint(maxPlaySec) + " сек).\n" +
		"• /tovoice в реплае на сообщение с медиа (в т.ч. музыку документом) →\n" +
		"  проиграю это медиа в палате.\n" +
		"• /tts <текст> → озвучу текст (piper TTS).\n" +
		"• Текст с @упоминанием меня ИЛИ (в личке) с #палатой → тоже озвучу.\n" +
		"• Палата: #6 или /palata 6 в сообщении/подписи; задать для чата: /palata 6\n" +
		"  (в группе — общая на всех); посмотреть текущую: /palata; по умолчанию: " + a.cfg.defPalata + ".\n\n" +
		"В группах я реагирую только на адресованные мне сообщения: @упоминание,\n" +
		"реплай на моё сообщение или #палата в подписи к медиа (команды /tts и\n" +
		"/tovoice работают всегда). Чтобы я вообще видел медиа, я должен быть\n" +
		"админом группы или с выключенным privacy mode\n" +
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
	cfg         config
	bot         *telego.Bot
	me          string // username бота в нижнем регистре, без @
	meID        int64  // ID бота (фильтр my_chat_member)
	pub         *Publisher
	tts         *TTS
	store       *palataStore
	soundsvcURL string // API для загрузки звуков
	// mediaSem ограничивает одновременные публикации медиа в палаты (/tovoice,
	// пересланное медиа): не более cap(mediaSem) разом. Защита от спама.
	mediaSem chan struct{}
}

const maxConcurrentMedia = 3 // не более 3 медиа-публикаций одновременно

// acquireMedia неблокирующе занимает слот очереди медиа. ok=false → очередь
// занята (уже maxConcurrentMedia в работе). release() освобождает слот.
func (a *app) acquireMedia() (release func(), ok bool) {
	select {
	case a.mediaSem <- struct{}{}:
		return func() { <-a.mediaSem }, true
	default:
		return func() {}, false
	}
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
			{Command: "tts", Description: "Озвучить текст или реплай: /tts привет"},
			{Command: "tovoice", Description: "В реплае на медиа: проиграть его в палате"},
			{Command: "soundboardsave", Description: "В реплае на медиа: сохранить в аптечку палаты"},
		},
	})

	a := &app{
		cfg:         cfg,
		bot:         bot,
		me:          strings.ToLower(me.Username),
		meID:        me.ID,
		pub:         NewPublisher(cfg.lkURL, cfg.lkKey, cfg.lkSecret, 60*time.Second),
		tts:         NewTTS(cfg.piperBin, cfg.piperModel),
		store:       newPalataStore(),
		soundsvcURL: env("SOUNDSVC_URL", "http://host.docker.internal:8080"),
		mediaSem:    make(chan struct{}, maxConcurrentMedia),
	}
	log.Printf("tgbridge up as @%s, default palata=%s, tts=%v", a.me, cfg.defPalata, a.tts.enabled())

	// HTTP для передачи аудио комнаты из API в Telegram.
	go a.serveHTTP(env("TGBRIDGE_HTTP_ADDR", ":8090"))

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

// replyErr: единая реакция на ошибку публикации в палату. Если бота кикнули
// (палата на кулдауне) — показываем ровно «tg bot в тюрьме» без префикса.
// Иначе — «<prefix>: <err>».
func (a *app) replyErr(ctx context.Context, chatID int64, prefix string, err error) {
	if IsJailed(err) {
		a.reply(ctx, chatID, "tg bot в тюрьме")
		return
	}
	a.reply(ctx, chatID, prefix+": "+err.Error())
}

// cmdTTS: /tts <текст> — явный вызов озвучки (работает и в группах:
// команда сама по себе адресована боту, упоминание не нужно).
// Если аргумент пуст, но команда — РЕПЛАЙ на сообщение с текстом, озвучиваем
// текст того сообщения (/tts в ответ на чей-то текст).
func (a *app) cmdTTS(ctx context.Context, m *telego.Message, arg, palata string) {
	if arg == "" && m.ReplyToMessage != nil {
		rt := m.ReplyToMessage
		if t := strings.TrimSpace(rt.Text); t != "" {
			arg = t
		} else if c := strings.TrimSpace(rt.Caption); c != "" {
			arg = c
		}
	}
	if arg == "" {
		a.reply(ctx, m.Chat.ID, "Использование: /tts <текст> ИЛИ ответь /tts на сообщение с текстом — озвучу его в палате "+palata+".")
		return
	}
	if !a.tts.enabled() {
		a.reply(ctx, m.Chat.ID, "TTS выключен (нет piper).")
		return
	}
	if err := a.streamTTS(ctx, palata, a.cleanText(arg)); err != nil {
		log.Printf("streamTTS: %v", err)
		a.replyErr(ctx, m.Chat.ID, "TTS ошибка", err)
		return
	}
	a.reply(ctx, m.Chat.ID, "✓ озвучено в палате "+palata)
}

// replyMediaFileID извлекает fileID медиа из сообщения-реплая (voice/audio/
// video/video_note/документ-аудио-видео). Возвращает fileID, длительность,
// isVideo, предложенное имя файла. Переиспользуется /tovoice и /soundboardsave.
func replyMediaFileID(rt *telego.Message) (fileID string, dur int, isVideo bool, fname string) {
	if rt == nil {
		return "", 0, false, ""
	}
	switch {
	case rt.Voice != nil:
		return rt.Voice.FileID, rt.Voice.Duration, false, "voice.ogg"
	case rt.Audio != nil:
		return rt.Audio.FileID, rt.Audio.Duration, false, "audio"
	case rt.VideoNote != nil:
		return rt.VideoNote.FileID, rt.VideoNote.Duration, true, "note.mp4"
	case rt.Video != nil:
		return rt.Video.FileID, rt.Video.Duration, true, "video.mp4"
	case rt.Document != nil:
		mt := strings.ToLower(rt.Document.MimeType)
		switch {
		case strings.HasPrefix(mt, "audio/"):
			return rt.Document.FileID, 0, false, rt.Document.FileName
		case strings.HasPrefix(mt, "video/"):
			return rt.Document.FileID, 0, true, rt.Document.FileName
		}
	}
	return "", 0, false, ""
}

// cmdSoundboardSave: /soundboardsave в РЕПЛАЕ на сообщение с аудио/видео/
// голосовым — скачиваем, транскодим в ogg/opus (режется до maxPlaySec) и
// заливаем в звуковую аптечку палаты (api POST /sounds). Label — подпись
// команды (/soundboardsave моё имя) либо имя файла.
func (a *app) cmdSoundboardSave(ctx context.Context, m *telego.Message, arg, palata string) {
	fileID, _, _, fname := replyMediaFileID(m.ReplyToMessage)
	if fileID == "" {
		a.reply(ctx, m.Chat.ID,
			"Ответь /soundboardsave на сообщение с голосовым/аудио/видео/музыкой — сохраню в аптечку палаты "+palata+".")
		return
	}
	if a.soundsvcURL == "" {
		a.reply(ctx, m.Chat.ID, "Сохранение в аптечку не настроено (нет SOUNDSVC_URL).")
		return
	}
	label := strings.TrimSpace(arg)
	if label == "" {
		// авто-label из имени файла + таймстемп: Telegram часто отдаёт
		// безликие имена (video.mp4 → "video"), кнопки аптечки неразличимы.
		base := strings.TrimSuffix(fname, filepath.Ext(fname))
		if base == "" {
			base = "save"
		}
		label = base + " " + time.Now().Format("02.01 15:04")
	}
	if err := a.saveToSoundboard(ctx, palata, fileID, label); err != nil {
		log.Printf("soundboardsave: %v", err)
		a.reply(ctx, m.Chat.ID, "Не удалось сохранить: "+err.Error())
		return
	}
	a.reply(ctx, m.Chat.ID, "✓ сохранено в аптечку палаты "+palata+" как «"+label+"»")
}

// cmdToVoice: /tovoice в РЕПЛАЕ на сообщение с медиа (голосовое/аудио/видео/
// кружок/музыка документом) — проиграть это медиа в палате. Явная команда:
// в группах не требует @упоминания на исходном сообщении.
func (a *app) cmdToVoice(ctx context.Context, m *telego.Message, palata string) {
	rt := m.ReplyToMessage
	if rt == nil {
		a.reply(ctx, m.Chat.ID,
			"Ответь /tovoice на сообщение с голосовым/аудио/видео/музыкой — проиграю его в палате "+palata+".")
		return
	}
	var fileID string
	var dur int
	isVideo := false
	switch {
	case rt.Voice != nil:
		fileID, dur = rt.Voice.FileID, rt.Voice.Duration
	case rt.Audio != nil:
		fileID, dur = rt.Audio.FileID, rt.Audio.Duration
	case rt.VideoNote != nil:
		fileID, dur, isVideo = rt.VideoNote.FileID, rt.VideoNote.Duration, true
	case rt.Video != nil:
		fileID, dur, isVideo = rt.Video.FileID, rt.Video.Duration, true
	case rt.Document != nil:
		// музыка часто приходит документом (mp3-файлом): берём по mime
		mt := strings.ToLower(rt.Document.MimeType)
		switch {
		case strings.HasPrefix(mt, "audio/"):
			fileID = rt.Document.FileID
		case strings.HasPrefix(mt, "video/"):
			fileID, isVideo = rt.Document.FileID, true
		}
	}
	if fileID == "" {
		a.reply(ctx, m.Chat.ID, "В этом сообщении нет медиа (нужны голосовое/аудио/видео/музыка).")
		return
	}
	// длинное медиа не отклоняем — транскод обрежет до maxPlaySec
	if dur > maxPlaySec {
		a.reply(ctx, m.Chat.ID, fmt.Sprintf("Запись %d сек — проиграю первые %d.", dur, maxPlaySec))
	}
	if err := a.streamFile(ctx, palata, fileID, isVideo); err != nil {
		log.Printf("streamFile: %v", err)
		a.replyErr(ctx, m.Chat.ID, "Не удалось проиграть", err)
		return
	}
	a.reply(ctx, m.Chat.ID, "✓ проиграно в палате "+palata)
}

func (a *app) handle(ctx context.Context, m *telego.Message) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("handle panic: %v", r)
		}
	}()

	// палата запоминается НА ЧАТ, а не на юзера: в группе — одна общая палата
	// для всех её участников; в личке chat.ID == личный чат (персонально).
	pkey := m.Chat.ID
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

	// --- палата: явная в тексте/подписи -> сохранённая -> дефолт ---
	// (до команд: /tts и /tovoice должны знать, куда играть)
	palata := palataFromText(caption)
	if palata == "" {
		palata = palataFromText(text)
	}
	if palata == "" {
		palata = a.store.get(pkey, a.cfg.defPalata)
	}
	if !palataRe.MatchString(palata) {
		a.reply(ctx, m.Chat.ID, "Неверный номер палаты.")
		return
	}

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
					a.reply(ctx, m.Chat.ID, "Текущая палата: "+palata)
					return
				}
				if !palataRe.MatchString(arg) {
					a.reply(ctx, m.Chat.ID, "Неверный номер палаты: только a-z, 0-9, -, _ (до 64 символов).")
					return
				}
				a.store.set(pkey, arg)
				a.reply(ctx, m.Chat.ID, "Палата запомнена: "+arg)
				return
			case "tts":
				a.cmdTTS(ctx, m, arg, palata)
				return
			case "tovoice":
				a.cmdToVoice(ctx, m, palata)
				return
			case "soundboardsave":
				a.cmdSoundboardSave(ctx, m, arg, palata)
				return
			}
			// неизвестная команда: в личке подскажем, в группе молчим (шумно)
			if !isGroup {
				a.reply(ctx, m.Chat.ID, "Не знаю такой команды. /help — справка.")
				return
			}
		}
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
		// длинное медиа не отклоняем — транскод обрежет до maxPlaySec
		if dur > maxPlaySec {
			a.reply(ctx, m.Chat.ID, fmt.Sprintf("Запись %d сек — проиграю первые %d.", dur, maxPlaySec))
		}
		isVideo := m.VideoNote != nil || m.Video != nil
		if err := a.streamFile(ctx, palata, fileID, isVideo); err != nil {
			log.Printf("streamFile: %v", err)
			a.replyErr(ctx, m.Chat.ID, "Не удалось проиграть", err)
			return
		}
		a.reply(ctx, m.Chat.ID, "✓ проиграно в палате "+palata)
		return
	}

	// --- YouTube-ссылка: скачать первые maxPlaySec секунд и проиграть видео.
	// В группах — только адресованное боту (упоминание/реплай/#палата), чтобы
	// не хватать каждую ссылку в чате.
	if yt := extractYouTubeURL(text + " " + caption); yt != "" {
		if !isGroup || mentioned || replyToBot || palataFromText(caption) != "" || palataFromText(text) != "" {
			a.reply(ctx, m.Chat.ID, "Качаю первые "+fmt.Sprint(maxPlaySec)+"с с YouTube в палату "+palata+"…")
			if err := a.streamYouTube(ctx, palata, yt); err != nil {
				log.Printf("streamYouTube: %v", err)
				a.replyErr(ctx, m.Chat.ID, "YouTube не удался", err)
				return
			}
			a.reply(ctx, m.Chat.ID, "✓ проиграно в палате "+palata)
			return
		}
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
				a.replyErr(ctx, m.Chat.ID, "TTS ошибка", err)
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

// errQueueFull — очередь медиа занята (уже maxConcurrentMedia публикуется).
var errQueueFull = fmt.Errorf("очередь занята: уже %d медиа играет, попробуй через пару секунд", maxConcurrentMedia)

func (a *app) streamFile(ctx context.Context, palata, fileID string, isVideo bool) error {
	// очередь: не более maxConcurrentMedia публикаций разом (/tovoice, медиа)
	release, ok := a.acquireMedia()
	if !ok {
		return errQueueFull
	}
	defer release()

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
	// очередь: TTS тоже под лимитом (иначе спам /tts → N piper-процессов + коннектов)
	release, ok := a.acquireMedia()
	if !ok {
		return errQueueFull
	}
	defer release()

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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("download req: %w", err)
	}
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
