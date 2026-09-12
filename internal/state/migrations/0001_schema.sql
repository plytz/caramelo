CREATE TABLE machine (
    id        INTEGER PRIMARY KEY CHECK (id = 1),
    json      TEXT NOT NULL,
    gauged_at TEXT NOT NULL
);

CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE "keys" (
    name        TEXT PRIMARY KEY,
    type        TEXT NOT NULL,
    public_key  TEXT NOT NULL,
    fingerprint TEXT NOT NULL UNIQUE,
    options     TEXT NOT NULL DEFAULT '',
    added_at    TEXT NOT NULL
);

CREATE TABLE setup_runs (
    run_id      TEXT NOT NULL,
    step        TEXT NOT NULL,
    status      TEXT NOT NULL,
    detail      TEXT NOT NULL DEFAULT '',
    error       TEXT NOT NULL DEFAULT '',
    started_at  TEXT NOT NULL,
    duration_ns INTEGER NOT NULL DEFAULT 0,
    version     TEXT NOT NULL DEFAULT ''
);

CREATE INDEX setup_runs_started_at ON setup_runs (started_at DESC);

CREATE TABLE apps (
    name           TEXT PRIMARY KEY,
    repo_path      TEXT NOT NULL,
    default_branch TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL,
    stack          TEXT NOT NULL DEFAULT ''
);

CREATE TABLE envs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    app         TEXT NOT NULL REFERENCES apps (name) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    branch      TEXT NOT NULL,
    "commit"    TEXT NOT NULL DEFAULT '',
    worktree    TEXT NOT NULL,
    port_base   INTEGER NOT NULL UNIQUE,
    port_count  INTEGER NOT NULL,
    status      TEXT NOT NULL,
    config_json TEXT NOT NULL DEFAULT '',
    vars_json   TEXT NOT NULL DEFAULT '',
    created_by  TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    vpn_ip      TEXT NOT NULL DEFAULT '',
    mode        TEXT NOT NULL DEFAULT 'dev',
    protected   INTEGER NOT NULL DEFAULT 0,
    release_id  INTEGER REFERENCES releases (id) ON DELETE SET NULL,
    deploy_id   INTEGER REFERENCES deploys (id) ON DELETE SET NULL,
    owner       TEXT NOT NULL DEFAULT '',
    via         TEXT NOT NULL DEFAULT '',
    UNIQUE (app, name)
);

CREATE INDEX envs_app ON envs (app, id);

