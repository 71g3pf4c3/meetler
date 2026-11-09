# meetler

Видеоконференции в браузере: LiveKit SFU, nginx и Go API.
Участники входят по имени палаты без аккаунтов. `APP_PASSWORD`
включает общий пароль для доступа к API.

- **web** — видеозвонки, шаринг экрана, громкость участников, общие звуки и портреты,
  голосования за кик и удаление, приветственные и прощальные звуки.
- **api** — вход по паролю, JWT для LiveKit, реестр палат, загрузки и голосования.
- **nginx** — HTTPS, статика, `/api/`, `/healthz` и WebSocket-сигналинг LiveKit.
- **LiveKit** — медиа и встроенный TURN. В Compose настроен Redis;
  NixOS-модуль не требует его для одной ноды.

## Тихие палаты

`PROTECTED_ROOMS` (csv, по умолчанию `tishina`) — палаты, которые всегда видны
в списке и которые нельзя закрыть голосованием или DELETE.

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
Файлы из `environmentFiles` читает API. Можно передать отдельные файлы
с ключами и паролем. Секреты хранятся вне Nix store.

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
API и signaling API LiveKit по умолчанию слушают loopback.
Порт API — `services.meetler.port`.
Отключить TURN: `services.livekit.settings.turn.enabled = false;`.
Для собственного firewall: `services.meetler.openFirewall = false;`.

## Данные и диагностика

Реестр палат, общая звуковая аптечка, галерея портретов и
джинглы welcome/farewell живут в SQLite: `ROOMS_DIR/meetler.db`
(в NixOS — `/var/lib/meetler/rooms/meetler.db`). Бэкап = копия файла
(или `sqlite3 meetler.db ".backup backup.db"`). Если API не может открыть
каталог, он пробует временное хранилище и пишет об этом в журнал; временные
данные не сохраняются между перезапусками.

Портреты, звуки и джинглы общие для всех палат. Закрытие палаты их
не удаляет. Лимиты загрузок — 200 звуков и 200 портретов на весь сервер.
API медиа: `/api/sounds`, `/api/avatars`, `/api/welcome`, `/api/farewell`;
старые маршруты `/api/rooms/<room>/…` для медиа больше не поддерживаются.
Голосование за удаление проводится в палате, результат удаляет общий файл.

Клиенты выбирают портрет по `hash(identity) % pictures.length`: сначала восемь
стандартных картинок, затем общие загрузки по id. При одинаковом списке все
зрители видят один портрет. Identity случайный при каждом входе; изменение
галереи может изменить назначенные портреты.

Это несовместимое изменение схемы: автоматической миграции нет. Старые БД
с медиаданными по палатам нужно мигрировать вручную; до этого API отключает
хранилище и пишет в журнал `manual migration to global media required`.
Старое файловое хранилище также не импортируется автоматически и сохраняется
при закрытии палат.

`/healthz` возвращает статус процесса API и не проверяет доступность LiveKit.
Голоса хранятся в памяти.
Логи NixOS: `journalctl -u meetler-api -u livekit`.

Клиент может сохранить последние 15 секунд звука палаты в WAV-файл.

## Разработка

CI (GitHub Actions) на каждый push/PR: `go build`/`go vet`/`go test` API и
проверка синтаксиса фронтенда.

## Nix-пакеты

Flake экспортирует `api` и `web`. Пакет по умолчанию — `api`.
Go-пакет использует `buildGoLatestModule` и кеш полных модулей. При изменении
`go.mod` или `go.sum` пересчитай `vendorHash` в `nix/api.nix`.
