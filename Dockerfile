# --- Build Stage ---
FROM golang:1.23-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /app

# Copy source and resolve dependencies
COPY . .
RUN go mod tidy

# Build both binaries
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-s -w" \
    -o /bin/api ./cmd/api && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-s -w" \
    -o /bin/worker ./cmd/worker

# --- API Runtime ---
FROM alpine:3.19 AS api

RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /bin/api /bin/api

EXPOSE 8080 9090
ENTRYPOINT ["/bin/api"]

# --- Worker Runtime ---
FROM alpine:3.19 AS worker

RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /bin/worker /bin/worker

EXPOSE 9090
ENTRYPOINT ["/bin/worker"]
