# Deploying Seera

What runs where:

| Piece | Host | Why |
| --- | --- | --- |
| `seera-web` (React/Vite) | Vercel | Static bundle, free, already the plan. |
| `seera-backend-services` (Go) | Google Cloud Run | Runs the existing `Dockerfile` unchanged, streams SSE natively, scales to zero. |
| Postgres + pgvector | Supabase | Managed, free, has `vector`, `pg_trgm` and `unaccent`. |

The code needs no changes to run hosted. The frontend reads its API origin from
`VITE_API_BASE_URL` (`src/lib/api/config.ts`) and the API reads everything from
the environment (`internal/config`). Deploying is configuration and one data
move.

---

## 1. Supabase

Create a project. Pick the region closest to your users — the API's latency to
Postgres is paid on every retrieval, several times per answer.

### Use the session pooler, not the direct connection

Supabase gives you three connection strings. The choice matters:

| String | Port | Use it? |
| --- | --- | --- |
| Direct (`db.<ref>.supabase.co`) | 5432 | **No.** IPv6-only on the free tier, and Cloud Run has no IPv6 egress. It will simply never connect. |
| Session pooler (`...pooler.supabase.com`) | 5432 | **Yes.** IPv4, and session mode supports prepared statements, which pgx uses by default. |
| Transaction pooler | 6543 | No. Transaction mode breaks pgx's statement cache unless you force simple protocol. |

So the URL you want looks like:

```
postgres://postgres.<project-ref>:<password>@aws-0-<region>.pooler.supabase.com:5432/postgres?sslmode=require
```

`sslmode=require` is not optional — Supabase refuses plaintext.

### Let the API build the schema

Do **not** restore a `pg_dump` schema into Supabase. A schema dump writes
`public.vector(1536)`, and Supabase installs pgvector into its `extensions`
schema, so the restore fails on the first embedding table. The migrations in
`internal/database/migrations` declare `vector(1536)` unqualified and resolve
through `search_path`, so they work either way.

Point the API at Supabase and let it migrate on boot:

```bash
# in seera-backend-services/.env
DATABASE_URL=postgres://postgres.<ref>:<password>@aws-0-<region>.pooler.supabase.com:5432/postgres?sslmode=require

go run ./cmd/api
```

Look for `database connected` followed by the migration log, then stop it.

### Move the corpus

878 articles and 879 Gemini embeddings already exist locally. Re-ingesting them
would cost 879 embedding calls — most of a free-tier day — and produce the same
vectors. Copy them instead:

```bash
docker compose up -d                 # the local database must be running
bash scripts/db-seed.sh "postgres://postgres.<ref>:...@...pooler.supabase.com:5432/postgres?sslmode=require"
```

The script dumps the eleven corpus tables in foreign-key order, loads them in a
single transaction, and then compares row counts on both sides and fails if they
disagree. It refuses to run against a target that already holds documents unless
you pass `--replace`.

### Row level security

Supabase exposes every table in `public` over PostgREST to two roles, `anon` and
`authenticated`, and the key that reaches them is the publishable key — which is
compiled into the browser bundle on purpose. It also ships a default-privilege
rule granting those roles full DML on every table `postgres` creates.

Taken together that meant, until migration `0010_row_level_security`, anyone who
opened devtools could `PATCH /rest/v1/articles` and rewrite the text of a law,
after which Seera would cite the altered version with a citation and full
confidence. Migration 0010 enables RLS on every table, revokes the grants, and
revokes the default-privilege rule so the next migration does not reopen it.

Two details worth knowing before changing any of it:

- **`TRUNCATE` ignores RLS.** A table with RLS on and every policy denying is
  still emptied by a role holding the TRUNCATE grant. Enabling RLS is not
  sufficient on its own; the REVOKE is what closes that.
- **The API is unaffected, by design.** It connects as the table owner, and an
  owner bypasses RLS unless `FORCE ROW LEVEL SECURITY` is set. It is not set,
  deliberately — access control lives in `requireUser`, and forcing RLS would
  lock out the only caller meant to see everything.

Check the posture after any migration that adds a table:

```bash
bash scripts/db-verify-rls.sh          # reads DATABASE_URL from .env
```

It fails the build if a table has RLS off, if `anon`/`authenticated` hold any
privilege, or if `anon` can still read a row — that last one by actually
assuming the role rather than reading the catalogue.

---

