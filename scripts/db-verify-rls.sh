#!/usr/bin/env bash
#
# Check that nothing in `public` is reachable from the browser-facing Supabase
# roles.
#
#   bash scripts/db-verify-rls.sh "$DATABASE_URL"
#   bash scripts/db-verify-rls.sh                 # reads DATABASE_URL from .env
#
# Run this after every migration that creates a table. Supabase grants `anon`
# and `authenticated` full DML on new tables in `public` by default, and
# 0010_row_level_security revoked that default — but a grant can come back by
# hand, from the dashboard, or from a restored dump, and the symptom is silent.
#
# Exits non-zero on any finding, so it can gate a deploy.

set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

TARGET="${1:-${DATABASE_URL:-}}"
if [[ -z "$TARGET" && -f .env ]]; then
	TARGET=$(grep -E '^DATABASE_URL=' .env | cut -d= -f2- || true)
fi
if [[ -z "$TARGET" ]]; then
	echo "usage: bash scripts/db-verify-rls.sh <database-url>" >&2
	exit 2
fi

PSQL=${PSQL:-psql}
if ! command -v "$PSQL" >/dev/null 2>&1; then
	for candidate in "/c/Program Files/PostgreSQL"/*/bin/psql; do
		[[ -x "$candidate" ]] && PSQL="$candidate" && break
	done
fi
if ! command -v "$PSQL" >/dev/null 2>&1 && [[ ! -x "$PSQL" ]]; then
	echo "error: no psql found. Set PSQL=/path/to/psql." >&2
	exit 2
fi

run() { PGURL="$TARGET" "$PSQL" "$TARGET" -X -tA -v ON_ERROR_STOP=1 -c "$1"; }

status=0

echo "==> tables in public without row level security"
missing=$(run "
	SELECT string_agg(c.relname, ', ' ORDER BY c.relname)
	FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
	WHERE n.nspname = 'public' AND c.relkind = 'r' AND NOT c.relrowsecurity")
if [[ -n "${missing// /}" ]]; then
	echo "    FAIL: $missing" >&2
	status=1
else
	echo "    ok — every table has RLS enabled"
fi

echo "==> privileges still held by anon / authenticated"
grants=$(run "
	SELECT string_agg(DISTINCT table_name || ' (' || grantee || ')', ', ')
	FROM information_schema.role_table_grants
	WHERE table_schema = 'public' AND grantee IN ('anon', 'authenticated')")
if [[ -n "${grants// /}" ]]; then
	echo "    FAIL: $grants" >&2
	status=1
else
	echo "    ok — no table privileges"
fi

# Scoped to the role running migrations, because that is the role that will
# create the next table. Supabase keeps a second rule under `supabase_admin`
# which we cannot revoke and which does not apply to anything we create; the
# per-table grant check above is the exhaustive backstop either way.
echo "==> default privileges that would grant tables created by the migrator"
defaults=$(run "
	SELECT string_agg(DISTINCT a.grantee::regrole::text, ', ')
	FROM pg_default_acl d
	JOIN pg_namespace n ON n.oid = d.defaclnamespace
	CROSS JOIN LATERAL aclexplode(d.defaclacl) a
	WHERE n.nspname = 'public'
	  AND d.defaclobjtype = 'r'
	  AND d.defaclrole = current_user::regrole
	  AND a.grantee::regrole::text IN ('anon', 'authenticated')")
if [[ -n "${defaults// /}" ]]; then
	echo "    FAIL: new tables would be granted to: $defaults" >&2
	status=1
else
	echo "    ok — new tables inherit nothing"
fi

# The proof rather than the paperwork: become the role the publishable key maps
# to and try to read the corpus. Rolled back regardless.
echo "==> what anon can actually read"
if run "SELECT 1 FROM pg_roles WHERE rolname='anon'" | grep -q 1; then
	# grep '=' keeps the result row and drops psql's BEGIN/SET/ROLLBACK tags.
	leaked=$(run "
		BEGIN;
		SET LOCAL ROLE anon;
		SELECT string_agg(t || '=' || n::text, ' ') FROM (
			SELECT 'articles' t, count(*) n FROM articles
			UNION ALL SELECT 'app_users', count(*) FROM app_users
			UNION ALL SELECT 'request_traces', count(*) FROM request_traces
		) s WHERE n > 0;
		ROLLBACK;" 2>/dev/null | grep '=' || true)

	if [[ -n "$leaked" ]]; then
		echo "    FAIL: anon can still read rows: $leaked" >&2
		status=1
	else
		echo "    ok — anon reads nothing"
	fi
else
	echo "    skipped — no anon role (not Supabase)"
fi

if [[ $status -eq 0 ]]; then
	echo "==> all checks passed"
else
	echo "==> FAILED — see above" >&2
fi
exit $status
