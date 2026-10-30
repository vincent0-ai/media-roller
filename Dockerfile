# ==========================================
# Stage 1: Build Go Binary
# ==========================================
FROM golang:1.26.4-alpine3.24 AS go-builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY src src
COPY templates templates

RUN CGO_ENABLED=0 go build -o media-roller ./src


# ==========================================
# Stage 2: Build Python Wheels & C Extensions
# ==========================================
FROM python:3.13-alpine AS py-builder

RUN apk add --no-cache gcc musl-dev python3-dev libffi-dev

WORKDIR /install
RUN pip install --prefix=/install/wheels --no-cache-dir curl_cffi --upgrade --pre
RUN pip install --prefix=/install/wheels --no-cache-dir -U yt-dlp-ejs


# ==========================================
# Stage 3: Final Minimal Runtime Image
# ==========================================
FROM python:3.13-alpine

ENV MR_DOWNLOAD_DIR="/download"
ENV PORT="3000"

# Install ONLY necessary runtime packages (no compilers/dev headers)
RUN apk add --update --no-cache \
    deno \
    curl \
    ca-certificates

# Copy static binaries & built artifacts
COPY --from=mwader/static-ffmpeg:8.0 /ffmpeg /usr/local/bin/
COPY --from=mwader/static-ffmpeg:8.0 /ffprobe /usr/local/bin/
COPY --from=go-builder /app/media-roller /app/media-roller
COPY --from=py-builder /install/wheels /usr/local

COPY templates /app/templates
COPY static /app/static

WORKDIR /app
RUN mkdir -p /download

# Download & verify standalone yt-dlp executable
RUN curl -L https://github.com/yt-dlp/yt-dlp/releases/download/2026.03.17/yt-dlp -o /usr/local/bin/yt-dlp && \
    echo "3bda0968a01cde70d26720653003b28553c71be14dcb2e5f4c24e9921fdad745  /usr/local/bin/yt-dlp" | sha256sum -c - && \
    chmod a+rx /usr/local/bin/yt-dlp && \
    yt-dlp --update --update-to nightly

# Sanity check
RUN yt-dlp --version && ffmpeg -version

EXPOSE 3000

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD curl -f http://localhost:${PORT:-3000}/health || exit 1

ENTRYPOINT ["/app/media-roller"]