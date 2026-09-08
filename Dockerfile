# syntax=docker/dockerfile:1
ARG GO_VERSION=1.26
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=1.0.0-rc.1
ARG COMMIT=development
ARG BUILD_DATE=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w -X github.com/DKPlugins/TokenResetsMonitor/internal/buildinfo.Version=${VERSION} -X github.com/DKPlugins/TokenResetsMonitor/internal/buildinfo.Commit=${COMMIT} -X github.com/DKPlugins/TokenResetsMonitor/internal/buildinfo.Date=${BUILD_DATE}" \
    -o /out/tokenresetsmonitor ./cmd/tokenresetsmonitor

FROM alpine:3.24
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -g 10001 monitor \
    && adduser -D -H -u 10001 -G monitor monitor \
    && mkdir -p /data /config \
    && chown 10001:10001 /data
COPY --from=build /out/tokenresetsmonitor /usr/local/bin/tokenresetsmonitor
USER 10001:10001
WORKDIR /data
ENV TRM_STATE_PATH=/data/state.db \
    TRM_LOGGING_FORMAT=json \
    TRM_LOGGING_FILE_ENABLED=false
VOLUME ["/data"]
ENTRYPOINT ["/usr/local/bin/tokenresetsmonitor"]
CMD ["run", "--config", "/config/config.yaml"]
