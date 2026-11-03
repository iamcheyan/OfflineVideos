#!/bin/bash
set -euo pipefail

ROOT="$(cd -- "$(dirname -- "$0")/.." && pwd)"
cd "$ROOT"

exec 9>"${XDG_RUNTIME_DIR:-/run/user/$(id -u)}/offlinevideos-publish.lock"
flock -n 9 || exit 0

branch="$(git branch --show-current)"
[ "$branch" = main ] || { echo "Refusing to publish from branch: $branch" >&2; exit 1; }
origin="$(git remote get-url origin)"
case "$origin" in
  *github*:*OfflineVideos.git*) ;;
  *) echo "Refusing unexpected origin" >&2; exit 1 ;;
esac
python3 "$ROOT/docs/refresh_catalog.py"
git add -- docs/data

if ! git diff --cached --quiet -- docs/data; then
  git commit -m "Refresh published OfflineVideos catalog"
  git push origin main
else
  echo "Published catalog is already current"
fi
