#!/usr/bin/env sh
# Installs the Argonmine node programs for the current user on Linux or macOS,
# in ~/.local/bin. Run from the unzipped release folder:
#   sh install.sh              (sh install.sh --uninstall to remove)
set -eu
SRC="$(cd "$(dirname "$0")" && pwd)"
BIN="$HOME/.local/bin"
PROGRAMS="argonmined argonmine-wallet argonmine-explorer argonmine-miner genesis"

if [ "${1:-}" = "--uninstall" ]; then
  for c in $PROGRAMS argonmine-app; do rm -f "$BIN/$c"; done
  # Launchers written by earlier versions of this script.
  rm -f "$HOME/.local/share/applications/argonmine-miner.desktop" "$HOME/Applications/Argonmine Miner.command"
  echo "Argonmine removed (chain data and wallets in ~/.argonmine were kept)."
  exit 0
fi

mkdir -p "$BIN"
for c in $PROGRAMS; do
  [ -f "$SRC/$c" ] && install -m 755 "$SRC/$c" "$BIN/$c"
done

echo "Installed to $BIN"
case ":$PATH:" in *":$BIN:"*) ;; *) echo "Add to your shell profile:  export PATH=\"\$HOME/.local/bin:\$PATH\"";; esac
echo "Start the node with:  argonmined"
