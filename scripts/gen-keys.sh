#!/usr/bin/env bash
# Генерирует LiveKit API keypair и записывает в .env (idempotent: не перетирает,
# если ключи уже заданы). Запускать один раз при первичной настройке.
set -euo pipefail
cd "$(dirname "$0")/.."

if [[ ! -f .env ]]; then
  cp .env.example .env
  echo "created .env из .env.example — заполни SITE_DOMAIN/TURN_DOMAIN"
fi

# shellcheck disable=SC1091
source .env

if [[ -n "${LIVEKIT_API_KEY:-}" && -n "${LIVEKIT_API_SECRET:-}" ]]; then
  echo "ключи уже заданы в .env, пропускаю генерацию"
  exit 0
fi

# Генерация без зависимости от openssl — читаем /dev/urandom.
# KEY: 12 hex. SECRET: 48 буквенно-цифровых (LiveKit требует >= 32 символов).
rand_hex() { head -c "$1" /dev/urandom | od -An -tx1 | tr -d ' \n'; }
rand_alnum() { head -c 64 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c "$1"; }

KEY="API$(rand_hex 6)"
SECRET="$(rand_alnum 48)"

if [[ ${#SECRET} -lt 32 ]]; then
  echo "secret generation failed (<32 chars)" >&2
  exit 1
fi

# in-place обновление .env
sed -i.bak "s|^LIVEKIT_API_KEY=.*|LIVEKIT_API_KEY=${KEY}|" .env
sed -i.bak "s|^LIVEKIT_API_SECRET=.*|LIVEKIT_API_SECRET=${SECRET}|" .env
rm -f .env.bak

echo "сгенерированы ключи:"
echo "  LIVEKIT_API_KEY=${KEY}"
echo "  LIVEKIT_API_SECRET=<48 chars>"
