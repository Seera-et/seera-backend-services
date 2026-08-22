docker-compose:
	docker compose --profile full up --build -d
docker-run:
	docker compose logs -f api          # watch startup; it says which features are enabled
	curl localhost:8081/healthz         # {"status":"ok","db":"up"}
	docker compose down                 # stop (add -v to wipe the database volume)