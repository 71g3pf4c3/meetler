# NixOS-модуль meetler.
#
# Обёртка над официальным services.livekit + свой token-svc + reverse-proxy
# (Caddy или nginx на выбор). Весь tuned-конфиг SFU в services.livekit.settings.
#
# Секреты НЕ в nix store:
#   - livekit: keyFile  (формат "<key>: <secret>", livekit-server generate-keys)
#   - token-svc: environmentFile (LIVEKIT_API_KEY / LIVEKIT_API_SECRET)
#   оба ДОЛЖНЫ содержать одну и ту же пару key/secret.
#
# TURN/TLS переиспользует ACME-серт, который reverse-proxy получает для домена.

{ self, mkWeb }:
{ config, lib, pkgs, ... }:

let
  cfg = config.services.meetler;
  tokenSvc = self.packages.${pkgs.system}.token-svc;
  tgbridge = self.packages.${pkgs.system}.tgbridge;
  web = mkWeb pkgs;

  proxyGroup = if cfg.reverseProxy == "nginx" then "nginx" else "caddy";

  # Централизуем серт на security.acme → стабильный путь для TURN независимо от proxy.
  # webserver использует useACMEHost (не управляет ACME сам), challenge — через webroot.
  acmeDir = "/var/lib/acme/${cfg.domain}";
  acmeChallengeRoot = "/var/lib/acme/acme-challenge";

  # Русские голоса piper (rhasspy/piper-voices, ru_RU-<voice>-medium).
  # Модель + конфиг должны лежать рядом: piper ищет <model>.onnx.json.
  piperVoices = lib.mapAttrs (voice: hashes: pkgs.runCommand "meetler-piper-${voice}" { } ''
    mkdir -p $out
    ln -s ${pkgs.fetchurl {
      url = "https://huggingface.co/rhasspy/piper-voices/resolve/main/ru/ru_RU/${voice}/medium/ru_RU-${voice}-medium.onnx";
      sha256 = hashes.model;
    }} $out/voice.onnx
    ln -s ${pkgs.fetchurl {
      url = "https://huggingface.co/rhasspy/piper-voices/resolve/main/ru/ru_RU/${voice}/medium/ru_RU-${voice}-medium.onnx.json";
      sha256 = hashes.config;
    }} $out/voice.onnx.json
  '') {
    dmitri = {
      model = "sha256-8HM1brxL0PgMWvWN8pU6WYi9W9qx6zhjXOlgsHH778s=";
      config = "sha256-Zn7zEXvGQsKJLf92kNi9yMpCKK6qeDstwUFt9jKFXg0=";
    };
    irina = {
      model = "sha256-j/OCEtI9owC743BcZF5uW5R18L/eAVWOsXgT4irKqqo=";
      config = "sha256-wuwouzjitZ6TuVmz5ANIwa/rvScvMP7V1BIF0I6Yqdc=";
    };
    ruslan = {
      model = "sha256-cqX4jgsgkoBk60XYjh2qIfivYtGGE1gNMsu0rtSNz38=";
      config = "sha256-cGpPsXvDBKvQeAm1Ut6uYV5k3L/7+9CYVLo3ylnogRc=";
    };
  };

  # выбранный голос (деривация) или null
  piperVoice = if cfg.telegram.piperVoice == null then null
    else piperVoices.${cfg.telegram.piperVoice};
