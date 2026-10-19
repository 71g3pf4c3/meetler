FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /tgbridge .

FROM debian:bookworm-slim

# ffmpeg (транскод) + piper (TTS) + yt-dlp (YouTube в палату).
# yt-dlp тянем бинарём с GitHub (apt-версия устаревает и ломается об YouTube).
RUN apt-get update && apt-get install -y --no-install-recommends \
      ffmpeg ca-certificates wget xz-utils python3 \
 && rm -rf /var/lib/apt/lists/*

# yt-dlp: статический бинарь (обновляемый самим yt-dlp при необходимости).
RUN wget -qO /usr/local/bin/yt-dlp \
      "https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp_linux" \
 && chmod +x /usr/local/bin/yt-dlp

# piper (amd64). Если не нужен TTS — переменные PIPER_* не задавай, бинарь просто лежит.
ARG PIPER_VERSION=2023.11.14-2
RUN set -eux; \
    arch="$(dpkg --print-architecture)"; \
    case "$arch" in \
      amd64) pf=amd64 ;; \
      arm64) pf=arm64 ;; \
      *) echo "unsupported arch $arch"; exit 1 ;; \
    esac; \
    wget -qO /tmp/piper.tar.gz \
      "https://github.com/rhasspy/piper/releases/download/${PIPER_VERSION}/piper_linux_x86_64.tar.gz"; \
    mkdir -p /opt && tar -xzf /tmp/piper.tar.gz -C /opt; \
    ln -sf /opt/piper/piper /usr/local/bin/piper; \
    rm -f /tmp/piper.tar.gz

COPY --from=build /tgbridge /usr/local/bin/tgbridge

ENV PIPER_BIN=/usr/local/bin/piper
# модель: смонтируй .onnx и задай PIPER_MODEL=/models/ru.onnx (иначе TTS выключен)
ENTRYPOINT ["/usr/local/bin/tgbridge"]
