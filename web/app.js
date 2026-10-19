// meetler браузерный клиент на livekit-client (пинованная версия).
// Фокус: latency + stability. Все tuning-решения прокомментированы.
import {
  Room,
  RoomEvent,
  Track,
  VideoPresets,
  LocalAudioTrack,
  DisconnectReason,
  createLocalScreenTracks,
} from '/vendor/livekit-client.esm.mjs';   // локально: без CDN, быстрее, оффлайн-safe

const $ = (id) => document.getElementById(id);
const grid = $('grid');
const tiles = new Map(); // participant.sid -> { el, videoEl }

let room = null;

// палата, для которой сейчас показываем/льём данные (звуки, портреты):
// в конференции — её имя; на главной — из поля формы или URL.
function currentRoom() {
  if (room && room.name) return room.name;
  const v = palataFromPath() || ($('room') ? $('room').value.trim() : '');
  return /^[a-zA-Z0-9_-]{1,64}$/.test(v) ? v : '';
}

const roomApi = (rm) => '/api/rooms/' + encodeURIComponent(rm);

// --- голосование за деструктив (кик / удаление звука / портрета) ---
// Действие исполняет СЕРВЕР только при voteThreshold РАЗНЫХ голосах. Один
// пациент не может кикать/стирать в одиночку. Прогресс отражаем цветом.
const VOTE_THRESHOLD = 3;

// стабильный анонимный id браузера (голосующий). Не identity — голосовать
// можно и вне палаты; дедуп на сервере по нему.
function clientId() {
  let c = localStorage.getItem('meetlerCid');
  if (!c) {
    c = 'c-' + Math.random().toString(36).slice(2) + Date.now().toString(36);
    localStorage.setItem('meetlerCid', c);
  }
  return c;
}

// отправить голос. Возвращает {count, threshold, done} или null при ошибке.
async function castVote(rm, kind, target) {
  try {
    const res = await fetch(roomApi(rm) + '/votes', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ kind, target, voter: clientId() }),
    });
    if (!res.ok) throw new Error((await res.text()) || res.status);
    return res.json();
  } catch (_) {
    return null;
  }
}

// покрасить элемент по числу голосов: 0 норма, 1 жёлтый, 2 оранжевый, 3 красный
function applyVoteColor(el, count) {
  el.classList.toggle('v1', count === 1);
  el.classList.toggle('v2', count === 2);
  el.classList.toggle('v3', count >= 3);
  if (count > 0) el.dataset.votes = `${count}/${VOTE_THRESHOLD}`;
  else delete el.dataset.votes;
}

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

// --- разворот тайла на всё окно ---
// Клик по телу тайла (не по контролам) ИЛИ отдельная кнопка ⤢/⤡ разворачивает
// трансляцию на весь экран; повторный клик/кнопка — сворачивает. Максимум один
// развёрнутый тайл.
function toggleMax(el) {
  const willMax = !el.classList.contains('max');
  // свернуть любой другой развёрнутый
  document.querySelectorAll('.tile.max').forEach((o) => {
    if (o !== el) { o.classList.remove('max'); syncMaxBtn(o); }
  });
  el.classList.toggle('max', willMax);
  document.body.classList.toggle('tile-maximized', willMax);
  syncMaxBtn(el);
}

function syncMaxBtn(el) {
  const b = el.querySelector('.maxbtn');
  if (!b) return;
  const maxed = el.classList.contains('max');
  b.textContent = maxed ? '⤡' : '⤢';
  b.title = maxed ? 'Свернуть' : 'Развернуть на всё окно';
}

// навесить кнопку разворота + клик-по-тайлу на элемент тайла
function addMaxControls(el) {
  const b = document.createElement('button');
  b.className = 'maxbtn';
  b.textContent = '⤢';
  b.title = 'Развернуть на всё окно';
  b.onclick = (e) => { e.stopPropagation(); toggleMax(el); };
  el.appendChild(b);
  // клик по самому тайлу тоже разворачивает/сворачивает, но НЕ когда кликнули
  // по контролу (ползунок/кик/кнопки) — у них свои обработчики
  el.addEventListener('click', (e) => {
    if (e.target.closest('.vol, .kick, .maxbtn, .sfxdel')) return;
    toggleMax(el);
  });
}

