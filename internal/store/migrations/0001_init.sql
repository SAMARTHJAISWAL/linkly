-- Links and their clicks. Timestamps are Unix seconds (UTC).
CREATE TABLE links (
    id         INTEGER PRIMARY KEY,
    code       TEXT    NOT NULL UNIQUE,
    url        TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    expires_at INTEGER          -- NULL means the link never expires
) STRICT;

-- One row per redirect. No IPs, user agents or full referrer URLs are kept.
CREATE TABLE clicks (
    id            INTEGER PRIMARY KEY,
    link_id       INTEGER NOT NULL REFERENCES links (id) ON DELETE CASCADE,
    clicked_at    INTEGER NOT NULL,
    referrer_host TEXT    NOT NULL DEFAULT ''
) STRICT;

-- Serves both the per-day histogram (range scan) and the total count.
CREATE INDEX clicks_link_time ON clicks (link_id, clicked_at);

-- Covering index for the top-referrers GROUP BY.
CREATE INDEX clicks_link_referrer ON clicks (link_id, referrer_host);
