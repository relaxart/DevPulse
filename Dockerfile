# syntax=docker/dockerfile:1

# ---------- build stage ----------
FROM golang:1.25-alpine AS build

WORKDIR /src

# Download modules first so dependency layers stay cached across code changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
# CGO is off so the binary runs on a minimal runtime image.
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags="-s -w -X main.version=${VERSION}" \
        -o /out/devpulse ./cmd/server

# ---------- runtime stage ----------
# Only the static binary and CA certificates ship; no Go toolchain.
FROM alpine:3.21 AS runtime

RUN apk add --no-cache ca-certificates tzdata wget \
 && adduser -D -u 10001 devpulse

COPY --from=build /out/devpulse /usr/local/bin/devpulse

USER devpulse
WORKDIR /home/devpulse

ENV LISTEN_ADDR=:8080
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/health >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/devpulse"]