function ensureTile(participant) {
  let t = tiles.get(participant.sid);
  if (t) return t;
  const el = document.createElement('div');
  el.className = 'tile';
  el.dataset.sid = participant.sid; // по нему вешается аватарка из пула
  const label = document.createElement('div');
  label.className = 'label';
  label.textContent = participant.name || participant.identity;
  const ph = document.createElement('div');
  ph.className = 'ph'; // фон = аватарка (из пула или дефолтная из CSS)
  const u = avatarUrlFor(participant.sid);
  if (u) ph.style.backgroundImage = `url(${u})`;
  el.appendChild(ph);
  el.appendChild(label);

  // индивидуальная громкость ЭТОГО участника (только у себя)
  const vol = document.createElement('input');
  vol.type = 'range';
  vol.min = '0';
  vol.max = '100';
  vol.value = '100';
  vol.className = 'vol';
  vol.title = 'громкость участника (у себя)';
  el.appendChild(vol);

  // кик — ПО ГОЛОСАМ: нужно VOTE_THRESHOLD разных пациентов. Цвет крестика
  // отражает прогресс. Когда порог взят — сервер выписывает, а мы шлём всем
  // «ломаемся» для minecraft-анимации.
  const kick = document.createElement('button');
  kick.className = 'kick';
  kick.textContent = '✕';
  kick.title = 'Голос за выписку (нужно 3)';
  kick.dataset.vkey = 'kick:' + participant.identity; // раскраска по опросу
  kick.onclick = async () => {
    if (!room) return;
    const r = await castVote(room.name, 'kick', participant.identity);
    if (!r) { $('err').textContent = 'Голос не засчитан'; return; }
    applyVoteColor(kick, r.done ? 3 : r.count);
    if (r.done) {
      breakTile(participant.sid);           // анимация у себя
      broadcastBreak(participant.sid);       // и у всех остальных
    }
  };
  el.appendChild(kick);

  addMaxControls(el); // кнопка разворота + клик по тайлу = на всё окно

  grid.appendChild(el);
  t = {
    el, videoEl: null, ph, label, kickBtn: kick,
    identity: participant.identity,
    audioEl: null, audioTrack: null, audioVol: 1,
  };
  vol.addEventListener('input', () => {
    t.audioVol = vol.value / 100;
    // громкость применяем двумя путями: напрямую к своему <audio>-элементу
    // и через livekit (setVolume применяет ко всем attached элементам трека)
    if (t.audioEl) {
      t.audioEl.volume = t.audioVol;
      t.audioEl.muted = t.audioVol === 0;
    }
    if (t.audioTrack && t.audioTrack.setVolume) {
      try { t.audioTrack.setVolume(t.audioVol); } catch (_) {}
    }
  });
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
    // применить выбранную ползунком громкость (мог выставить до attach)
    if (t.audioVol !== undefined) {
      el.volume = t.audioVol;
      el.muted = t.audioVol === 0;
    }
    t.audioEl = el;
    t.audioTrack = track;
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
  // запомнить имя: не заставляем вводить заново при каждом визите
  if (name) localStorage.setItem('meetlerName', name);
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
    $('stage').style.display = 'flex';
    ensureLocalTile(); // свой тайл с аватаркой, даже если камеры нет
    const crumb = $('roomCrumb');
    if (crumb) crumb.textContent = roomName;
    refreshSoundboard();
    if (sbTimer) clearInterval(sbTimer);
    sbTimer = setInterval(refreshSoundboard, 20000); // fallback, если data-пинг потерялся
    refreshAvatars(); // галерея этой палаты — для тайлов (пул мог обновиться)
    // пока в палате — подхватывать портреты, которые принесли другие
    if (avatarsTimer) clearInterval(avatarsTimer);
    avatarsTimer = setInterval(refreshAvatars, 30000);
    refreshWardNav();
    if (wnavTimer) clearInterval(wnavTimer);
    wnavTimer = setInterval(refreshWardNav, 30000);
    refreshVotes();
    if (votesTimer) clearInterval(votesTimer);
    votesTimer = setInterval(refreshVotes, 4000); // прогресс голосов — быстрее
    playWelcome(); // поприветствовать и самого вошедшего
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
  r.on(RoomEvent.TrackUnsubscribed, (track, _pub, participant) => {
    detachTrack(track);
    // видео ушло (unsubscribe) — без этого тайл оставался чёрным
    if (track.kind === Track.Kind.Video) {
      const t = tiles.get(participant.sid);
      if (t) {
        t.videoEl = null;
        t.ph.style.display = ''; // вернуть аватарку
      }
    }
  });
  // mute/unmute камеры у удалённых: чёрный экран ↔ аватарка
  r.on(RoomEvent.TrackMuted, (pub, participant) => {
    if (pub.kind === Track.Kind.Video) setTileVideo(participant, false);
  });
  r.on(RoomEvent.TrackUnmuted, (pub, participant) => {
    if (pub.kind === Track.Kind.Video) setTileVideo(participant, true);
  });
  r.on(RoomEvent.ParticipantConnected, (p) => {
    ensureTile(p);
    playWelcome(); // звук приветствия: кто-то вошёл
  });
  r.on(RoomEvent.ParticipantDisconnected, (p) => removeTile(p));
  r.on(RoomEvent.ParticipantNameChanged, (_name, p) => updateTileLabel(p));
  // данные от других клиентов: сейчас — только «ломаемся» (minecraft-анимация
  // кикнутого тайла у всех зрителей синхронно)
  r.on(RoomEvent.DataReceived, (payload) => {
    try {
      const msg = JSON.parse(new TextDecoder().decode(payload));
      if (msg && msg.t === 'broke' && msg.sid) breakTile(msg.sid);
      // кто-то залил/удалил звук — обновить аптечку сразу, без перезагрузки
      if (msg && msg.t === 'sfxupd') refreshSoundboard();
    } catch (_) {}
  });
  r.on(RoomEvent.LocalTrackPublished, (pub) => {
    if (pub.track && pub.track.kind === Track.Kind.Video) {
      attachLocalVideo(pub.track);
    }
  });
  // стоп-шеринг / выключение своей камеры: видео снято — вернуть аватарку
  r.on(RoomEvent.LocalTrackUnpublished, (pub) => {
    if (pub.kind === Track.Kind.Video) removeLocalVideo();
  });
  r.on(RoomEvent.LocalTrackMuted, (pub) => {
    if (pub.kind === Track.Kind.Video) setLocalVideoVisible(false);
  });
  r.on(RoomEvent.LocalTrackUnmuted, (pub) => {
    if (pub.kind === Track.Kind.Video) setLocalVideoVisible(true);
  });
  r.on(RoomEvent.Disconnected, (reason) => {
    // смена палаты (switchRoom): как только комната сменилась, поздние
    // события СТАРОЙ комнаты не должны трогать UI новой
    if (room && room !== r) return;
    // личный кик (RemoveParticipant) → страница "тюрьма" с плохой концовкой
    if (reason === DisconnectReason.PARTICIPANT_REMOVED) {
      location.href = '/jail.html';
      return;
    }
    // отличаем свой уход/закрытие палаты от потери связи: сообщение об ошибке
    // показываем только когда человек не сам нажал "Выписаться"
    const unexpected = !selfLeaving && reason !== DisconnectReason.CLIENT_INITIATED;
    selfLeaving = false;
    if (room === r) room = null; // currentRoom() дальше читает поле формы
    cleanup();
    if (unexpected) {
      $('err').textContent = reason === DisconnectReason.ROOM_DELETED
        ? 'Палату закрыли.'
        : 'Соединение с палатой потерялось.';
    }
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

// --- minecraft-style поломка тайла при выписке по голосам ---
// крошим блок: фаза с «трещинами», затем разлёт и исчезновение. Тайл удалит
// ParticipantDisconnected (сервер выписал); анимация — визуальная приправа.
function breakTile(sid) {
  const t = tiles.get(sid);
  const el = t ? t.el : (localTile && localTile.dataset.sid === sid ? localTile : null);
  if (!el || el.classList.contains('breaking')) return;
  const crack = document.createElement('div');
  crack.className = 'crack';
  el.appendChild(crack);
  el.classList.add('breaking');
  // по окончании — убрать (если ParticipantDisconnected не пришёл, напр. свой)
  setTimeout(() => { try { el.remove(); } catch (_) {} }, 900);
}

// разослать всем в палате «такой-то тайл ломается» (LiveKit data, reliable)
function broadcastBreak(sid) {
  if (!room || !room.localParticipant) return;
  try {
    const data = new TextEncoder().encode(JSON.stringify({ t: 'broke', sid }));
    room.localParticipant.publishData(data, { reliable: true });
  } catch (_) {}
}

// сообщить всем, что аптечка изменилась (залили/удалили звук) → мгновенно обновить
function broadcastSfxUpdate() {
  if (!room || !room.localParticipant) return;
  try {
    const data = new TextEncoder().encode(JSON.stringify({ t: 'sfxupd' }));
    room.localParticipant.publishData(data, { reliable: true });
  } catch (_) {}
}

// --- звук приветствия при входе участника ---
let welcomeAudio = null;
function playWelcome() {
  if (!room) return;
  // кастомный звук палаты, иначе дефолтный /welcome.mp3. HEAD не делаем —
  // <audio> сам отвалится на 404 кастома, тогда играем дефолт.
  const custom = roomApi(room.name) + '/welcome';
  const vol = sbVolume();
  if (vol <= 0) return; // уважаем индивидуальный mute аптечки
  const a = new Audio(custom);
  a.volume = vol;
  a.onerror = () => {
    const d = new Audio('/welcome.mp3');
    d.volume = vol;
    d.play().catch(() => {});
  };
  a.play().catch(() => {
    const d = new Audio('/welcome.mp3');
    d.volume = vol;
    d.play().catch(() => {});
  });
  welcomeAudio = a;
}

// локальный тайл: создаётся СРАЗУ после входа (с аватаркой из пула), видео
// подменяет фон когда включается. Раньше без камеры своего тайла не было.
function ensureLocalTile() {
  if (localTile) return localTile;
  const el = document.createElement('div');
  el.className = 'tile';
  el.dataset.sid = room.localParticipant.sid;
  const label = document.createElement('div');
  label.className = 'label';
  const myName = (room && room.localParticipant && room.localParticipant.name) || 'Вы';
  label.textContent = `${myName} (вы)`;
  const ph = document.createElement('div');
  ph.className = 'ph';
  const u = avatarUrlFor(room.localParticipant.sid);
  if (u) ph.style.backgroundImage = `url(${u})`;
  el.appendChild(ph);
  el.appendChild(label);
  addMaxControls(el); // разворот своего окошка на весь экран
  grid.insertBefore(el, grid.firstChild);
  localTile = el;
  return localTile;
}

function attachLocalVideo(track) {
  const el = ensureLocalTile();
  const ph = el.querySelector('.ph');
  if (ph) ph.style.display = 'none'; // есть видео — аватарку прячем
  const v = track.attach();
  v.autoplay = true;
  v.playsInline = true;
  v.muted = true; // локальное видео без эха
  const old = localTile.querySelector('video');
  if (old) old.remove();
  localTile.insertBefore(v, localTile.firstChild);
}

// своё видео снято целиком (стоп-шеринг): убрать элемент, показать аватарку
function removeLocalVideo() {
  if (!localTile) return;
  const v = localTile.querySelector('video');
  if (v) v.remove();
  const ph = localTile.querySelector('.ph');
  if (ph) ph.style.display = '';
}

// mute/unmute своей камеры: видео есть но не идёт ↔ аватарка
function setLocalVideoVisible(show) {
  if (!localTile) return;
  const v = localTile.querySelector('video');
  if (v) v.style.display = show ? '' : 'none';
  const ph = localTile.querySelector('.ph');
  if (ph) ph.style.display = show && v ? 'none' : '';
}

// показать/спрятать видео удалённого участника (mute камеры)
function setTileVideo(participant, show) {
  const t = tiles.get(participant.sid);
  if (!t) return;
  if (t.videoEl) t.videoEl.style.display = show ? '' : 'none';
  t.ph.style.display = show && t.videoEl ? 'none' : '';
}

// controls
$('joinBtn').onclick = join;
$('room').addEventListener('keydown', (e) => { if (e.key === 'Enter') join(); });
$('name').addEventListener('keydown', (e) => { if (e.key === 'Enter') join(); });
// ник сохраняется на каждый ввод: не вводим заново при следующем визите
$('name').addEventListener('input', () => {
  const v = $('name').value.trim();
  if (v) localStorage.setItem('meetlerName', v);
});

// URL-резолв палаты (как jitsi): /<palata> → при входе спрашиваем только ник.
(function initFromUrl() {
  // сохранённое имя из прошлого визита
  const saved = localStorage.getItem('meetlerName');
  if (saved) $('name').value = saved;
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
// демонстрация экрана: toggle — старт/стоп своей трансляции
let screenTracks = [];
$('shareBtn').onclick = async () => {
  if (screenTracks.length) {
    // уже шарим — остановить
    await stopShare();
    return;
  }
  try {
    const tracks = await createLocalScreenTracks({ audio: true });
    for (const t of tracks) {
      await room.localParticipant.publishTrack(t);
      // пользователь жмёт "Остановить доступ" в системной плашке браузера →
      // трек заканчивается: подхватываем и приводим кнопку в исходное
      t.mediaStreamTrack.addEventListener('ended', stopShare, { once: true });
    }
    screenTracks = tracks;
    $('shareBtn').textContent = 'Стоп демонстрация';
    $('shareBtn').classList.add('danger');
  } catch (e) {
    $('err').textContent = 'Демонстрация отменена: ' + e.message;
  }
};

async function stopShare() {
  const tracks = screenTracks;
  screenTracks = [];
  for (const t of tracks) {
    try { await room.localParticipant.unpublishTrack(t, true); } catch (_) {}
    try { t.stop(); } catch (_) {}
  }
  const b = $('shareBtn');
  if (b) { b.textContent = 'Демонстрация'; b.classList.remove('danger'); }
}
// --- Soundboard: проиграть звук по URL для ВСЕХ в палате (как ГОЙДА) ---
let sfxCtx = null;
const sfxCache = new Map();   // url -> decoded AudioBuffer
let sfxBusy = false;          // не наслаиваем звуки друг на друга
let sfxMonitorGain = null;    // активный gain локального монитора (для live-ползунка)

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
// Локальный монитор идёт через gain: индивидуальный ползунок громкости
// меняет громкость ТОЛЬКО у себя (в палату и в запись — как было).
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
    const monitor = sfxCtx.createGain();
    monitor.gain.value = sbVolume(); // индивидуальная громкость (у себя)
    sfxMonitorGain = monitor;        // чтобы ползунок менял её НА ЛЕТУ
    src.connect(monitor);
    monitor.connect(sfxCtx.destination); // себе в колонки

    const mst = dest.stream.getAudioTracks()[0];
    addToRecordMix(mst);             // попадает в запись 15с
    track = new LocalAudioTrack(mst);
    pub = await room.localParticipant.publishTrack(track, {
      name: 'sfx', source: Track.Source.Unknown,
    });

    await new Promise((resolve) => {
      // кэп длительности: дольше 20с в эфир не льём (форсированное обрезание)
      const cap = setTimeout(() => { try { src.stop(); } catch (_) {} }, 20000);
      src.onended = () => { clearTimeout(cap); resolve(); };
      src.start();
    });
    await new Promise((r) => setTimeout(r, 300)); // хвост для подписчиков
  } catch (e) {
    $('err').textContent = 'Звук не удался: ' + (e.message || e);
  } finally {
    try { if (pub) await room.localParticipant.unpublishTrack(track); } catch (_) {}
    sfxMonitorGain = null;
    if (btnEl) btnEl.classList.remove('playing');
    sfxBusy = false;
  }
}

// --- индивидуальная громкость звуковой аптечки (только у себя) ---
function sbVolume() {
  const v = Number(localStorage.getItem('sbVolume'));
  return Number.isFinite(v) ? Math.min(1, Math.max(0, v)) : 1;
}

(function initSbVol() {
  const slider = $('sbVol');
  if (!slider) return;
  slider.value = String(Math.round(sbVolume() * 100));
  slider.addEventListener('input', () => {
    const v = slider.value / 100;
    localStorage.setItem('sbVolume', String(v));
    // применить НА ЛЕТУ к своему монитору, если звук уже играет
    if (sfxMonitorGain) sfxMonitorGain.gain.value = v;
  });
})();

// --- панель soundboard: список кнопок с сервера + загрузка (всё в ЭТОЙ палате) ---
async function refreshSoundboard() {
  const box = $('sbBtns');
  if (!box || !room) return;
  try {
    const res = await fetch(roomApi(room.name) + '/sounds');
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
      const row = document.createElement('span');
      row.className = 'sfxrow';
      const b = document.createElement('button');
      b.className = 'sfx';
      b.textContent = s.label;
      b.title = s.label;
      b.onclick = () => playUrlToRoom(s.url, b);
      const del = document.createElement('button');
      del.className = 'sfxdel';
      del.textContent = '✕';
      del.title = 'Голос за удаление (нужно 3)';
      del.dataset.vkey = 'sound:' + s.id; // для раскраски по опросу голосов
      del.onclick = async () => {
        const r = await castVote(room.name, 'sound', s.id);
        if (!r) { $('sbErr').textContent = 'Голос не засчитан'; return; }
        applyVoteColor(del, r.done ? 3 : r.count);
        if (r.done) { await refreshSoundboard(); broadcastSfxUpdate(); }
      };
      row.append(b, del);
      box.appendChild(row);
    }
    refreshVotes(); // подтянуть текущий прогресс голосов (цвет у всех)
  } catch (_) { /* soundboard опционален */ }
}

// опрос прогресса голосов: красит ВСЕ элементы с data-vkey ("kind:target")
// по текущему числу голосов — чтобы цвет был одинаков у всех пациентов.
let votesTimer = null;
let sbTimer = null; // периодический опрос аптечки (fallback к data-пингу)
async function refreshVotes() {
  if (!room) return;
  let counts = {};
  try {
    const res = await fetch(roomApi(room.name) + '/votes');
    counts = (await res.json()) || {};
  } catch (_) { return; }
  document.querySelectorAll('[data-vkey]').forEach((el) => {
    applyVoteColor(el, counts[el.dataset.vkey] || 0);
  });
}

async function uploadSound(file, label) {
  if (!room) throw new Error('не в палате');
  const fd = new FormData();
  fd.append('file', file);
  if (label) fd.append('label', label);
  const res = await fetch(roomApi(room.name) + '/sounds', { method: 'POST', body: fd });
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
    // длительность НЕ ограничиваем: любое аудио проходит, при проигрывании
    // форсированно обрезается до 20с (кэп в playUrlToRoom).
    await uploadSound(f);
    await refreshSoundboard();
    broadcastSfxUpdate(); // остальные увидят звук сразу, без перезагрузки
  } catch (e) {
    if (sbErr) sbErr.textContent = 'Загрузка не удалась: ' + (e.message || e);
  }
};

// --- список палат на главной (GET /api/rooms: персистентный реестр ∪ живые) ---
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
      td.colSpan = 4;
      td.className = 'muted';
      td.textContent = 'тихо — палат нет';
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
      const tdDel = document.createElement('td');
      // закрыть палату — ПО ГОЛОСАМ: нужно 3 разных пациента (как кик/звук).
      // Цвет крестика = прогресс. Порог взят — палата исчезает из списка.
      const del = document.createElement('button');
      del.className = 'roomdel';
      del.textContent = '✕';
      del.title = 'Голос за закрытие палаты (нужно 3)';
      del.dataset.vkey = 'room:' + r.name;
      del.dataset.room = r.name;
      del.onclick = async () => {
        const v = await castVote(r.name, 'room', r.name);
        if (!v) {
          const re = $('roomsErr');
          if (re) re.textContent = 'Голос не засчитан';
          return;
        }
        applyVoteColor(del, v.done ? 3 : v.count);
        if (v.done) await refreshRooms();
      };
      tdDel.appendChild(del);
      tr.append(tdName, tdCnt, tdAge, tdDel);
      body.appendChild(tr);
    }
    refreshRoomVotes(); // раскрасить прогресс закрытия у всех
  } catch (_) {
    // не показываем ошибку: список палат — не критичная фича
  }
}

