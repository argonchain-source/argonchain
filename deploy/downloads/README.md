# Release downloads

The node release archives are served at `https://docs.argonmine.com/downloads/<file>`:
the website (`argonmine-web -downloads-upstream`, Railway variable
`DOWNLOADS_UPSTREAM=http://downloads.railway.internal:8080`) forwards those
requests to the Railway service `downloads`, built from this folder.

To publish a release:

1. `scripts/release.ps1 -Version <v>` (or `scripts/release.sh <v>`) writes the
   zips and `SHA256SUMS` to `release/`.
2. Copy this folder and the release files into a staging folder outside the
   repository, with the archives under `files/`, and deploy it:
   `railway up <staging> -s downloads -d`.
3. Update the version in the file names in `docs/NODE.md` ("Download"). The
   checksums live only in `SHA256SUMS`, never in the docs, because the docs
   ship inside the archives.
