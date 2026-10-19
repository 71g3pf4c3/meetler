{ self }:
{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.services.meetler;
  packages = self.packages.${pkgs.stdenv.hostPlatform.system};
  livekit = config.services.livekit.settings;
  livekitURL = "http://127.0.0.1:${toString livekit.port}";
  backendURL = "http://127.0.0.1:${toString cfg.port}";
in
{
  options.services.meetler = {
    enable = lib.mkEnableOption "meetler video meetings";
    domain = lib.mkOption {
      type = lib.types.str;
      example = "meet.example.com";
      description = "Public hostname for HTTPS and TURN.";
    };
    port = lib.mkOption {
      type = lib.types.port;
      default = 8080;
      description = "Loopback port for the backend.";
    };
    sslCertificate = lib.mkOption {
      type = lib.types.path;
      description = "Full path to the certificate chain for nginx and TURN.";
    };
    sslCertificateKey = lib.mkOption {
      type = lib.types.path;
      description = "Full path to the private key, readable by the nginx group.";
    };
    livekitKeyFile = lib.mkOption {
      type = lib.types.path;
      description = "LiveKit key file in <key>: <secret> format.";
    };
    environmentFiles = lib.mkOption {
      type = lib.types.listOf lib.types.path;
      default = [ ];
      description = "Runtime environment files with LIVEKIT_API_KEY, LIVEKIT_API_SECRET and optional APP_PASSWORD. Shared with the Telegram bridge.";
    };
    stateDirectory = lib.mkOption {
      type = lib.types.str;
      default = "meetler";
      description = "Directory under /var/lib for rooms, sounds and avatars.";
    };
    openFirewall = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = "Open HTTP, HTTPS, ICE and TURN ports.";
    };
    telegram = {
      enable = lib.mkEnableOption "the meetler Telegram bridge";
      environmentFile = lib.mkOption {
        type = lib.types.path;
        description = "Runtime environment file containing TELEGRAM_BOT_TOKEN.";
      };
      port = lib.mkOption {
        type = lib.types.port;
        default = 8090;
        description = "Loopback HTTP port for sending room audio to Telegram.";
      };
      piperModel = lib.mkOption {
        type = lib.types.nullOr lib.types.path;
        default = null;
        description = "Piper ONNX model with an adjacent .onnx.json file. Null disables TTS.";
      };
    };
  };

  config = lib.mkIf cfg.enable {
    services.livekit = {
      enable = true;
      keyFile = cfg.livekitKeyFile;
      settings = {
        bind_addresses = lib.mkDefault [ "127.0.0.1" ];
        rtc.tcp_port = lib.mkDefault 7881;
        turn = {
          enabled = lib.mkDefault true;
          domain = lib.mkDefault cfg.domain;
          tls_port = lib.mkDefault 5349;
          cert_file = cfg.sslCertificate;
          key_file = cfg.sslCertificateKey;
          relay_range_start = lib.mkDefault 30000;
          relay_range_end = lib.mkDefault 40000;
        };
      };
    };
    systemd.services.livekit.serviceConfig.SupplementaryGroups = [ config.services.nginx.group ];

    systemd.services.meetler-api = {
      description = "meetler API";
      wantedBy = [ "multi-user.target" ];
      wants = [ "livekit.service" ];
      after = [ "livekit.service" ];
      environment = {
        LISTEN_ADDR = "127.0.0.1:${toString cfg.port}";
        LIVEKIT_URL = livekitURL;
        ROOMS_DIR = "/var/lib/${cfg.stateDirectory}/rooms";
      }
      // lib.optionalAttrs cfg.telegram.enable {
        TGBRIDGE_URL = "http://127.0.0.1:${toString cfg.telegram.port}";
      };
      serviceConfig = {
        ExecStart = lib.getExe packages.api;
        EnvironmentFile = cfg.environmentFiles;
        DynamicUser = true;
        StateDirectory = [
          cfg.stateDirectory
          "${cfg.stateDirectory}/rooms"
        ];
        StateDirectoryMode = "0700";
        PrivateTmp = true;
        Restart = "on-failure";
        RestartSec = "5s";
      };
    };

    systemd.services.meetler-tgbridge = lib.mkIf cfg.telegram.enable {
      description = "meetler Telegram bridge";
      wantedBy = [ "multi-user.target" ];
      wants = [
        "network-online.target"
        "meetler-api.service"
      ];
      after = [
        "network-online.target"
        "meetler-api.service"
      ];
      path = [
        pkgs.ffmpeg
        pkgs.yt-dlp
      ]
      ++ lib.optional (cfg.telegram.piperModel != null) pkgs.piper-tts;
      environment = {
        LIVEKIT_URL = livekitURL;
        SOUNDSVC_URL = backendURL;
        TGBRIDGE_HTTP_ADDR = "127.0.0.1:${toString cfg.telegram.port}";
        XDG_CACHE_HOME = "/var/cache/meetler-tgbridge";
      }
      // lib.optionalAttrs (cfg.telegram.piperModel != null) {
        PIPER_MODEL = toString cfg.telegram.piperModel;
        PIPER_BIN = lib.getExe pkgs.piper-tts;
      };
      serviceConfig = {
        ExecStart = lib.getExe packages.tgbridge;
        EnvironmentFile = cfg.environmentFiles ++ [ cfg.telegram.environmentFile ];
        DynamicUser = true;
        CacheDirectory = "meetler-tgbridge";
        PrivateTmp = true;
        Restart = "on-failure";
        RestartSec = "5s";
      };
    };

    services.nginx = {
      enable = true;
      recommendedProxySettings = true;
      recommendedTlsSettings = true;
      virtualHosts.${cfg.domain} = {
        forceSSL = true;
        inherit (cfg) sslCertificate sslCertificateKey;
        root = "${packages.web}";
        locations = {
          "/api/".proxyPass = backendURL;
          "/healthz".proxyPass = backendURL;
          "/rtc" = {
            proxyPass = livekitURL;
            proxyWebsockets = true;
            extraConfig = ''
              proxy_read_timeout 86400s;
              proxy_send_timeout 86400s;
            '';
          };
          "/twirp/".proxyPass = livekitURL;
          "/".tryFiles = "$uri $uri/ /index.html";
        };
      };
    };

    networking.firewall = lib.mkIf cfg.openFirewall {
      allowedTCPPorts = [
        80
        443
      ]
      ++ lib.optional (livekit.rtc.tcp_port != 0) livekit.rtc.tcp_port
      ++ lib.optional livekit.turn.enabled livekit.turn.tls_port;
      allowedUDPPortRanges = [
        {
          from = livekit.rtc.port_range_start;
          to = livekit.rtc.port_range_end;
        }
      ]
      ++ lib.optional livekit.turn.enabled {
        from = livekit.turn.relay_range_start;
        to = livekit.turn.relay_range_end;
      };
    };
  };
}
