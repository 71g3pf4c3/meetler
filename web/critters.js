// critters.js — декоративная живность поверх UI (муха + таракан).
// Чистый оверлей: pointer-events:none, не трогает палату/клики.
//
// Поведение: тварь НЕ висит постоянно. Она появляется на ~10 секунд в
// СЛУЧАЙНЫЕ промежутки времени, бегает/летает со своим звуком, затем прячется
// и снова ждёт случайный интервал. Муха и таракан независимы.
//
// Звук: реальные mp3 (/fly.mp3, /roach.mp3), зациклены пока тварь видна.
// Громкость рулится WebAudio GainNode (у мухи — по близости к центру экрана;
// у таракана — фикс тихо). Autoplay политика: AudioContext поднимаем на первом
// жесте пользователя; если тварь появилась до жеста — просто без звука.

(function () {
  'use strict';

  // --- оверлей-слой ---
  const layer = document.createElement('div');
  layer.id = 'critters';
  layer.style.cssText =
    'position:fixed;inset:0;z-index:150;pointer-events:none;overflow:hidden;';
  document.body.appendChild(layer);

  const W = () => window.innerWidth;
  const H = () => window.innerHeight;
  const rand = (a, b) => a + Math.random() * (b - a);

  const VISIBLE_MS = 10000; // сколько тварь на экране за один выход

  // ================= АУДИО =================
  // Общий WebAudio-контекст. Каждая тварь — <audio loop> через
  // MediaElementSource → GainNode → destination. Гейн рулим плавно.
  let actx = null;
  let armed = false;

  function initAudio() {
    if (actx) return;
    try {
      actx = new (window.AudioContext || window.webkitAudioContext)();
    } catch (_) { /* нет WebAudio — живность будет немой */ }
  }

  // audioNode: обёртка над <audio loop> с управляемым gain.
  function makeSound(url) {
    const el = new Audio(url);
    el.loop = true;
    el.preload = 'auto';
    el.crossOrigin = 'anonymous';
    let gain = null, srcNode = null, wired = false;
    function wire() {
      if (wired || !actx) return;
      try {
        srcNode = actx.createMediaElementSource(el);
        gain = actx.createGain();
        gain.gain.value = 0;
        srcNode.connect(gain);
        gain.connect(actx.destination);
        wired = true;
      } catch (_) { /* уже подключён или нет WebAudio */ }
    }
    return {
      el,
      wire,
      start() {
        wire();
        try { el.currentTime = 0; } catch (_) {}
        el.play().catch(() => {});
      },
      stop() {
        try { el.pause(); } catch (_) {}
      },
      setVol(v) {
        const vol = Math.max(0, Math.min(1, v));
        if (gain) gain.gain.value += (vol - gain.gain.value) * 0.1; // плавно
        else el.volume = vol; // fallback без WebAudio
      },
    };
  }

  const flySound = makeSound('/fly.mp3');
  const roachSound = makeSound('/roach.mp3');

  // поднять аудио на первом жесте (autoplay policy). Один раз.
  function armAudio() {
    initAudio();
    if (actx && actx.state === 'suspended') actx.resume().catch(() => {});
    flySound.wire();
    roachSound.wire();
    armed = true;
    window.removeEventListener('pointerdown', armAudio);
    window.removeEventListener('keydown', armAudio);
  }
  window.addEventListener('pointerdown', armAudio);
  window.addEventListener('keydown', armAudio);

  // ================= МУХА =================
  const fly = document.createElement('div');
  fly.textContent = '🪰';
  fly.style.cssText =
    'position:absolute;font-size:20px;will-change:transform;display:none;' +
    'filter:drop-shadow(0 1px 1px rgba(0,0,0,.4));';
  layer.appendChild(fly);

  const flyState = {
    active: false,
    x: 0, y: 0, tx: 0, ty: 0, vx: 0, vy: 0, angle: 0,
    targetTimer: null,
  };

  function pickFlyTarget() {
    flyState.tx = rand(20, W() - 20);
    flyState.ty = rand(20, H() - 20);
  }

  function stepFly() {
    const s = flyState;
    if (!s.active) return;
    const ax = (s.tx - s.x) * 0.0016 + rand(-0.4, 0.4);
    const ay = (s.ty - s.y) * 0.0016 + rand(-0.4, 0.4);
    s.vx = (s.vx + ax) * 0.92;
    s.vy = (s.vy + ay) * 0.92;
    const sp = Math.hypot(s.vx, s.vy);
    const max = 6;
    if (sp > max) { s.vx = (s.vx / sp) * max; s.vy = (s.vy / sp) * max; }
    s.x += s.vx; s.y += s.vy;
    if (s.x < 0) { s.x = 0; s.vx = Math.abs(s.vx); }
    if (s.x > W() - 20) { s.x = W() - 20; s.vx = -Math.abs(s.vx); }
    if (s.y < 0) { s.y = 0; s.vy = Math.abs(s.vy); }
    if (s.y > H() - 20) { s.y = H() - 20; s.vy = -Math.abs(s.vy); }
    if (sp > 0.3) s.angle = Math.atan2(s.vy, s.vx);
    fly.style.transform = `translate(${s.x}px,${s.y}px) rotate(${s.angle}rad)`;
    // громче у центра («рядом»), очень тихо в целом
    const cx = W() / 2, cy = H() / 2;
    const dist = Math.hypot(s.x - cx, s.y - cy);
    const prox = 1 - Math.min(1, dist / Math.hypot(cx, cy));
    flySound.setVol(0.08 + prox * 0.32); // тихо..умеренно
  }

  function spawnFly() {
    const s = flyState;
    s.active = true;
    s.x = rand(0, W()); s.y = rand(0, H());
    s.vx = 0; s.vy = 0;
    pickFlyTarget();
    fly.style.display = 'block';
    flySound.start();
    s.targetTimer = setInterval(pickFlyTarget, 1400); // рывки цели
    setTimeout(despawnFly, VISIBLE_MS);
  }

  function despawnFly() {
    const s = flyState;
    s.active = false;
    fly.style.display = 'none';
    flySound.stop();
    if (s.targetTimer) { clearInterval(s.targetTimer); s.targetTimer = null; }
    scheduleFly();
  }

  function scheduleFly() {
    setTimeout(spawnFly, rand(20000, 60000)); // случайный промежуток 20–60с
  }

  // ================= ТАРАКАН =================
  const roach = document.createElement('div');
  roach.textContent = '🪳';
  roach.style.cssText =
    'position:absolute;font-size:22px;will-change:transform;display:none;' +
    'filter:drop-shadow(0 1px 1px rgba(0,0,0,.5));';
  layer.appendChild(roach);

  const roachState = {
    active: false,
    x: 0, y: 0, dir: 1, speed: 0, phase: 'run', until: 0, bob: 0,
  };

  function roachNext(now) {
    const r = roachState;
    if (Math.random() < 0.35) {
      r.phase = 'pause'; r.speed = 0; r.until = now + rand(300, 1200);
    } else {
      r.phase = 'run';
      if (Math.random() < 0.4) r.dir *= -1;
      r.speed = rand(2.5, 6); r.until = now + rand(500, 1800);
    }
  }

  function stepRoach(now) {
    const r = roachState;
    if (!r.active) return;
    if (now >= r.until) roachNext(now);
    if (r.phase === 'run') {
      r.x += r.dir * r.speed;
      r.bob += 0.5;
      if (r.x < -24) r.x = W() + 10;
      if (r.x > W() + 24) r.x = -10;
      r.y = H() - 26 + Math.sin(r.bob) * 1.5;
    }
    const flip = r.dir < 0 ? -1 : 1;
    roach.style.transform = `translate(${r.x}px,${r.y}px) scaleX(${flip})`;
  }

  function spawnRoach() {
    const r = roachState;
    r.active = true;
    r.dir = Math.random() < 0.5 ? -1 : 1;
    r.x = r.dir < 0 ? W() + 10 : -10; // вбегает из-за края
    r.y = H() - 26;
    r.phase = 'run'; r.speed = rand(2.5, 6); r.until = performance.now() + rand(500, 1500);
    roach.style.display = 'block';
    roachSound.start();
    roachSound.setVol(0.5); // бег тихий, но слышный
    setTimeout(despawnRoach, VISIBLE_MS);
  }

  function despawnRoach() {
    roachState.active = false;
    roach.style.display = 'none';
    roachSound.stop();
    scheduleRoach();
  }

  function scheduleRoach() {
    setTimeout(spawnRoach, rand(30000, 90000)); // случайный промежуток 30–90с
  }

  window.addEventListener('resize', () => {
    if (roachState.active) roachState.y = H() - 26;
  });

  // ================= ЦИКЛ =================
  function loop(now) {
    stepFly();
    stepRoach(now);
    requestAnimationFrame(loop);
  }
  requestAnimationFrame(loop);

  // первый выход — не сразу, но и не слишком долго ждать
  setTimeout(spawnFly, rand(5000, 15000));
  setTimeout(spawnRoach, rand(8000, 20000));
})();
