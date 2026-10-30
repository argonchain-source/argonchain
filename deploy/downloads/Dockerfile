# The node release archives, served on the private network to the website
# (argonmine-web -downloads-upstream). Deployed from a staging folder that
# holds this file, main.go, go.mod and files/ (the zips and SHA256SUMS):
# see deploy/downloads/README.md.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /downloads .

FROM alpine:3.20
RUN adduser -D -H app
COPY --from=build /downloads /usr/local/bin/downloads
COPY files /srv/files
WORKDIR /srv
USER app
ENTRYPOINT ["downloads"]
