# --- Build Stage ---
FROM golang:1.23-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /app

# Cache Go modules (only re-downloaded when go.mod/go.sum change)
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build API binary
FROM builder AS build-api
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-s -w" \
    -o /bin/api ./cmd/api

# Build Worker binary
FROM builder AS build-worker
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-s -w" \
    -o /bin/worker ./cmd/worker

# --- API Runtime ---
FROM alpine:3.19 AS api

RUN apk add --no-cache ca-certificates tzdata
COPY --from=build-api /bin/api /bin/api

EXPOSE 8080 9090
ENTRYPOINT ["/bin/api"]

# --- Worker Runtime ---
FROM alpine:3.19 AS worker

RUN apk add --no-cache ca-certificates tzdata
COPY --from=build-worker /bin/worker /bin/worker

EXPOSE 9090
ENTRYPOINT ["/bin/worker"]