// опрос голосов за закрытие по каждой палате из списка (цвет виден всем).
async function refreshRoomVotes() {
  const btns = document.querySelectorAll('#roomsBody .roomdel');
  for (const b of btns) {
    const rm = b.dataset.room;
    if (!rm) continue;
    try {
      const res = await fetch('/api/rooms/' + encodeURIComponent(rm) + '/votes');
      const counts = (await res.json()) || {};
      applyVoteColor(b, counts['room:' + rm] || 0);
    } catch (_) { /* не критично */ }
  }
}

refreshRooms();
setInterval(refreshRooms, 30000);

// --- вертикальное меню палат в конференции (слева) ---
// Быстрая смена палаты в 1 клик — как комнаты в чате. Ник уже сохранён,
// повторно вводить ничего не нужно.
let wnavTimer = null;

async function refreshWardNav() {
  const box = $('wardNavList');
  if (!box || !room) return;
  try {
    const res = await fetch('/api/rooms');
    const rooms = (await res.json()) || [];
    box.innerHTML = '';
    if (!rooms.length) {
      const d = document.createElement('div');
      d.className = 'wnav-empty';
      d.textContent = 'других палат нет';
      box.appendChild(d);
      return;
    }
    for (const r of rooms) {
      const it = document.createElement('div');
      it.className = 'wnav-item' + (r.name === room.name ? ' current' : '');
      // имя с сервера — строим через DOM, не innerHTML
      const nm = document.createElement('span');
      nm.textContent = r.name;
      const cnt = document.createElement('span');
      cnt.className = 'cnt';
      cnt.textContent = r.participants ? String(r.participants) : '';
      it.append(nm, cnt);
      if (r.name !== room.name) {
        it.title = 'Перейти в палату ' + r.name;
        it.onclick = () => switchRoom(r.name);
      } else {
        it.title = 'Вы здесь';
      }
      box.appendChild(it);
    }
  } catch (_) { /* меню палат опционально */ }
}

