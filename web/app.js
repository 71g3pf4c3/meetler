// meetler браузерный клиент на livekit-client (пинованная версия).
// Фокус: latency + stability. Все tuning-решения прокомментированы.
import {
  Room,
  RoomEvent,
  Track,
  VideoPresets,
  LocalAudioTrack,
  createLocalScreenTracks,
} from '/vendor/livekit-client.esm.mjs';   // локально: без CDN, быстрее, оффлайн-safe

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
  ph.className = 'ph';   // фон = дефолтная аватарка (CSS)
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
    // МИНИМАЛЬНАЯ задержка приёма: режем jitter buffer приёмника до ~0.
    // Главный рычаг latency на стороне получателя (экономит 50-150ms).
    try {
      const receiver = track.receiver;
      if (receiver && 'jitterBufferTarget' in receiver) {
        receiver.jitterBufferTarget = 0;
      }
    } catch (_) {}
    // подмешать в микс для записи (весь звук комнаты)
    addToRecordMix(track.mediaStreamTrack);
  }
}

function detachTrack(track) {
  track.detach().forEach((el) => el.remove());
}

// палата из URL-пути: /<palata> (как jitsi meet.jit.si/room). '' если корень.
function palataFromPath() {
  const seg = decodeURIComponent(location.pathname.replace(/^\/+|\/+$/g, ''));
  return /^[a-zA-Z0-9_-]{1,64}$/.test(seg) ? seg : '';
}

