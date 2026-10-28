# meetler

Видеоконференции в браузере: LiveKit SFU, nginx, Go API и необязательный
Telegram-мост. Участники входят по имени палаты без аккаунтов. `APP_PASSWORD`
включает общий пароль для доступа к API.

- **web** — видеозвонки, шаринг экрана, громкость участников, звуки и портреты палат,
  голосования за кик и удаление, приветственные и прощальные звуки.
- **api** — вход по паролю, JWT для LiveKit, реестр палат, загрузки, голосования
  и передача аудио комнаты в Telegram.
- **tgbridge** — медиа и TTS из Telegram в палаты, сохранение звуков в аптечку,
  отправка аудио палаты в привязанные чаты.

## Тихие палаты

`PROTECTED_ROOMS` (csv, по умолчанию `tishina`) — палаты без шума: всегда
в списке, их нельзя закрыть голосованием или DELETE, tgbridge в них не
публикует (медиа, TTS, YouTube и `/soundboardsave` отклоняются). Список
должен совпадать у api и tgbridge.
- **nginx** — HTTPS, статика, `/api/`, `/healthz` и WebSocket-сигналинг LiveKit.
- **LiveKit** — медиа и встроенный TURN. В Compose настроен Redis;
  NixOS-модуль не требует его для одной ноды.

## NixOS

Добавь input и `meetler.nixosModules.default` в конфигурацию системы:

```nix
inputs.meetler = {
  url = "github:71g3pf4c3/meetler";
  inputs.nixpkgs.follows = "nixpkgs";
};
```

Для локального checkout можно указать `path:/полный/путь/к/meetler`.

```nix
services.meetler = {
  enable = true;
  domain = "meet.example.com";
  port = 8085;
  sslCertificate = "/etc/ssl/meetler/certificate-chain.crt";
  sslCertificateKey = "/etc/ssl/meetler/private.key";
  livekitKeyFile = "/run/secrets/livekit-keys.yaml";
  environmentFiles = [ "/run/secrets/meetler-api.env" ];

  telegram = {
    enable = true;
    environmentFile = "/run/secrets/telegram.env";
    piperModel = "/var/lib/piper/glados.onnx";
  };
};
```

`livekitKeyFile` содержит `<key>: <secret>`. В `environmentFiles` должны быть
те же значения:

```dotenv
LIVEKIT_API_KEY=...
LIVEKIT_API_SECRET=...
APP_PASSWORD=...
```

`APP_PASSWORD` необязателен: пустое значение отключает проверку пароля.
Файлы из `environmentFiles` читают и API, и Telegram-мост. Можно передать
отдельные файлы с ключами и паролем. Бот получает `TELEGRAM_BOT_TOKEN` из
`telegram.environmentFile`. Секреты хранятся вне Nix store.

Модель Piper задаётся полным путём; рядом нужен файл `<model>.onnx.json`.
Без `piperModel` TTS выключен. Модуль добавляет ffmpeg, yt-dlp и, при указанной
модели, Piper.

Сертификат и ключ задаются **полными путями с любыми именами файлов**.
Их используют nginx и TURN; файлы должны быть доступны группе `nginx`.
Выпуск и обновление сертификата настраиваются отдельно. Для `security.acme`
укажи у своего сертификата:

```nix
reloadServices = [ "nginx.service" "livekit.service" ];
```

Параметры SFU задаются через `services.livekit.settings`. Например:

```nix
services.livekit.settings = {
  port = 7890;
  rtc = {
    node_ip = "203.0.113.10";
    tcp_port = 7891;
    port_range_start = 41000;
    port_range_end = 42000;
  };
  turn = {
    tls_port = 5351;
    relay_range_end = 39999;
  };
};
```

Модуль подставляет порты во внутренние URL, nginx и firewall.
API, signaling API LiveKit и Telegram HTTP по умолчанию слушают loopback.
Порт API — `services.meetler.port`, порт Telegram HTTP —
`services.meetler.telegram.port` (по умолчанию 8090).
Отключить TURN: `services.livekit.settings.turn.enabled = false;`.
Для собственного firewall: `services.meetler.openFirewall = false;`.

## Данные и диагностика

Всё палатное содержимое — реестр палат, звуковая аптечка, галерея портретов,
джинглы welcome/farewell — живёт в SQLite: `ROOMS_DIR/meetler.db`
(в NixOS — `/var/lib/meetler/rooms/meetler.db`). Бэкап = копия файла
(или `sqlite3 meetler.db ".backup backup.db"`). Если API не может открыть
каталог, он пробует временное хранилище и пишет об этом в журнал; временные
данные не сохраняются между перезапусками. Старое файловое хранилище
(каталоги `sounds/`/`avatars/` с sidecar-файлами) автоматически мигрируется
в БД при первом старте; исходные файлы не удаляются.

`/healthz` возвращает статус процесса API и не проверяет доступность LiveKit.
Голоса хранятся в памяти. Привязки Telegram-чатов к палатам — в SQLite
(`TG_DB_PATH`; в NixOS — `/var/lib/meetler-tgbridge/meetler.db`).
Логи NixOS: `journalctl -u meetler-api -u meetler-tgbridge -u livekit`.

## Telegram

- `/help` — справка; `/start` — её алиас.
- `/palata <room>` — выбрать палату для чата; `/palata` — показать текущую.
- `/tts <текст>` — озвучить текст; можно ответить командой на текстовое сообщение.
- `/tovoice` в ответ на медиа — проиграть его в палате.
- `/soundboardsave` в ответ на медиа — сохранить звук в аптечке палаты.
- Голосовые, аудио, видео, кружки и ссылки YouTube — проиграть в выбранной палате.

Явная `#палата` имеет приоритет над выбором `/palata`, затем используется
`DEFAULT_PALATA` (по умолчанию `telegram`). Выбор общий для чата; по умолчанию
сбрасывается после перезапуска бота — задай `TG_DB_PATH` (SQLite-БД),
чтобы привязки переживали рестарт. В тихие палаты (`PROTECTED_ROOMS`) бот
не заходит и отвечает отказом. В группах бот обрабатывает адресованные ему
сообщения: упоминание, ответ на его сообщение или явную палату для
медиа/YouTube.
Для получения обычных медиа в группе боту нужны права администратора или
отключённый privacy mode через BotFather.

Медиа и TTS обрезаются до 20 секунд. Это отдельный лимит от браузерной записи:
клиент сохраняет или отправляет в Telegram последние 15 секунд микса палаты.
Для отправки в Telegram чат должен выбрать палату через `/palata`.

## Разработка

CI (GitHub Actions) на каждый push/PR: `go build`/`go vet` api и tgbridge,
проверка синтаксиса фронтенда.

## Nix-пакеты

Flake экспортирует `api`, `tgbridge` и `web`. Пакет по умолчанию — `api`.
Go-пакеты используют `buildGoLatestModule` и кеш полных модулей. При изменении
`go.mod` или `go.sum` пересчитай `vendorHash` в `nix/api.nix` или `nix/tgbridge.nix`.
