# One image for the whole public footprint on Railway (or any Docker host).
# Which program runs is chosen at start time by ARGONMINE_ROLE: node, explorer, web, bridge, swap or miner.
# See deploy/railway/README.md for the service layout and variables.
#
#   docker build -f deploy/railway/Dockerfile -t argonmine/stack .
#   docker run -e ARGONMINE_ROLE=node -e RPC_TOKEN=secret -v argonmine:/data -p 19333:19333 argonmine/stack

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# The browser miner and wallet (WebAssembly) are built here, with the loader
# of this Go release, so the website never embeds a stale copy.
RUN CGO_ENABLED=0 GOOS=js GOARCH=wasm go build -trimpath -ldflags="-s -w" -o cmd/argonmine-web/site/static/miner.wasm ./cmd/argonmine-webminer \
 && cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" cmd/argonmine-web/site/static/wasm_exec.js
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/argonmined ./cmd/argonmined \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/argonmine-explorer ./cmd/argonmine-explorer \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/argonmine-web ./cmd/argonmine-web \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/argonmine-bridge ./cmd/argonmine-bridge \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/argonmine-swap ./cmd/argonmine-swap \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/argonmine-miner ./cmd/argonmine-miner

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/ /usr/local/bin/
# The website renders these at runtime (argonmine-web -repo /srv).
COPY README.md SECURITY.md /srv/
COPY docs /srv/docs
COPY deploy/README.md /srv/deploy/README.md
COPY deploy/railway/start.sh /usr/local/bin/argonmine-start
# No VOLUME instruction: Railway rejects it; the /data volume is attached in the service settings.
RUN chmod +x /usr/local/bin/argonmine-start && mkdir -p /data
ENTRYPOINT ["argonmine-start"]
