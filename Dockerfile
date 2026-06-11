# ── Build stage ───────────────────────────────────────────────────────────────
FROM golang:1.22-alpine AS builder

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /build

COPY app/go.mod app/go.sum ./
RUN go mod download

COPY app/ .

# Build-time metadata (override at `docker build` with --build-arg)
ARG COMMIT_SHA=dev
ARG BUILD_VERSION=dev
ARG BUILD_TIME

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    BUILD_TIME=${BUILD_TIME:-$(date -u +%Y-%m-%dT%H:%M:%SZ)} && \
    go build -trimpath \
      -ldflags="-s -w \
        -X 'main.BuildCommit=${COMMIT_SHA}' \
        -X 'main.BuildTime=${BUILD_TIME}' \
        -X 'main.BuildVersion=${BUILD_VERSION}'" \
      -o shipyard .

# ── Runtime stage — scratch keeps the image to ~6 MB ──────────────────────────
FROM scratch

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=builder /build/shipyard /shipyard

EXPOSE 8080

USER 65534:65534

ENTRYPOINT ["/shipyard"]
