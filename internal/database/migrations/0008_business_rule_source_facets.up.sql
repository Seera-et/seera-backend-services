-- Which part of a rule each citation supports.
--
-- Without this, a structure's citations were an undifferentiated list and every
-- finding about that structure cited all of them. The visible result was a
-- recommendation whose capital reason cited Art. 174 ("Types of Business
-- Organizations") instead of Art. 496 ("Capital") — a reference that resolves
-- correctly, points at real law, and does not support the sentence next to it.
--
-- That is the failure mode AC-005 is written against: a citation is correct only
-- when the referenced source actually supports the associated claim. Grouping
-- citations by facet lets the engine cite the provision behind the specific
-- finding it is reporting.
--
-- 'general' identifies the form itself rather than any one of its bounds, and is
-- the fallback when an author has not tagged a more specific source.
ALTER TABLE business_rule_sources
    ADD COLUMN facet text NOT NULL DEFAULT 'general';

ALTER TABLE business_rule_sources
    ADD CONSTRAINT business_rule_sources_facet_check
    CHECK (facet IN ('general', 'members', 'capital', 'liability', 'public_shares'));

-- The uniqueness rule has to admit the same article under two facets: Art. 495
-- carries both the member range (495/4) and the limited-liability statement
-- (495/1), and both are worth citing separately.
ALTER TABLE business_rule_sources
    DROP CONSTRAINT business_rule_sources_key;

ALTER TABLE business_rule_sources
    ADD CONSTRAINT business_rule_sources_key
    UNIQUE (subject_kind, subject_id, document_id, article_no, pinpoint, facet);
