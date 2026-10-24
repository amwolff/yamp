# syntax=docker/dockerfile:1
# Build from the repository root:
#   docker buildx build --platform linux/amd64 -f deploy/Dockerfile -t yamp .

FROM --platform=$BUILDPLATFORM golang:1.27-trixie AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/yamp ./cmd/yamp

FROM debian:trixie-slim
# ffmpeg 7.1 from Debian has VAAPI, QSV (libvpl), zscale and libplacebo.
# On amd64 the Intel media driver (non-free) and the oneVPL GPU runtime give
# hardware encoding on Intel iGPUs such as the UHD 730.
RUN sed -i 's/^Components: main$/Components: main non-free non-free-firmware/' /etc/apt/sources.list.d/debian.sources \
 && apt-get update \
 && apt-get install -y --no-install-recommends ffmpeg ca-certificates tzdata \
 && if [ "$(dpkg --print-architecture)" = "amd64" ]; then \
      apt-get install -y --no-install-recommends intel-media-va-driver-non-free libmfx-gen1.2 vainfo; \
    fi \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --system --uid 10001 --home-dir /data --shell /usr/sbin/nologin yamp \
 && mkdir -p /data /cache /media && chown yamp:yamp /data /cache

COPY --from=build /out/yamp /usr/local/bin/yamp

ENV YAMP_LISTEN=0.0.0.0:8080 \
    YAMP_DATA_DIR=/data \
    YAMP_CACHE_DIR=/cache \
    YAMP_LIBRARY=/media \
    LIBVA_DRIVER_NAME=iHD
USER yamp
EXPOSE 8080
VOLUME ["/data", "/cache"]
ENTRYPOINT ["/usr/local/bin/yamp"]
