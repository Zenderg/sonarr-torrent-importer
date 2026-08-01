# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e

FROM golang:1.25-alpine@sha256:56961d79ea8129efddcc0b8643fd8a5416b4e6228cfd477e3fd61deb2672c587 AS dependencies
WORKDIR /src
COPY go.mod ./
RUN go mod download

FROM dependencies AS build
ARG VERSION=dev
ARG COMMIT_SHA=unknown
ARG BUILD_TIME=unknown
COPY cmd ./cmd
COPY internal ./internal
RUN go test ./... \
    && go vet ./... \
    && CGO_ENABLED=0 go build \
      -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT_SHA} -X main.buildTime=${BUILD_TIME}" \
      -o /out/sonarr-torrent-importer ./cmd/importer

FROM alpine:3.23@sha256:fd791d74b68913cbb027c6546007b3f0d3bc45125f797758156952bc2d6daf40 AS runtime
ARG VERSION=dev
ARG COMMIT_SHA=unknown
ARG BUILD_TIME=unknown
LABEL org.opencontainers.image.title="sonarr-torrent-importer" \
      org.opencontainers.image.description="Conservative explicit importer for Sonarr and qBittorrent" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT_SHA}" \
      org.opencontainers.image.created="${BUILD_TIME}" \
      org.opencontainers.image.source="https://github.com/zenderg/sonarr-torrent-importer"
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S -g 10001 importer \
    && adduser -S -D -H -u 10001 -G importer importer \
    && mkdir -p /data \
    && chown importer:importer /data
COPY --from=build /out/sonarr-torrent-importer /usr/local/bin/sonarr-torrent-importer
USER 10001:10001
VOLUME ["/data"]
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:8080/health || exit 1
ENTRYPOINT ["/usr/local/bin/sonarr-torrent-importer"]
CMD ["serve"]
