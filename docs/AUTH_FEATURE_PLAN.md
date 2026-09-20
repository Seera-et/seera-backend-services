# Authentication — Feature Plan

## Context

Seera is about to launch publicly on seera.site with Q&A, Explorer and Advisor
built. Three things force auth to exist now rather than later:

1. `POST /api/v1/qa/query` costs a Gemini call per request and is open to
   anyone who reads the network tab. The only thing standing between the corpus
   and a scripted loop is a per-IP token bucket.
2. The frontend renders `PLACEHOLDER_USER` — a fictional "Yoseph R." — in the
   header (`src/app/session.ts`). Shipping that to real visitors is a lie in the
   UI.
3. Conversations and bookmarks live in `localStorage`
   (`src/lib/store/createLocalStore.ts`), so they are per-browser. A signed-in
   product that forgets everything when you open your laptop is not credible.

Sign-in is **Google only**. No email/password, no magic links — one provider,
one button, no password reset flow to build or secrets to store.

## Decisions

**Supabase Auth, not a hand-rolled OAuth flow.** The database is already
Supabase (see `DEPLOY.md`), so this adds no infrastructure. Supabase owns the
Google client secret, the PKCE exchange, refresh rotation and the user table.
Writing that in Go would be a few hundred lines of security-critical code
replacing a dashboard toggle.

**The API verifies the JWT itself; it does not call Supabase per request.** The
project signs tokens with ES256 and publishes the public key:

```
https://mypyfucrnyzigeibpgzp.supabase.co/auth/v1/.well-known/jwks.json
→ {"keys":[{"alg":"ES256","kty":"EC","kid":"d76cc4ec-…", …}]}
```

Asymmetric signing means the API needs no shared secret — only the public key,
fetched once and cached. A network round-trip to Supabase on every request would
put a third party in the latency path of every answer.

**No foreign key to `auth.users`.** Supabase's auth schema lives in the same
database, so an FK would compile — and would break every local `docker compose`
run, where no `auth` schema exists, and would couple our migrations to a
vendor's table. Instead `public.app_users` holds our own row keyed by the
Supabase user id (the JWT `sub`), upserted on first authenticated request.
Migrations stay portable and `internal/database/migrations` stays the single
source of schema truth.

**Guard on both sides.** The frontend guard is UX; the Go middleware is the
actual control. Per the architecture rules, access control is deterministic Go
code, not something the client is trusted to enforce.

## What is public

| Route | Access | Why |
|---|---|---|
| `/` | public | Landing page. The Ask box is visible but gated on submit. |
| `/explorer`, `/documents/:id` | public | Browsing published law. Read-only, no LLM cost, and the shop window for the corpus. |
| `/about` | public | A login wall in front of an About page reads as broken. |
| `/chat` | **auth** | Costs a Gemini call per question. |
| `/advisor` | **auth** | Per-user intake and saved recommendations. |
| `/bookmarks`, `/history` | **auth** | Per-user by definition. |

API mirror: `/documents/*`, `/corpus/stats`, `/citations/{id}` stay public;
`/qa/query`, `/business/*`, and everything new under `/me`, `/conversations`,
`/bookmarks` require a verified user.

---

## Phase 1 — Auth foundation — **built**

Launchable on its own: real sign-in, real guard, real enforcement. Everything in
this phase is implemented and tested; the only outstanding items are the two
dashboard settings in "Before this works" at the end of this document.

### Backend — `internal/auth/` (new package)

Follows the existing vertical feature-package shape (`internal/qa`,
`internal/business`: handler / service / repository in one package), not the
`internal/api|service|repository` split in the architecture doc, which the
codebase does not actually use.