CREATE TABLE env_resources (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    env_id     INTEGER NOT NULL REFERENCES envs (id) ON DELETE CASCADE,
    kind       TEXT NOT NULL,
    name       TEXT NOT NULL,
    dep        TEXT NOT NULL DEFAULT '',
    port       INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    service    TEXT NOT NULL DEFAULT '',
    replica    INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX env_resources_env ON env_resources (env_id, id);

CREATE TABLE env_services (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    env_id         INTEGER NOT NULL REFERENCES envs (id) ON DELETE CASCADE,
    name           TEXT NOT NULL,
    image          TEXT NOT NULL DEFAULT '',
    container      TEXT NOT NULL DEFAULT '',
    port           INTEGER NOT NULL DEFAULT 0,
    container_port INTEGER NOT NULL DEFAULT 0,
    protocol       TEXT NOT NULL DEFAULT 'tcp',
    command_json   TEXT NOT NULL DEFAULT '',
    vars_json      TEXT NOT NULL DEFAULT '',
    health_json    TEXT NOT NULL DEFAULT '',
    status         TEXT NOT NULL,
    updated_at     TEXT NOT NULL,
    replicas       INTEGER NOT NULL DEFAULT 1,
    UNIQUE (env_id, name)
);

CREATE INDEX env_services_env ON env_services (env_id, id);

CREATE UNIQUE INDEX env_resources_identity
    ON env_resources (env_id, kind, name, dep, service);

CREATE TABLE peers (
    name           TEXT PRIMARY KEY,
    public_key     TEXT NOT NULL UNIQUE,
    ip             TEXT NOT NULL UNIQUE,
    added_by       TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL,
    last_handshake TEXT NOT NULL DEFAULT ''
);

CREATE UNIQUE INDEX envs_vpn_ip ON envs (vpn_ip) WHERE vpn_ip <> '';

CREATE TABLE vpn (
    id          INTEGER PRIMARY KEY CHECK (id = 1),
    private_key TEXT NOT NULL,
    subnet      TEXT NOT NULL,
    listen      TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);

CREATE TABLE routes (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    env_id     INTEGER NOT NULL REFERENCES envs (id) ON DELETE CASCADE,
    service    TEXT NOT NULL,
    host       TEXT NOT NULL UNIQUE,
    kind       TEXT NOT NULL DEFAULT 'https',
    created_at TEXT NOT NULL,
    managed    INTEGER NOT NULL DEFAULT 1
);

CREATE INDEX routes_env ON routes (env_id, id);

CREATE TABLE edge_targets (
    route_id   INTEGER NOT NULL REFERENCES routes (id) ON DELETE CASCADE,
    replica    INTEGER NOT NULL,
    port       INTEGER NOT NULL,
    state      TEXT NOT NULL,
    inflight   INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (route_id, replica)
);

CREATE TABLE releases (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    app         TEXT NOT NULL REFERENCES apps (name) ON DELETE CASCADE,
    "commit"    TEXT NOT NULL,
    tree        TEXT NOT NULL,
    ref         TEXT NOT NULL DEFAULT '',
    config_json TEXT NOT NULL DEFAULT '',
    images_json TEXT NOT NULL DEFAULT '',
    built_by    TEXT NOT NULL DEFAULT '',
    built_at    TEXT NOT NULL,
    machine     TEXT NOT NULL DEFAULT '',
    UNIQUE (app, tree)
);

CREATE INDEX releases_app ON releases (app, id DESC);

CREATE TABLE deploys (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    env_id          INTEGER NOT NULL REFERENCES envs (id) ON DELETE CASCADE,
    release_id      INTEGER REFERENCES releases (id) ON DELETE SET NULL,
    from_release_id INTEGER REFERENCES releases (id) ON DELETE SET NULL,
    kind            TEXT NOT NULL DEFAULT 'deploy',
    status          TEXT NOT NULL,
    reason          TEXT NOT NULL DEFAULT '',
    identity        TEXT NOT NULL DEFAULT '',
    started_at      TEXT NOT NULL,
    finished_at     TEXT NOT NULL DEFAULT '',
    error           TEXT NOT NULL DEFAULT ''
);

CREATE INDEX deploys_env ON deploys (env_id, id DESC);

CREATE TABLE vault (
    scope      TEXT NOT NULL,
    app        TEXT NOT NULL DEFAULT '',
    env        TEXT NOT NULL DEFAULT '',
    name       TEXT NOT NULL,
    ciphertext BLOB NOT NULL,
    nonce      BLOB NOT NULL,
    version    INTEGER NOT NULL DEFAULT 1,
    updated_by TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL,
    PRIMARY KEY (scope, app, env, name)
);

CREATE TABLE env_events (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    env_id   INTEGER REFERENCES envs (id) ON DELETE CASCADE,
    app      TEXT NOT NULL DEFAULT '',
    env      TEXT NOT NULL DEFAULT '',
    action   TEXT NOT NULL,
    status   TEXT NOT NULL,
    detail   TEXT NOT NULL DEFAULT '',
    identity TEXT NOT NULL DEFAULT '',
    service  TEXT NOT NULL DEFAULT '',
    replica  INTEGER NOT NULL DEFAULT 0,
    step     TEXT NOT NULL DEFAULT '',
    json     TEXT NOT NULL DEFAULT '',
    at       TEXT NOT NULL,
    machine  TEXT NOT NULL DEFAULT ''
);

CREATE INDEX env_events_env ON env_events (env_id, id DESC);

CREATE INDEX env_events_at ON env_events (at);

CREATE INDEX env_events_app ON env_events (app, id DESC);

CREATE TABLE machines (
    name       TEXT PRIMARY KEY,
    role       TEXT NOT NULL,
    public_key TEXT NOT NULL UNIQUE,
    subnet     TEXT NOT NULL UNIQUE,
    arch       TEXT NOT NULL DEFAULT '',
    os         TEXT NOT NULL DEFAULT '',
    endpoint   TEXT NOT NULL DEFAULT '',
    private    INTEGER NOT NULL DEFAULT 0,
    joined_at  TEXT NOT NULL,
    last_seen  TEXT NOT NULL DEFAULT '',
    gauge_json TEXT NOT NULL DEFAULT ''
);

CREATE INDEX machines_role ON machines (role, name);

CREATE TABLE env_directory (
    app        TEXT NOT NULL,
    env        TEXT NOT NULL,
    machine    TEXT NOT NULL,
    address    TEXT NOT NULL DEFAULT '',
    owner      TEXT NOT NULL DEFAULT '',
    mode       TEXT NOT NULL DEFAULT '',
    via        TEXT NOT NULL DEFAULT '',
    hosts      TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (app, env)
);

CREATE INDEX env_directory_machine ON env_directory (machine, app, env);

CREATE TABLE release_images (
    release_id INTEGER NOT NULL REFERENCES releases (id) ON DELETE CASCADE,
    service    TEXT NOT NULL,
    arch       TEXT NOT NULL,
    machine    TEXT NOT NULL,
    image_id   TEXT NOT NULL DEFAULT '',
    built_at   TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (release_id, service, arch, machine)
);

CREATE INDEX release_images_release ON release_images (release_id, arch);

CREATE TABLE join_tokens (
    token_hash TEXT PRIMARY KEY,
    created_by TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    used_by    TEXT NOT NULL DEFAULT '',
    used_at    TEXT NOT NULL DEFAULT ''
);
