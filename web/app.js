// hit-meet браузерный клиент на livekit-client (пинованная версия).
// Фокус: latency + stability. Все tuning-решения прокомментированы.
import {
  Room,
  RoomEvent,
  Track,
  VideoPresets,
  LocalAudioTrack,
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
  t = { el, videoEl: null, ph, label };
  tiles.set(participant.sid, t);
  return t;
}

// обновить ник на тайле (name пришёл/изменился после создания тайла)
function updateTileLabel(participant) {
  const t = tiles.get(participant.sid);
  if (t) t.label.textContent = participant.name || participant.identity;
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
    // подмешать в микс для записи (весь звук комнаты)
    addToRecordMix(track.mediaStreamTrack);
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
  // getUserMedia доступен только в secure context (https или localhost).
  // По http на IP/домене navigator.mediaDevices === undefined → нет запроса доступа.
  if (!window.isSecureContext || !navigator.mediaDevices) {
    $('err').textContent =
      'Нужен HTTPS (или localhost). По http:// браузер блокирует доступ к микрофону/камере. ' +
      'Открой через https:// либо с localhost.';
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
        // Аудио: минимальная задержка, минимум "жатия".
        // DTX ВЫКЛ — DTX режет начала фраз (comfort noise вместо голоса на старте).
        // RED ВЫКЛ — простой путь, чинит "одно ухо".
        // Повышенный битрейт opus (48k) — меньше компрессионных артефактов.
        dtx: false,
        red: false,
        audioPreset: { maxBitrate: 48000 },
        // стартовый кодек видео: vp8 максимально совместим; vp9/av1 — через negotiate.
        videoCodec: 'vp8',
      },
    });

    wireRoomEvents(room);

    await room.connect(wsUrl(), token, {
      // автоподписка на все треки в комнате
      autoSubscribe: true,
    });

    // Публикуем микрофон и камеру РАЗДЕЛЬНО: если нет камеры (или отказ по видео),
    // звук всё равно должен работать. Общий enableCameraAndMicrophone падал целиком.
    let micOk = false, camOk = false;
    try {
      // mono-захват, минимальная обработка для низкой задержки.
      // echoCancellation оставляем (без него эхо хуже всего). NS/AGC выключены —
      // они добавляют latency и "жмут" звук. latency hint = 0 (минимальный буфер).
      await room.localParticipant.setMicrophoneEnabled(true, {
        channelCount: 1,
        echoCancellation: true,
        noiseSuppression: false,
        autoGainControl: false,
        latency: 0,
      });
      micOk = true;
    } catch (e) {
      console.warn('mic failed:', e);
    }
    try {
      await room.localParticipant.setCameraEnabled(true);
      camOk = true;
    } catch (e) {
      console.warn('cam failed:', e);
    }
    if (!micOk && !camOk) {
      $('err').textContent =
        'Нет доступа к микрофону и камере. Разреши доступ в браузере (значок замка в адресной строке).';
    } else if (!micOk) {
      $('err').textContent = 'Микрофон недоступен — другие тебя не услышат.';
    } else if (!camOk) {
      $('err').textContent = 'Камера недоступна — звук работает, видео нет.';
    }
    $('micBtn').textContent = 'Mic: ' + (micOk ? 'on' : 'off');
    $('camBtn').textContent = 'Cam: ' + (camOk ? 'on' : 'off');

    // поднять запись микса + подмешать свой микрофон
    initRecordMix();
    if (micOk) {
      const micPub = room.localParticipant.getTrackPublication(Track.Source.Microphone);
      if (micPub && micPub.track) addToRecordMix(micPub.track.mediaStreamTrack);
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
  r.on(RoomEvent.ParticipantNameChanged, (_name, p) => updateTileLabel(p));
  r.on(RoomEvent.LocalTrackPublished, (pub) => {
    if (pub.track && pub.track.kind === Track.Kind.Video) {
      attachLocalVideo(pub.track);
    }
  });
  r.on(RoomEvent.Disconnected, () => {
    cleanup();
  });
  // Зелёная подсветка говорящих. ActiveSpeakersChanged даёт актуальный список
  // активных спикеров (включая локального участника).
  r.on(RoomEvent.ActiveSpeakersChanged, (speakers) => {
    const speakingSids = new Set(speakers.map((p) => p.sid));
    const localSpeaking = r.localParticipant && speakingSids.has(r.localParticipant.sid);

    // remote-тайлы
    tiles.forEach((t, sid) => {
      t.el.classList.toggle('speaking', speakingSids.has(sid));
    });
    // локальный тайл (если есть)
    if (localTile) {
      localTile.classList.toggle('speaking', !!localSpeaking);
    }
  });
}

