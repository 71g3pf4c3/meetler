package main

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
)

// jailCooldown — сколько бот НЕ лезет в палату после того, как его оттуда
// выписали (кик голосованием/миной). Чтобы не долбиться в закрытую дверь.
const jailCooldown = 30 * time.Second

// errJailed — палата на кулдауне после кика бота. Пользователю показываем
// «tg bot в тюрьме».
var errJailed = errors.New("tg bot в тюрьме")

// IsJailed: ошибка означает, что бот сидит в тюрьме (кулдаун после кика)?
func IsJailed(err error) bool { return errors.Is(err, errJailed) }

// isConnClosed распознаёт ошибку публикации в разорванный (кикнутый) коннект.
func isConnClosed(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "connection closed") ||
		strings.Contains(s, "InvalidStateError") ||
		strings.Contains(s, "closed")
}

// Publisher переиспользует подключение к каждой палате и закрывает его по idle TTL.
type Publisher struct {
	url, apiKey, apiSecret string
	idleTTL                time.Duration

	mu    sync.Mutex
	rooms map[string]*roomConn
	jail  map[string]time.Time // палата -> до какого момента бот «в тюрьме»
}

type roomConn struct {
	room     *lksdk.Room
	lastUsed time.Time
	// intentional — общий с callback-ом флаг «закрываем умышленно» (idle/jail).
	// Указатель захватывается замыканием OnDisconnected: когда мы сами рвём
	// коннект, выставляем *intentional=true, и callback не джейлит палату.
	intentional *atomic.Bool
}

func NewPublisher(url, apiKey, apiSecret string, idleTTL time.Duration) *Publisher {
	p := &Publisher{
		url: url, apiKey: apiKey, apiSecret: apiSecret, idleTTL: idleTTL,
		rooms: map[string]*roomConn{},
		jail:  map[string]time.Time{},
	}
	go p.reaper()
	return p
}

// jailRoom помечает палату «в тюрьме» на jailCooldown и выбрасывает её коннект:
// бот не будет переподключаться, пока кулдаун не истечёт. Disconnect делаем
// ВНЕ мьютекса — он синхронно дёргает OnDisconnected (который снова лочит mu).
func (p *Publisher) jailRoom(palata string) {
	p.mu.Lock()
	p.jail[palata] = time.Now().Add(jailCooldown)
	var toClose *lksdk.Room
	if rc, ok := p.rooms[palata]; ok {
		rc.intentional.Store(true) // мы сами рвём — callback не должен повторно джейлить
		toClose = rc.room
		delete(p.rooms, palata)
	}
	p.mu.Unlock()
	if toClose != nil {
		toClose.Disconnect() // вне лока — избегаем reentrant lock в OnDisconnected
	}
}

// jailed: палата ещё на кулдауне? (чистит истёкшие)
func (p *Publisher) jailed(palata string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	until, ok := p.jail[palata]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(p.jail, palata)
		return false
	}
	return true
}

func (p *Publisher) reaper() {
	t := time.NewTicker(10 * time.Second)
	for range t.C {
		var toClose []*lksdk.Room
		p.mu.Lock()
		for name, rc := range p.rooms {
			if time.Since(rc.lastUsed) > p.idleTTL {
				rc.intentional.Store(true) // idle-close, НЕ кик → OnDisconnected не джейлит
				toClose = append(toClose, rc.room)
				delete(p.rooms, name)
			}
		}
		p.mu.Unlock()
		for _, r := range toClose {
			r.Disconnect() // вне лока — избегаем reentrant lock в OnDisconnected
		}
	}
}

