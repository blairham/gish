#!/usr/bin/env bash
# Fetch bash's own test suite so `make bash-suite` can run it through koi (#211).
#
# Never committed: every file in bash's tests/ carries a GPLv3 header and
# koi is MIT, so vendoring them would relicense this repository by
# accident. This downloads into build/, which is gitignored.
#
# It also builds bash's recho/zecho/printenv from support/*.c. Those are
# not optional: without them both shells print "command not found" for
# most assertions and the comparison measures nothing.
set -euo pipefail

VERSION="${BASH_SUITE_VERSION:-5.3}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BUILD="$ROOT/build"
DEST="$BUILD/bash-tests"
HELPERS="$BUILD/bash-helpers"
TARBALL="$BUILD/bash-$VERSION.tar.gz"
URL="https://ftp.gnu.org/gnu/bash/bash-$VERSION.tar.gz"

mkdir -p "$BUILD"

if [ -f "$DEST/run-all" ] && [ -x "$HELPERS/recho" ]; then
  echo "bash $VERSION tests already in $DEST"
  exit 0
fi

if [ ! -f "$TARBALL" ]; then
  echo "fetching $URL"
  curl -fSL --retry 3 -o "$TARBALL" "$URL"
fi

echo "extracting tests/ and support/"
rm -rf "$BUILD/bash-$VERSION"
tar xzf "$TARBALL" -C "$BUILD" "bash-$VERSION/tests" "bash-$VERSION/support"
rm -rf "$DEST"
mv "$BUILD/bash-$VERSION/tests" "$DEST"

echo "building bash's test helpers"
mkdir -p "$HELPERS"
for h in recho zecho printenv; do
  cc -o "$HELPERS/$h" "$BUILD/bash-$VERSION/support/$h.c"
done
rm -rf "$BUILD/bash-$VERSION"

echo "ready: $DEST ($(ls "$DEST"/*.tests | wc -l | tr -d ' ') test files), helpers in $HELPERS"
