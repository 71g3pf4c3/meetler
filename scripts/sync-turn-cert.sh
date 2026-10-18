#!/usr/bin/env bash
# Вытаскивает TLS-серт, который Caddy получил через ACME, в ./certs для встроенного
# TURN LiveKit. Caddy хранит серты в своём volume caddy_data.
#
# Запускать ПОСЛЕ первого старта Caddy (когда ACME-серт уже выписан) и по cron/таймеру
# для ротации (Caddy обновляет серт сам, TURN читает файл — нужен ресинк + reload).
set -euo pipefail
cd "$(dirname "$0")/.."

set -a; source .env; set +a
: "${TURN_DOMAIN:?}"

mkdir -p certs

# путь внутри caddy_data: /data/caddy/certificates/<ca>/<domain>/<domain>.{crt,key}
CRT_PATH="$(docker compose exec -T caddy sh -c \
  "find /data/caddy/certificates -name '${TURN_DOMAIN}.crt' | head -1")"
KEY_PATH="${CRT_PATH%.crt}.key"

if [[ -z "${CRT_PATH}" ]]; then
  echo "серт для ${TURN_DOMAIN} ещё не выписан Caddy — подожди ACME и повтори" >&2
  exit 1
fi

docker compose exec -T caddy cat "${CRT_PATH}" > certs/turn.crt
docker compose exec -T caddy cat "${KEY_PATH}" > certs/turn.key
chmod 600 certs/turn.key

echo "синхронизирован TURN cert → certs/turn.{crt,key}"
echo "перезапусти livekit для подхвата: docker compose restart livekit"
