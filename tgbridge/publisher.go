package main

import (
	"fmt"
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
)

// Publisher держит по одному подключению к палате и льёт в неё медиа-файлы
// последовательно. Подключение переиспользуется; простаивающее закрывается по TTL.
type Publisher struct {
	url, apiKey, apiSecret string
	idleTTL                time.Duration

	mu    sync.Mutex
	rooms map[string]*roomConn
}

type roomConn struct {
	room     *lksdk.Room
	lastUsed time.Time
}

func NewPublisher(url, apiKey, apiSecret string, idleTTL time.Duration) *Publisher {
	p := &Publisher{
		url: url, apiKey: apiKey, apiSecret: apiSecret, idleTTL: idleTTL,
		rooms: map[string]*roomConn{},
	}
	go p.reaper()
	return p
}

func (p *Publisher) reaper() {
	t := time.NewTicker(10 * time.Second)
	for range t.C {
		p.mu.Lock()
		for name, rc := range p.rooms {
			if time.Since(rc.lastUsed) > p.idleTTL {
				rc.room.Disconnect()
				delete(p.rooms, name)
			}
		}
		p.mu.Unlock()
	}
}

func (p *Publisher) getRoom(palata string) (*lksdk.Room, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if rc, ok := p.rooms[palata]; ok {
		rc.lastUsed = time.Now()
		return rc.room, nil
	}
	room, err := lksdk.ConnectToRoom(p.url, lksdk.ConnectInfo{
		APIKey:              p.apiKey,
		APISecret:           p.apiSecret,
		RoomName:            palata,
		ParticipantIdentity: "tgbridge",
		ParticipantName:     "Telegram",
	}, lksdk.NewRoomCallback())
	if err != nil {
		return nil, fmt.Errorf("connect palata %q: %w", palata, err)
	}
	p.rooms[palata] = &roomConn{room: room, lastUsed: time.Now()}
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
		return fmt.Errorf("publish video: %w", err)
	}
	apub, err := room.LocalParticipant.PublishTrack(atrack, &lksdk.TrackPublicationOptions{
		Name: "tg-video-audio", Source: livekit.TrackSource_MICROPHONE,
	})
	if err != nil {
		_ = room.LocalParticipant.UnpublishTrack(vpub.SID())
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
