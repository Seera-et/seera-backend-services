-- A decision/shape record for debugging a past request, not a transcript
-- store: no question text, no answer text. Every real bug found so far in
-- live testing was diagnosed by grepping raw process logs while they were
-- still on screen; this makes "why did it answer this way" queryable after
-- the fact for one request at a time, without adopting a separate
-- observability service.

CREATE TABLE request_traces (
    request_id      text PRIMARY KEY,
    created_at      timestamptz NOT NULL DEFAULT now(),

    route_intent    text        NOT NULL,
    route_category  text        NOT NULL DEFAULT '',
    route_depth     text        NOT NULL DEFAULT '',
    route_fallback  boolean     NOT NULL DEFAULT false,

    kind            text        NOT NULL,
    abstained       boolean     NOT NULL DEFAULT false,
    grounded        boolean     NOT NULL DEFAULT false,
    citation_count  integer     NOT NULL DEFAULT 0,
    confidence      text        NOT NULL DEFAULT '',
    model           text        NOT NULL DEFAULT '',
    reranker        text        NOT NULL DEFAULT '',
    timings         jsonb       NOT NULL DEFAULT '{}'::jsonb
);

-- Traces are looked up by request_id (the primary key) or browsed by
-- recency; nothing else is queried yet, so no additional index is added
-- speculatively.
CREATE INDEX request_traces_created_at_idx ON request_traces (created_at DESC);
