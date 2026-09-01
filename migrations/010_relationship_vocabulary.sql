-- +goose Up

-- The extraction prompt asks for "a short verb phrase", and 141 memories
-- answered with 503 distinct relationship labels over 1272 edges -- 341 of them
-- used exactly once. "holds", "holds role" and "holds-role" sat on one pair of
-- nodes as three separate edges. Worse, the same fact was stored twice whenever
-- the model narrated it from the other side: 111 pairs existed as both A->B and
-- B->A ("employs" beside "works-at", "used-by" beside "uses").
--
-- The vocabulary is now closed (memory.Relationships) with one canonical
-- direction per predicate, and the prompt names it. This folds the historical
-- labels onto it:
--
--   * a label already in the vocabulary is kept as-is
--   * a known alias becomes its canonical predicate, with the endpoints swapped
--     when the alias states the relation backwards ("employs" -> "works-at")
--   * anything else falls to 'relates-to', which is the intended catch-all
--
-- Nothing is lost to the fold: relationship_raw keeps the model's own wording.
--
-- The VALUES block below is generated from memory.relationshipAliases, and the
-- CHECK from memory.Relationships; both must stay in step with the Go code, as
-- migration 008 does for entity types.

ALTER TABLE relations ADD COLUMN relationship_raw text;
UPDATE relations SET relationship_raw = relationship;

