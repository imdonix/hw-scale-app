# syntax=docker/dockerfile:1

# ---- web: build the React PWA ------------------------------------------------
FROM node:22-alpine AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run lint && npm run build

# ---- go: test and build the single binary (web app embedded) -----------------
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY --from=web /web/dist ./internal/webui/dist
ARG VERSION=dev
RUN go vet ./... \
 && go test ./... \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/sscale ./cmd/sscale

# ---- runtime -------------------------------------------------------------------
FROM alpine:3.22
RUN apk add --no-cache tzdata
COPY --from=build /out/sscale /usr/local/bin/sscale
# sscale talks to the host's bluetoothd through the system D-Bus socket,
# which docker-compose.yml bind-mounts into the container.
ENV SSCALE_KEYS=/config/keys.json \
    SSCALE_DATA=/data \
    SSCALE_HTTP=:8080 \
    DBUS_SYSTEM_BUS_ADDRESS=unix:path=/run/dbus/system_bus_socket
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s \
  CMD ["sscale", "healthcheck"]
ENTRYPOINT ["sscale"]
# The container is the "background": keep the daemon as the foreground process.
CMD ["daemon", "--foreground"]
