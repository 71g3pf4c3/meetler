# meetler

Self-hosted видеоконференции на WebRTC в эстетике старого форума/дурдома. SFU —
[LiveKit](https://github.com/livekit/livekit) (Go, pion под капотом). Фокус:
минимальная задержка и коннект, который не рвётся. Вход анонимный — без аккаунтов,
по номеру «палаты».

Это не форк Jitsi. Та же архитектура (SFU + сигналинг + TURN + браузерный клиент),
но на Go-SFU с production-grade congestion control: TWCC send-side BWE, dynacast,
adaptive stream, агрессивный авто-fallback UDP → ICE/TCP → TURN/TLS.

## Скриншоты

Приёмное отделение (вход в палату):

![Вход](docs/screenshots/01-join.png)

Палата с участниками (зелёная рамка = говорит):

![Палата](docs/screenshots/02-ward.png)

Выключенная камера → дефолтная аватарка:

![Аватар](docs/screenshots/03-avatar.png)

## Фичи

- Анонимный вход, рандомный identity, без PII.
- Видео (simulcast off, 180p низкий битрейт — приоритет «пролезть всегда»).
- Аудио opus mono, DTX/RED off — минимум задержки и обработки.
- Зелёная подсветка говорящих (`ActiveSpeakersChanged`).
- Дефолтная аватарка при выключенной камере.
- Кнопка **ГОЙДА** — проигрывает mp3 на всю палату.
- **Скачать 15с** — кольцевой PCM-буфер микса комнаты → .wav (ровно 15с).
- Устойчивость: бесконечный reconnect, захват устройств с таймаутом (зависший
  getUserMedia не блокирует вход), fallback на TCP/TURN при любой проблеме UDP.

## Архитектура

```
браузер ──wss(443)──► Caddy ──► LiveKit signaling (7880)
   │                    └──► token-svc (анонимный room-JWT)
   │
   └──RTP/UDP(50000-60000)──► LiveKit SFU   (напрямую, host-network)
   └──TURN/TLS(5349)────────► LiveKit TURN  (fallback за firewall)
```

- **livekit** — SFU + встроенный TURN. `network_mode: host` — UDP media идёт напрямую
  на публичный IP без Docker userland proxy (иначе ICE деградирует).
- **token-svc** — Go, выдаёт короткоживущий room-JWT со случайным `anon-<id>`.
  identity всегда серверный рандом — клиент не может выдать себя за другого.
- **caddy** — TLS (авто-ACME), проксирует wss-сигналинг, token API, отдаёт статику.
- **redis** — состояние комнат/роутинг (нужен даже на одной ноде).

## Требования

- Публичный IP на хосте (прямой, без NAT перед ним — иначе см. раздел NAT).
- DNS A/AAAA `SITE_DOMAIN` → этот хост.
- Открытые порты (firewall / security group):
  - `443/tcp`, `80/tcp` — Caddy (TLS + ACME).
  - `50000-60000/udp` — RTP media.
  - `7881/tcp` — ICE-over-TCP fallback.
  - `5349/tcp` — TURN/TLS.
  - `30000-40000/udp` — TURN relay.
- Docker + docker compose, `bash`, `sed`, coreutils.

## Запуск

```bash
make keys          # сгенерить API keypair в .env
# отредактируй .env: SITE_DOMAIN, TURN_DOMAIN (обычно = SITE_DOMAIN)

make up            # поднять стек (TURN пока off — серта ещё нет)
# дождись, пока Caddy выпишет ACME-серт (make logs)

make turn-cert     # вытащить серт Caddy в ./certs для TURN
# в .env: TURN_ENABLED=true
make up            # перезапуск — теперь TURN/TLS активен
```

Открой `https://SITE_DOMAIN`, введи комнату → ты в конференции.

## Tuned-настройки (что и зачем)

Сервер (`livekit/livekit.yaml.template`):
- `congestion_control.use_send_side_bwe: true` — SFU оценивает пропускную по TWCC и
  режет/переключает слои **до** потерь. Это и есть "минимизация задержки" на практике.
- `allow_tcp_fallback` + `tcp_fallback_rtt_threshold: 150` + `allow_udp_unstable_fallback`
  — миграция лоссёвого UDP на ICE/TCP или TURN/TLS без реконнекта.
- `turn` TLS/5349 — проходит сквозь корпоративные firewall (~50% "стабильности" за NAT).
- `room.playout_delay.min: 0` — минимальный буфер, растёт только при джиттере.
- `audio/red` + `active_red_encoding` — FEC голоса, скрывает потери пакетов.

Клиент (`web/app.js`):
- `simulcast: true` c 3 слоями (180p/360p/720p).
- `adaptiveStream` — клиент просит слой под размер тайла.
- `dynacast` — SFU не гоняет неиспользуемые слои.
- `dtx` + `red` на аудио.

## Анонимность

- Нет аккаунтов/IdP/email. `identity` = серверный `anon-<rand>`.
- Токен ограничен одной комнатой + TTL (`TOKEN_TTL`).
- В логах нет PII. Хочешь строже — выключи логи token-svc, сократи TTL.
- E2EE (сервер не видит медиа) не включён — это клиентский флаг `livekit-client`
  (insertable streams). Можно добавить позже, см. LiveKit E2EE docs.

## Если хост за NAT (OpenStack floating IP, облако)

`use_external_ip: true` в конфиге определяет публичный IP через STUN. Если self-ping
не проходит (security group не пускает трафик самому себе) — в `livekit.yaml.template`:
- выстави `node_ip: <floating-ip>` и `use_external_ip: false`, либо
- добавь `skip_external_ip_validation: true`.
Floating IP должен 1:1 мапиться на порт инстанса, security group — открыть UDP range.

## Ротация TURN-серта

Caddy обновляет ACME-серт сам, но встроенный TURN читает файл. По таймеру:
```bash
make turn-cert && docker compose restart livekit
```

## Observability

LiveKit отдаёт Prometheus-метрики на `:6789/metrics` (host-network). Скрейпь своим
Prometheus/VictoriaMetrics. Логи структурные (JSON) → VictoriaLogs.

## Деплой через NixOS (альтернатива docker-compose)

Flake экспортирует `nixosModules.default`, который оборачивает официальный
`services.livekit` с tuned-настройками + token-svc (systemd, DynamicUser) +
reverse-proxy (Caddy или nginx) + ACME + firewall.

```nix
# flake.nix твоей системы (репозиторий на GitHub пока называется hit-meet)
{
  inputs.meetler.url = "github:71g3pf4c3/hit-meet";
  outputs = { nixpkgs, meetler, ... }: {
    nixosConfigurations.meet = nixpkgs.lib.nixosSystem {
      system = "x86_64-linux";
      modules = [
        meetler.nixosModules.default
        {
          services.meetler = {
            enable = true;
            domain = "meet.example.com";
            acmeEmail = "admin@example.com";
            reverseProxy = "nginx";          # или "caddy" (дефолт)
            livekitKeyFile = "/run/secrets/livekit-keys.yaml";
            tokenEnvironmentFile = "/run/secrets/token.env";
            nodeIp = "203.0.113.10";         # floating IP для облака/NAT
          };
        }
      ];
    };
  };
}
```

Секреты (вне nix store):
- `livekitKeyFile` — формат `<key>: <secret>` (`livekit-server generate-keys`).
- `tokenEnvironmentFile` — `LIVEKIT_API_KEY=...` и `LIVEKIT_API_SECRET=...`,
  та же пара key/secret. Клади через sops-nix / agenix / Vault, не в git.

Выбор reverse-proxy: nginx настраивается с `proxyWebsockets` для wss-сигналинга
(без этого livekit-сигналинг не поднимется) и 24h read/send timeout на `/rtc`.
Caddy проксирует WS прозрачно. ACME централизован в `security.acme`, серт шарится
между webserver и встроенным TURN livekit через группу-владельца.

На первом деплое (серта ещё нет) можно `services.meetler.enableTurn = false`,
после выписки серта — снять флаг и пересобрать.

Dev-shell: `nix develop` (go, gopls, livekit, livekit-cli, caddy, nginx).

Примечание: nixpkgs тянет livekit из своего пина (может быть чуть старее docker-пина
v1.13.10 — tuned-поля конфига идентичны, API совместим).

## Лимиты

- Один инстанс LiveKit. Для кластера (много нод, общий redis) — см. LiveKit distributed
  deployment; `redis` уже в стеке.
- `max_participants` per-room навязывается через RoomService CreateRoom — token-svc
  сейчас выдаёт токен на auto-create комнату; жёсткий лимит — TODO (создавать комнату
  с лимитом до выдачи первого токена).
- Recording/SIP — отдельные компоненты LiveKit (egress/SIP), в этот стек не входят.
