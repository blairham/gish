// Package compat is the bash-compatibility scoreboard (#101): a corpus
// of real-world bash snippets, run differentially against bash and
// gish, scored honestly and published.
//
// The corpus is curated around what people actually paste into a new
// shell within an hour of an announcement — tool init hooks, install
// one-liners, parameter expansion, arrays, process substitution — not
// around the bash manual's table of contents. Every case names its
// provenance so the list can be argued with.
package compat

// Category groups cases for the published table.
type Category string

// The corpus categories, ordered by how early a switcher hits them.
const (
	CatToolInit    Category = "tool init hooks"
	CatParamExp    Category = "parameter expansion"
	CatArrays      Category = "arrays"
	CatRedirection Category = "redirection & heredocs"
	CatSubst       Category = "substitution"
	CatControl     Category = "control flow & errors"
	CatFunctions   Category = "functions & scoping"
	CatStrings     Category = "string & arithmetic ops"
	CatOneLiners   Category = "install one-liners"
)

// Case is one snippet: run under bash and gish, compare stdout and
// exit status. Provenance says where the pattern comes from — the
// scoreboard is only credible if each row can be traced.
type Case struct {
	Name       string
	Category   Category
	Script     string
	Provenance string
}

// Corpus is the full case list. Additions are welcome; removals need a
// reason, because a shrinking corpus is how a scoreboard starts lying.
var Corpus = []Case{
	// --- tool init hooks: the first thing pasted into any new shell ---
	{
		Name: "eval a tool init hook", Category: CatToolInit,
		Provenance: "nvm/rbenv/pyenv/direnv/starship all ship `eval \"$(tool init bash)\"`",
		Script: `init() { echo 'export TOOL_HOME=/opt/tool'; echo 'tool_fn() { echo ran-$1; }'; }
eval "$(init)"
echo "$TOOL_HOME"
tool_fn hello`,
	},
	{
		Name: "single quote escaped inside an assignment", Category: CatToolInit,
		Provenance: "`'\\''` is the universal way to quote an apostrophe; every generated rc line uses it",
		Script: `x='a'\''b'
echo "[$x]"
echo 'a'\''b'`,
	},
	{
		Name: "escaped declaration bypasses an alias", Category: CatToolInit,
		Provenance: "conda's `shell.bash hook` writes `\\export` and `\\local` to bypass aliases",
		Script: `alias export='echo hijacked'
\export FOO=bar
echo "[$FOO]"`,
	},
	{
		Name: "declare -F tests for a function", Category: CatToolInit,
		Provenance: "fzf and bash-completion both gate whole branches on `declare -F name`",
		Script: `f() { :; }
declare -F f >/dev/null && echo have-f || echo missing-f`,
	},
	{
		Name: "shell detection via $0 and BASH_VERSION", Category: CatToolInit,
		Provenance: "init scripts branch on shell identity before emitting hooks",
		Script: `if [ -n "${BASH_VERSION:-}" ]; then echo bash-ish; else echo other; fi
case "${0##*/}" in *sh) echo sh-family;; *) echo unknown;; esac`,
	},
	{
		Name: "PATH prepend guard", Category: CatToolInit,
		Provenance: "every tool installer's rc snippet",
		Script: `PATH_ADD=/opt/tool/bin
case ":$PATH:" in
  *":$PATH_ADD:"*) echo already ;;
  *) PATH="$PATH_ADD:$PATH"; echo added ;;
esac
echo "${PATH%%:*}"`,
	},
	{
		Name: "command -v guard", Category: CatToolInit,
		Provenance: "the standard 'is it installed' idiom in rc files",
		Script: `if command -v echo >/dev/null 2>&1; then echo have-echo; fi
command -v definitely-not-a-real-binary-xyz >/dev/null 2>&1 || echo missing-ok`,
	},

	// --- parameter expansion: the densest source of "does it work" ---
	{
		Name: "defaults and alternates", Category: CatParamExp,
		Provenance: "r/fishshell migration threads cite `:-` first",
		Script: `unset u
echo "${u:-fallback}" "${u-fallback2}"
set_var=value
echo "${set_var:+present}" "${u:+absent}"
echo "${u:=assigned}" "$u"`,
	},
	{
		Name: "prefix and suffix stripping", Category: CatParamExp,
		Provenance: "path munging in every script",
		Script: `p=/usr/local/lib/libfoo.so.1
echo "${p##*/}" "${p%/*}" "${p%.so*}" "${p#/usr}"`,
	},
	{
		Name: "substring and length", Category: CatParamExp,
		Provenance: "common in prompt and log-trimming code",
		Script: `s=abcdefgh
echo "${#s}" "${s:2:3}" "${s: -2}"`,
	},
	{
		Name: "pattern substitution", Category: CatParamExp,
		Provenance: "sed-free string replacement",
		Script: `s=a-b-c
echo "${s/-/+}" "${s//-/+}" "${s/#a/A}" "${s/%c/C}"`,
	},
	{
		Name: "case conversion", Category: CatParamExp,
		Provenance: "bash 4+ idiom, common in newer scripts",
		Script: `s=MixedCase
echo "${s,,}" "${s^^}"`,
	},
	{
		Name: "indirect expansion", Category: CatParamExp,
		Provenance: "config-driven scripts; a known mvdan/sh edge",
		Script: `target=value
name=target
echo "${!name}"`,
	},

	// --- arrays: the classic "your shell isn't bash" test ---
	{
		Name: "indexed array basics", Category: CatArrays,
		Provenance: "the snippet people paste to test bash-ness",
		Script: `arr=(one two three)
echo "${arr[0]}" "${arr[2]}" "${#arr[@]}"
arr+=(four)
echo "${arr[@]}" "${!arr[@]}"`,
	},
	{
		Name: "array iteration with quoting", Category: CatArrays,
		Provenance: "the correct-quoting idiom style guides teach",
		Script: `arr=("with space" "another one")
for a in "${arr[@]}"; do echo "[$a]"; done
printf '%s\n' "${arr[@]}"`,
	},
	{
		Name: "array slicing", Category: CatArrays,
		Provenance: "argument shuffling in wrapper scripts",
		Script: `arr=(a b c d e)
echo "${arr[@]:1:3}" "${arr[@]: -2}"`,
	},
	{
		Name: "associative arrays", Category: CatArrays,
		Provenance: "bash 4+ maps; a frequent compat cliff",
		Script: `declare -A m
m[key]=value
m[other]=thing
echo "${m[key]}" "${m[other]}" "${#m[@]}"`,
	},

	// --- redirection and heredocs ---
	{
		Name: "heredoc with and without expansion", Category: CatRedirection,
		Provenance: "config generation, the most-pasted multi-line construct",
		Script: `name=world
cat <<EOF
hello $name
EOF
cat <<'EOF'
literal $name
EOF`,
	},
	{
		Name: "indented heredoc", Category: CatRedirection,
		Provenance: "heredocs inside indented functions",
		Script: `f() {
	cat <<-EOF
	indented body
	EOF
}
f`,
	},
	{
		Name: "herestring", Category: CatRedirection,
		Provenance: "`<<<` shows up in every awk/grep one-liner",
		Script:     `tr a-z A-Z <<< "shout this"`,
	},
	{
		Name: "fd redirection and merging", Category: CatRedirection,
		Provenance: "`2>&1` is in every build script",
		Script: `{ echo to-stdout; echo to-stderr >&2; } 2>&1 | tr a-z A-Z
exec 3>&1
echo via-fd3 >&3
exec 3>&-`,
	},
	{
		Name: "noclobber and append", Category: CatRedirection,
		Provenance: "log-appending scripts",
		Script: `tmp=$(mktemp)
echo first > "$tmp"
echo second >> "$tmp"
cat "$tmp"
rm -f "$tmp"`,
	},

	// --- substitution ---
	{
		Name: "command substitution nesting", Category: CatSubst,
		Provenance: "fish's original `$()` gap is the canonical grudge",
		Script: `echo "$(echo "$(echo deep)")"
echo "outer $(echo "inner $(echo core)")"`,
	},
	{
		Name: "process substitution", Category: CatSubst,
		Provenance: "`diff <(a) <(b)` — the bashism people test first",
		Script:     `diff <(printf 'a\nb\n') <(printf 'a\nc\n') || echo differed`,
	},
	{
		Name: "arithmetic expansion", Category: CatSubst,
		Provenance: "loop counters everywhere",
		Script: `i=5
echo "$((i * 2)) $((i++)) $i $((RANDOM >= 0))"
(( i > 3 )) && echo greater`,
	},
	{
		Name: "brace expansion", Category: CatSubst,
		Provenance: "`mkdir -p a/{b,c}` is muscle memory",
		Script: `echo {1..5}
echo pre{a,b,c}post
echo {a..e}`,
	},

	// --- control flow and error handling ---
	{
		Name: "set -e with a failing command", Category: CatControl,
		Provenance: "the most argued-about bash semantic",
		Script: `set -e
echo before
false || echo tolerated
echo after`,
	},
	{
		Name: "set -u and set -o pipefail", Category: CatControl,
		Provenance: "the 'unofficial strict mode' header",
		Script: `set -uo pipefail
echo "${HOME:?home must be set}" >/dev/null && echo home-ok
(false | true); echo "pipestatus=$?"`,
	},
	{
		Name: "trap on EXIT", Category: CatControl,
		Provenance: "cleanup handlers in every serious script",
		Script: `trap 'echo cleanup-ran' EXIT
echo body`,
	},
	{
		Name: "case with fallthrough patterns", Category: CatControl,
		Provenance: "arg parsing in hand-rolled CLIs",
		Script: `for x in apple banana cherry; do
  case "$x" in
    a*|b*) echo "$x: early" ;;
    *) echo "$x: late" ;;
  esac
done`,
	},
	{
		Name: "while read with IFS", Category: CatControl,
		Provenance: "the canonical line-processing loop",
		Script:     `printf 'a:1\nb:2\n' | while IFS=: read -r k v; do echo "$k=$v"; done`,
	},
	{
		Name: "until loop and break/continue", Category: CatControl,
		Provenance: "retry loops",
		Script: `i=0
until [ "$i" -ge 3 ]; do i=$((i+1)); [ "$i" = 2 ] && continue; echo "i=$i"; done`,
	},
	{
		Name: "C-style for loop", Category: CatControl,
		Provenance: "bash-specific loop form",
		Script:     `for ((i = 0; i < 3; i++)); do echo "n=$i"; done`,
	},

	// --- functions and scoping ---
	{
		Name: "local variables and return codes", Category: CatFunctions,
		Provenance: "the shape of every rc-file helper",
		Script: `f() { local x=inner; echo "$x"; return 3; }
x=outer
f || echo "rc=$?"
echo "$x"`,
	},
	{
		Name: "recursion and arguments", Category: CatFunctions,
		Provenance: "tree-walking helpers",
		Script: `countdown() { [ "$1" -le 0 ] && { echo done; return; }; echo "$1"; countdown $(( $1 - 1 )); }
countdown 3`,
	},
	{
		Name: "$@ vs $* quoting", Category: CatFunctions,
		Provenance: "the argument-forwarding bug class",
		Script: `show() { printf '[%s]' "$@"; echo; printf '[%s]' "$*"; echo; }
show one "two three" four`,
	},
	{
		Name: "shift and positional params", Category: CatFunctions,
		Provenance: "hand-rolled flag parsing",
		Script: `set -- a b c
echo "$# $1"
shift
echo "$# $1 $*"`,
	},

	// --- string and arithmetic ops ---
	{
		Name: "test operators", Category: CatStrings,
		Provenance: "conditionals in every script",
		Script: `[ -n "x" ] && echo nonempty
[ -z "" ] && echo empty
[ 5 -gt 3 ] && echo numeric
[ "a" = "a" ] && echo streq`,
	},
	{
		Name: "double-bracket tests and regex", Category: CatStrings,
		Provenance: "`[[ =~ ]]` is a top bashism",
		Script: `s=hello123
[[ "$s" == hello* ]] && echo glob-match
[[ "$s" =~ ^[a-z]+[0-9]+$ ]] && echo regex-match
[[ -n "$s" && "$s" != nope ]] && echo compound`,
	},
	{
		Name: "positional parameters passed to -c", Category: CatToolInit,
		Provenance: "`sh -c 'cmd \"$1\"' _ \"$value\"` is the safe way to pass a value into a snippet",
		Script:     `set -- alpha beta; echo "1=$1 2=$2 count=$#"`,
	},
	{
		Name: "printf formatting", Category: CatStrings,
		Provenance: "portable output formatting",
		Script:     `printf '%s=%d (%05.2f) %q\n' name 42 3.14159 "needs quoting"`,
	},

	// --- install one-liners ---
	{
		Name: "install-script shape", Category: CatOneLiners,
		Provenance: "curl | sh installers: set -euo, functions, traps, mktemp",
		Script: `set -euo pipefail
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
detect_os() { case "$(uname -s)" in Darwin) echo darwin ;; Linux) echo linux ;; *) echo other ;; esac; }
OS=$(detect_os)
[ -d "$TMP" ] && echo "staged in tmp on $OS"`,
	},
	{
		Name: "pipeline with subshell and exit status", Category: CatOneLiners,
		Provenance: "the shape of tool-detection one-liners",
		Script: `result=$( (echo alpha; echo beta) | grep beta | tr a-z A-Z )
echo "$result"
echo "status=$?"`,
	},
}
