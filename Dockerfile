# syntax=docker/dockerfile:1

# ---- build ----
FROM golang:1.26-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Pure-Go SQLite driver, so a fully static binary works without CGO.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/catalogsync ./cmd/catalogsync

# ---- run ----
# Static distroless image: no shell, no package manager. It runs as root so the
# app can write to the mounted volume at /data.
FROM gcr.io/distroless/static-debian12
COPY --from=build /out/catalogsync /catalogsync

EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["/catalogsync", "-addr", "0.0.0.0:8080", "-db", "/data/catalog.db"]
