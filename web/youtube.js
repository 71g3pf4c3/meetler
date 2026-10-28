let api;

function loadAPI() {
  if (window.YT?.Player) return Promise.resolve(window.YT);
  return api ||= new Promise((resolve, reject) => {
    const script = document.createElement('script');
    const timeout = setTimeout(fail, 20000);
    function fail() {
      clearTimeout(timeout);
      script.remove();
      api = null;
      reject(new Error('Не удалось загрузить YouTube.'));
    }
    window.onYouTubeIframeAPIReady = () => {
      clearTimeout(timeout);
      resolve(window.YT);
    };
    script.onerror = fail;
    script.src = 'https://www.youtube.com/iframe_api';
    document.head.appendChild(script);
  });
}

function videoID(value) {
  if (/^[\w-]{11}$/.test(value)) return value;
  try {
    const url = new URL(value.includes('://') ? value : 'https://' + value);
    const host = url.hostname.replace(/^(www\.|m\.|music\.)/, '');
    const parts = url.pathname.split('/');
    const id = host === 'youtu.be' ? parts[1] :
      host === 'youtube.com' || host === 'youtube-nocookie.com' ?
        (url.pathname === '/watch' ? url.searchParams.get('v') :
          ['shorts', 'live', 'embed'].includes(parts[1]) ? parts[2] : '') : '';
    if (/^[\w-]{11}$/.test(id || '')) return id;
  } catch (_) {}
  throw new Error('Нужна ссылка на видео YouTube.');
}

function timeLabel(seconds) {
  seconds = Math.max(0, Math.floor(seconds));
  const minutes = Math.floor(seconds / 60);
  return `${minutes}:${String(seconds % 60).padStart(2, '0')}`;
}

