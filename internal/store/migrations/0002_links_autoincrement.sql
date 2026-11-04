-- Link ids must never be reused. A click waits in the recorder's queue
-- with the id of the link its redirect resolved; if that link is deleted
-- and a new one takes its id before the flush, the click would be credited
-- to the wrong link. INTEGER PRIMARY KEY alone hands out max(id)+1, which
-- reuses the id of a deleted newest link; AUTOINCREMENT never goes back.
--
-- SQLite cannot add AUTOINCREMENT to an existing table, so links is
-- rebuilt (https://sqlite.org/lang_altertable.html#otheralter). migrate
-- runs every migration with foreign keys off and checks them before
-- committing, so dropping the old table does not cascade to clicks, whose
-- REFERENCES links (id) then resolves to the new table.
--
-- Ids freed before this migration ran cannot be recovered, but that is
-- harmless: the click queue lives in memory and does not survive the
-- restart that applies this migration.
CREATE TABLE links_new (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    code       TEXT    NOT NULL UNIQUE,
    url        TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    expires_at INTEGER          -- NULL means the link never expires
) STRICT;

INSERT INTO links_new (id, code, url, created_at, expires_at)
    SELECT id, code, url, created_at, expires_at FROM links;

DROP TABLE links;

ALTER TABLE links_new RENAME TO links;
