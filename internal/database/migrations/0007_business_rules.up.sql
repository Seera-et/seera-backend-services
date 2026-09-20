-- The Business & License Advisor's rule data.
--
-- Why this is a schema and not a Go file: architecture rule 9 — no legal
-- thresholds in application code. "A private limited company may not have less
-- than two or more than fifty members" (Commercial Code Art. 495/4) and "the
-- capital may not be less than fifteen thousand Ethiopian Birr" (Art. 496/1)
-- are legal facts with a source and an effective date. They live here, cited,
-- or they do not exist. The engine in internal/business/rules.go reads these
-- rows and contains no number of its own.
--
-- Rule sets are versioned and published the same way documents are, for the
-- same reason: the law changes, and an answer must be able to say which rule
-- set it applied.

CREATE TABLE business_rule_sets (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    label        text        NOT NULL,
    -- Where the set came from and who curated it. This is legal content, so
    -- its provenance is part of the record.
    notes        text        NOT NULL DEFAULT '',
    status       text        NOT NULL DEFAULT 'draft',
    created_at   timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz,

    CONSTRAINT business_rule_sets_status_check
        CHECK (status IN ('draft', 'published', 'superseded')),
    CONSTRAINT business_rule_sets_label_key UNIQUE (label)
);

-- At most one live rule set. A recommendation must be reproducible, which it is
-- not if two sets could both be current.
CREATE UNIQUE INDEX business_rule_sets_one_published
    ON business_rule_sets ((status))
    WHERE status = 'published';

-- The legal forms a business may take. One row per form, per rule set.
--
-- Every numeric bound is NULLABLE and NULL means "not established from the
-- indexed corpus" — which is not the same as "no limit". The engine treats an
-- unknown bound as permissive and says so, the same way legal.Version.InForceAt
-- treats an unknown effective date: silently excluding a lawful option is worse
-- than offering one whose limit is unverified, as long as the gap is stated.
CREATE TABLE business_structures (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    rule_set_id uuid NOT NULL REFERENCES business_rule_sets (id) ON DELETE CASCADE,

    code    text NOT NULL,   -- stable identifier used by the API and the UI
    name    text NOT NULL,   -- "Private Limited Company"
    -- Required suffix to the firm-name, where the law prescribes one
    -- (Art. 497 for a PLC, Art. 246 for a share company).
    name_suffix text NOT NULL DEFAULT '',
    summary text NOT NULL DEFAULT '',
    -- How members are exposed to the organisation's debts, in the law's own
    -- terms. Free text, not an enum: the distinctions the Code draws do not
    -- reduce to two or three cases.
    liability text NOT NULL DEFAULT '',

    min_members int,
    max_members int,

    min_capital   numeric(14, 2),
    min_share_par numeric(14, 2),
    currency      text NOT NULL DEFAULT 'ETB',

    -- Whether shares may be offered to the public. NULL where the indexed text
    -- does not settle it.
    allows_public_subscription boolean,

    ordinal int NOT NULL,

    CONSTRAINT business_structures_code_key UNIQUE (rule_set_id, code),
    CONSTRAINT business_structures_members_check
        CHECK (min_members IS NULL OR max_members IS NULL OR min_members <= max_members),
    CONSTRAINT business_structures_members_positive
        CHECK (min_members IS NULL OR min_members >= 1)
);

-- The activities a business may carry on, as enumerated by Commercial Code
-- Art. 5 ("Persons to be Regarded as Traders").
--
-- A curated subset of the article's 37 categories rather than a taxonomy
-- invented for this product: the intake question has to offer something, and
-- inventing categories would put a classification of Ethiopian commerce into
-- application data with no source behind it.
CREATE TABLE business_activities (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    rule_set_id uuid NOT NULL REFERENCES business_rule_sets (id) ON DELETE CASCADE,

    code    text NOT NULL,
    name    text NOT NULL,
    -- True where the activity is a regulated sector needing a licence beyond
    -- ordinary commercial registration.
    regulated boolean NOT NULL DEFAULT false,
    ordinal int NOT NULL,

    CONSTRAINT business_activities_code_key UNIQUE (rule_set_id, code)
);

-- Everything the advisor tells a reader to do, be aware of, or obtain.
--
-- One table for steps, licences and caveats because they are the same shape —
-- an ordered, titled, sourced statement — and three near-identical tables would
-- triple the join surface for no gain.
CREATE TABLE business_requirements (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    rule_set_id uuid NOT NULL REFERENCES business_rule_sets (id) ON DELETE CASCADE,

    kind text NOT NULL,

    -- NULL structure_code means the requirement applies whatever form is
    -- chosen; NULL activity_code means it applies whatever the business does.
    -- A licence row is normally scoped by activity, a registration step by
    -- structure, and a caveat by either or neither.
    structure_code text,
    activity_code  text,

    title  text NOT NULL,
    detail text NOT NULL DEFAULT '',
    -- The body a reader must deal with, where the law names one.
    authority text NOT NULL DEFAULT '',

    ordinal int NOT NULL,

    CONSTRAINT business_requirements_kind_check
        CHECK (kind IN ('step', 'licence', 'caveat'))
);

CREATE INDEX business_requirements_lookup_idx
    ON business_requirements (rule_set_id, kind, ordinal);

-- What each structure, activity or requirement is based on.
--
-- Cited by (document_id, article_no) rather than by articles.id on purpose.
-- Article rows belong to a document *version*: re-ingesting a document creates
-- new article rows with new ids, which would silently orphan every citation
-- pointing at the old ones. A document's identity is stable and an article
-- number is how a lawyer cites, so this pair survives re-ingestion and always
-- resolves to whatever the currently published version says.
CREATE TABLE business_rule_sources (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    rule_set_id uuid NOT NULL REFERENCES business_rule_sets (id) ON DELETE CASCADE,

    subject_kind text NOT NULL,
    subject_id   uuid NOT NULL,

    document_id uuid NOT NULL REFERENCES legal_documents (id) ON DELETE RESTRICT,
    article_no  text NOT NULL,
    -- Which sub-article carries the point, where it is worth naming ("495/4").
    pinpoint text NOT NULL DEFAULT '',

    CONSTRAINT business_rule_sources_subject_check
        CHECK (subject_kind IN ('structure', 'activity', 'requirement')),
    CONSTRAINT business_rule_sources_key
        UNIQUE (subject_kind, subject_id, document_id, article_no, pinpoint)
);

CREATE INDEX business_rule_sources_subject_idx
    ON business_rule_sources (subject_kind, subject_id);
