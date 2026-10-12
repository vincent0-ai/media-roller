FROM golang:1.26.4-alpine3.24 AS builder

RUN apk add --no-cache curl

WORKDIR /app

COPY src src
COPY templates templates
COPY go.mod go.mod
COPY go.sum go.sum

RUN go mod download
RUN CGO_ENABLED=0 go build -o media-roller ./src

# yt-dlp needs python. Pinned to stable 3.13 (3.15 alphas break pip/yt-dlp in prod).
FROM python:3.13-alpine

# This is where the downloaded files will be saved in the container.
ENV MR_DOWNLOAD_DIR="/download"
# Default listen port. Platforms like Railway/Render/Dokploy/Coolify inject
# PORT at runtime — the app respects MR_PORT first, then PORT (see src/main.go).
ENV PORT="3000"

RUN apk add --update --no-cache \
    # https://github.com/yt-dlp/yt-dlp/issues/14404 \
    deno \
    curl \
    gcc \
    musl-dev \
    python3-dev \
    libffi-dev

# https://hub.docker.com/r/mwader/static-ffmpeg/tags
# https://github.com/wader/static-ffmpeg
COPY --from=mwader/static-ffmpeg:8.0 /ffmpeg  /usr/local/bin/
COPY --from=mwader/static-ffmpeg:8.0 /ffprobe /usr/local/bin/
COPY --from=builder /app/media-roller /app/media-roller
COPY templates /app/templates
COPY static /app/static

WORKDIR /app

RUN mkdir -p /download

EXPOSE 3000

RUN pip install curl_cffi --upgrade --pre
RUN pip install -U yt-dlp-ejs

# Get new releases here https://github.com/yt-dlp/yt-dlp/releases
RUN curl -L https://github.com/yt-dlp/yt-dlp/releases/download/2026.03.17/yt-dlp -o /usr/local/bin/yt-dlp && \
    echo "3bda0968a01cde70d26720653003b28553c71be14dcb2e5f4c24e9921fdad745 /usr/local/bin/yt-dlp" | sha256sum -c - && \
    chmod a+rx /usr/local/bin/yt-dlp

RUN yt-dlp --update --update-to nightly

# Sanity check
RUN yt-dlp --version && \
    ffmpeg -version

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD curl -f http://localhost:${PORT:-3000}/health || exit 1

ENTRYPOINT ["/app/media-roller"]
