#!/usr/bin/env bash
# Report whether the koi an agent will actually run matches this tree (#284).
#
# Every differential gate builds from source, so all of them answer "is this
# branch correct?" and none answer "is the koi on this machine correct?".
# Those drifted 15 commits apart once without anything saying so: main was
# green while the installed binary failed 15 of 17 agent-gate cases, among
# them exec -a (#241) under Claude Code's find and grep shims.
#
# Exit 0 in sync, 1 drifted, 2 nothing installed. Drift is a real exit code
# because a report nobody can gate on is a report nobody reads.
set -uo pipefail

prefix=${KOI_PREFIX:-$HOME/.local}
bin=${KOI_BIN:-$prefix/bin/koi-bash}

if [ ! -x "$bin" ]; then
	echo "no installed koi at $bin"
	echo "  install one with: make install-agent"
	exit 2
fi

# Resolve the symlink, since koi-bash and koi-agent-bash both point at koi
# and the interesting identity is the binary's, not the name's.
target=$bin
while [ -L "$target" ]; do
	link=$(readlink "$target")
	case $link in
	/*) target=$link ;;
	*) target=$(dirname "$target")/$link ;;
	esac
done

installed=$("$bin" --version 2>/dev/null)
installed_commit=$(printf '%s\n' "$installed" | sed -n 's/.*commit \([0-9a-f]*\).*/\1/p')
head_commit=$(git rev-parse --verify --quiet HEAD)

echo "installed: $bin"
[ "$target" != "$bin" ] && echo "           -> $target"
echo "           $installed"
echo "tree HEAD: ${head_commit:-unknown}"

if [ -z "$installed_commit" ] || [ -z "$head_commit" ]; then
	echo
	echo "cannot compare: a commit is missing from one side"
	exit 1
fi

if [ "$installed_commit" = "$head_commit" ]; then
	echo
	echo "in sync"
	exit 0
fi

# Distance is the actionable number: "behind by 15" is a different problem
# from "built from a branch", and the count says which one this is.
echo
if git merge-base --is-ancestor "$installed_commit" "$head_commit" 2>/dev/null; then
	behind=$(git rev-list --count "$installed_commit..$head_commit" 2>/dev/null)
	echo "DRIFTED: the installed koi is $behind commits behind this tree"
	echo
	git log --oneline "$installed_commit..$head_commit" 2>/dev/null | sed 's/^/  /'
	echo
	echo "  gate it as it stands: make gate-installed"
	echo "  bring it up to date:  make install-agent"
else
	echo "DRIFTED: the installed koi is not an ancestor of this tree"
	echo "  it was built from a different branch, or from a commit not present here"
	echo
	echo "  gate it as it stands: make gate-installed"
	echo "  bring it up to date:  make install-agent"
fi
exit 1
