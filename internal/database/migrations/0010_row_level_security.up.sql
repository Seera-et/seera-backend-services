-- Close the corpus and the accounts to Supabase's browser-facing roles.
--
-- WHAT WAS WRONG
--
-- Supabase ships an ALTER DEFAULT PRIVILEGES rule that grants `anon` and
-- `authenticated` the full set (arwdDxtm — select, insert, update, delete,
-- truncate, references, trigger, maintain) on every table the `postgres` role
-- creates in `public`. Every table these migrations have ever created picked it
-- up silently, and row level security was never switched on.
--
-- PostgREST serves `public` to those two roles, and the key that reaches them
-- is the publishable key — which is compiled into the browser bundle and is
-- meant to be public. So before this migration, anyone who opened devtools
-- could:
--
--     DELETE /rest/v1/articles      → erase the corpus
--     PATCH  /rest/v1/articles      → rewrite the text of a law
--     GET    /rest/v1/app_users     → read every user's email address
--     GET    /rest/v1/request_traces→ read what people asked Seera
--     GET    /rest/v1/chunk_embeddings_gemini_1536 → take the embeddings
--
-- The second one is the serious one: Seera would then cite the altered text,
-- with a citation and full confidence, and nothing in the answer path could
-- tell. That is the exact failure the versioning rules exist to prevent.
--
-- WHY BOTH REVOKE AND RLS
--
-- Neither alone is sufficient, which is worth stating because "enable RLS" is
-- the usual advice and it is not enough here:
--
--   * TRUNCATE is not subject to row level security. A table with RLS enabled
--     and every policy denying still gets emptied by a role holding the
--     TRUNCATE grant. Only the REVOKE stops that.
--   * RLS is the backstop for everything else, and it is what makes a future
--     accidental GRANT harmless rather than fatal.
--
-- WHAT STILL WORKS
--
-- The Go API connects as the table owner, and an owner bypasses RLS unless
-- FORCE ROW LEVEL SECURITY is set. It is deliberately not set: the API is the
-- component that enforces access (internal/server.requireUser), and forcing RLS
-- would lock out the one caller that is supposed to see everything. Nothing in
-- internal/ changes because of this migration.
--
-- PORTABILITY
--
-- These migrations also run against plain Postgres in docker, where the roles
-- `anon`/`authenticated` and the function `auth.uid()` do not exist. Every
-- Supabase-specific statement below is guarded so it is a no-op there rather
-- than an error that stops local development.

-- ---------------------------------------------------------------------------
-- 1. Row level security on every table, with no permissive policy.
--
-- No policy means deny, for every role that is not the owner. Listed one by one
-- rather than looped over pg_class so that this file is a readable statement of
-- intent, and so a reviewer sees each table named.
-- ---------------------------------------------------------------------------

ALTER TABLE schema_migrations            ENABLE ROW LEVEL SECURITY;

-- The corpus. Read-only to the world through the Go API, which serves only
-- published versions — direct table access would hand out drafts and
-- superseded text as though they were current law, which is worse than no
-- access at all.
ALTER TABLE legal_documents              ENABLE ROW LEVEL SECURITY;
ALTER TABLE document_versions            ENABLE ROW LEVEL SECURITY;
ALTER TABLE articles                     ENABLE ROW LEVEL SECURITY;
ALTER TABLE chunks                       ENABLE ROW LEVEL SECURITY;

-- Embeddings: a paid asset, and of no use to a browser.
ALTER TABLE chunk_embeddings_openai_1536 ENABLE ROW LEVEL SECURITY;
ALTER TABLE chunk_embeddings_gemini_1536 ENABLE ROW LEVEL SECURITY;

-- What people asked. Never public.
ALTER TABLE request_traces               ENABLE ROW LEVEL SECURITY;

-- The advisor's rule set: curated legal content on the same footing as the
-- corpus, served through the API that resolves its citations.
ALTER TABLE business_rule_sets           ENABLE ROW LEVEL SECURITY;
ALTER TABLE business_structures          ENABLE ROW LEVEL SECURITY;
ALTER TABLE business_activities          ENABLE ROW LEVEL SECURITY;
ALTER TABLE business_requirements        ENABLE ROW LEVEL SECURITY;
ALTER TABLE business_rule_sources        ENABLE ROW LEVEL SECURITY;

-- Accounts. Gets the one real policy, below.
ALTER TABLE app_users                    ENABLE ROW LEVEL SECURITY;

-- ---------------------------------------------------------------------------
-- 2. Take back the grants.
-- ---------------------------------------------------------------------------

DO $$
DECLARE
    browser_role text;
BEGIN
    FOREACH browser_role IN ARRAY ARRAY['anon', 'authenticated'] LOOP
        -- Absent on plain Postgres; this whole section is then a no-op.
        IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = browser_role) THEN
            CONTINUE;
        END IF;

        EXECUTE format(
            'REVOKE ALL ON ALL TABLES IN SCHEMA public FROM %I', browser_role);
        EXECUTE format(
            'REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM %I', browser_role);
        EXECUTE format(
            'REVOKE ALL ON ALL FUNCTIONS IN SCHEMA public FROM %I', browser_role);

        -- The durable half. Without this, the next migration to CREATE TABLE
        -- reopens exactly the same hole, because the default-privilege rule
        -- that caused this is still in force. Applies to objects this role
        -- creates from now on, which is what the migrator does.
        EXECUTE format(
            'ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON TABLES FROM %I',
            browser_role);
        EXECUTE format(
            'ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON SEQUENCES FROM %I',
            browser_role);
        EXECUTE format(
            'ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON FUNCTIONS FROM %I',
            browser_role);
    END LOOP;
END $$;

-- service_role is deliberately left alone. It is reachable only with the secret
-- service key, which never leaves a server; revoking it would break Supabase's
-- own tooling for no gain against the threat this migration addresses, which is
-- a key that is published on purpose.

-- ---------------------------------------------------------------------------
-- 3. The one policy worth writing: a user may read their own account row.
--
-- Nothing depends on this today — the browser talks to the Go API, never to
-- PostgREST. It exists so that the answer to "can a signed-in user read someone
-- else's account?" is enforced by the database and not only by the fact that we
-- have not built the feature yet. If supabase-js is ever used directly (for
-- realtime, say), this is already correct.
--
-- Guarded on auth.uid() because that function is Supabase's, and this migration
-- also runs against plain Postgres.
-- ---------------------------------------------------------------------------

DO $$
BEGIN
    IF to_regprocedure('auth.uid()') IS NULL THEN
        RETURN;
    END IF;

    -- app_users.id IS the Supabase user id (the JWT `sub`), so this is a
    -- direct comparison and needs no join.
    EXECUTE $policy$
        CREATE POLICY app_users_select_own ON app_users
            FOR SELECT
            TO authenticated
            USING (id = (SELECT auth.uid()))
    $policy$;
END $$;

-- Note the SELECT wrapper around auth.uid(). Called bare, it is re-evaluated
-- per row; wrapped, the planner treats it as a one-time initplan. On a
-- single-row lookup this is irrelevant, but it is the habit that keeps RLS off
-- the profile of the larger per-user tables arriving in the next migration.

-- ---------------------------------------------------------------------------
-- No SELECT grant is restored to anon or authenticated anywhere above, so the
-- policy in section 3 currently has no way to be reached: the grant is missing
-- and RLS would still refuse. That is intentional — a policy that is correct
-- and unreachable is the right resting state for a door nobody uses yet.
-- Re-granting SELECT on app_users to `authenticated` is the single, deliberate
-- step that would open it.
-- ---------------------------------------------------------------------------