| File | Responsibility |
|---|---|
| `jwks.go` | Fetches and caches the JWKS; refreshes on unknown `kid` (key rotation), with a floor between refetches so an invalid token cannot trigger a fetch storm. |
| `token.go` | Parses and validates: ES256 only, `iss` = the project's auth URL, `aud` = `authenticated`, `exp`/`nbf`. Returns a `Claims` struct. |
| `context.go` | `WithUser`/`UserFrom` — the user on the request context. |
| `middleware.go` | `RequireUser` (401 on missing/invalid) and `OptionalUser` (attaches if present, never rejects). |
| `repository.go` | `Upsert` into `app_users` from claims. |
| `handler.go` | `GET /api/v1/me`. |

Dependencies: **one**, `github.com/golang-jwt/jwt/v5`, for parsing and signature
verification — the part nobody should hand-roll. The JWKS cache is written here
instead (`jwks.go`, ~180 lines with comments): converting an EC or RSA JWK to a
public key is small and explicit, and a second dependency for a map keyed by
`kid` did not earn its place against this module's very short go.mod.

Config (`internal/config`): `SUPABASE_PROJECT_URL` and `SUPABASE_JWT_AUDIENCE`
(default `authenticated`). Following the existing pattern in `buildHandlers`, a
missing project URL leaves the protected routes unregistered and logs a warning
rather than serving routes that 500 on every call.

### Backend — changes to existing files

- **`internal/server/middleware.go`, `cors`** — add `Authorization` to
  `Access-Control-Allow-Headers`. It is currently `Content-Type, Accept`.
  **This will not show up in local development**, where the Vite proxy makes
  everything same-origin and no preflight happens. In production `seera.site` →
  `api.seera.site` is cross-origin and every authenticated request fails
  preflight. This single line is the most likely thing to break the launch.
- **`internal/server/middleware.go`, `ipRateLimiter`** — key on user id when
  a user is on the context, IP otherwise. A signed-in user behind a shared NAT
  should not be limited by their neighbours, and an authenticated abuser should
  not escape by rotating addresses.
- **`internal/server/routes.go`** — a third group alongside the public and admin
  groups, wrapped in `auth.RequireUser`.

### Frontend — `src/lib/auth/` (new)

- `client.ts` — the `@supabase/supabase-js` client, built from
  `VITE_SUPABASE_URL` and `VITE_SUPABASE_PUBLISHABLE_KEY` (both already in
  `.env`; the publishable key is designed to be public).
- `session.ts` — `AuthProvider` + `useSession()`, wrapping
  `supabase.auth.onAuthStateChange`. Exposes `status: 'loading' | 'signed-in' |
  'signed-out'`; `loading` is a distinct state so a page refresh does not flash
  the sign-in screen while the session is being restored.
- `token.ts` — a registered async token getter. `lib/api` imports *this*, never
  `supabase-js`, so the API layer stays provider-agnostic and the existing
  tests keep working without a Supabase mock.

### Frontend — `src/features/auth/` (new)

- `SignInPage.tsx` — one Google button, the disclaimer, and an explanation of
  *why* sign-in is being asked for, carried from the `next` param.
- `AuthCallbackPage.tsx` — handles the OAuth return, then redirects to `next`.
- `RequireAuth.tsx` — a layout route: renders `<Outlet/>` when signed in, a
  spinner while `loading`, and `<Navigate to="/signin?next=…" replace/>` when
  signed out.

### Frontend — changes to existing files

- **`src/app/routes.tsx`** — add `/signin` and `/auth/callback`; move `/chat`,
  `/advisor`, `/bookmarks`, `/history` under a `RequireAuth` layout route.
- **`src/features/home/AskBox.tsx`** — the flow you asked for. `submit()`
  currently navigates straight to `chatPath(...)`. When signed out it instead
  navigates to `/signin?next=<encoded chatPath>`, so the question the visitor
  typed survives Google's redirect and the chat page answers it on arrival.
- **`src/app/session.ts`** — delete `PLACEHOLDER_USER` and `AUTH_ENABLED`.
- **`src/app/layout/UserMenu.tsx`** — real name/email/avatar from the session;
  working Sign out; a Sign in button when signed out. The menu already has the
  right items, they are just inert.
