docker-compose:
	docker compose --profile full up --build -d
docker-run:
	docker compose logs -f api
	curl localhost:8081/healthz
	docker compose down
	