func (p *Publisher) getRoom(palata string) (*lksdk.Room, error) {
	// на кулдауне после кика — не лезем (вне мьютекса: jailed сам лочит)
	if p.jailed(palata) {
		return nil, errJailed
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if rc, ok := p.rooms[palata]; ok {
		rc.lastUsed = time.Now()
		return rc.room, nil
	}
	intentional := new(atomic.Bool)
	cb := lksdk.NewRoomCallback()
	// кик бота = разрыв соединения. Джейлим палату на кулдаун, чтобы не
	// переподключаться следующие jailCooldown секунд. Если коннект рвём МЫ сами
	// (idle/jail) — *intentional=true, и тогда НЕ джейлим (это не кик).
	cb.OnDisconnected = func() {
		if !intentional.Load() {
			p.jailRoom(palata)
		}
	}
	room, err := lksdk.ConnectToRoom(p.url, lksdk.ConnectInfo{
		APIKey:              p.apiKey,
		APISecret:           p.apiSecret,
		RoomName:            palata,
		ParticipantIdentity: "tgbridge",
		ParticipantName:     "Telegram",
	}, cb)
	if err != nil {
		return nil, fmt.Errorf("connect palata %q: %w", palata, err)
	}
	p.rooms[palata] = &roomConn{room: room, lastUsed: time.Now(), intentional: intentional}
	return room, nil
}

// PublishAudio льёт один .ogg (opus) файл в палату и ждёт окончания.
func (p *Publisher) PublishAudio(palata, oggPath string) error {
	room, err := p.getRoom(palata)
	if err != nil {
		return err
	}
	done := make(chan struct{})
	track, err := lksdk.NewLocalFileTrack(oggPath,
		lksdk.ReaderTrackWithOnWriteComplete(func() { close(done) }),
	)
	if err != nil {
		return fmt.Errorf("audio track: %w", err)
	}
	pub, err := room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{
		Name:   "tg-audio",
		Source: livekit.TrackSource_MICROPHONE,
	})
	if err != nil {
		if isConnClosed(err) { // бота кикнули между getRoom и publish
			p.jailRoom(palata)
			return errJailed
		}
		return fmt.Errorf("publish audio: %w", err)
	}
	<-done
	// небольшой хвост, чтобы последние пакеты дошли до подписчиков
	time.Sleep(300 * time.Millisecond)
	_ = room.LocalParticipant.UnpublishTrack(pub.SID())
	p.touch(palata)
	return nil
}

// PublishVideo льёт .ivf (видео) + .ogg (аудио) синхронно и ждёт окончания обоих.
func (p *Publisher) PublishVideo(palata, ivfPath, oggPath string) error {
	room, err := p.getRoom(palata)
	if err != nil {
		return err
	}
	var vdone, adone = make(chan struct{}), make(chan struct{})

	vtrack, err := lksdk.NewLocalFileTrack(ivfPath,
		lksdk.ReaderTrackWithOnWriteComplete(func() { close(vdone) }),
	)
	if err != nil {
		return fmt.Errorf("video track: %w", err)
	}
	atrack, err := lksdk.NewLocalFileTrack(oggPath,
		lksdk.ReaderTrackWithOnWriteComplete(func() { close(adone) }),
	)
	if err != nil {
		return fmt.Errorf("audio track: %w", err)
	}

	vpub, err := room.LocalParticipant.PublishTrack(vtrack, &lksdk.TrackPublicationOptions{
		Name: "tg-video", Source: livekit.TrackSource_CAMERA,
	})
	if err != nil {
		if isConnClosed(err) {
			p.jailRoom(palata)
			return errJailed
		}
		return fmt.Errorf("publish video: %w", err)
	}
	apub, err := room.LocalParticipant.PublishTrack(atrack, &lksdk.TrackPublicationOptions{
		Name: "tg-video-audio", Source: livekit.TrackSource_MICROPHONE,
	})
	if err != nil {
		_ = room.LocalParticipant.UnpublishTrack(vpub.SID())
		if isConnClosed(err) {
			p.jailRoom(palata)
			return errJailed
		}
		return fmt.Errorf("publish video-audio: %w", err)
	}

	<-vdone
	<-adone
	time.Sleep(300 * time.Millisecond)
	_ = room.LocalParticipant.UnpublishTrack(vpub.SID())
	_ = room.LocalParticipant.UnpublishTrack(apub.SID())
	p.touch(palata)
	return nil
}

func (p *Publisher) touch(palata string) {
	p.mu.Lock()
	if rc, ok := p.rooms[palata]; ok {
		rc.lastUsed = time.Now()
	}
	p.mu.Unlock()
}