CREATE TEMP TABLE rel_folded AS
WITH alias(alias, canon, reversed) AS (VALUES
    ('abbreviates','same-as',false),
    ('accessed-by','uses',true),
    ('accessed-via','uses',false),
    ('accesses','uses',false),
    ('accountable-to','leads',true),
    ('activates-across','relates-to',false),
    ('acts-as','has-role',false),
    ('addresses','targets',false),
    ('aims-to','targets',false),
    ('aims-to-build','targets',false),
    ('alias-of','same-as',false),
    ('allows-access','uses',true),
    ('alternative-to','compares-with',false),
    ('analyzes','works-on',false),
    ('applies-to','uses',false),
    ('associated-with','relates-to',false),
    ('at','located-in',false),
    ('aunt','family-of',false),
    ('aunt-of','family-of',false),
    ('available-in','located-in',false),
    ('based-in','located-in',false),
    ('based-on','uses',false),
    ('being-evaluated-for','relates-to',false),
    ('belongs-to','part-of',false),
    ('billed-for','provides',false),
    ('billed-under','part-of',false),
    ('bills-to','provides',false),
    ('blocked-by','depends-on',false),
    ('builds','creates',false),
    ('builds-on','uses',false),
    ('built','creates',false),
    ('built-as','creates',false),
    ('built-by','creates',true),
    ('built-with','uses',false),
    ('classified-as','is-a',false),
    ('client-of','works-at',false),
    ('co-worked-with','collaborates-with',false),
    ('collaborates-on','collaborates-with',false),
    ('commonly-called','same-as',false),
    ('compared-against','compares-with',false),
    ('compared-to','compares-with',false),
    ('compared-with','compares-with',false),
    ('compares','compares-with',false),
    ('compatible-with','integrates-with',false),
    ('competes-with','compares-with',false),
    ('competing-platforms','compares-with',false),
    ('competitor-of','compares-with',false),
    ('completed','works-on',false),
    ('comprises','part-of',true),
    ('connected-to','integrates-with',false),
    ('connects-to','integrates-with',false),
    ('considers','relates-to',false),
    ('contact-for','relates-to',false),
    ('contacts','collaborates-with',false),
    ('contains','part-of',true),
    ('contains-department','part-of',true),
    ('contributes-to','works-on',false),
    ('coordinates-with','collaborates-with',false),
    ('cousin-of','family-of',false),
    ('covers','relates-to',false),
    ('created','creates',false),
    ('created-by','creates',true),
    ('currently-holds','has-role',false),
    ('defined','creates',false),
    ('defined-by','creates',true),
    ('defines','creates',false),
    ('delivers','provides',false),
    ('designed-as','creates',false),
    ('designed-for','targets',false),
    ('designs','creates',false),
    ('develops','creates',false),
    ('discussed','relates-to',false),
    ('discussed-by','relates-to',false),
    ('discusses','works-on',false),
    ('dislikes','avoids',false),
    ('distributes-to','provides',false),
    ('distributes-via','provides',false),
    ('does-not-prefer','avoids',false),
    ('egress-via','uses',false),
    ('employed-by','works-at',false),
    ('employs','works-at',true),
    ('enabled-by','provides',true),
    ('enables','provides',false),
    ('encompasses','part-of',true),
    ('established','creates',false),
    ('established-by','creates',true),
    ('evaluates','works-on',false),
    ('excludes','avoids',false),
    ('executes','works-on',false),
    ('exemplified-by','is-a',true),
    ('exemplifies','is-a',false),
    ('explores','works-on',false),
    ('finds-unfeasible','avoids',false),
    ('focuses-on','targets',false),
    ('followed-by','relates-to',false),
    ('generates','creates',false),
    ('handles','works-on',false),
    ('has','part-of',true),
    ('has-cousin','family-of',false),
    ('has-department','part-of',true),
    ('has-goal-of','targets',false),
    ('has-part','part-of',true),
    ('has-partner','collaborates-with',false),
    ('has-permission','uses',false),
    ('has-project','part-of',true),
    ('has-target-value','measured-by',false),
    ('has-use-case','part-of',true),
    ('held-by','has-role',true),
    ('holds','has-role',false),
    ('holds-role','has-role',false),
    ('holds-role-as','has-role',false),
    ('hosts','hosted-on',true),
    ('identical','same-as',false),
    ('identifies','relates-to',false),
    ('implemented-as','implements',false),
    ('implementing','implements',false),
    ('improves','works-on',false),
    ('improving','works-on',false),
    ('includes','part-of',true),
    ('includes-component','part-of',true),
    ('includes-pillar','part-of',true),
    ('includes-response','part-of',true),
    ('indicates','relates-to',false),
    ('instantiated-by','creates',true),
    ('integrates','integrates-with',false),
    ('intends-to-buy','targets',false),
    ('interacts-via','integrates-with',false),
    ('interacts-with','integrates-with',false),
    ('interested-in','targets',false),
    ('involves','relates-to',false),
    ('is','is-a',false),
    ('is-an-instance-of','is-a',false),
    ('is-aunt-of','family-of',false),
    ('is-based-in','located-in',false),
    ('is-built-on','uses',false),
    ('is-cousin-of','family-of',false),
    ('is-designed-for','targets',false),
    ('is-implemented-as','implements',false),
    ('is-implemented-by','implements',true),
    ('is-implementing','implements',false),
    ('is-known-as','same-as',false),
    ('is-led-by','leads',true),
    ('is-member-of','part-of',false),
    ('is-officially-named','same-as',false),
    ('is-part-of','part-of',false),
    ('is-phasing-out','supersedes',false),
    ('is-pillar-of','part-of',false),
    ('is-same-as','same-as',false),
    ('is-transitioning-to','supersedes',true),
    ('is-type-of','is-a',false),
    ('is-use-case-of','part-of',false),
    ('kind-of','is-a',false),
    ('known-as','same-as',false),
    ('lacks','avoids',false),
    ('lead-by','leads',true),
    ('leads-in','leads',false),
    ('led','leads',false),
    ('led-by','leads',true),
    ('leverages','uses',false),
    ('likes','prefers',false),
    ('listed-on','located-in',false),
    ('located-at','located-in',false),
    ('located-in-region-of','located-in',false),
    ('loves','prefers',false),
    ('maintains','owns',false),
    ('managed-by','leads',true),
    ('manages','leads',false),
    ('maps','relates-to',false),
    ('measures','measured-by',true),
    ('member-of','part-of',false),
    ('mentioned','relates-to',false),
    ('mentor','mentors',false),
    ('mentored','mentors',false),
    ('migrates-to','supersedes',true),
    ('migrating-to','supersedes',true),
    ('monitored-by','measured-by',false),
    ('monitors','measured-by',true),
    ('moves-away-from','supersedes',false),
    ('needs','depends-on',false),
    ('offers-service','provides',false),
    ('operates','uses',false),
    ('operates-on','uses',false),
    ('operationalizes','implements',false),
    ('overlaps-with','compares-with',false),
    ('owned-by','owns',true),
    ('owner-of','owns',false),
    ('performs','works-on',false),
    ('plays','has-role',false),
    ('powers','provides',false),
    ('preferred-by','prefers',true),
    ('preferred-for','prefers',false),
    ('preferred-over','prefers',false),
    ('prefers-over','prefers',false),
    ('prefers-receiving','prefers',false),
    ('processes','works-on',false),
    ('produces','creates',false),
    ('protects','provides',false),
    ('provided-by','provides',true),
    ('provides-information-about','relates-to',false),
    ('provides-service','provides',false),
    ('rebuilt-by','creates',true),
    ('recommended-for','targets',false),
    ('referenced-in','relates-to',false),
    ('refers-to','relates-to',false),
    ('related-to','relates-to',false),
    ('relies-on','depends-on',false),
    ('replaced-by','supersedes',true),
    ('replaces','supersedes',false),
    ('reports','relates-to',false),
    ('reports-to','leads',true),
    ('represented-by','same-as',true),
    ('represents','is-a',false),
    ('requires','depends-on',false),
    ('responsible-for','owns',false),
    ('runs','uses',false),
    ('runs-on','hosted-on',false),
    ('saves','stores',false),
    ('sells','provides',false),
    ('sends-events-to','integrates-with',false),
    ('sends-metrics-to','integrates-with',false),
    ('sends-traces-to','integrates-with',false),
    ('serves','provides',false),
    ('shifts-to','supersedes',true),
    ('ships-to','provides',false),
    ('shortlists','prefers',false),
    ('should-use-instead-of','supersedes',false),
    ('similar-to','compares-with',false),
    ('specializes-in','works-on',false),
    ('staffed-by','works-at',true),
    ('stakeholder-of','relates-to',false),
    ('standardizing','works-on',false),
    ('stands-for','same-as',false),
    ('steers','leads',false),
    ('stores-in','stores',true),
    ('struggles-with','relates-to',false),
    ('submits-to','stores',true),
    ('submitted-to','stores',true),
    ('superseded-by','supersedes',true),
    ('supervises','leads',false),
    ('supported-by','provides',true),
    ('supports','provides',false),
    ('synchronizes','integrates-with',false),
    ('syncs-with','integrates-with',false),
    ('taught-by','mentors',true),
    ('tracked-on','measured-by',false),
    ('tracks','measured-by',true),
    ('transitioning-to','supersedes',true),
    ('type-of','is-a',false),
    ('unauthorized-access-to','relates-to',false),
    ('uploads','stores',false),
    ('uploads-to','stores',true),
    ('used-by','uses',true),
    ('used-for','uses',true),
    ('used-in','uses',true),
    ('used-on','uses',true),
    ('uses-for','uses',false),
    ('uses-provider','uses',false),
    ('using','uses',false),
    ('utilizes','uses',false),
    ('worked-for','works-at',false),
    ('worked-on','works-on',false),
    ('working-on','works-on',false),
    ('works-for','works-at',false),
    ('works-in','works-at',false),
    ('works-in-role','has-role',false),
    ('works-with','collaborates-with',false),
    ('written-in','uses',false)
),
keyed AS (
    -- The key expression is memory.relationshipKey: migration 009's name fold,
    -- then spaces back to hyphens. "holds role", "holds-role" and "HOLDS_ROLE"
    -- all arrive here as 'holds-role'.
    SELECT r.from_entity, r.to_entity, r.memory_id, r.confidence, r.relationship_raw,
           replace(
               btrim(
                   regexp_replace(
                       regexp_replace(
                           CASE
                               WHEN btrim(regexp_replace(lower(btrim(r.relationship)), '\s*\([^()]*\)\s*$', '', 'g')) = ''
                                   THEN lower(btrim(r.relationship))
                               ELSE regexp_replace(lower(btrim(r.relationship)), '\s*\([^()]*\)\s*$', '', 'g')
                           END,
                       '[-_/]+', ' ', 'g'),
                   '\s+', ' ', 'g'),
               ' ''"`'),
           ' ', '-') AS key
    FROM relations r
),
folded AS (
    SELECT k.from_entity, k.to_entity, k.memory_id, k.confidence, k.relationship_raw,
           CASE WHEN k.key IN ('is-a','same-as','part-of','located-in','works-at','leads','has-role','collaborates-with','mentors','family-of','works-on','creates','owns','provides','targets','uses','depends-on','integrates-with','implements','hosted-on','stores','supersedes','compares-with','contradicts','prefers','avoids','measured-by','relates-to') THEN k.key
                WHEN a.canon IS NOT NULL           THEN a.canon
                ELSE 'relates-to' END AS canon,
           CASE WHEN k.key IN ('is-a','same-as','part-of','located-in','works-at','leads','has-role','collaborates-with','mentors','family-of','works-on','creates','owns','provides','targets','uses','depends-on','integrates-with','implements','hosted-on','stores','supersedes','compares-with','contradicts','prefers','avoids','measured-by','relates-to') THEN false
                WHEN a.canon IS NOT NULL           THEN a.reversed
                ELSE false END AS reversed
    FROM keyed k
    LEFT JOIN alias a ON a.alias = k.key
),
oriented AS (
    -- Swap the endpoints of a backwards alias, then put symmetric predicates in
    -- a fixed endpoint order so A-B and B-A collapse onto one row. Postgres
    -- orders uuid by its bytes and Go compares the hex string, which is the
    -- same ordering -- memory.Service.applyRelation relies on that.
    SELECT CASE WHEN reversed THEN to_entity   ELSE from_entity END AS f,
           CASE WHEN reversed THEN from_entity ELSE to_entity   END AS t,
           canon, memory_id, confidence, relationship_raw
    FROM folded
),
ordered AS (
    SELECT CASE WHEN canon IN ('collaborates-with','compares-with','contradicts','family-of','integrates-with','relates-to','same-as') AND f > t THEN t ELSE f END AS from_entity,
           CASE WHEN canon IN ('collaborates-with','compares-with','contradicts','family-of','integrates-with','relates-to','same-as') AND f > t THEN f ELSE t END AS to_entity,
           canon AS relationship, memory_id, confidence, relationship_raw
    FROM oriented
)
SELECT from_entity,
       to_entity,
       relationship,
       (array_agg(memory_id ORDER BY confidence DESC))[1]        AS memory_id,
       max(confidence)                                           AS confidence,
       (array_agg(relationship_raw ORDER BY confidence DESC))[1] AS relationship_raw