// Смена палаты без перезагрузки страницы: выходим из текущей (cleanup идёт
// синхронно по Disconnected), чиним URL/заголовок и входим в новую через
// join() — ник тянется из localStorage/поля, ничего вводить заново не надо.
async function switchRoom(newRoom) {
  if (!room || newRoom === room.name) return;
  switching = true;             // cleanup не будет прятать stage (без мигания)
  selfLeaving = true;
  const old = room;
  room = null;
  try { old.disconnect(); } catch (_) {}
  selfLeaving = false;
  cleanup();                    // сносит тайлы/таймеры, но stage остаётся виден
  // мгновенно показываем пункт назначения в UI, пока идёт reconnect в фоне
  history.replaceState(null, '', '/' + encodeURIComponent(newRoom));
  document.title = `meetler :: палата ${newRoom}`;
  const crumb = $('roomCrumb');
  if (crumb) crumb.textContent = newRoom;
  $('grid').innerHTML = '';
  $('room').value = newRoom;
  try {
    await join();
  } finally {
    // держим switching до конца входа: поздний Disconnected СТАРОЙ комнаты
    // не должен показать форму входа посреди перехода
    switching = false;
  }
}

// --- пул аватарок ПАЛАТЫ: случайная картинка каждому участнику ---
// Аватарка = детерминированный хеш sid по СТАБИЛЬНОМУ списку с сервера
// (list отсортирован по id). Все зрители вычисляют одинаковую картинку —
// никакой синхронизации не нужно. Список пуст → дефолтный /avatar.jpg.
// Портреты у каждой палаты СВОИ (иерархия: палата → галерея).
let avatarList = []; // [{id, url}] текущей палаты
let avatarsTimer = null; // опрос галереи, пока в палате (успел кто-то залить — все увидят)

