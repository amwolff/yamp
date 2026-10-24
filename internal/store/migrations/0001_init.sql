-- All timestamps are Unix seconds unless the column name says otherwise.

CREATE TABLE users (
    id INTEGER PRIMARY KEY,
    username TEXT NOT NULL UNIQUE COLLATE NOCASE,
    pw_hash TEXT NOT NULL,
    discreet INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL
);

CREATE TABLE sessions (
    token_hash BLOB PRIMARY KEY,
    user_id INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);

CREATE INDEX sessions_user ON sessions (user_id);

CREATE TABLE settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE media (
    id INTEGER PRIMARY KEY,
    path TEXT NOT NULL UNIQUE,
    root TEXT NOT NULL,
    rel_dir TEXT NOT NULL,
    title TEXT NOT NULL,
    size INTEGER NOT NULL,
    mtime_ns INTEGER NOT NULL,
    fingerprint BLOB NOT NULL UNIQUE,
    pid TEXT NOT NULL UNIQUE,
    duration_ms INTEGER NOT NULL,
    container TEXT NOT NULL,
    width INTEGER NOT NULL,
    height INTEGER NOT NULL,
    video_codec TEXT NOT NULL,
    audio_codec TEXT NOT NULL,
    mime TEXT NOT NULL,
    codecs TEXT NOT NULL,
    hdr INTEGER NOT NULL,
    playability TEXT NOT NULL,
    probe_json TEXT NOT NULL,
    has_thumb INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL DEFAULT 'ok',
    added_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE INDEX media_dir ON media (root, rel_dir);

CREATE TABLE profiles (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    params_json TEXT NOT NULL,
    params_hash TEXT NOT NULL,
    auto_policy TEXT NOT NULL,
    priority INTEGER NOT NULL DEFAULT 0,
    builtin INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE renditions (
    id INTEGER PRIMARY KEY,
    media_id INTEGER NOT NULL REFERENCES media (id) ON DELETE CASCADE,
    profile_id INTEGER NOT NULL REFERENCES profiles (id) ON DELETE CASCADE,
    params_hash TEXT NOT NULL,
    path TEXT NOT NULL UNIQUE,
    size INTEGER NOT NULL,
    mime TEXT NOT NULL,
    codecs TEXT NOT NULL,
    width INTEGER NOT NULL,
    height INTEGER NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE INDEX renditions_media ON renditions (media_id);

CREATE TABLE subtitles (
    id INTEGER PRIMARY KEY,
    media_id INTEGER NOT NULL REFERENCES media (id) ON DELETE CASCADE,
    stream_index INTEGER NOT NULL,
    lang TEXT NOT NULL,
    label TEXT NOT NULL,
    path TEXT NOT NULL,
    UNIQUE (media_id, stream_index)
);

CREATE TABLE jobs (
    id INTEGER PRIMARY KEY,
    media_id INTEGER NOT NULL REFERENCES media (id) ON DELETE CASCADE,
    profile_id INTEGER NOT NULL REFERENCES profiles (id) ON DELETE CASCADE,
    state TEXT NOT NULL,
    priority INTEGER NOT NULL DEFAULT 0,
    progress REAL NOT NULL DEFAULT 0,
    error TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    started_at INTEGER,
    finished_at INTEGER
);

CREATE INDEX jobs_queue ON jobs (state, priority DESC, id);

-- At most one queued or running job for each (media, profile) pair.
CREATE UNIQUE INDEX jobs_active ON jobs (media_id, profile_id)
WHERE state IN ('queued', 'running');

CREATE TABLE playback (
    user_id INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    media_id INTEGER NOT NULL REFERENCES media (id) ON DELETE CASCADE,
    position_ms INTEGER NOT NULL,
    completed INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (user_id, media_id)
);

CREATE TABLE play_events (
    id INTEGER PRIMARY KEY,
    play_key TEXT NOT NULL UNIQUE,
    user_id INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    media_id INTEGER NOT NULL REFERENCES media (id) ON DELETE CASCADE,
    started_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    watched_ms INTEGER NOT NULL DEFAULT 0,
    counted INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX play_events_media ON play_events (media_id, counted);

CREATE TABLE ratings (
    user_id INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    media_id INTEGER NOT NULL REFERENCES media (id) ON DELETE CASCADE,
    stars INTEGER NOT NULL CHECK (stars BETWEEN 1 AND 5),
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (user_id, media_id)
);
