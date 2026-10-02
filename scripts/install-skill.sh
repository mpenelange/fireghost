#!/usr/bin/env bash
# Installs the Fireghost agent skill for the current user: copies it out of the
# checkout (so switching branches never changes the installed copy) and links
# it into Claude Code, the shared Agent Skills directory used by pi, and PATH.
# Re-run after updating skills/fireghost.
set -euo pipefail

source_dir="$(cd "$(dirname "$0")/../skills/fireghost" && pwd)"
dest="$HOME/.local/share/fireghost/skills/fireghost"

rm -rf "$dest"
mkdir -p "$(dirname "$dest")"
cp -R "$source_dir" "$dest"
chmod +x "$dest/bin/fireghost"

for skills_dir in "$HOME/.claude/skills" "$HOME/.agents/skills"; do
  mkdir -p "$skills_dir"
  ln -sfn "$dest" "$skills_dir/fireghost"
done
mkdir -p "$HOME/.local/bin"
ln -sfn "$dest/bin/fireghost" "$HOME/.local/bin/fireghost"

echo "Installed $dest"
command -v firecrawl >/dev/null || echo "Next: npm install -g firecrawl-cli"
security find-generic-password -s fireghost-api >/dev/null 2>&1 ||
  echo "Next: store the router key in the Keychain item 'fireghost-api' (or set FIREGHOST_API_KEY)"