async function refreshAvatars() {
  const rm = currentRoom();
  const box = $('avatarThumbs');
  const cnt = $('avatarCount');
  if (!rm) {
    avatarList = [];
    if (cnt) cnt.textContent = '0';
    if (box) box.innerHTML = '';
    document.querySelectorAll('.tile .ph').forEach((ph) => { ph.style.backgroundImage = ''; });
    return;
  }
  try {
    const res = await fetch(roomApi(rm) + '/avatars');
    const avatars = (await res.json()) || [];
    avatarList = avatars;
    if (cnt) cnt.textContent = String(avatars.length);
    // перекрасить уже существующие тайлы (вдруг пул обновился)
    document.querySelectorAll('.tile .ph').forEach(applyAvatarToPh);
    if (box) {
      box.innerHTML = '';
      if (!avatars.length) box.textContent = 'портретов нет — пациенты ходят со стандартными';
      for (const a of avatars) {
        const th = document.createElement('div');
        th.className = 'thumb rar' + rarityForId(a.id); // палитра раритетности
        th.title = ['обычный', 'необычный', 'редкий', 'эпический', 'легендарный'][rarityForId(a.id)];
        const img = document.createElement('img');
        img.src = a.url;
        img.alt = a.id;
        img.loading = 'lazy';
        th.appendChild(img);
        // удаление портрета — ПО ГОЛОСАМ (нужно 3), цвет крестика = прогресс
        const del = document.createElement('button');
        del.textContent = '✕';
        del.title = 'Голос за удаление (нужно 3)';
        del.dataset.vkey = 'avatar:' + a.id;
        del.onclick = async () => {
          const r = await castVote(rm, 'avatar', a.id);
          if (!r) { const e = $('avatarErr'); if (e) e.textContent = 'Голос не засчитан'; return; }
          applyVoteColor(del, r.done ? 3 : r.count);
          if (r.done) await refreshAvatars();
        };
        th.appendChild(del);
        box.appendChild(th);
      }
    }
    refreshVotes(); // прогресс голосов за удаление портретов
  } catch (_) { /* аватарки опциональны, дефолт остаётся */ }
}

