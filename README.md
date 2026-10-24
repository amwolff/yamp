# YAMP — Yet Another Media Player

A single-binary video server. Point it at a library and it serves the files as a browsable grid. Files a browser can play are served as they are; the rest are transcoded once in the background and kept.

## Run

```sh
go build -o yamp ./cmd/yamp
./yamp -library /path/to/videos
```

Needs `ffmpeg` and `ffprobe`. Open <http://127.0.0.1:8080> and create the first account (every account is an admin).

## Deploy

```sh
# Set the host paths in deploy/compose.yaml first.
RENDER_GID=$(getent group render | cut -d: -f3) docker compose -f deploy/compose.yaml up -d --build
tailscale serve --bg 8080
```

The image ships Debian's ffmpeg with the Intel media driver and gets `/dev/dri/renderD128` for QSV/VAAPI. Admin → Transcoding shows which encoders work on the host.

## Configuration

Flags, or `YAMP_*` environment variables (`-cache-dir` → `YAMP_CACHE_DIR`):

| Flag | Default | |
|---|---|---|
| `-library` | required | Library root. Repeatable; `:`-separated in the environment. Never written to. |
| `-listen` | `127.0.0.1:8080` | |
| `-data-dir` | `./data` | SQLite database. |
| `-cache-dir` | `<data-dir>/cache` | Thumbnails, subtitles, transcodes. All of it can be regenerated. |
| `-ffmpeg`, `-ffprobe` | from `PATH` | |
| `-vaapi-device` | `/dev/dri/renderD128` | |
| `-cookie-secure` | `true` | Set to `false` for plain HTTP on anything other than localhost. |
| `-fsync` | `false` | See [Storage](#storage). |
| `-log-level` | `info` | |

Transcode profiles, parallel jobs and the scan interval are set in the admin panel.

## Transcoding

Every file is classified when it is scanned:

- **direct** (H.264 + AAC in MP4): served as is.
- **conditional** (HEVC, AV1, VP9): served to browsers that can decode it, plus an H.264 copy for the rest.
- **remux** (compatible codecs, wrong container): streams copied into MP4.
- **audio** (compatible video, AC-3/DTS audio): audio re-encoded to AAC.
- **transcode** (Hi10P, MPEG-2, interlaced, HDR): full encode to H.264, with HDR tone-mapped to SDR.

The player offers every version and the browser picks the first one it can decode. Editing a profile marks its transcodes stale; they keep playing until they are redone.

## Storage

No fsync by default: the cache can be regenerated, and on ZFS fsyncing large files is slow. Damaged transcodes are detected at startup and redone. On ZFS, give the cache its own dataset (`recordsize=1M`, no snapshots) and the data directory a small `recordsize` (e.g. 16K).

## Development

```sh
go test -race ./...                        # integration tests need ffmpeg; -short skips them
golangci-lint run
sqlc generate                              # after changing internal/store/queries or migrations
scripts/make-fixtures.sh testdata/lib 30   # sample library: one file per class
```

## Not done yet

Live (HLS) transcoding, seek previews, burned-in image subtitles, hardware tone mapping.