let localTile = null;
function attachLocalVideo(track) {
  if (!localTile) {
    const el = document.createElement('div');
    el.className = 'tile';
    const label = document.createElement('div');
    label.className = 'label';
    const myName = (room && room.localParticipant && room.localParticipant.name) || 'Вы';
    label.textContent = `${myName} (вы)`;
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
// --- ГОЙДА: проиграть mp3 для всех в комнате ---
let goidaCtx = null;
let goidaBuffer = null;
let goidaTrack = null;      // опубликованный LocalAudioTrack
let goidaPub = null;        // publication для unpublish
let goidaPlaying = false;

async function loadGoida() {
  if (goidaBuffer) return;
  goidaCtx = goidaCtx || new (window.AudioContext || window.webkitAudioContext)();
  const res = await fetch('./goida.mp3');
  const buf = await res.arrayBuffer();
  goidaBuffer = await goidaCtx.decodeAudioData(buf);
}

$('goidaBtn').onclick = async () => {
  if (!room) return;
  if (goidaPlaying) return;  // не запускать поверх играющего
  try {
    await loadGoida();
    if (goidaCtx.state === 'suspended') await goidaCtx.resume();

    // граф: buffer -> gain -> [dest для публикации] + [speakers локально]
    const src = goidaCtx.createBufferSource();
    src.buffer = goidaBuffer;
    const dest = goidaCtx.createMediaStreamDestination();
    src.connect(dest);              // в комнату
    src.connect(goidaCtx.destination); // себе в колонки

    // публикуем аудио-трек в комнату — слышат все
    const mst = dest.stream.getAudioTracks()[0];
    addToRecordMix(mst);            // ГОЙДА попадает в запись 15с
    goidaTrack = new LocalAudioTrack(mst);
    goidaPub = await room.localParticipant.publishTrack(goidaTrack, {
      name: 'goida',
      source: Track.Source.Unknown,
    });

    goidaPlaying = true;
    $('goidaBtn').classList.add('playing');

    src.onended = async () => {
      goidaPlaying = false;
      $('goidaBtn').classList.remove('playing');
      try {
        if (goidaPub) await room.localParticipant.unpublishTrack(goidaTrack);
      } catch (_) {}
      goidaTrack = null; goidaPub = null;
    };
    src.start();
  } catch (e) {
    $('err').textContent = 'ГОЙДА не удалась: ' + (e.message || e);
    goidaPlaying = false;
    $('goidaBtn').classList.remove('playing');
  }
};

// --- Запись последних 15 секунд микса комнаты ---
// PCM-кольцевой буфер (ScriptProcessor). Даёт РОВНО последние 15с и валидный файл
// (webm нельзя обрезать по времени на клиенте — заголовок только в первом чанке).
// На выходе .wav (мгновенно, без энкодера, корректная обрезка по сэмплам).
const REC_WINDOW_SEC = 15;
let recCtx = null;
let recProc = null;                 // ScriptProcessorNode
let recMixGain = null;              // сюда сходятся все источники
let recRing = null;                 // Float32Array кольцевой буфер
let recRingSize = 0;
let recWritePos = 0;
let recFilled = 0;
let recSampleRate = 48000;
const recConnected = new WeakSet();

function initRecordMix() {
  if (recCtx) return;
  recCtx = new (window.AudioContext || window.webkitAudioContext)();
  recSampleRate = recCtx.sampleRate;
  recRingSize = Math.ceil(recSampleRate * REC_WINDOW_SEC);
  recRing = new Float32Array(recRingSize);
  recWritePos = 0; recFilled = 0;

  recMixGain = recCtx.createGain();
  recProc = recCtx.createScriptProcessor(4096, 1, 1);
  recMixGain.connect(recProc);
  // ScriptProcessor требует подключения к destination, иначе не тикает.
  // Через нулевой gain, чтобы не было двойного воспроизведения/эха.
  const mute = recCtx.createGain();
  mute.gain.value = 0;
  recProc.connect(mute);
  mute.connect(recCtx.destination);

  recProc.onaudioprocess = (e) => {
    const inBuf = e.inputBuffer.getChannelData(0);
    for (let i = 0; i < inBuf.length; i++) {
      recRing[recWritePos] = inBuf[i];
      recWritePos = (recWritePos + 1) % recRingSize;
      if (recFilled < recRingSize) recFilled++;
    }
  };
}

// подмешать произвольный MediaStreamTrack (mic/remote/goida) в микс записи
function addToRecordMix(mst) {
  if (!mst || !recCtx || recConnected.has(mst)) return;
  try {
    const src = recCtx.createMediaStreamSource(new MediaStream([mst]));
    src.connect(recMixGain);
    recConnected.add(mst);
  } catch (e) {
    console.warn('addToRecordMix failed:', e);
  }
}

function encodeWav(samples, sampleRate) {
  const buf = new ArrayBuffer(44 + samples.length * 2);
  const view = new DataView(buf);
  const wr = (off, s) => { for (let i = 0; i < s.length; i++) view.setUint8(off + i, s.charCodeAt(i)); };
  wr(0, 'RIFF');
  view.setUint32(4, 36 + samples.length * 2, true);
  wr(8, 'WAVE'); wr(12, 'fmt ');
  view.setUint32(16, 16, true);
  view.setUint16(20, 1, true);        // PCM
  view.setUint16(22, 1, true);        // mono
  view.setUint32(24, sampleRate, true);
  view.setUint32(28, sampleRate * 2, true);
  view.setUint16(32, 2, true);
  view.setUint16(34, 16, true);
  wr(36, 'data');
  view.setUint32(40, samples.length * 2, true);
  let off = 44;
  for (let i = 0; i < samples.length; i++, off += 2) {
    const s = Math.max(-1, Math.min(1, samples[i]));
    view.setInt16(off, s < 0 ? s * 0x8000 : s * 0x7fff, true);
  }
  return new Blob([buf], { type: 'audio/wav' });
}

$('recBtn').onclick = () => {
  if (!recCtx || recFilled === 0) {
    $('err').textContent = 'Пока нечего скачивать (запись ещё копится).';
    return;
  }
  // собрать линейный хвост из кольцевого буфера (последние recFilled сэмплов)
  const out = new Float32Array(recFilled);
  const start = (recWritePos - recFilled + recRingSize) % recRingSize;
  for (let i = 0; i < recFilled; i++) out[i] = recRing[(start + i) % recRingSize];

  const blob = encodeWav(out, recSampleRate);
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  const ts = new Date().toISOString().replace(/[:.]/g, '-');
  a.href = url;
  a.download = `hit-meet-${ts}.wav`;
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 10000);
};

$('leaveBtn').onclick = () => { if (room) room.disconnect(); };

function cleanup() {
  try { if (recRecorder && recRecorder.state !== 'inactive') recRecorder.stop(); } catch (_) {}
  try { if (recCtx) recCtx.close(); } catch (_) {}
  recCtx = null; recDest = null; recRecorder = null; recChunks = [];
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
