FROM golang:1.26-alpine3.23 AS build
RUN apk add --no-cache build-base pkgconf opus-dev opusfile-dev
WORKDIR /src
COPY voice/go.mod voice/go.sum ./
RUN go mod download
COPY voice/ ./
RUN CGO_ENABLED=1 GOMAXPROCS=2 go build -p=2 -mod=readonly -trimpath -o /out/stackchan-server .

FROM alpine:3.23
RUN apk add --no-cache bash ca-certificates opus opusfile libstdc++
WORKDIR /app
COPY --from=build /out/stackchan-server /app/stackchan-server
COPY install/standalone.sh /standalone.sh
COPY install/voice-entrypoint.sh /voice-entrypoint.sh
RUN chmod 755 /standalone.sh /voice-entrypoint.sh && mkdir -p /app/manifest/config /data
ENTRYPOINT ["/voice-entrypoint.sh"]
