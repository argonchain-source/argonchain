# Seed node image: builds argonmined (and the explorer) from source, runs argonmined.
# Pure Go, no cgo, so the runtime image is a tiny static Alpine.
#
#   docker build -f deploy/Dockerfile -t argonmine/node .
#   docker run -d --name seed -p 9333:9333 -v argonmine-data:/data argonmine/node -network mainnet

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/argonmined ./cmd/argonmined \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/argonmine-explorer ./cmd/argonmine-explorer

FROM alpine:3.20
RUN adduser -D -h /data argonmine && apk add --no-cache ca-certificates
COPY --from=build /out/ /usr/local/bin/
USER argonmine
VOLUME /data
# P2P ports (mainnet, testnet), RPC stays on localhost inside the container.
EXPOSE 9333 19333
ENTRYPOINT ["argonmined", "-datadir", "/data"]
CMD ["-network", "mainnet"]
