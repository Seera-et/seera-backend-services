docker-compose:
	docker compose --profile full up --build -d
docker-run:
	docker compose logs -f api
	curl localhost:8081/healthz
	docker compose down

# The corpus pipeline. Each stage is separate and resumable on purpose; see
# README.md "Adding documents".
crawl:
	go run ./cmd/crawler run -limit $(or $(LIMIT),20) -data ./data

extract:
	cd workers/extraction && python extract.py ../../data/raw/*/original.pdf \
		--language $(or $(LANG),en) --store-root ../../data

ingest-dry:
	go run ./cmd/ingest -data ./data -dry-run

ingest:
	go run ./cmd/ingest -data ./data

test:
	go test ./...
	cd workers/extraction && python -m unittest discover -s .

test-race:
	go test -race ./...

# Needs a Postgres with pgvector: docker compose up -d
test-integration:
	go test -tags integration ./tests/integration/... -v