// djb2 — короткий стабильный хеш строки
function strHash(s) {
  let h = 5381;
  for (let i = 0; i < s.length; i++) h = ((h << 5) + h + s.charCodeAt(i)) | 0;
  return Math.abs(h);
}

// дефолтный набор аватарок: базовый портрет (web/avatar.jpg), покрашенный в
// разные цвета (web/dav/*.png). Работает из коробки. Порядок стабильный.
const DEFAULT_AVATARS = [1, 2, 3, 4, 5, 6, 7, 8].map((n) => `/dav/${n}.png`);

// Веса раздачи аватарок (детерминированно по hash(sid), одинаково у всех):
//   - дефолтные — ПРИОРИТЕТНЕЕ (высокий вес), выпадают чаще;
//   - загруженные — реже, и КАЖДАЯ НОВАЯ ещё реже (вес убывает по порядку).
// Чем больше портретов в палате — тем ниже шанс конкретного (total растёт).
const DEFAULT_WEIGHT = 3;
function uploadedWeight(i) { return DEFAULT_WEIGHT / (i + 2); } // 1.0, 0.75, 0.6, ...

// buildAvatarPool: [{url, weight, rarity}] — дефолты + загруженные (по порядку id).
function buildAvatarPool() {
  const pool = DEFAULT_AVATARS.map((url) => ({ url, weight: DEFAULT_WEIGHT }));
  avatarList.forEach((a, i) => pool.push({ url: a.url, weight: uploadedWeight(i), id: a.id }));
  const total = pool.reduce((s, e) => s + e.weight, 0);
  for (const e of pool) e.rarity = rarityTier(e.weight / total);
  return { pool, total };
}

