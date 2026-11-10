#!/usr/bin/env bash
# Builds a throwaway contracts repo: example/v1 committed as HEAD,
# example/v2 applied to the working tree as the "proposed" change.
set -euo pipefail
here="$(cd "$(dirname "$0")/.." && pwd)"
demo="${1:-/tmp/selvage-demo}"
rm -rf "$demo" && mkdir -p "$demo"
cp -R "$here/example/v1/." "$demo/"
git -C "$demo" init -q
git -C "$demo" add -A
git -C "$demo" -c user.name=selvage -c user.email=selvage@localhost commit -qm "v1 contracts"
cp -R "$here/example/v2/." "$demo/"
echo "demo repo: $demo (HEAD = v1, working tree = v2)"
echo "  go run . check -repo $demo -base HEAD"
echo "  go run . serve -repo $demo"
