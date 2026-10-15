#!/usr/bin/env bash
# Assemble the GitHub Pages artifact:
#   website/ static files at / (excluding Dockerfile and nginx.conf)
#   Astro docs-site/dist at /docs/
#   CNAME → agentic.hosting
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
out="${1:-"$root/pages-dist"}"
docs_dist="${DOCS_DIST:-"$root/docs-site/dist"}"
website_dir="$root/website"

if [[ ! -d "$docs_dist" ]]; then
  echo "error: docs build not found at $docs_dist (run npm run build in docs-site/ first)" >&2
  exit 1
fi
if [[ ! -f "$website_dir/index.html" ]]; then
  echo "error: website/index.html not found" >&2
  exit 1
fi

rm -rf "$out"
mkdir -p "$out"

# Homepage and static assets at the Pages root. Keep server-only files off the artifact.
while IFS= read -r -d '' item; do
  name="$(basename "$item")"
  case "$name" in
    Dockerfile|nginx.conf) continue ;;
  esac
  cp -a "$item" "$out/"
done < <(find "$website_dir" -mindepth 1 -maxdepth 1 -print0)

# Preserve the existing apex URLs that the website image already served.
cp -a "$root/llms.txt" "$out/llms.txt"
cp -a "$root/llms-full.txt" "$out/llms-full.txt"

mkdir -p "$out/docs"
cp -a "$docs_dist"/. "$out/docs/"

printf '%s\n' 'agentic.hosting' > "$out/CNAME"
# Actions Pages does not run Jekyll, but a CNAME + underscore assets is safer with this present.
touch "$out/.nojekyll"

fail=0
for path in \
  index.html \
  changelog/index.html \
  dashboard/index.html \
  dashboard/app.js \
  install.sh \
  install-cli.sh \
  install-skill.sh \
  docs/index.html \
  CNAME \
  .nojekyll
do
  if [[ ! -e "$out/$path" ]]; then
    echo "error: missing $path in $out" >&2
    fail=1
  fi
done

if [[ "$(tr -d '[:space:]' < "$out/CNAME")" != "agentic.hosting" ]]; then
  echo "error: CNAME must be exactly agentic.hosting" >&2
  fail=1
fi

if grep -q 'Dockerfile\|nginx.conf' <<<"$(find "$out" -maxdepth 1 -type f -printf '%f\n')"; then
  echo "error: Dockerfile or nginx.conf leaked into the Pages artifact" >&2
  fail=1
fi

if [[ "$fail" -ne 0 ]]; then
  exit 1
fi

echo "assembled GitHub Pages artifact at $out"
