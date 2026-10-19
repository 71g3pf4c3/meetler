.PHONY: keys render up down logs ps

keys:              ## сгенерировать API keypair в .env
	./scripts/gen-keys.sh

render:            ## отрендерить livekit.yaml из шаблона
	./scripts/render.sh

up: render         ## поднять стек (рендерит конфиг перед стартом)
	docker compose up -d --build

down:              ## остановить
	docker compose down

logs:              ## хвост логов
	docker compose logs -f --tail=100

ps:
	docker compose ps