- **`src/app/navigation.ts`** — mark which items need auth so signed-out
  visitors are not shown Bookmarks and History.
- **`src/lib/api/http.ts`, `send()`** — attach `Authorization: Bearer <token>`.
  One change covers JSON *and* SSE, because `requestStream` already goes through
  `send`. (This is why SSE auth is a non-issue here: the client is `fetch` +
  `ReadableStream`, not `EventSource`, which cannot set headers.)
- **`src/lib/api/errors.ts`** — a 401 must be distinguishable so the UI can send
  the user back to sign-in instead of showing "something went wrong".

### Migration `0009_app_users.up.sql`

```sql
CREATE TABLE app_users (
    id            uuid PRIMARY KEY,           -- Supabase auth user id (JWT sub)
    email         text NOT NULL,
    display_name  text,
    avatar_url    text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_seen_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX app_users_email_idx ON app_users (email);
```

---

## Before this works

Two things only you can do, both in the Supabase dashboard:

1. **Enable the Google provider** (Authentication → Providers → Google) with a
   Google Cloud OAuth client. Supabase shows the callback URL to paste into
   Google.
2. **Register the redirect URLs** (Authentication → URL Configuration):
   `http://localhost:5173/auth/callback`, `https://seera.site/auth/callback`,
   `https://www.seera.site/auth/callback`. A URL that is not on this list comes
   back as `error=redirect_uri_mismatch`, which `AuthCallbackPage` surfaces
   rather than hanging.

And one value to copy: `VITE_SUPABASE_PUBLISHABLE_KEY` in `seera-web/.env`
(Project Settings → API). It is currently empty, so `authConfigured` is false
and the sign-in screen says so instead of offering a button that cannot work.

## Phase 2 — User data in Postgres

Replaces the two `localStorage` stores. `createLocalStore.ts` was written for
exactly this moment — its own doc comment says "when those endpoints exist,
these stores are what gets replaced, the components reading them do not change."
Keep the exported function signatures (`useConversations`, `appendTurn`,
`toggleSourceBookmark`, …) and swap the bodies to react-query, so `ChatPage`,
`BookmarksPage` and `HistoryPage` need minimal edits.

### Migration `0010_user_content.up.sql`

Mirrors the existing client types — `Conversation`/`StoredTurn` in
`features/qa/conversations.ts` and `Bookmark` in `features/bookmarks/store.ts`.

```sql
CREATE TABLE conversations (
    id         uuid PRIMARY KEY,
    user_id    uuid NOT NULL REFERENCES app_users (id) ON DELETE CASCADE,
    title      text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE conversation_turns (
    id              uuid PRIMARY KEY,
    conversation_id uuid NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    position        integer NOT NULL,
    question        text NOT NULL,
    answer          text NOT NULL,
    -- Citations are denormalised: a turn must render exactly the sources it was
    -- answered from, even after the corpus is re-ingested and chunk ids change.
    citations       jsonb NOT NULL DEFAULT '[]',
    kind            text,
    language        text,
    abstained       boolean NOT NULL DEFAULT false,
    grounded        boolean NOT NULL DEFAULT false,
    confidence      text,
    truncated       boolean NOT NULL DEFAULT false,
    model           text,
    reranker        text,
    asked_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (conversation_id, position)
);

CREATE TABLE bookmarks (
    id         uuid PRIMARY KEY,
    user_id    uuid NOT NULL REFERENCES app_users (id) ON DELETE CASCADE,
    kind       text NOT NULL CHECK (kind IN ('citation', 'answer')),
    -- For a citation bookmark this is the chunk id, which is what makes saving
    -- the same source twice idempotent, as the client store does today.
    chunk_id   uuid REFERENCES chunks (id) ON DELETE SET NULL,
    payload    jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX bookmarks_user_chunk_idx
    ON bookmarks (user_id, chunk_id) WHERE chunk_id IS NOT NULL;
```

