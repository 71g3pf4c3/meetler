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

  // кик: кто угодно может выписать кого угодно
  const kick = document.createElement('button');
  kick.className = 'kick';
  kick.textContent = '✕';
  kick.title = 'Выписать из палаты';
  kick.onclick = async () => {
    const who = participant.name || participant.identity;
    if (!confirm(`Выписать ${who} из палаты?`)) return;
    try {
      const res = await fetch('/api/kick', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ room: room.name, identity: participant.identity }),
      });
      if (!res.ok) throw new Error((await res.text()) || res.status);
    } catch (e) {
      $('err').textContent = 'Не удалось выписать: ' + (e.message || e);
    }
  };
  el.appendChild(kick);

  grid.appendChild(el);
  t = {
    el, videoEl: null, ph, label,
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
    refreshAvatars(); // галерея этой палаты — для тайлов (пул мог обновиться)
    // пока в палате — подхватывать портреты, которые принесли другие
    if (avatarsTimer) clearInterval(avatarsTimer);
    avatarsTimer = setInterval(refreshAvatars, 30000);
    refreshWardNav();
    if (wnavTimer) clearInterval(wnavTimer);
    wnavTimer = setInterval(refreshWardNav, 30000);
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
  r.on(RoomEvent.ParticipantConnected, (p) => ensureTile(p));
  r.on(RoomEvent.ParticipantDisconnected, (p) => removeTile(p));
  r.on(RoomEvent.ParticipantNameChanged, (_name, p) => updateTileLabel(p));
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
    monitor.gain.value = sbVolume(); // индивидуальная громкость
    src.connect(monitor);
    monitor.connect(sfxCtx.destination); // себе в колонки

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
    localStorage.setItem('sbVolume', String(slider.value / 100));
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
      del.title = 'Убрать звук из аптечки';
      del.onclick = async () => {
        if (!confirm('Убрать звук из аптечки?')) return;
        try {
          const res = await fetch(s.url, { method: 'DELETE' });
          if (!res.ok) throw new Error((await res.text()) || res.status);
          await refreshSoundboard();
        } catch (e) {
          $('sbErr').textContent = 'Не удалось удалить: ' + (e.message || e);
        }
      };
      row.append(b, del);
      box.appendChild(row);
    }
  } catch (_) { /* soundboard опционален */ }
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

// --- список палат на главной (GET /api/rooms: персистентный реестр ∪ живые) ---
function fmtAge(unixSec) {
  const s = Math.max(0, Math.floor(Date.now() / 1000 - unixSec));
  if (s < 60) return 'только что';
  if (s < 3600) return Math.floor(s / 60) + ' мин';
  if (s < 86400) return Math.floor(s / 3600) + ' ч';
  return Math.floor(s / 86400) + ' дн';
}