async function join() {
  $('err').textContent = '';
  // палата из URL имеет приоритет; иначе из поля формы
  const roomName = palataFromPath() || $('room').value.trim();
  const name = $('name').value.trim();
  if (!roomName) {
    $('err').textContent = 'Укажите номер палаты';
    return;
  }
  // зафиксировать палату в URL (чтобы ссылку можно было расшарить)
  if (palataFromPath() !== roomName) {
    history.replaceState(null, '', '/' + encodeURIComponent(roomName));
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
      adaptiveStream: true,
      dynacast: true,
      disconnectOnPageLeave: true,
      // МАКСИМАЛЬНАЯ устойчивость реконнекта (качество побоку).
      // Короткие частые попытки, много попыток — коннект держится до последнего.
      reconnectPolicy: {
        nextRetryDelayInMs: (ctx) => (ctx.retryCount < 3 ? 300 : ctx.retryCount < 10 ? 1000 : 2000),
        maxRetries: 100,
      },
      publishDefaults: {
        // simulcast ВЫКЛ: один слой надёжнее и дешевле, чем жонглирование тремя.
        simulcast: false,
        // видео на минимум — лишь бы пролезло в любой канал. Качество не важно.
        videoEncoding: { maxBitrate: 150000, maxFramerate: 15 },
        videoCodec: 'vp8',
        // Аудио: минимальная задержка. ptime=10ms (вместо 20) → меньше пакетизационной
        // задержки на кодере. Низкий битрейт (качество побоку).
        dtx: false,
        red: false,
        audioPreset: { maxBitrate: 24000, ptime: 10 },
      },
    });

    wireRoomEvents(room);

    await room.connect(wsUrl(), token, {
      // автоподписка на все треки в комнате
      autoSubscribe: true,
    });

    // Публикуем микрофон и камеру РАЗДЕЛЬНО: если нет камеры (или отказ по видео),
    // звук всё равно должен работать. Общий enableCameraAndMicrophone падал целиком.
    // Захват устройств с таймаутом: зависший getUserMedia (битый драйвер, занятое
    // устройство) НЕ должен блокировать вход в палату. Вошли — потом доберём медиа.
    const withTimeout = (p, ms) =>
      Promise.race([p, new Promise((_, rej) => setTimeout(() => rej(new Error('timeout')), ms))]);

    let micOk = false, camOk = false;
    try {
      await withTimeout(room.localParticipant.setMicrophoneEnabled(true, {
        echoCancellation: true,
        noiseSuppression: false,
        autoGainControl: false,
      }), 8000);
      micOk = true;
    } catch (e) {
      console.warn('mic failed:', e);
    }
    try {
      await withTimeout(room.localParticipant.setCameraEnabled(true), 8000);
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
    $('stage').style.display = 'block';
    const crumb = $('roomCrumb');
    if (crumb) crumb.textContent = roomName;
    refreshSoundboard();
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
$('name').addEventListener('keydown', (e) => { if (e.key === 'Enter') join(); });

// URL-резолв палаты (как jitsi): /<palata> → при входе спрашиваем только ник.
(function initFromUrl() {
  const palata = palataFromPath();
  if (palata) {
    // прячем строку выбора палаты, оставляем только ник
    const roomInput = $('room');
    const row = roomInput.closest('tr');
    if (row) row.style.display = 'none';
    roomInput.value = palata;
    const crumb = $('roomCrumb');
    if (crumb) crumb.textContent = palata;
    document.title = `meetler :: палата ${palata}`;
    const nameInput = $('name');
    if (nameInput) setTimeout(() => nameInput.focus(), 50);
  }
})();

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
// --- Soundboard: проиграть звук по URL для ВСЕХ в палате (как ГОЙДА) ---
let sfxCtx = null;
const sfxCache = new Map();   // url -> decoded AudioBuffer
let sfxBusy = false;          // не наслаиваем звуки друг на друга

async function loadBuffer(url) {
  if (sfxCache.has(url)) return sfxCache.get(url);
  sfxCtx = sfxCtx || new (window.AudioContext || window.webkitAudioContext)();
  const res = await fetch(url);
  if (!res.ok) throw new Error('fetch ' + res.status);
  const buf = await sfxCtx.decodeAudioData(await res.arrayBuffer());
  sfxCache.set(url, buf);
  return buf;
}

// проигрывает url: публикует в палату (слышат все) + локально + в запись 15с.
// Жёсткий кэп 15с — как и всё проигрываемое в tgbridge.
async function playUrlToRoom(url, btnEl) {
  if (!room || sfxBusy) return;
  sfxBusy = true;
  if (btnEl) btnEl.classList.add('playing');
  let pub = null, track = null;
  try {
    const buffer = await loadBuffer(url);
    if (sfxCtx.state === 'suspended') await sfxCtx.resume();

    const src = sfxCtx.createBufferSource();
    src.buffer = buffer;
    const dest = sfxCtx.createMediaStreamDestination();
    src.connect(dest);               // в палату
    src.connect(sfxCtx.destination); // себе в колонки

    const mst = dest.stream.getAudioTracks()[0];
    addToRecordMix(mst);             // попадает в запись 15с
    track = new LocalAudioTrack(mst);
    pub = await room.localParticipant.publishTrack(track, {
      name: 'sfx', source: Track.Source.Unknown,
    });

    await new Promise((resolve) => {
      // кэп длительности: дольше 15с в эфир не льём
      const cap = setTimeout(() => { try { src.stop(); } catch (_) {} }, 15000);
      src.onended = () => { clearTimeout(cap); resolve(); };
      src.start();
    });
    await new Promise((r) => setTimeout(r, 300)); // хвост для подписчиков
  } catch (e) {
    $('err').textContent = 'Звук не удался: ' + (e.message || e);
  } finally {
    try { if (pub) await room.localParticipant.unpublishTrack(track); } catch (_) {}
    if (btnEl) btnEl.classList.remove('playing');
    sfxBusy = false;
  }
}

// --- панель soundboard: список кнопок с сервера + загрузка ---
async function refreshSoundboard() {
  const box = $('sbBtns');
  if (!box) return;
  try {
    const res = await fetch('/api/sounds');
    // пустой список приходит как JSON null (Go nil slice)
    const sounds = (await res.json()) || [];
    box.innerHTML = '';
    box.classList.remove('empty');
    if (!sounds.length) {
      box.classList.add('empty');
      box.textContent = 'аптечка пуста — залей первый звук кнопкой ниже';
      return;
    }
    for (const s of sounds) {
      const b = document.createElement('button');
      b.className = 'sfx';
      b.textContent = s.label;
      b.title = s.label;
      b.onclick = () => playUrlToRoom(s.url, b);
      box.appendChild(b);
    }
  } catch (_) { /* soundboard опционален */ }
}

async function uploadSound(file, label) {
  const fd = new FormData();
  fd.append('file', file);
  if (label) fd.append('label', label);
  const res = await fetch('/api/sounds', { method: 'POST', body: fd });
  if (!res.ok) throw new Error((await res.text()) || res.status);
}

// загрузка: скрытый file input за кнопкой (клиентские pre-checks = быстрый фидбек)
$('sbUploadBtn').onclick = () => $('sbFile').click();
$('sbFile').onchange = async () => {
  const f = $('sbFile').files[0];
  $('sbFile').value = ''; // разрешить повторный выбор того же файла
  const sbErr = $('sbErr');
  if (sbErr) sbErr.textContent = '';
  if (!f) return;
  try {
    if (f.size > 2 * 1024 * 1024) throw new Error('больше 2 МБ — не влезет');
    if (!/\.(mp3|ogg|oga|wav|m4a|webm|opus)$/i.test(f.name)) {
      throw new Error('разрешены mp3/ogg/wav/m4a/webm/opus');
    }
    // лимит длительности ≤10с: декодируем на клиенте ДО отправки
    // (server-side проверки длительности нет — ffmpeg в token-svc не живёт;
    // кэп на проигрывание 15с в playUrlToRoom остаётся страховкой)
    sfxCtx = sfxCtx || new (window.AudioContext || window.webkitAudioContext)();
    const probe = await sfxCtx.decodeAudioData(await f.arrayBuffer());
    if (probe.duration > 10) {
      throw new Error(`длиннее 10 секунд (${probe.duration.toFixed(0)}с) — обрежь`);
    }
    await uploadSound(f);
    await refreshSoundboard();
  } catch (e) {
    if (sbErr) sbErr.textContent = 'Загрузка не удалась: ' + (e.message || e);
  }
};

// --- список открытых палат на главной (GET /api/rooms, LiveKit ListRooms) ---
function fmtAge(unixSec) {
  const s = Math.max(0, Math.floor(Date.now() / 1000 - unixSec));
  if (s < 60) return 'только что';
  if (s < 3600) return Math.floor(s / 60) + ' мин';
  if (s < 86400) return Math.floor(s / 3600) + ' ч';
  return Math.floor(s / 86400) + ' дн';
}

async function refreshRooms() {
  const body = $('roomsBody');
  if (!body) return;
  if ($('join').style.display === 'none') return; // в палате — не тратим запросы
  try {
    const res = await fetch('/api/rooms');
    const rooms = (await res.json()) || [];
    body.innerHTML = '';
    if (!rooms.length) {
      const tr = document.createElement('tr');
      const td = document.createElement('td');
      td.colSpan = 3;
      td.className = 'muted';
      td.textContent = 'тихо — открытых палат нет';
      tr.appendChild(td);
      body.appendChild(tr);
      return;
    }
    for (const r of rooms) {
      const tr = document.createElement('tr');
      // имя палаты строим через DOM (не innerHTML): имя комнаты приходит
      // с сервера и в общем случае не санитизировано
      const tdName = document.createElement('td');
      const a = document.createElement('a');
      a.href = '/' + encodeURIComponent(r.name);
      a.textContent = r.name;
      tdName.appendChild(a);
      const tdCnt = document.createElement('td');
      tdCnt.textContent = String(r.participants);
      const tdAge = document.createElement('td');
      tdAge.textContent = fmtAge(r.createdAt);
      tr.append(tdName, tdCnt, tdAge);
      body.appendChild(tr);
    }
  } catch (_) {
    // не показываем ошибку: список палат — не критичная фича
  }
}

refreshRooms();
setInterval(refreshRooms, 30000);

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
  a.download = `meetler-${ts}.wav`;
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 10000);
};

$('leaveBtn').onclick = () => { if (room) room.disconnect(); };

function cleanup() {
  try { if (recProc) recProc.disconnect(); } catch (_) {}
  try { if (recCtx) recCtx.close(); } catch (_) {}
  recCtx = null; recProc = null; recMixGain = null; recRing = null;
  recWritePos = 0; recFilled = 0;
  tiles.forEach((t) => t.el.remove());
  tiles.clear();
  if (localTile) { localTile.remove(); localTile = null; }
  $('stage').style.display = 'none';
  $('join').style.display = 'block';
  $('joinBtn').disabled = false;
  refreshRooms(); // вернулись на главную — обновить список палат
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