FROM ordered
-- An edge from a node to itself carries no information. Six survived the
-- earlier merges; folding aliases would have created more.
WHERE from_entity <> to_entity
GROUP BY from_entity, to_entity, relationship;

-- Nothing references relations.id, so the rows are replaced wholesale rather
-- than updated in place -- the fold collapses many rows onto one key, which an
-- UPDATE could not express against the UNIQUE constraint.
DELETE FROM relations;

INSERT INTO relations (from_entity, to_entity, relationship, memory_id, confidence, relationship_raw)
SELECT from_entity, to_entity, relationship, memory_id, confidence, relationship_raw
FROM rel_folded;

DROP TABLE rel_folded;

ALTER TABLE relations ALTER COLUMN relationship_raw SET NOT NULL;

ALTER TABLE relations ADD CONSTRAINT relations_relationship_check CHECK (
    relationship IN ('is-a','same-as','part-of','located-in','works-at','leads','has-role','collaborates-with','mentors','family-of','works-on','creates','owns','provides','targets','uses','depends-on','integrates-with','implements','hosted-on','stores','supersedes','compares-with','contradicts','prefers','avoids','measured-by','relates-to')
);

ALTER TABLE relations ADD CONSTRAINT relations_no_self_edge CHECK (from_entity <> to_entity);

-- +goose Down

-- The fold is not reversible as a whole, but the model's own wording survived
-- it, so Down restores that and drops the constraints over it.
ALTER TABLE relations DROP CONSTRAINT IF EXISTS relations_no_self_edge;
ALTER TABLE relations DROP CONSTRAINT IF EXISTS relations_relationship_check;
UPDATE relations SET relationship = relationship_raw WHERE relationship_raw IS NOT NULL;
ALTER TABLE relations DROP COLUMN relationship_raw;
