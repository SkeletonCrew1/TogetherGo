-- Free-text search over a trip's title and description.
--
-- The discovery endpoint's `q` parameter is a full-text match, not a substring
-- match. `ILIKE '%carpath%'` cannot use an index at all — the leading wildcard
-- makes it a sequential scan plus a per-row comparison — and it matches the
-- wrong things anyway: no stemming, no ranking, and "hike" never finds
-- "hiking".
--
-- The vector is a generated column rather than a trigger-maintained one. A
-- trigger is a second writer that has to be remembered on every future
-- INSERT/UPDATE path; a STORED generated column cannot drift from the columns
-- it is derived from, because Postgres recomputes it on every write of either.

-- +goose Up

ALTER TABLE trips
    ADD COLUMN search_vector tsvector
    GENERATED ALWAYS AS (
        -- Weighted, title above description: two trips that both mention
        -- "Hoverla" are not equally relevant if one of them is called it.
        -- Nothing ranks on this yet — discovery is ordered by departure time,
        -- because a keyset cursor has to be total and stable and `ts_rank` is
        -- neither — but the weights cost nothing to store and a "sort by
        -- relevance" option would otherwise mean rewriting every row.
        setweight(to_tsvector('english', coalesce(title, '')), 'A') ||
        setweight(to_tsvector('english', coalesce(description, '')), 'B')
    ) STORED;

-- 'english', not 'simple', and the trade-off is deliberate. The corpus is
-- mixed Ukrainian and English. The English configuration stems English words
-- ("hikes", "hiking" -> "hike") and drops English stop words, which is the
-- behaviour a search box needs for the half of the corpus it understands.
-- Cyrillic tokens pass through the English snowball stemmer essentially
-- unchanged, so they are indexed as written — no worse than 'simple' would
-- have done, and better everywhere else. A query made entirely of stop words
-- ("the a of") produces an empty tsquery and therefore matches nothing; that
-- is the accepted cost.

CREATE INDEX trips_search_gin ON trips USING GIN (search_vector);

-- +goose Down

DROP INDEX trips_search_gin;
ALTER TABLE trips DROP COLUMN search_vector;