// rarityTier: по вероятности выпадения → тир раритетности (ниже p = реже = выше тир)
function rarityTier(p) {
  if (p >= 0.10) return 0; // common
  if (p >= 0.05) return 1; // uncommon
  if (p >= 0.025) return 2; // rare
  if (p >= 0.012) return 3; // epic
  return 4;                 // legendary (самые редкие)
}

function avatarUrlFor(sid) {
  const { pool, total } = buildAvatarPool();
  if (!pool.length) return null;
  // детерминированный взвешенный выбор: доля хеша × суммарный вес
  let x = (strHash(sid) % 100000) / 100000 * total;
  for (const e of pool) {
    x -= e.weight;
    if (x < 0) return e.url;
  }
  return pool[pool.length - 1].url;
}

// rarityForId: тир раритетности загруженного портрета (для палитры в галерее)
function rarityForId(id) {
  const { pool } = buildAvatarPool();
  const e = pool.find((x) => x.id === id);
  return e ? e.rarity : 0;
}

function applyAvatarToPh(ph) {
  const tile = ph.closest('.tile');
  const sid = tile && tile.dataset.sid;
  if (!sid) return;
  const u = avatarUrlFor(sid);
  // '' → фон из CSS (дефолтный /avatar.jpg)
  ph.style.backgroundImage = u ? `url(${u})` : '';
}

// загрузка портрета (как звуки: скрытый input за кнопкой) — в текущую палату
$('avatarUploadBtn').onclick = () => $('avatarFile').click();
$('avatarFile').onchange = async () => {
  const f = $('avatarFile').files[0];
  $('avatarFile').value = '';
  const avErr = $('avatarErr');
  if (avErr) avErr.textContent = '';
  if (!f) return;
  try {
    const rm = currentRoom();
    if (!rm) throw new Error('сначала укажи палату — портреты у каждой свои');
    if (f.size > 4 * 1024 * 1024) throw new Error('больше 4 МБ — не влезет');
    if (!/\.(jpe?g|png|gif|webp)$/i.test(f.name)) {
      throw new Error('разрешены jpg/png/gif/webp');
    }
    // ужимаем по разрешению и кодеку: downscale ≤256px + webp (fallback jpeg).
    // Экономит трафик/диск и нормализует формат (сервер хранит как есть).
    const { blob, ext } = await compressImage(f, 256, 0.82);
    const fd = new FormData();
    fd.append('file', blob, 'avatar.' + ext);
    const res = await fetch(roomApi(rm) + '/avatars', { method: 'POST', body: fd });
    if (!res.ok) throw new Error((await res.text()) || res.status);
    await refreshAvatars();
  } catch (e) {
    if (avErr) avErr.textContent = 'Загрузка не удалась: ' + (e.message || e);
  }
};

// compressImage: downscale до maxDim (по длинной стороне) и перекодирование.
// webp если поддерживается браузером, иначе jpeg. Возвращает {blob, ext}.
async function compressImage(file, maxDim, quality) {
  const url = URL.createObjectURL(file);
  try {
    const img = await new Promise((resolve, reject) => {
      const i = new Image();
      i.onload = () => resolve(i);
      i.onerror = () => reject(new Error('битая картинка'));
      i.src = url;
    });
    let { width: w, height: h } = img;
    if (Math.max(w, h) > maxDim) {
      const k = maxDim / Math.max(w, h);
      w = Math.round(w * k); h = Math.round(h * k);
    }
    const canvas = document.createElement('canvas');
    canvas.width = w; canvas.height = h;
    canvas.getContext('2d').drawImage(img, 0, 0, w, h);
    const tryType = (type) => new Promise((res) => canvas.toBlob(res, type, quality));
    let blob = await tryType('image/webp');
    let ext = 'webp';
    if (!blob || blob.type !== 'image/webp') { // старый браузер → jpeg
      blob = await tryType('image/jpeg');
      ext = 'jpg';
    }
    if (!blob) throw new Error('не удалось перекодировать');
    return { blob, ext };
  } finally {
    URL.revokeObjectURL(url);
  }
}

// галерея портретов живёт в палате (not на главной): обновляется из join()

