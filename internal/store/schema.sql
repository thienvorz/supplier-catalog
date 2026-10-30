

CREATE TABLE update_jobs (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    feed             TEXT    NOT NULL CHECK (length(feed) > 0),
    -- running     : a worker in THIS process is applying batches
    -- interrupted : stopped before the supplier's final batch; can be resumed
    -- completed   : the supplier's final batch was applied (see CHECK below)
    -- cancelled   : abandoned by staff; never reported as complete
    status           TEXT    NOT NULL CHECK (status IN ('running','interrupted','completed','cancelled')),
    -- Cursor of the NEXT batch to fetch. Advanced in the same transaction that
    -- applies the batch, so progress and data can never disagree.
    next_cursor      TEXT    NOT NULL DEFAULT '',
    batches_done     INTEGER NOT NULL DEFAULT 0 CHECK (batches_done >= 0),
    supplier_done    INTEGER NOT NULL DEFAULT 0 CHECK (supplier_done IN (0, 1)),
    resumes          INTEGER NOT NULL DEFAULT 0,

    received         INTEGER NOT NULL DEFAULT 0,
    created          INTEGER NOT NULL DEFAULT 0,
    updated          INTEGER NOT NULL DEFAULT 0,
    relisted         INTEGER NOT NULL DEFAULT 0,
    withdrawn        INTEGER NOT NULL DEFAULT 0,
    unchanged        INTEGER NOT NULL DEFAULT 0,
    duplicates       INTEGER NOT NULL DEFAULT 0,
    stale            INTEGER NOT NULL DEFAULT 0,
    invalid          INTEGER NOT NULL DEFAULT 0,
    conflicts        INTEGER NOT NULL DEFAULT 0,

    last_error       TEXT    NOT NULL DEFAULT '',
    created_at       TEXT    NOT NULL,
    updated_at       TEXT    NOT NULL,
    finished_at      TEXT,

    -- A job can only be "completed" if the supplier's final batch was applied.
    CHECK (status <> 'completed' OR supplier_done = 1),
    -- finished_at is set exactly for the two terminal states.
    CHECK ((status IN ('completed','cancelled')) = (finished_at IS NOT NULL))
);

-- At most one unfinished update per feed (two workers must not race on a feed).
CREATE UNIQUE INDEX idx_one_open_job_per_feed
    ON update_jobs (feed) WHERE status IN ('running','interrupted');

CREATE TABLE products (
    sku           TEXT    PRIMARY KEY
                  CHECK (length(sku) > 0 AND sku = upper(trim(sku))),
    name          TEXT    NOT NULL DEFAULT '',
    description   TEXT    NOT NULL DEFAULT '',
    category      TEXT    NOT NULL DEFAULT '',
    price_cents   INTEGER NOT NULL DEFAULT 0 CHECK (price_cents >= 0),
    currency      TEXT    NOT NULL DEFAULT '',
    status        TEXT    NOT NULL CHECK (status IN ('active','withdrawn')),
    -- Supplier's revision counter of the newest information applied.
    version       INTEGER NOT NULL CHECK (version >= 1),
    withdrawn_at  TEXT,
    created_at    TEXT    NOT NULL,
    updated_at    TEXT    NOT NULL,
    CHECK ((status = 'withdrawn') = (withdrawn_at IS NOT NULL))
);

CREATE INDEX idx_products_status ON products (status, sku);

-- Older information must never replace newer information.
CREATE TRIGGER products_no_downgrade
BEFORE UPDATE ON products
WHEN NEW.version < OLD.version
BEGIN
    SELECT RAISE(ABORT, 'product version must not decrease');
END;

-- Older (or equal) information must never bring back a withdrawn product.
CREATE TRIGGER products_no_stale_relist
BEFORE UPDATE ON products
WHEN OLD.status = 'withdrawn' AND NEW.status = 'active' AND NEW.version <= OLD.version
BEGIN
    SELECT RAISE(ABORT, 'a withdrawn product can only be relisted by a newer version');
END;

-- What changed, and when. Written in the same transaction as the product row.
CREATE TABLE product_changes (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id         INTEGER NOT NULL REFERENCES update_jobs (id),
    batch_index    INTEGER NOT NULL,
    sku            TEXT    NOT NULL,
    change_type    TEXT    NOT NULL CHECK (change_type IN ('created','updated','withdrawn','relisted')),
    version_before INTEGER,
    version_after  INTEGER NOT NULL,
    changed_fields TEXT    NOT NULL DEFAULT '[]',   -- JSON array of field names
    event_id       TEXT    NOT NULL DEFAULT '',
    before_json    TEXT,                            -- NULL for a brand-new product
    after_json     TEXT    NOT NULL,
    created_at     TEXT    NOT NULL
);

CREATE INDEX idx_changes_job ON product_changes (job_id, id);
CREATE INDEX idx_changes_sku ON product_changes (sku, id);


CREATE TABLE issues (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id          INTEGER NOT NULL REFERENCES update_jobs (id),   -- update that saw it first
    batch_index     INTEGER NOT NULL,
    record_index    INTEGER NOT NULL,
    kind            TEXT    NOT NULL CHECK (kind IN ('invalid_record','version_conflict')),
    sku             TEXT    NOT NULL DEFAULT '',
    event_id        TEXT    NOT NULL DEFAULT '',
    message         TEXT    NOT NULL,
    raw_record      TEXT    NOT NULL,               -- the supplier's record, as received (truncated if huge)
    fingerprint     TEXT    NOT NULL,               -- sha256 of kind + record
    occurrences     INTEGER NOT NULL DEFAULT 1,     -- how many times it was seen while open
    status          TEXT    NOT NULL DEFAULT 'open' CHECK (status IN ('open','resolved')),
    resolution_note TEXT    NOT NULL DEFAULT '',
    created_at      TEXT    NOT NULL,
    resolved_at     TEXT,
    CHECK ((status = 'resolved') = (resolved_at IS NOT NULL))
);

CREATE UNIQUE INDEX idx_issues_one_open_per_fingerprint ON issues (fingerprint) WHERE status = 'open';
CREATE INDEX idx_issues_status ON issues (status, id);

CREATE TABLE issue_sightings (
    issue_id     INTEGER NOT NULL REFERENCES issues (id),
    job_id       INTEGER NOT NULL REFERENCES update_jobs (id),
    batch_index  INTEGER NOT NULL,
    record_index INTEGER NOT NULL,
    -- A record has one position in one batch of one update: replays cannot double-count.
    UNIQUE (job_id, batch_index, record_index)
);

CREATE INDEX idx_sightings_job ON issue_sightings (job_id, issue_id);
