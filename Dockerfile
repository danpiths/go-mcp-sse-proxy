# syntax=docker/dockerfile:1
### Builder Stage ###
FROM golang:1.24.2-alpine AS builder
WORKDIR /app

# Install build deps
RUN apk add --no-cache git ca-certificates tzdata

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source & build static binary
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags='-w -s -extldflags=-static' \
    -o proxy ./cmd/proxy

### Final Stage ###
FROM alpine:latest
WORKDIR /app

# Install only Docker CLI and essential tools
RUN apk add --no-cache \
    docker-cli \
    ca-certificates \
    tzdata \
    nodejs \
    npm \
    && npm install -g npm@latest \
    && npm install -g supergateway@latest

# Copy necessary files from builder
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=builder /app/proxy /app/proxy

EXPOSE 8000

ENTRYPOINT ["/app/proxy"]