// --- звук приветствия палаты: кастомизация ---
$('welUploadBtn').onclick = () => $('welFile').click();
$('welFile').onchange = async () => {
  const f = $('welFile').files[0];
  $('welFile').value = '';
  const e = $('welErr');
  if (e) e.textContent = '';
  if (!f || !room) return;
  try {
    if (f.size > 2 * 1024 * 1024) throw new Error('больше 2 МБ');
    if (!/\.(mp3|ogg|oga|wav|m4a|webm|opus)$/i.test(f.name)) throw new Error('разрешены mp3/ogg/wav/m4a/webm/opus');
    const fd = new FormData();
    fd.append('file', f);
    const res = await fetch(roomApi(room.name) + '/welcome', { method: 'POST', body: fd });
    if (!res.ok) throw new Error((await res.text()) || res.status);
    if (e) e.textContent = 'звук приветствия обновлён';
  } catch (err) {
    if (e) e.textContent = 'Не удалось: ' + (err.message || err);
  }
};
$('welResetBtn').onclick = async () => {
  if (!room) return;
  const e = $('welErr');
  try {
    await fetch(roomApi(room.name) + '/welcome', { method: 'DELETE' });
    if (e) e.textContent = 'вернули дефолтный';
  } catch (_) {}
};
$('welTestBtn').onclick = () => playWelcome();

// --- тумблер темы (светлая / Solarized dark), запоминается в localStorage ---
function applyTheme(t) {
  document.body.classList.toggle('dark', t === 'dark');
  localStorage.setItem('meetlerTheme', t);
}
$('themeToggle').onclick = () => {
  applyTheme(document.body.classList.contains('dark') ? 'light' : 'dark');
};
applyTheme(localStorage.getItem('meetlerTheme') === 'dark' ? 'dark' : 'light');

// --- центральная аутентификация: общий пароль на всё приложение ---
// На загрузке спрашиваем сервер, авторизованы ли мы (cookie). Если нет —
// показываем сплэш-гейт поверх всего. Верный пароль → cookie → гейт прячется.
const gateEl = $('gate');
function showGate() { if (gateEl) { gateEl.hidden = false; setTimeout(() => $('gatePass').focus(), 50); } }
function hideGate() { if (gateEl) gateEl.hidden = true; }

async function submitGate() {
  const pass = $('gatePass').value;
  $('gateErr').textContent = '';
  try {
    const res = await fetch('/api/auth', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ password: pass }),
    });
    if (res.ok) { hideGate(); return; }
    $('gateErr').textContent = res.status === 401 ? 'Неверный пароль' : 'Ошибка входа';
  } catch (_) {
    $('gateErr').textContent = 'Сервер недоступен';
  }
}

(async function initGate() {
  if (!gateEl) return;
  try {
    const res = await fetch('/api/auth'); // 200 если cookie валиден или гейт выключен
    if (!res.ok) showGate();
  } catch (_) { showGate(); }
  $('gateBtn').onclick = submitGate;
  $('gatePass').addEventListener('keydown', (e) => { if (e.key === 'Enter') submitGate(); });
})();

// Esc — свернуть развёрнутый на всё окно тайл
document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape') {
    const m = document.querySelector('.tile.max');
    if (m) toggleMax(m);
  }
});

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
    if (!recRing) return; // race: cleanup зануляет буфер до последнего тика
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

// свой уход: помечаем, чтобы Disconnected не показал "вас выписали"
let selfLeaving = false;
let switching = false; // смена палаты: cleanup не прячет stage (без мигания)
$('leaveBtn').onclick = () => {
  selfLeaving = true;
  if (room) room.disconnect();
};

function cleanup() {
  // сначала глушим колбэк записи: ScriptProcessor может тикнуть между
  // disconnect и обнулением буферов → "Cannot set properties of null"
  try { if (recProc) recProc.onaudioprocess = null; } catch (_) {}
  try { if (recProc) recProc.disconnect(); } catch (_) {}
  try { if (recCtx) recCtx.close(); } catch (_) {}
  recCtx = null; recProc = null; recMixGain = null; recRing = null;
  recWritePos = 0; recFilled = 0;
  tiles.forEach((t) => t.el.remove());
  tiles.clear();
  if (localTile) { localTile.remove(); localTile = null; }
  document.body.classList.remove('tile-maximized'); // сбросить разворот
  screenTracks = []; // сброс состояния демонстрации
  { const b = $('shareBtn'); if (b) { b.textContent = 'Демонстрация'; b.classList.remove('danger'); } }
  if (avatarsTimer) { clearInterval(avatarsTimer); avatarsTimer = null; }
  if (wnavTimer) { clearInterval(wnavTimer); wnavTimer = null; }
  if (votesTimer) { clearInterval(votesTimer); votesTimer = null; }
  if (sbTimer) { clearInterval(sbTimer); sbTimer = null; }
  stopStats();
  // при смене палаты stage остаётся виден (мгновенный переход, без мигания
  // формы входа); обычный выход — прячем stage и возвращаем форму
  if (switching) return;
  // room обнуляет Disconnected-хендлер (инстанс-гард): cleanup не знает,
  // какая комната владеет UI во время смены палаты
  $('stage').style.display = 'none';
  $('join').style.display = 'block';
  $('joinBtn').disabled = false;
  refreshRooms(); // вернулись на главную — обновить список палат
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
