# Multi-stage: build the frontend, then both Go binaries, then ship each in its
# own tiny image.

# --- frontend ---
FROM node:22-alpine AS web
WORKDIR /web
# Copy manifests first so npm ci is cached independently of source changes.
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# --- go ---
FROM golang:1.27-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ ./cmd/
COPY internal/ ./internal/

# The api binary serves the built frontend as well as the API, so there is one
# deploy and one origin. The embedfrontend tag activates cmd/api/embed.go, which
# needs this directory to exist.
COPY --from=web /web/dist ./cmd/api/dist

ENV CGO_ENABLED=0 GOOS=linux
RUN go build -tags embedfrontend -trimpath -ldflags="-s -w" -o /out/api ./cmd/api && \
    go build -trimpath -ldflags="-s -w" -o /out/worker ./cmd/worker

# --- api (serves the app and the API) ---
FROM alpine:3.21 AS api
RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -u 10001 app
COPY --from=build /out/api /usr/local/bin/api
USER app
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/api"]

# --- worker ---
FROM alpine:3.21 AS worker
RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -u 10001 app
COPY --from=build /out/worker /usr/local/bin/worker
USER app
EXPOSE 8081
ENTRYPOINT ["/usr/local/bin/worker"]