in
{
  options.services.meetler = {
    enable = lib.mkEnableOption "meetler WebRTC conferencing";

    domain = lib.mkOption {
      type = lib.types.str;
      description = "Публичный домен (TLS, сигналинг, TURN). DNS A/AAAA → этот хост.";
      example = "meet.example.com";
    };

    acmeEmail = lib.mkOption {
      type = lib.types.str;
      description = "Email для ACME (Let's Encrypt).";
    };

    reverseProxy = lib.mkOption {
      type = lib.types.enum [ "caddy" "nginx" ];
      default = "caddy";
      description = ''
        Чем терминировать TLS и проксировать сигналинг/статику/token API.
        nginx требует явного proxyWebsockets для wss-сигналинга livekit
        (модуль настраивает это сам).
      '';
    };

    livekitKeyFile = lib.mkOption {
      type = lib.types.path;
      description = ''
        Файл ключей LiveKit, формат "<key>: <secret>".
        Сгенерировать: livekit-server generate-keys
        Та же пара key/secret должна быть в tokenEnvironmentFile.
      '';
    };

    tokenEnvironmentFile = lib.mkOption {
      type = lib.types.path;
      description = ''
        EnvironmentFile для token-svc с LIVEKIT_API_KEY и LIVEKIT_API_SECRET.
        Значения должны совпадать с livekitKeyFile.
      '';
    };

    tokenTTL = lib.mkOption {
      type = lib.types.str;
      default = "6h";
      description = "TTL анонимного room-токена.";
    };

    roomMaxParticipants = lib.mkOption {
      type = lib.types.int;
      default = 30;
      description = "Лимит участников на комнату.";
    };

    appPassword = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = ''
        Общий пароль на весь сайт (центральная аутентификация, как basic auth).
        null = аутентификация выключена (сайт открыт).
        ВНИМАНИЕ: значение попадёт в /nix/store (мир-читаемо). Для прода задавай
        APP_PASSWORD через tokenEnvironmentFile, а не здесь.
      '';
    };

    nodeIp = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = ''
        Явный публичный IP ноды (для облака/NAT, напр. OpenStack floating IP).
        Если задан — use_external_ip выключается. Если null — IP ищется через STUN.
      '';
      example = "203.0.113.10";
    };

    mediaPortRange = lib.mkOption {
      type = lib.types.attrsOf lib.types.port;
      default = { start = 50000; end = 60000; };
      description = "UDP-диапазон для RTP media.";
    };

    enableTurn = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = ''
        Встроенный TURN/TLS на 5349. Требует выписанного ACME-серта для domain.
        На самом первом деплое (серта ещё нет) можно выставить false, затем true.
      '';
    };

    telegram = {
      enable = lib.mkEnableOption "Telegram-мост (tgbridge): медиа и TTS в палату";
      botToken = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        example = "123456:AA...";
        description = ''
          Токен бота прямой строкой. ПРОСТО, но НЕБЕЗОПАСНО: значение попадёт в
          /nix/store (мир-читаемо, любой локальный юзер прочитает). Для прода
          используй environmentFile. Задай ЛИБО botToken, ЛИБО environmentFile.
        '';
      };
      environmentFile = lib.mkOption {
        type = lib.types.nullOr lib.types.path;
        default = null;
        example = "/run/secrets/tg.env";
        description = ''
          EnvironmentFile с токеном бота (безопасный способ, секрет вне store).
          Файл должен содержать строку:
            TELEGRAM_BOT_TOKEN=123456:AA...
          Клади через sops-nix/agenix/Vault или вручную в /run/secrets (chmod 600).
        '';
      };
      defaultPalata = lib.mkOption {
        type = lib.types.str;
        default = "telegram";
        description = "Палата по умолчанию, если в сообщении не указана #палата.";
      };
      piperModel = lib.mkOption {
        type = lib.types.nullOr lib.types.path;
        default = null;
        description = ''
          Путь к piper .onnx модели для озвучки текста (@бот/#палата).
          null = TTS выключен (медиа всё равно работает).
          Взаимоисключим с piperVoice.
        '';
      };
      piperVoice = lib.mkOption {
        type = lib.types.nullOr (lib.types.enum [ "dmitri" "irina" "ruslan" ]);
        default = null;
        description = ''
          Русский голос piper — модель (ru_RU-<voice>-medium) скачивается
          автоматически в nix store при сборке системы. Взаимоисключим с piperModel.
        '';
      };
    };
  };

  config = lib.mkIf cfg.enable {

    assertions = [
      {
        assertion = !cfg.telegram.enable
          || (cfg.telegram.botToken != null) != (cfg.telegram.environmentFile != null);
        message = "services.meetler.telegram: задай РОВНО одно — telegram.botToken (прямо) ИЛИ telegram.environmentFile (файл).";
      }
      {
        assertion = cfg.telegram.piperVoice == null || cfg.telegram.piperModel == null;
        message = "services.meetler.telegram: задай ЛИБО piperVoice (авто-модель из nix store), ЛИБО piperModel (свой путь).";
      }
    ];

    # --- LiveKit SFU (официальный модуль + tuned settings) ---
    services.livekit = {
      enable = true;
      keyFile = cfg.livekitKeyFile;
      openFirewall = true;            # открывает media UDP range + tcp_port
      redis = {
        createLocally = true;
        host = "127.0.0.1";
        port = 6379;
      };

      settings = {
        port = 7880;
        bind_addresses = [ "0.0.0.0" ];

        rtc = {
          port_range_start = cfg.mediaPortRange.start;
          port_range_end = cfg.mediaPortRange.end;
          tcp_port = 7881;
          use_external_ip = cfg.nodeIp == null;
        } // lib.optionalAttrs (cfg.nodeIp != null) {
          node_ip = cfg.nodeIp;
        } // {
          # congestion control — ядро минимизации задержки
          congestion_control = {
            enabled = true;
            allow_pause = true;
            use_send_side_bwe = true;
            send_side_bwe_pacer = "no-queue";
          };
          # connection stability — авто-fallback UDP → ICE/TCP → TURN/TLS
          allow_tcp_fallback = true;
          tcp_fallback_rtt_threshold = 150;
          allow_udp_unstable_fallback = true;
          pli_throttle = {
            low_quality = "500ms";
            mid_quality = "1s";
            high_quality = "1s";
          };
        };

        room = {
          auto_create = true;
          empty_timeout = 300;
          departure_timeout = 20;
          max_participants = 0;
          enabled_codecs = [
            { mime = "audio/opus"; }   # только opus mono — простой/надёжный путь
            { mime = "video/vp8"; }
            { mime = "video/vp9"; }
            { mime = "video/h264"; }
          ];
          playout_delay = {
            enabled = true;
            min = 0;
            max = 1000;
          };
        };

        audio.active_red_encoding = false;  # RED выкл — приоритет простоты/latency
        prometheus.port = 6789;
        logging = { level = "info"; json = true; };
      } // lib.optionalAttrs cfg.enableTurn {
        turn = {
          enabled = true;
          tls_port = 5349;
          domain = cfg.domain;
          cert_file = "${acmeDir}/fullchain.pem";
          key_file = "${acmeDir}/key.pem";
          relay_range_start = 30000;
          relay_range_end = 40000;
        };
      };
    };

    # ACME HTTP-01 через общий webroot. Один cert-order, серт в /var/lib/acme/<domain>,
    # читается и webserver'ом (useACMEHost), и livekit TURN (через SupplementaryGroups).
    # Challenge-файлы кладутся в webroot; оба webserver'а обслуживают
    # /.well-known/acme-challenge/ из него (см. ниже). Это чинит "websocket error":
    # без выписанного серта HTTPS невалиден и браузер рвёт wss.
    security.acme.certs.${cfg.domain} = {
      group = proxyGroup;
      webroot = acmeChallengeRoot;
    };
    # services.livekit использует DynamicUser → нет статического users.users.livekit.
    # Даём динамическому юзеру доступ к ACME-серту (для TURN/TLS) через
    # SupplementaryGroups = группа-владелец серта.
    systemd.services.livekit.serviceConfig.SupplementaryGroups =
      lib.mkIf cfg.enableTurn [ proxyGroup ];

    # --- token-svc ---
    systemd.services.meetler-token = {
      description = "meetler anonymous room-JWT issuer";
      wantedBy = [ "multi-user.target" ];
      after = [ "network.target" ];
      serviceConfig = {
        ExecStart = "${lib.getExe tokenSvc}";
        EnvironmentFile = cfg.tokenEnvironmentFile;
        Environment = [
          "LISTEN_ADDR=127.0.0.1:8080"
          "TOKEN_TTL=${cfg.tokenTTL}"
          "ROOM_MAX_PARTICIPANTS=${toString cfg.roomMaxParticipants}"
          # список палат (/api/rooms): livekit на том же хосте
          "LIVEKIT_URL=ws://127.0.0.1:7880"
          # персистентный реестр палат: /var/lib/meetler/rooms/<палата>/{sounds,avatars}
          # (DynamicUser-friendly, в том же StateDirectory)
          "ROOMS_DIR=/var/lib/meetler/rooms"
        ] ++ lib.optional (cfg.appPassword != null) "APP_PASSWORD=${cfg.appPassword}";
        DynamicUser = true;
        # systemd создаёт И чистит владение на КАЖДОМ старте под текущий
        # DynamicUser UID. Явно даём rooms-подкаталог, иначе при смене UID
        # между версиями /var/lib/meetler/rooms оставался чужим → MkdirAll
        # падал с EACCES и token-svc писал "room storage disabled".
        StateDirectory = "meetler meetler/rooms";
        StateDirectoryMode = "0700";
        Restart = "on-failure";
        RestartSec = "2s";
        # hardening
        NoNewPrivileges = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        PrivateTmp = true;
        PrivateDevices = true;
        RestrictAddressFamilies = [ "AF_INET" "AF_INET6" ];
        SystemCallFilter = [ "@system-service" ];
      };
    };

    # --- tgbridge (Telegram-мост), опционально ---
    systemd.services.meetler-tgbridge = lib.mkIf cfg.telegram.enable {
      description = "meetler Telegram media/TTS bridge";
      wantedBy = [ "multi-user.target" ];
      after = [ "network-online.target" "livekit.service" ];
      wants = [ "network-online.target" ];
      # ffmpeg (транскод) + piper (TTS) в PATH
      path = [ pkgs.ffmpeg pkgs.piper-tts ];
      serviceConfig = {
        ExecStart = "${lib.getExe tgbridge}";
        # LIVEKIT_API_KEY/SECRET берём из того же файла, что и token-svc (одна пара).
        # TELEGRAM_BOT_TOKEN — из environmentFile ИЛИ прямой botToken (в Environment).
        EnvironmentFile =
          [ cfg.tokenEnvironmentFile ]
          ++ lib.optional (cfg.telegram.environmentFile != null) cfg.telegram.environmentFile;
        Environment = [
          "LIVEKIT_URL=ws://127.0.0.1:7880"
          "DEFAULT_PALATA=${cfg.telegram.defaultPalata}"
        ] ++ lib.optional (cfg.telegram.botToken != null) "TELEGRAM_BOT_TOKEN=${cfg.telegram.botToken}"
          ++ lib.optional (cfg.telegram.piperModel != null) "PIPER_MODEL=${cfg.telegram.piperModel}"
          ++ lib.optional (piperVoice != null) "PIPER_MODEL=${piperVoice}/voice.onnx"
          ++ lib.optional (cfg.telegram.piperModel != null || piperVoice != null) "PIPER_BIN=${pkgs.piper-tts}/bin/piper";
        DynamicUser = true;
        Restart = "on-failure";
        RestartSec = "5s";
        NoNewPrivileges = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        PrivateTmp = true;
        RestrictAddressFamilies = [ "AF_INET" "AF_INET6" ];
      };
    };

    # --- reverse-proxy: Caddy ---
    # Серт из security.acme (useACMEHost), challenge раздаётся на :80 из webroot.
    services.caddy = lib.mkIf (cfg.reverseProxy == "caddy") {
      enable = true;
      # HTTP :80 — ACME HTTP-01 challenge (серт выписывается здесь), остальное → https.
      virtualHosts."http://${cfg.domain}".extraConfig = ''
        handle /.well-known/acme-challenge/* {
          root * ${acmeChallengeRoot}
          file_server
        }
        handle {
          redir https://{host}{uri} permanent
        }
      '';
      virtualHosts.${cfg.domain} = {
        useACMEHost = cfg.domain;
        extraConfig = ''
          encode zstd gzip

          handle /api/* {
            reverse_proxy 127.0.0.1:8080
          }
          handle /rtc* {
            reverse_proxy 127.0.0.1:7880
          }
          handle /twirp* {
            reverse_proxy 127.0.0.1:7880
          }
          handle {
            root * ${web}
            try_files {path} /index.html
            file_server
          }
        '';
      };
    };

    # --- reverse-proxy: nginx ---
    # useACMEHost: nginx использует централизованный серт из security.acme.
    services.nginx = lib.mkIf (cfg.reverseProxy == "nginx") {
      enable = true;
      recommendedProxySettings = true;
      recommendedTlsSettings = true;
      recommendedGzipSettings = true;
      recommendedOptimisation = true;

      virtualHosts.${cfg.domain} = {
        forceSSL = true;
        useACMEHost = cfg.domain;
        root = "${web}";

        locations = {
          # ACME HTTP-01 challenge из общего webroot (серт выписывается здесь).
          "/.well-known/acme-challenge/" = {
            root = acmeChallengeRoot;
          };
          # token API
          "/api/" = {
            proxyPass = "http://127.0.0.1:8080";
          };
          # LiveKit signaling (wss). Клиент ходит на /rtc И /rtc/v1 — prefix /rtc
          # покрывает оба. proxyWebsockets обязателен для upgrade.
          "/rtc" = {
            proxyPass = "http://127.0.0.1:7880";
            proxyWebsockets = true;
            extraConfig = ''
              proxy_read_timeout 86400s;
              proxy_send_timeout 86400s;
            '';
          };
          # RoomService / Twirp API (HTTP, без WS)
          "/twirp/" = {
            proxyPass = "http://127.0.0.1:7880";
          };
          # статика клиента (SPA fallback на index.html)
          "/" = {
            tryFiles = "$uri $uri/ /index.html";
          };
        };
      };
    };

    security.acme = {
      acceptTerms = true;
      defaults.email = cfg.acmeEmail;
    };

    # --- firewall ---
    networking.firewall = {
      allowedTCPPorts = [ 80 443 7881 ]
        ++ lib.optionals cfg.enableTurn [ 5349 ];
      # media UDP range открывает сам services.livekit (openFirewall=true);
      # TURN relay-range открываем здесь
      allowedUDPPortRanges = lib.optionals cfg.enableTurn [
        { from = 30000; to = 40000; }
      ];
    };
  };
}
