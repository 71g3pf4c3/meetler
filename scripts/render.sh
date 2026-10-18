#!/usr/bin/env bash
# Рендерит livekit/livekit.yaml из шаблона, подставляя значения из .env.
# LiveKit НЕ раскрывает ${VAR} в YAML сам, поэтому подстановку делаем здесь.
# Используем sed (без зависимости от gettext/envsubst), только наши 4 плейсхолдера.
set -euo pipefail
cd "$(dirname "$0")/.."

if [[ ! -f .env ]]; then
  echo "нет .env — сначала ./scripts/gen-keys.sh" >&2
  exit 1
fi

set -a
# shellcheck disable=SC1091
source .env
set +a

: "${LIVEKIT_API_KEY:?не задан в .env}"
: "${LIVEKIT_API_SECRET:?не задан в .env}"
: "${TURN_DOMAIN:?не задан в .env}"
# TURN выключен, пока нет ACME-серта. После ./scripts/sync-turn-cert.sh
# выстави TURN_ENABLED=true в .env и перезапусти.
TURN_ENABLED="${TURN_ENABLED:-false}"

# экранируем значения для sed (на случай спецсимволов в secret)
esc() { printf '%s' "$1" | sed -e 's/[&|\\]/\\&/g'; }

sed \
  -e "s|\${LIVEKIT_API_KEY}|$(esc "$LIVEKIT_API_KEY")|g" \
  -e "s|\${LIVEKIT_API_SECRET}|$(esc "$LIVEKIT_API_SECRET")|g" \
  -e "s|\${TURN_DOMAIN}|$(esc "$TURN_DOMAIN")|g" \
  -e "s|\${TURN_ENABLED}|$(esc "$TURN_ENABLED")|g" \
  livekit/livekit.yaml.template > livekit/livekit.yaml

echo "rendered livekit/livekit.yaml (turn.enabled=${TURN_ENABLED})"