`request_traces` also gains a nullable `user_id` so a trace can be tied to the
account that produced it.

The same migration must enable RLS on all three new tables, following
`0010_row_level_security`:

```sql
ALTER TABLE conversations       ENABLE ROW LEVEL SECURITY;
ALTER TABLE conversation_turns  ENABLE ROW LEVEL SECURITY;
ALTER TABLE bookmarks           ENABLE ROW LEVEL SECURITY;
```

No permissive policy is needed while the browser only ever talks to the Go API —
the API connects as the table owner and bypasses RLS, and RLS with no policy
denies everyone else. Write owner-scoped policies only at the point supabase-js
is used directly, and write them per operation rather than with `FOR ALL`:

```sql
CREATE POLICY conversations_own ON conversations
    FOR SELECT TO authenticated USING (user_id = (SELECT auth.uid()));

-- A turn is owned through its conversation. EXISTS rather than a join, so the
-- policy stays a filter the planner can push down.
CREATE POLICY conversation_turns_own ON conversation_turns
    FOR SELECT TO authenticated USING (EXISTS (
        SELECT 1 FROM conversations c
        WHERE c.id = conversation_turns.conversation_id
          AND c.user_id = (SELECT auth.uid())));
```

Note `(SELECT auth.uid())` rather than a bare call: wrapped, the planner
evaluates it once as an initplan instead of per row. On a table holding every
turn of every conversation that is the difference between an index scan and a
sequential one. Run `bash scripts/db-verify-rls.sh` after the migration.

### Endpoints

```
GET    /api/v1/me
GET    /api/v1/conversations
POST   /api/v1/conversations            create or append a turn (idempotent on turn id)
GET    /api/v1/conversations/{id}
DELETE /api/v1/conversations/{id}
GET    /api/v1/bookmarks
POST   /api/v1/bookmarks
DELETE /api/v1/bookmarks/{id}
POST   /api/v1/me/import                one-time localStorage adoption
```

`/me/import` exists so a visitor who has been using Seera anonymously does not
lose their threads the moment they sign in: on first successful sign-in the
client posts whatever is in `localStorage`, the server takes what the account
does not already have, and the client clears it.

---

## Things that will bite

- **CORS `Authorization` header** — invisible locally, fatal in production. See
  Phase 1.
- **Supabase redirect allow-list** — `https://seera.site/auth/callback`,
  `https://www.seera.site/auth/callback` and `http://localhost:5173/auth/callback`
  must all be registered in the Supabase dashboard, or Google returns to a
  blocked URL.
- **`VITE_ADMIN_API_TOKEN` must stay empty in Vercel.** Unrelated to user auth,
  but it is in the same file and ships to the browser if set.
- **Session-restore flash** — guard on a tri-state, never on a boolean.
- **The rate limiter keys on IP today** and its comment says auth is out of
  scope; update the comment with the code so it does not mislead the next reader.

## Verification

1. `go test ./...` — new table-driven tests in `internal/auth` for expired,
   wrong-issuer, wrong-audience, wrong-algorithm (including the `alg: none` and
   HS256-signed-with-the-public-key substitution attempts) and unknown-`kid`
   tokens. Architecture rule 14 names auth as must-test logic.
2. `npm test` — `RequireAuth` redirects when signed out, renders children when
   signed in, and shows neither while loading; `AskBox` routes a signed-out
   submit to `/signin` with the question preserved in `next`.
3. Manual, signed out: load `/`, type a question, press Enter → sign-in screen →
   Google → land on `/chat` with that question already answering.
4. Manual, deep link: open `/bookmarks` signed out → sign-in → land on
   `/bookmarks`, not `/`.
5. `curl -X POST https://<api>/api/v1/qa/query -d '{"question":"test"}'` with no
   token → 401, not an answer. This is the check that the guard is real.
6. Cross-origin preflight: `curl -X OPTIONS` with
   `Access-Control-Request-Headers: authorization` → the response must echo it.
