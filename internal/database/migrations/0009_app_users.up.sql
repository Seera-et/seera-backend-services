-- The account a signed-in caller owns things through.
--
-- Deliberately *not* a foreign key to Supabase's auth.users, even though that
-- table lives in this same database. Two reasons:
--
--   1. Local development runs plain Postgres in docker, where no auth schema
--      exists. An FK to it would make these migrations unrunnable off Supabase,
--      and the migration set is meant to describe the schema everywhere.
--   2. It would put a vendor's table in the dependency graph of our own data.
--      Moving auth providers should cost a re-mapping of ids, not a rewrite of
--      every table that references a user.
--
-- id is the Supabase user id — the `sub` claim of the JWT. The row is created
-- on demand from verified token claims, so there is no sign-up endpoint and no
-- way for this table to disagree with who Supabase says the caller is.
CREATE TABLE app_users (
    id           uuid PRIMARY KEY,
    email        text NOT NULL,
    -- Both come from the Google profile via user_metadata and are cosmetic:
    -- a user with neither is still a valid user, so neither is NOT NULL.
    display_name text,
    avatar_url   text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now()
);

-- Support lookup by email for administrative questions ("who is this account?")
-- without scanning. Not unique: Supabase, not this table, decides identity, and
-- two provider identities can legitimately surface the same address.
CREATE INDEX app_users_email_idx ON app_users (email);
