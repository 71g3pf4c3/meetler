// hit-meet браузерный клиент на livekit-client (пинованная версия).
// Фокус: latency + stability. Все tuning-решения прокомментированы.
import {
  Room,
  RoomEvent,
  Track,
  VideoPresets,
  createLocalScreenTracks,
} from 'https://cdn.jsdelivr.net/npm/livekit-client@2.23.0/dist/livekit-client.esm.mjs';

const $ = (id) => document.getElementById(id);
const grid = $('grid');
const tiles = new Map(); // participant.sid -> { el, videoEl }

let room = null;

async function mintToken(roomName, name) {
  const res = await fetch('/api/token', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ room: roomName, name }),
  });
  if (!res.ok) throw new Error(await res.text());
  return res.json();
}

function wsUrl() {
  // signaling идёт через тот же домен по wss (Caddy проксирует /rtc).
  const proto = location.protocol === 'https:' ? 'wss' : 'ws';
  return `${proto}://${location.host}`;
}

function ensureTile(participant) {
  let t = tiles.get(participant.sid);
  if (t) return t;
  const el = document.createElement('div');
  el.className = 'tile';
  const label = document.createElement('div');
  label.className = 'label';
  label.textContent = participant.name || participant.identity;
  const ph = document.createElement('div');
  ph.className = 'ph';
  ph.textContent = 'нет видео';
  el.appendChild(ph);
  el.appendChild(label);
  grid.appendChild(el);
  t = { el, videoEl: null, ph };
  tiles.set(participant.sid, t);
  return t;
}

function removeTile(participant) {
  const t = tiles.get(participant.sid);
  if (t) {
    t.el.remove();
    tiles.delete(participant.sid);
  }
}

function attachTrack(participant, track) {
  const t = ensureTile(participant);
  if (track.kind === Track.Kind.Video) {
    const el = track.attach();
    el.autoplay = true;
    el.playsInline = true;
    if (t.videoEl) t.videoEl.remove();
    t.ph.style.display = 'none';
    t.el.insertBefore(el, t.el.firstChild);
    t.videoEl = el;
  } else if (track.kind === Track.Kind.Audio) {
    // аудио: attach к скрытому элементу, autoplay
    const el = track.attach();
    el.autoplay = true;
    el.style.display = 'none';
    document.body.appendChild(el);
  }
}

function detachTrack(track) {
  track.detach().forEach((el) => el.remove());
}

async function join() {
  $('err').textContent = '';
  const roomName = $('room').value.trim();
  const name = $('name').value.trim();
  if (!roomName) {
    $('err').textContent = 'Укажите комнату';
    return;
  }
  $('joinBtn').disabled = true;

  try {
    const { token } = await mintToken(roomName, name);

    room = new Room({
      // adaptiveStream: клиент сам просит у SFU слой под размер видимого тайла —
      // экономит downlink, снижает лаг при многих участниках.
      adaptiveStream: true,
      // dynacast: SFU не гоняет слои, которые никто не смотрит.
      dynacast: true,
      // reconnect-политика для стабильности: агрессивнее держимся за соединение.
      disconnectOnPageLeave: true,
      publishDefaults: {
        // simulcast ВКЛючён: публикуем 3 слоя, SFU раздаёт под bandwidth получателя.
        simulcast: true,
        videoSimulcastLayers: [
          VideoPresets.h180,
          VideoPresets.h360,
          VideoPresets.h720,
        ],
        // DTX + RED для аудио: тишина не жрёт битрейт, потери скрываются.
        dtx: true,
        red: true,
        // стартовый кодек видео: vp8 максимально совместим; vp9/av1 — через negotiate.
        videoCodec: 'vp8',
      },
    });

    wireRoomEvents(room);

    await room.connect(wsUrl(), token, {
      // автоподписка на все треки в комнате
      autoSubscribe: true,
    });

    // публикуем камеру+микрофон после коннекта (не блокируем вход при отказе доступа)
    try {
      await room.localParticipant.enableCameraAndMicrophone();
    } catch (e) {
      $('err').textContent = 'Нет доступа к камере/микрофону: ' + e.message;
    }

    $('join').style.display = 'none';
    $('stage').style.display = 'flex';
    startStats();
  } catch (e) {
    $('err').textContent = 'Ошибка входа: ' + (e.message || e);
    $('joinBtn').disabled = false;
  }
}

function wireRoomEvents(r) {
  r.on(RoomEvent.TrackSubscribed, (track, _pub, participant) => {
    attachTrack(participant, track);
  });
  r.on(RoomEvent.TrackUnsubscribed, (track) => {
    detachTrack(track);
  });
  r.on(RoomEvent.ParticipantConnected, (p) => ensureTile(p));
  r.on(RoomEvent.ParticipantDisconnected, (p) => removeTile(p));
  r.on(RoomEvent.LocalTrackPublished, (pub) => {
    if (pub.track && pub.track.kind === Track.Kind.Video) {
      attachLocalVideo(pub.track);
    }
  });
  r.on(RoomEvent.Disconnected, () => {
    cleanup();
  });
  // ConnectionQuality: можно индикатор качества на тайл; оставлено как hook.
}

let localTile = null;
function attachLocalVideo(track) {
  if (!localTile) {
    const el = document.createElement('div');
    el.className = 'tile';
    const label = document.createElement('div');
    label.className = 'label';
    label.textContent = 'Вы';
    el.appendChild(label);
    grid.insertBefore(el, grid.firstChild);
    localTile = el;
  }
  const v = track.attach();
  v.autoplay = true;
  v.playsInline = true;
  v.muted = true; // локальное видео без эха
  const old = localTile.querySelector('video');
  if (old) old.remove();
  localTile.insertBefore(v, localTile.firstChild);
}

// controls
$('joinBtn').onclick = join;
$('room').addEventListener('keydown', (e) => { if (e.key === 'Enter') join(); });

$('micBtn').onclick = async () => {
  const enabled = room.localParticipant.isMicrophoneEnabled;
  await room.localParticipant.setMicrophoneEnabled(!enabled);
  $('micBtn').textContent = 'Mic: ' + (!enabled ? 'on' : 'off');
};
$('camBtn').onclick = async () => {
  const enabled = room.localParticipant.isCameraEnabled;
  await room.localParticipant.setCameraEnabled(!enabled);
  $('camBtn').textContent = 'Cam: ' + (!enabled ? 'on' : 'off');
};
$('shareBtn').onclick = async () => {
  try {
    const tracks = await createLocalScreenTracks({ audio: true });
    for (const t of tracks) await room.localParticipant.publishTrack(t);
  } catch (e) {
    $('err').textContent = 'Демонстрация отменена: ' + e.message;
  }
};
$('leaveBtn').onclick = () => { if (room) room.disconnect(); };

function cleanup() {
  tiles.forEach((t) => t.el.remove());
  tiles.clear();
  if (localTile) { localTile.remove(); localTile = null; }
  $('stage').style.display = 'none';
  $('join').style.display = 'flex';
  $('joinBtn').disabled = false;
  stopStats();
}

// --- лёгкий HUD с качеством соединения (для отладки latency/loss) ---
let statsTimer = null;
function startStats() {
  statsTimer = setInterval(() => {
    if (!room) return;
    const q = room.localParticipant.connectionQuality;
    $('stats').textContent = `link: ${q} · peers: ${room.numParticipants}`;
  }, 1000);
}
function stopStats() {
  if (statsTimer) clearInterval(statsTimer);
  statsTimer = null;
  $('stats').textContent = '';
}