export function createYouTubePlayer() {
  const $ = (id) => document.getElementById(id);
  const form = $('youtubeForm');
  const input = $('youtubeURL');
  const submit = $('youtubeSubmit');
  const viewer = $('youtubeViewer');
  const frame = $('youtubeFrame');
  const play = $('youtubePlay');
  const seek = $('youtubeSeek');
  const clock = $('youtubeTime');
  const remove = $('youtubeRemove');
  const volume = $('youtubeVolume');
  const error = $('youtubeErr');
  let session = null;
  const savedVolume = Number(localStorage.getItem('meetlerYouTubeVolume') ?? 100);
  volume.value = Number.isFinite(savedVolume) ? Math.max(0, Math.min(100, savedVolume)) : 100;

  function position(s) {
    return s.state.position + (s.state.playing ? (performance.now() - s.receivedAt) / 1000 : 0);
  }

  function controls(s) {
    const duration = s?.ready ? s.player.getDuration() : 0;
    const playing = s?.state?.playing && !s.blocked;
    play.textContent = playing ? 'Пауза' : 'Смотреть';
    play.setAttribute('aria-pressed', String(!!playing));
    play.disabled = !s?.ready || s.busy || s.failed;
    seek.disabled = !duration || s.busy || s.failed;
    remove.disabled = !s?.state?.videoId || s.busy;
    submit.disabled = !s || s.busy;
    if (!s?.scrubbing) {
      seek.max = duration || 1;
      seek.value = s?.state?.videoId ? Math.min(position(s), duration) : 0;
    }
    clock.textContent = `${timeLabel(Number(seek.value))} / ${timeLabel(duration)}`;
  }

  function destroyPlayer(s) {
    s.player?.destroy();
    s.player = null;
    s.ready = false;
    s.loadedID = '';
    frame.replaceChildren();
  }

  async function ensurePlayer(s) {
    if (s.player || s.loading || !s.state.videoId) return;
    s.loading = true;
    try {
      const YT = await loadAPI();
      if (session !== s || !s.state.videoId) return;
      const host = document.createElement('div');
      frame.replaceChildren(host);
      s.player = new YT.Player(host, {
        width: '100%', height: '100%', videoId: s.state.videoId,
        playerVars: { controls: 0, disablekb: 1, playsinline: 1, origin: location.origin, hl: 'ru' },
        events: {
          onReady: ({ target }) => {
            if (session !== s || s.player !== target) return;
            s.ready = true;
            target.setVolume(Number(volume.value));
            syncPlayer(s);
          },
          onAutoplayBlocked: ({ target }) => {
            if (session !== s || s.player !== target) return;
            s.blocked = true;
            controls(s);
          },
          onStateChange: ({ target, data }) => {
            if (session !== s || s.player !== target) return;
            if (data === YT.PlayerState.PLAYING) s.blocked = false;
            if (data === YT.PlayerState.ENDED && s.state.playing) {
              change('pause', { position: target.getDuration() });
            }
            controls(s);
          },
          onError: ({ target, data }) => {
            if (session !== s || s.player !== target) return;
            s.failed = true;
            error.textContent = data === 100 ? 'Видео удалено или недоступно.' :
              data === 101 || data === 150 ? 'Автор запретил просмотр этого видео на других сайтах.' :
                'Не удалось воспроизвести видео YouTube.';
            controls(s);
          },
        },
      });
    } catch (err) {
      if (session === s) error.textContent = err.message;
    } finally {
      s.loading = false;
    }
  }

  function syncPlayer(s) {
    if (session !== s || !s.ready || !s.state.videoId || s.busy || s.failed || s.scrubbing) return;
    const target = position(s);
    const player = s.player;
    if (s.loadedID !== s.state.videoId) {
      s.loadedID = s.state.videoId;
      s.appliedRevision = s.state.revision;
      const video = { videoId: s.loadedID, startSeconds: target };
      if (s.state.playing) player.loadVideoById(video);
      else player.cueVideoById(video);
      return;
    }
    if (s.blocked && s.state.playing) return;
    const duration = player.getDuration();
    const at = duration ? Math.min(target, duration) : target;
    const state = player.getPlayerState();
    if (s.appliedRevision !== s.state.revision ||
      (state !== window.YT.PlayerState.BUFFERING && Math.abs(player.getCurrentTime() - at) > 1.5)) {
      player.seekTo(at, true);
      s.appliedRevision = s.state.revision;
    }
    // Buffering and autoplay events must not change playback for everyone else.
    if (!s.state.playing && state !== window.YT.PlayerState.PAUSED) player.pauseVideo();
    else if (s.state.playing && !s.blocked && state !== window.YT.PlayerState.PLAYING &&
      state !== window.YT.PlayerState.BUFFERING && (!duration || at < duration)) player.playVideo();
  }

  function accept(s, state, sentAt) {
    if (session !== s || state.revision < (s.state?.revision ?? 0)) return;
    if (state.videoId !== s.state?.videoId) {
      s.blocked = false;
      s.failed = false;
      error.textContent = '';
    }
    s.state = state;
    s.receivedAt = (sentAt + performance.now()) / 2;
    viewer.hidden = !state.videoId;
    if (!state.videoId) destroyPlayer(s);
    else { ensurePlayer(s); syncPlayer(s); }
    controls(s);
  }

  async function request(s, suffix = '', options = {}) {
    const sentAt = performance.now();
    const response = await fetch(s.path + suffix, { ...options, signal: s.abort.signal, cache: 'no-store' });
    if (!response.ok) throw new Error('Не удалось обновить YouTube.');
    accept(s, await response.json(), sentAt);
  }

  async function refresh(s) {
    if (session !== s || s.polling || s.busy) return;
    s.polling = true;
    try { await request(s); }
    catch (err) { if (session === s && err.name !== 'AbortError') error.textContent = err.message; }
    finally { s.polling = false; }
  }

  async function change(action, body, method = 'POST') {
    const s = session;
    if (!s || s.busy) return;
    s.busy = true;
    error.textContent = '';
    controls(s);
    try {
      await request(s, '/' + action, {
        method, headers: { 'Content-Type': 'application/json' },
        body: body === undefined ? undefined : JSON.stringify(body),
      });
      if (action === 'video' && session === s) {
        s.failed = false;
        s.blocked = false;
        s.loadedID = '';
      }
      return session === s;
    } catch (err) {
      if (session === s && err.name !== 'AbortError') error.textContent = err.message;
    } finally {
      s.busy = false;
      if (session === s) { syncPlayer(s); controls(s); }
    }
  }

  form.onsubmit = async (event) => {
    event.preventDefault();
    try {
      if (await change('video', { videoId: videoID(input.value.trim()) }, 'PUT')) input.value = '';
    } catch (err) { error.textContent = err.message; }
  };
  play.onclick = () => {
    const s = session;
    if (!s?.ready || s.busy) return;
    if (s.blocked && s.state.playing) {
      s.player.seekTo(position(s), true);
      s.player.playVideo();
      return;
    }
    const duration = s.player.getDuration();
    const at = position(s);
    const next = !s.state.playing && duration && at >= duration ? 0 : at;
    if (s.state.playing) s.player.pauseVideo();
    else { s.player.seekTo(next, true); s.player.playVideo(); }
    change(s.state.playing ? 'pause' : 'play', { position: next });
  };
  seek.oninput = () => { if (session) { session.scrubbing = true; controls(session); } };
  seek.onchange = () => {
    if (session) session.scrubbing = false;
    change('seek', { position: Number(seek.value) });
  };
  seek.onblur = () => { if (session) session.scrubbing = false; };
  remove.onclick = () => change('video', undefined, 'DELETE');
  volume.oninput = () => {
    localStorage.setItem('meetlerYouTubeVolume', volume.value);
    if (session?.ready) session.player.setVolume(Number(volume.value));
  };

  function disconnect() {
    const s = session;
    session = null;
    if (s) {
      clearInterval(s.timer);
      s.abort.abort();
      destroyPlayer(s);
    }
    viewer.hidden = true;
    input.value = '';
    error.textContent = '';
    controls(null);
  }

  disconnect();
  return {
    connect(roomName) {
      disconnect();
      const s = session = {
        path: '/api/rooms/' + encodeURIComponent(roomName) + '/youtube',
        abort: new AbortController(),
      };
      refresh(s);
      s.timer = setInterval(() => { refresh(s); syncPlayer(s); controls(s); }, 1000);
    },
    disconnect,
  };
}
