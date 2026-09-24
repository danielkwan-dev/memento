# Multi-stage: build both binaries once, then ship each in its own tiny image.
FROM golang:1.27-alpine AS build

WORKDIR /src

# Copy manifests first so dependency download is cached independently of source
# changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO is off so the binaries are static and run on a scratch/alpine base.
# Trimpath and stripped symbols keep the images small.
ENV CGO_ENABLED=0 GOOS=linux
RUN go build -trimpath -ldflags="-s -w" -o /out/api    ./cmd/api && \
    go build -trimpath -ldflags="-s -w" -o /out/worker ./cmd/worker

# --- api ---
FROM alpine:3.21 AS api
# ca-certificates is required for outbound TLS; tzdata so timestamps localise.
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