async function deleteRoom(name) {
  const res = await fetch('/api/rooms/' + encodeURIComponent(name), { method: 'DELETE' });
  if (!res.ok) throw new Error((await res.text()) || res.status);
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
      // закрыть палату может любой (как удалить звук/портрет)
      const del = document.createElement('button');
      del.className = 'roomdel';
      del.textContent = '✕';
      del.title = 'Закрыть палату';
      del.onclick = async () => {
        if (!confirm(`Закрыть палату ${r.name}?\nВсе её звуки и портреты будут удалены, пациенты — выписаны.`)) return;
        try {
          await deleteRoom(r.name);
          await refreshRooms();
        } catch (e) {
          const re = $('roomsErr');
          if (re) re.textContent = 'Не удалось закрыть: ' + (e.message || e);
        }
      };
      tdDel.appendChild(del);
      tr.append(tdName, tdCnt, tdAge, tdDel);
      body.appendChild(tr);
    }
  } catch (_) {
    // не показываем ошибку: список палат — не критичная фича
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
  selfLeaving = true; // свой уход — без "вас выписали"
  const old = room;
  room = null; // хендлер старой комнаты увидит room==null и отработает cleanup
  try { old.disconnect(); } catch (_) {}
  selfLeaving = false;
  cleanup(); // страховка: если Disconnected уже отработал — идемпотентно
  history.replaceState(null, '', '/' + encodeURIComponent(newRoom));
  document.title = `meetler :: палата ${newRoom}`;
  $('room').value = newRoom; // join() читает поле, если URL-путь не сработал
  await join();
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
    if (box) {
      box.innerHTML = '';
      box.textContent = 'укажи палату выше — портреты у каждой свои';
    }
    // сбросить фоны тайлов к дефолтной аватарке
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
    // превью галереи на главной; удалить может любой — "лишнее" решает толпа
    if (box) {
      box.innerHTML = '';
      if (!avatars.length) box.textContent = 'в этой палате пока никто не принёс портрет — пациенты ходят со стандартными';
      for (const a of avatars) {
        const th = document.createElement('div');
        th.className = 'thumb';
        const img = document.createElement('img');
        img.src = a.url;
        img.alt = a.id;
        img.loading = 'lazy';
        th.appendChild(img);
        const del = document.createElement('button');
        del.textContent = '✕';
        del.title = 'Убрать портрет из галереи';
        del.onclick = async () => {
          if (!confirm('Убрать портрет из галереи?')) return;
          try {
            // url уже содержит палату — удаляем прямо по нему
            const res = await fetch(a.url, { method: 'DELETE' });
            if (!res.ok) throw new Error((await res.text()) || res.status);
            await refreshAvatars(); // обновить счётчик/превью и перекрасить тайлы
          } catch (e) {
            const avErr = $('avatarErr');
            if (avErr) avErr.textContent = 'Не удалось удалить: ' + (e.message || e);
          }
        };
        th.appendChild(del);
        box.appendChild(th);
      }
    }
  } catch (_) { /* аватарки опциональны, дефолт остаётся */ }
}

// djb2 — короткий стабильный хеш строки
function strHash(s) {
  let h = 5381;
  for (let i = 0; i < s.length; i++) h = ((h << 5) + h + s.charCodeAt(i)) | 0;
  return Math.abs(h);
}

// дефолтный набор аватарок: базовый портрет (web/avatar.jpg), покрашенный в
// разные цвета (web/dav/*.png, сгенерирован imagemagick'ом). Работает из
// коробки, до того как в палату принесли портреты. Порядок стабильный
// (файлы 1..8) — детерминизм хеша сохраняется.
const DEFAULT_AVATARS = [1, 2, 3, 4, 5, 6, 7, 8].map((n) => `/dav/${n}.png`);

function avatarUrlFor(sid) {
  // приоритет у портретов палаты; пусто — стандартный набор
  const pool = avatarList.length ? avatarList.map((a) => a.url) : DEFAULT_AVATARS;
  return pool[strHash(sid) % pool.length];
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
    const fd = new FormData();
    fd.append('file', f);
    const res = await fetch(roomApi(rm) + '/avatars', { method: 'POST', body: fd });
    if (!res.ok) throw new Error((await res.text()) || res.status);
    await refreshAvatars();
  } catch (e) {
    if (avErr) avErr.textContent = 'Загрузка не удалась: ' + (e.message || e);
  }
};

refreshAvatars();
// на главной галерея следует за полем «Палата №» (debounce — дешёвый JSON)
let avDebounce = null;
$('room').addEventListener('input', () => {
  clearTimeout(avDebounce);
  avDebounce = setTimeout(refreshAvatars, 250);
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
  if (avatarsTimer) { clearInterval(avatarsTimer); avatarsTimer = null; }
  if (wnavTimer) { clearInterval(wnavTimer); wnavTimer = null; }
  // room обнуляет Disconnected-хендлер (инстанс-гард): cleanup не знает,
  // какая комната владеет UI во время смены палаты
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
