#!/usr/bin/env bash
#
# Copy the local corpus from the docker Postgres into a hosted database.
#
#   bash scripts/db-seed.sh "postgres://...@aws-0-eu-central-1.pooler.supabase.com:5432/postgres?sslmode=require"
#   bash scripts/db-seed.sh --replace "$TARGET"   # wipe the target corpus first
#
# The target must already have the schema. Boot the API against it once
# (`go run ./cmd/api`) and let its own migrations build it — do NOT restore a
# schema dump. Migrations declare `vector(1536)` unqualified, so they work
# whether the pgvector extension lives in `public` (docker) or `extensions`
# (Supabase). A pg_dump schema writes `public.vector(1536)`, which fails on
# Supabase for no reason worth debugging.
#
# Data moves in foreign-key order, one table at a time, because pg_dump orders
# --data-only output alphabetically and `articles` would land before the
# `legal_documents` rows it references.
#
# Both pg_dump and psql run inside the local pgvector container, so no host
# Postgres client is needed and the client version always matches the server.

set -euo pipefail

CONTAINER="${SEERA_PG_CONTAINER:-seera-postgres}"
SOURCE_DB="${SEERA_PG_DB:-seera}"
SOURCE_USER="${SEERA_PG_USER:-seera}"

REPLACE=false
if [[ "${1:-}" == "--replace" ]]; then
	REPLACE=true
	shift
fi

TARGET="${1:-${TARGET_DATABASE_URL:-}}"
if [[ -z "$TARGET" ]]; then
	echo "usage: bash scripts/db-seed.sh [--replace] <target-database-url>" >&2
	echo "       (or set TARGET_DATABASE_URL)" >&2
	exit 2
fi

# Foreign-key order: parents before children. request_traces and
# schema_migrations are deliberately absent — traces are local debugging noise,
# and the target's migration ledger is written by the API that migrated it.
TABLES=(
	legal_documents
	document_versions
	articles
	chunks
	chunk_embeddings_gemini_1536
	chunk_embeddings_openai_1536
	business_rule_sets
	business_structures
	business_activities
	business_requirements
	business_rule_sources
)

# psql against the target. The URL goes in via the environment rather than the
# command line: it carries the database password, and `docker inspect` and the
# process table both show arguments.
target_psql() {
	docker exec -e PGURL="$TARGET" "$CONTAINER" \
		sh -c 'psql "$PGURL" -v ON_ERROR_STOP=1 "$@"' sh "$@"
}

source_psql() {
	docker exec "$CONTAINER" psql -U "$SOURCE_USER" -d "$SOURCE_DB" -v ON_ERROR_STOP=1 "$@"
}

if ! docker inspect "$CONTAINER" >/dev/null 2>&1; then
	echo "error: container '$CONTAINER' not found. Start it with: docker compose up -d" >&2
	exit 1
fi

echo "==> connecting to target"
target_psql -tAc "select 'server ' || current_setting('server_version')"

# The schema has to exist already; a missing table here means the API was never
# pointed at this database.
missing=$(target_psql -tAc "
	select string_agg(t, ', ')
	from unnest(array[$(printf "'%s'," "${TABLES[@]}" | sed 's/,$//')]) as t
	where to_regclass('public.' || t) is null")
if [[ -n "${missing// /}" ]]; then
	echo "error: target is missing tables: $missing" >&2
	echo "       Point the API at it once so its migrations run:" >&2
	echo "         DATABASE_URL='<target>' go run ./cmd/api" >&2
	exit 1
fi

existing=$(target_psql -tAc "select count(*) from legal_documents")
existing="${existing// /}"
if [[ "$existing" != "0" ]]; then
	if [[ "$REPLACE" != true ]]; then
		echo "error: target already holds $existing legal_documents rows." >&2
		echo "       Re-run with --replace to truncate the corpus and reload it." >&2
		exit 1
	fi
	echo "==> truncating target corpus ($existing documents)"
	# CASCADE reaches the dependent tables; business_rule_sources references
	# legal_documents with ON DELETE RESTRICT, so it must go in the same
	# statement rather than one table at a time.
	target_psql -c "TRUNCATE TABLE $(
		IFS=,
		echo "${TABLES[*]}"
	) CASCADE"
fi

DUMP_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/tmp/deploy"
mkdir -p "$DUMP_DIR"
DUMP="$DUMP_DIR/corpus-data.sql"
: >"$DUMP"

echo "==> dumping ${#TABLES[@]} tables from $CONTAINER/$SOURCE_DB"
for t in "${TABLES[@]}"; do
	docker exec -i "$CONTAINER" pg_dump -U "$SOURCE_USER" -d "$SOURCE_DB" \
		--data-only --no-owner --no-acl --table="public.$t" >>"$DUMP"
done
echo "    $(wc -c <"$DUMP" | tr -d ' ') bytes"

# One transaction: a load that fails halfway leaves the target empty rather
# than half a corpus that retrieval would happily serve.
echo "==> loading into target"
# -o /dev/null discards result sets — the dump's set_config() preamble returns
# one row per call and would otherwise bury the report below. Errors go to
# stderr and are unaffected.
docker exec -i -e PGURL="$TARGET" "$CONTAINER" \
	sh -c 'psql "$PGURL" -v ON_ERROR_STOP=1 --single-transaction -q -o /dev/null -f -' <"$DUMP"

echo "==> verifying"
counts_sql="select 'x' t, 0 n where false"
for t in "${TABLES[@]}"; do
	counts_sql+=" union all select '$t', count(*) from $t"
done
counts_sql+=" order by 1"

src=$(source_psql -tAF, -c "$counts_sql")
dst=$(target_psql -tAF, -c "$counts_sql")

if [[ "$src" == "$dst" ]]; then
	echo "$dst" | awk -F, '{ printf "    %-30s %s\n", $1, $2 }'
	echo "==> row counts match"
else
	echo "error: row counts differ" >&2
	diff <(echo "$src") <(echo "$dst") >&2 || true
	exit 1
fi
