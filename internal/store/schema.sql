CREATE TABLE IF NOT EXISTS providers (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT NOT NULL UNIQUE,
    base_url    TEXT NOT NULL,
    api_format  TEXT NOT NULL DEFAULT 'chat',
    platform    TEXT DEFAULT 'unknown',
    status      TEXT DEFAULT 'unknown',
    health      REAL DEFAULT 0,
    last_balance REAL,
    last_error  TEXT DEFAULT '',
    created_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at  DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS check_runs (
    id              TEXT PRIMARY KEY,
    mode            TEXT NOT NULL,
    status          TEXT NOT NULL,
    trigger_type    TEXT NOT NULL DEFAULT 'manual',
    started_at      DATETIME NOT NULL,
    ended_at        DATETIME,
    providers_count INTEGER DEFAULT 0,
    models_count    INTEGER DEFAULT 0,
    ok_count        INTEGER DEFAULT 0,
    correct_count   INTEGER DEFAULT 0,
    summary         TEXT
);

CREATE TABLE IF NOT EXISTS check_results (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id        TEXT NOT NULL,
    provider_id   INTEGER NOT NULL REFERENCES providers(id),
    model         TEXT NOT NULL,
    vendor        TEXT NOT NULL,
    status        TEXT NOT NULL,
    correct       BOOLEAN DEFAULT 0,
    answer        TEXT,
    latency_ms    INTEGER NOT NULL,
    error_msg     TEXT,
    has_reasoning BOOLEAN DEFAULT 0,
    checked_at    DATETIME DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_cr_prov_model ON check_results(provider_id, model, checked_at);
CREATE INDEX IF NOT EXISTS idx_cr_run ON check_results(run_id);

CREATE TABLE IF NOT EXISTS fingerprint_results (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    provider_id     INTEGER NOT NULL REFERENCES providers(id),
    model           TEXT NOT NULL,
    vendor          TEXT NOT NULL,
    total_score     INTEGER NOT NULL,
    l1 INTEGER, l2 INTEGER, l3 INTEGER, l4 INTEGER,
    expected_tier   TEXT,
    expected_min    INTEGER,
    verdict         TEXT NOT NULL,
    self_id_verdict TEXT,
    self_id_detail  TEXT,
    answers_json    TEXT,
    checked_at      DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS capabilities (
    provider_id INTEGER NOT NULL REFERENCES providers(id),
    model       TEXT NOT NULL,
    streaming   BOOLEAN,
    tool_use    BOOLEAN,
    tested_at   DATETIME,
    PRIMARY KEY (provider_id, model)
);

CREATE TABLE IF NOT EXISTS events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    type        TEXT NOT NULL,
    provider    TEXT NOT NULL,
    model       TEXT,
    old_value   TEXT,
    new_value   TEXT,
    message     TEXT NOT NULL,
    read        BOOLEAN DEFAULT 0,
    created_at  DATETIME DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_events_unread ON events(read, created_at);
