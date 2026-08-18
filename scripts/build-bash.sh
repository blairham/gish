#!/usr/bin/env bash
# Build the bash the differential suites compare against (#273).
#
# Two test harnesses need a *specific* bash, not merely some bash:
#
#   - interp's TestRunnerRunConfirm runs every case through real bash and
#     compares. checkBash() requires $BASH_VERSION to start with 5.3, so an
#     older bash makes the whole suite skip rather than fail — which is how
#     it went unnoticed that it has never run in CI.
#   - the bash-suite scoreboard uses bash as its oracle, and an oracle of a
#     different vintage answers different questions.
#
# GitHub's ubuntu and macos runners both ship something older, so we build
# it. The tarball is the same one fetch-bash-tests.sh already downloads and
# then throws away everything but tests/ and support/ from; this reuses it
# when it is there.
#
# Output is build/bash-bin/bash, which is gitignored. Idempotent: an
# existing binary of the right version short-circuits the whole script, so
# it costs nothing on a cache hit.
set -euo pipefail

VERSION="${BASH_SUITE_VERSION:-5.3}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BUILD="$ROOT/build"
TARBALL="$BUILD/bash-$VERSION.tar.gz"
SRC="$BUILD/bash-src-$VERSION"
BIN="$BUILD/bash-bin"
URL="https://ftp.gnu.org/gnu/bash/bash-$VERSION.tar.gz"

if [ -x "$BIN/bash" ] && "$BIN/bash" -c 'echo -n $BASH_VERSION' | grep -q "^$VERSION"; then
  echo "bash $VERSION already built at $BIN/bash"
  exit 0
fi

mkdir -p "$BUILD" "$BIN"

if [ ! -f "$TARBALL" ]; then
  echo "fetching $URL"
  curl -fSL --retry 3 -o "$TARBALL" "$URL"
fi

echo "extracting bash $VERSION source"
rm -rf "$SRC"
mkdir -p "$SRC"
tar xzf "$TARBALL" -C "$SRC" --strip-components=1

echo "configuring and building (this is the slow part; cache build/bash-bin)"
cd "$SRC"
# --without-bash-malloc: bash's bundled allocator does not build cleanly on
# every platform we test on, and we want the system one anyway so the shell
# under test behaves like an installed bash rather than a special build.
./configure --without-bash-malloc >/dev/null
make -j"$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 2)" >/dev/null

cp "$SRC/bash" "$BIN/bash"
# Leave the source tree before deleting it: removing the shell's own cwd
# makes every later command emit "getcwd: cannot access parent directories".
cd "$ROOT"
rm -rf "$SRC"

echo "built $("$BIN/bash" -c 'echo $BASH_VERSION') at $BIN/bash"