## 2. Cloud Run

Secrets first — they should not sit in `--set-env-vars`, which is visible in the
console and in `gcloud run services describe`:

```bash
gcloud secrets create seera-database-url --data-file=- <<<'postgres://postgres....'
gcloud secrets create seera-gemini-key   --data-file=- <<<'...'
gcloud secrets create seera-admin-token  --data-file=- <<<"$(openssl rand -hex 32)"
```

Then deploy. `--source .` picks up the existing `Dockerfile`:

```bash
gcloud run deploy seera-api \
  --source . \
  --region europe-west1 \
  --allow-unauthenticated \
  --port 8080 \
  --cpu 1 --memory 512Mi \
  --min-instances 0 \
  --max-instances 4 \
  --timeout 300 \
  --set-env-vars 'ENV=prod,LOG_LEVEL=info,CORS_ALLOWED_ORIGINS=https://seera.site,https://www.seera.site' \
  --set-secrets 'DATABASE_URL=seera-database-url:latest,GEMINI_API_KEY=seera-gemini-key:latest,ADMIN_API_TOKEN=seera-admin-token:latest'
```

Three flags are load-bearing:

- **`--max-instances 4`** — `database.Connect` hardcodes `MaxConns = 10`
  (`internal/database/postgres.go`), so instances × 10 is your connection
  ceiling. At the default of 100 instances a traffic spike would open a thousand
  connections and Supabase would start refusing them. Four keeps the worst case
  at forty.
- **`--timeout 300`** — an SSE answer stream runs for tens of seconds. The
  60-second default would cut long answers off mid-sentence.
- **`--port 8080`** — matches `EXPOSE 8080`. Do not pass `PORT` in
  `--set-env-vars`; Cloud Run injects it itself and rejects the override.

Leave `COHERE_API_KEY` unset unless you have one — the API falls back to the LLM
reranker and logs that it did.

### Watch item: idle connections

Cloud Run throttles CPU between requests, which also pauses pgx's pool health
checker (`HealthCheckPeriod`, one minute). If an instance sits idle long enough
for Supavisor to drop its connections, the next request can fail on a stale one.
If you see sporadic database errors on the first request after a quiet period,
redeploy with `--no-cpu-throttling` — it keeps the pool's pruner running, at the
cost of billing the instance for idle time.

---

## 3. Vercel

Import `seera-web`. `vercel.json` already pins the framework, the output
directory, and the SPA rewrite that keeps deep links like `/explorer/<id>` from
404ing.

Set one environment variable, for Production:

```
VITE_API_BASE_URL = https://seera-api-<hash>-ew.a.run.app
```

(or `https://api.seera.site` once section 4 is done — changing it needs a
redeploy, because Vite inlines `VITE_*` at build time.)

**Do not set `VITE_ADMIN_API_TOKEN` on Vercel.** Anything in a `VITE_` variable
is compiled into the bundle and served to every visitor. It exists for local use
of the ingest screen only; the production ingest token belongs in Cloud Run's
secret, where the browser never sees it.

---

## 4. DNS on Namecheap

In **Domain List → Manage → Advanced DNS**. Use the exact record values Vercel
shows you under the domain's settings — they vary by account — but they are
normally:

| Type | Host | Value |
| --- | --- | --- |
| A | `@` | `76.76.21.21` |
| CNAME | `www` | `cname.vercel-dns.com.` |

Delete Namecheap's default parking records (the `@` and `www` URL-redirect
entries) or they will fight the above.

For `api.seera.site`, a Cloud Run domain mapping needs the domain verified in
Google Search Console and is not offered in every region. It is worth doing, but
it is not on the critical path: the `*.run.app` URL works, CORS is what actually
authorises the frontend, and you can move to the subdomain later by remapping and
rebuilding the frontend with the new `VITE_API_BASE_URL`.

---

## 5. Check it

```bash
curl https://<api-host>/healthz
curl 'https://<api-host>/api/v1/documents'
```

Then from the browser on `https://seera.site`: ask a question and confirm the
answer streams token by token rather than arriving in one block. A single block
means something between you and Cloud Run is buffering the SSE response.

Check the Cloud Run logs for the boot lines — `Q&A enabled`, `ingestion
enabled`, `database connected`. Any feature whose credentials are missing logs a
warning there and leaves its route unregistered, so a 404 on `/api/v1/qa/query`
is a configuration problem, not a routing one.
