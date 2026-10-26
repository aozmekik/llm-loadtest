#!/bin/sh
# Generates the files GoReleaser puts into the Linux packages but that do not
# live in the repository as such: the compressed man page and the Debian
# changelog. Run by the "before" hook in .goreleaser.yaml.
#
# Usage: scripts/package-files.sh VERSION COMMIT-DATE   (COMMIT-DATE in RFC 3339)
set -eu

version="$1"
date="$(date -u -R -d "$2")"
out=build/package
mkdir -p "$out"

gzip -9n < docs/llm-loadtest.1 > "$out/llm-loadtest.1.gz"

# One entry per release that points at CHANGELOG.md, which has the details.
cat > "$out/changelog.Debian" <<EOF
llm-loadtest (${version}-1) unstable; urgency=medium

  * New upstream release. Release notes:
    https://github.com/aozmekik/llm-loadtest/blob/main/CHANGELOG.md

 -- Ahmed Semih Özmekik <ahmed-semih@outlook.com>  ${date}
EOF
gzip -9n < "$out/changelog.Debian" > "$out/changelog.Debian.gz"
rm "$out/changelog.Debian"
