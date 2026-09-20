# syntax=docker/dockerfile:1

# --- build stage ---------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

WORKDIR /src

# Dependencies first, so source edits do not invalidate the module cache.
COPY go.mod go.sum* ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
      -ldflags "-s -w \
        -X main.Version=${VERSION} \
        -X main.Commit=${COMMIT} \
        -X main.BuildDate=${BUILD_DATE}" \
      -o /out/mirrorpilot ./cmd/mirrorpilot

# --- runtime stage -------------------------------------------------------
# Deliberately NOT distroless: those images live on gcr.io, which is
# unreachable from many of the networks this project exists to serve.
FROM alpine:3.21

RUN apk --no-cache add ca-certificates tzdata \
 && addgroup -g 10001 -S mirrorpilot \
 && adduser  -u 10001 -S -G mirrorpilot -h /data -s /sbin/nologin mirrorpilot \
 && mkdir -p /data \
 && chown mirrorpilot:mirrorpilot /data

COPY --from=build /out/mirrorpilot /usr/local/bin/mirrorpilot

VOLUME ["/data"]
EXPOSE 8080

# Never run as root inside the container.
USER mirrorpilot

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/mirrorpilot"]
