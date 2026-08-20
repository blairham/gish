// Copyright (c) 2017, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

package interp_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blairham/koi-shell/internal/shell/expand"
	"github.com/blairham/koi-shell/internal/shell/interp"
	"github.com/blairham/koi-shell/internal/shell/shinternal"
	"github.com/go-quicktest/qt"
	"mvdan.cc/sh/v3/syntax"
)

// runnerRunTimeout is the context timeout used by any tests calling [Runner.Run].
// The timeout saves us from hangs or burning too much CPU if there are bugs.
// All the test cases are designed to be inexpensive and stop in a very short
// amount of time, so 5s should be plenty even for busy machines.
const runnerRunTimeout = 5 * time.Second

// Some program which should be in $PATH. Needs to run before runTests is
// initialized (so an init function wouldn't work), because runTest uses it.
var pathProg = func() string {
	if runtime.GOOS == "windows" {
		return "cmd"
	}
	return "sh"
}()

func parse(tb testing.TB, parser *syntax.Parser, src string) *syntax.File {
	if parser == nil {
		parser = syntax.NewParser()
	}
	file, err := parser.Parse(strings.NewReader(src), "")
	if err != nil {
		tb.Fatal(err)
	}
	return file
}

func BenchmarkRun(b *testing.B) {
	b.ReportAllocs()

	src := `
echo a b c d
echo ./$foo/etc $(echo foo bar)
foo="bar"
x=y :
fn() {
	local a=b
	for i in 1 2 3; do
		echo $i | cat
	done
}
[[ $foo == bar ]] && fn
echo a{b,c}d *.go
let i=(2 + 3)
`
	file := parse(b, nil, src)
	r, _ := interp.New()
	ctx := b.Context()

	for b.Loop() {
		r.Reset()
		if err := r.Run(ctx, file); err != nil {
			b.Fatal(err)
		}
	}
}

var hasBash53 bool

// koi-local: see skipIfOracleGap.
var oracleTildeIgnoresHome bool

func TestMain(m *testing.M) {
	if os.Getenv("GOSH_PROG") != "" {
		switch os.Getenv("GOSH_CMD") {
		case "exit_0":
			os.Exit(0)
		case "exit_5":
			os.Exit(5)
		case "print_ok":
			fmt.Printf("exec ok\n")
			os.Exit(0)
		case "print_fail":
			fmt.Printf("exec fail\n")
			os.Exit(1)
		case "pid_and_hang":
			fmt.Println(os.Getpid())
			time.Sleep(time.Hour)
			os.Exit(0)
		case "foo_null_bar":
			fmt.Println("foo\x00bar")
			os.Exit(0)
		case "lookpath":
			_, err := exec.LookPath(pathProg)
			if err != nil {
				fmt.Println(err)
				os.Exit(1)
			}
			fmt.Printf("%s found\n", pathProg)
			os.Exit(0)
		}
		r := strings.NewReader(os.Args[1])
		file, err := syntax.NewParser().Parse(r, "")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		runner, _ := interp.New(
			interp.StdIO(os.Stdin, os.Stdout, os.Stderr),
			interp.ExecHandlers(testExecHandler),
		)
		ctx := context.Background()
		if err := runner.Run(ctx, file); err != nil {
			var es interp.ExitStatus
			if errors.As(err, &es) {
				os.Exit(int(es))
			}

			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	prog, err := os.Executable()
	if err != nil {
		panic(err)
	}
	os.Setenv("GOSH_PROG", prog)

	shinternal.TestMainSetup()

	hasBash53 = checkBash()
	oracleTildeIgnoresHome = checkOracleTilde()

	wd, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	os.Setenv("GO_TEST_DIR", wd)

	os.Setenv("INTERP_GLOBAL", "value")
	os.Setenv("MULTILINE_INTERP_GLOBAL", "\nwith\nnewlines\n\n")

	// Double check that env vars on Windows are case insensitive.
	if runtime.GOOS == "windows" {
		os.Setenv("mixedCase_INTERP_GLOBAL", "value")
	} else {
		os.Setenv("MIXEDCASE_INTERP_GLOBAL", "value")
	}

	os.Setenv("PATH_PROG", pathProg)

	// To print env vars. Only a builtin on Windows.
	if runtime.GOOS == "windows" {
		os.Setenv("ENV_PROG", "cmd /c set")
	} else {
		os.Setenv("ENV_PROG", "env")
	}

	m.Run()
}

func checkBash() bool {
	out, err := exec.Command("bash", "-c", "echo -n $BASH_VERSION").Output()
	if err != nil {
		return false
	}
	return strings.HasPrefix(string(out), "5.3")
}

// concBuffer wraps a [bytes.Buffer] in a mutex so that concurrent writes
// to it don't upset the race detector.
type concBuffer struct {
	buf bytes.Buffer
	sync.Mutex
}

func (c *concBuffer) Write(p []byte) (int, error) {
	c.Lock()
	n, err := c.buf.Write(p)
	c.Unlock()
	return n, err
}

func (c *concBuffer) WriteString(s string) (int, error) {
	c.Lock()
	n, err := c.buf.WriteString(s)
	c.Unlock()
	return n, err
}

func (c *concBuffer) String() string {
	c.Lock()
	s := c.buf.String()
	c.Unlock()
	return s
}

func (c *concBuffer) Reset() {
	c.Lock()
	c.buf.Reset()
	c.Unlock()
}

type runTest struct {
	in, want string
}

var runTests = []runTest{
	// no-op programs
	{"", ""},
	{"true", ""},
	{":", ""},
	{"exit", ""},
	{"exit 0", ""},
	{"{ :; }", ""},
	{"(:)", ""},

	// exit status codes
	{"exit 1", "exit status 1"},
	{"exit -1", "exit status 255"},
	{"exit 300", "exit status 44"},
	{"false", "exit status 1"},
	{"false foo", "exit status 1"},
	{"! false", ""},
	{"true foo", ""},
	{": foo", ""},
	{"! true", "exit status 1"},
	{"false; true", ""},
	{"false; exit", "exit status 1"},
	{"exit; echo foo", ""},
	{"exit 0; echo foo", ""},
	{"printf", "usage: printf format [arguments]\nexit status 2 #JUSTERR"},
	{"break", "break is only useful in a loop\n #JUSTERR"},
	{"continue", "continue is only useful in a loop\n #JUSTERR"},
	{"cd a b", "usage: cd [dir]\nexit status 2 #JUSTERR"},
	{"shift a", "usage: shift [n]\nexit status 2 #JUSTERR"},
	{
		"shouldnotexist",
		"\"shouldnotexist\": executable file not found in $PATH\nexit status 127 #JUSTERR",
	},
	{
		"for i in 1; do continue a; done",
		"usage: continue [n]\nexit status 2 #JUSTERR",
	},
	{
		"for i in 1; do break a; done",
		"usage: break [n]\nexit status 2 #JUSTERR",
	},
	{"false; a=b", ""},
	{"false; false &", ""},
	{
		"GOSH_CMD=exit_0 $GOSH_PROG; echo next",
		"next\n",
	},
	{
		"GOSH_CMD=exit_5 $GOSH_PROG; echo next",
		"next\n",
	},
	{
		"! GOSH_CMD=exit_0 $GOSH_PROG",
		"exit status 1",
	},
	{
		"! GOSH_CMD=exit_5 $GOSH_PROG",
		"",
	},

	// we don't need to follow bash error strings
	{"exit a", "invalid exit status code: \"a\"\nexit status 2 #JUSTERR"},
	{"exit 1 2", "exit cannot take multiple arguments\nexit status 1 #JUSTERR"},
	{"f() { return a; }; f", "invalid return status code: \"a\"\nexit status 2 #JUSTERR"},

	// echo
	{"echo", "\n"},
	{"echo a b c", "a b c\n"},
	{"echo -n foo", "foo"},
	{`echo -e '\t'`, "\t\n"},
	{`echo -E '\t'`, "\\t\n"},
	{`echo -e 'before\x00after'`, "before\x00after\n"},
	{"echo -x foo", "-x foo\n"},
	{"echo -e -x -e foo", "-x -e foo\n"},

	// printf
	{"printf foo", "foo"},
	{"printf %%", "%"},
	{"printf %", "missing format char\nexit status 1 #JUSTERR"},
	{"printf %; echo foo", "missing format char\nfoo\n #IGNORE"},
	{"printf %1", "missing format char\nexit status 1 #JUSTERR"},
	{"printf %+", "missing format char\nexit status 1 #JUSTERR"},
	{"printf %B foo", "invalid format char: B\nexit status 1 #JUSTERR"},
	{"printf %12-s foo", "invalid format char: -\nexit status 1 #JUSTERR"},
	{"printf ' %s \n' bar", " bar \n"},
	{"printf '\\A'", "\\A"},
	{"printf %s foo", "foo"},
	{"printf %s", ""},
	{"printf %d,%i 3 4", "3,4"},
	{"printf %d", "0"},
	{"printf %d,%d 010 0x10", "8,16"},
	{"printf %c,%c,%c foo àa", "f,\xc3,\x00"}, // TODO: use a rune?
	{"printf %3s a", "  a"},
	{"printf %3i 1", "  1"},
	{"printf %+i%+d 1 -3", "+1-3"},
	{"printf %-5x 10", "a    "},
	{"printf %02x 1", "01"},
	{"printf 'a% 5s' a", "a    a"},
	{"printf 'nofmt' 1 2 3", "nofmt"},
	{"printf '%d_' 1 2 3", "1_2_3_"},
	{"printf '%02d %02d\n' 1 2 3", "01 02\n03 00\n"},
	{`printf '0%s1' 'a\bc'`, `0a\bc1`},
	{`printf '0%b1' 'a\bc'`, "0a\bc1"},
	{"printf 'a%bc'", "ac"},
	{"printf 'before\\x00after'", "before\x00after"},

	// printf escape sequences at end of format string (must not panic)
	{"printf '\\0'", "\x00"},
	{"printf '\\01'", "\x01"},
	{"printf '\\x'", "\\x #IGNORE bash prints a warning to stderr"},
	{"printf 'a\\0'", "a\x00"},
	{"printf '\\\\'", "\\"},

	// words and quotes
	{"echo  foo ", "foo\n"},
	{"echo ' foo '", " foo \n"},
	{`echo " foo "`, " foo \n"},
	{`echo a'b'c"d"e`, "abcde\n"},
	{`a=" b c "; echo $a`, "b c\n"},
	{`a=" b c "; echo "$a"`, " b c \n"},
	{`a=" b c "; echo foo${a}bar`, "foo b c bar\n"},
	{`a="b    c"; echo foo${a}bar`, "foob cbar\n"},
	{`echo "$(echo ' b c ')"`, " b c \n"},
	{"echo \"`echo \\\"foobar\\\"`\"", "foobar\n"},
	{"echo ''", "\n"},
	{`$(echo)`, ""},
	{`echo -n '\\'`, `\\`},
	{`echo -n "\\"`, `\`},
	{`set -- a b c; x="$@"; echo "$x"`, "a b c\n"},
	{`set -- b c; echo a"$@"d`, "ab cd\n"},
	{`count() { echo $#; }; set --; count "$@"`, "0\n"},
	{`count() { echo $#; }; set -- ""; count "$@"`, "1\n"},
	{`count() { echo $#; }; set -- ""; shift; count "$@"`, "0\n"},
	{`count() { echo $#; }; a=(); count "${a[@]}"`, "0\n"},
	{`count() { echo $#; }; count "${unset_var[@]}"`, "0\n"},
	{`count() { echo $#; }; a=(""); count "${a[@]}"`, "1\n"},
	{`echo $1 $3; set -- a b c; echo $1 $3`, "\na c\n"},
	// ${10} and beyond are positional parameters (#362); bare $10 stays
	// $1 followed by 0.
	{`set -- 1 2 3 4 5 6 7 8 9 ten eleven; echo "[${10}][${11}][${12:-none}][$10]"`, "[ten][eleven][none][10]\n"},
	{`[[ $0 == "bash" || $0 == "gosh" ]]`, ""},

	// dollar quotes
	{`echo $'foo\nbar'`, "foo\nbar\n"},
	{`echo $'\r\t\\'`, "\r\t\\\n"},
	{`echo $"foo\nbar"`, "foo\\nbar\n"},
	{`echo $'%s'`, "%s\n"},
	{`a=$'\r\t\\'; echo "$a"`, "\r\t\\\n"},
	{`a=$"foo\nbar"; echo "$a"`, "foo\\nbar\n"},
	{`echo $'\a\b\e\E\f\v'`, "\a\b\x1b\x1b\f\v\n"},
	{`echo $'\\\'\"\?'`, "\\'\"?\n"},
	{`echo $'\1\45\12345\777\9'`, "\x01%S45\xff\\9\n"},
	{`echo $'\x\xf\x09\xAB'`, "\\x\x0f\x09\xab\n"},
	{`echo $'\u\uf\u09\uABCD\u00051234'`, "\\u\u000f\u0009\uabcd\u00051234\n"},
	{`echo $'\U\Uf\U09\UABCD\U00051234'`, "\\U\u000f\u0009\uabcd\U00051234\n"},
	{
		"echo 'before\x00after'",
		"beforeafter\n",
	},
	{
		"echo \"before\x00after\"",
		"beforeafter\n",
	},
	{
		"echo $'before\x00after'",
		"beforeafter\n",
	},
	{
		"echo $'before\\x00after'",
		"before\n",
	},
	{
		"echo $'before\\xafter'",
		"before\xafter\n",
	},
	{
		"a='before\x00after'; eval \"echo -n ${a} ${a@Q}\";",
		"beforeafter beforeafter",
	},
	{
		"a=$'before\\x00after'; eval \"echo -n ${a} ${a@Q}\";",
		"before before",
	},
	{
		"i\x00f true; then echo before\x00; \x00fi",
		"before\n",
	},
	{
		"echo $(GOSH_CMD=foo_null_bar $GOSH_PROG)",
		"foobar\n #IGNORE",
	},
	// See the TODO where foo_NULL_BAR is set.
	// {
	// 	"echo $foo_NULL_BAR \"${foo_NULL_BAR}\"",
	// 	"foo\n",
	// },

	// escaped chars
	{"echo a\\b", "ab\n"},
	{"echo a\\ b", "a b\n"},
	{"echo \\$a", "$a\n"},
	{"echo \"a\\b\"", "a\\b\n"},
	{"echo 'a\\b'", "a\\b\n"},
	{"echo \"a\\\nb\"", "ab\n"},
	{"echo 'a\\\nb'", "a\\\nb\n"},
	{`echo "\""`, "\"\n"},
	{`echo \\`, "\\\n"},
	{`echo \\\\`, "\\\\\n"},
	{`echo \`, "\\\n"},

	// escape characters in double quote literal
	{`echo "\\"`, "\\\n"},     // special character is preserved
	{`echo "\b"`, "\\b\n"},    // non-special character has both characters preserved
	{`echo "\\\\"`, "\\\\\n"}, // sequential backslashes (escape characters repeated sequentially)

	// vars
	{"foo=bar; echo $foo", "bar\n"},
	{"foo=bar foo=etc; echo $foo", "etc\n"},
	{"foo=bar; foo=etc; echo $foo", "etc\n"},
	{"foo=bar; foo=; echo $foo", "\n"},
	{"unset foo; echo $foo", "\n"},
	{"foo=bar; unset foo; echo $foo", "\n"},
	{"echo $INTERP_GLOBAL", "value\n"},
	{"INTERP_GLOBAL=; echo $INTERP_GLOBAL", "\n"},
	{"unset INTERP_GLOBAL; echo $INTERP_GLOBAL", "\n"},
	{"echo $MIXEDCASE_INTERP_GLOBAL", "value\n"},
	{"foo=bar; foo=x true; echo $foo", "bar\n"},
	{"foo=bar; foo=x true; echo $foo", "bar\n"},
	{"foo=bar; $ENV_PROG | grep '^foo='", "exit status 1"},
	{"foo=bar $ENV_PROG | grep '^foo='", "foo=bar\n"},
	{"foo=a foo=b $ENV_PROG | grep '^foo='", "foo=b\n"},
	{"$ENV_PROG | grep -i '^interp_global='", "INTERP_GLOBAL=value\n"},
	{"INTERP_GLOBAL=new; $ENV_PROG | grep -i '^interp_global='", "INTERP_GLOBAL=new\n"},
	{"INTERP_GLOBAL=; $ENV_PROG | grep -i '^interp_global='", "INTERP_GLOBAL=\n"},
	{"unset INTERP_GLOBAL; $ENV_PROG | grep -i '^interp_global='", "exit status 1"},
	{"a=b; a+=c x+=y; echo $a $x", "bc y\n"},
	{`a=" x  y"; b=$a c="$a"; echo $b; echo $c`, "x y\nx y\n"},
	{`a=" x  y"; b=$a c="$a"; echo "$b"; echo "$c"`, " x  y\n x  y\n"},
	{`arr=("foo" "bar" "lala" "foobar"); echo ${arr[@]:2}; echo ${arr[*]:2}`, "lala foobar\nlala foobar\n"},
	{`arr=("foo" "bar" "lala" "foobar"); echo ${arr[@]:2:4}; echo ${arr[*]:1:4}`, "lala foobar\nbar lala foobar\n"},
	{`arr=("foo" "bar"); echo ${arr[@]}; echo ${arr[*]}`, "foo bar\nfoo bar\n"},
	{`arr=("foo"); echo ${arr[@]:99}`, "\n"},
	{`echo ${arr[@]:1:99}; echo ${arr[*]:1:99}`, "\n\n"},
	{`arr=(0 1 2 3 4 5 6 7 8 9 0 a b c d e f g h); echo ${arr[@]:3:4}`, "3 4 5 6\n"},

	// quoted array slicing
	{`a=(1 2 3 4 5); echo "${a[@]:2:2}"`, "3 4\n"},
	{`a=(1 2 3 4 5); echo "${a[*]:2:2}"`, "3 4\n"},
	{`a=(1 2 3 4 5); b=("${a[@]:2:2}"); echo ${#b[@]}`, "2\n"},
	{`a=(1 2 3 4 5); echo "${a[@]:3}"`, "4 5\n"},
	{`a=(1 2 3 4 5); echo "${a[@]: -2}"`, "4 5\n"},
	{`a=(1 2 3 4 5); echo "${a[@]: -99}"`, "\n"},

	// positional parameter slicing (1-based offset, $0 at offset 0)
	{`f() { echo "${@:2:2}"; }; f a b c d e`, "b c\n"},
	{`f() { echo ${@:2:2}; }; f a b c d e`, "b c\n"},
	{`f() { echo "${@:1}"; }; f a b c`, "a b c\n"},
	{`f() { echo "${*:2:2}"; }; f a b c d e`, "b c\n"},
	{`f() { echo "${@: -2}"; }; f a b c d e`, "d e\n"},
	{`f() { echo "${@: -3:2}"; }; f a b c d e`, "c d\n"},
	{`f() { echo "${@:1:0}"; }; f a b c`, "\n"},
	{`f() { echo "${@:99}"; }; f a b c`, "\n"},
	{`set -- a b c; v=("${@:0:2}"); echo "${#v[@]}"`, "2\n"},
	{`f() { for x in "${@:2:2}"; do echo "$x"; done; }; f a b c d e`, "b\nc\n"},
	{`set --; v=("${@:0}"); echo "${#v[@]}"`, "1\n"},
	{`f() { echo "${@: -10}"; }; f a b c`, "\n"},

	{`echo ${foo[@]}; echo ${foo[*]}`, "\n\n"},
	// TODO: reenable once we figure out the broken pipe error
	//{`$ENV_PROG | while read line; do if test -z "$line"; then echo empty; fi; break; done`, ""}, // never begin with an empty element

	// inline variables have special scoping
	{
		"f() { echo $inline; inline=bar true; echo $inline; }; inline=foo f",
		"foo\nfoo\n",
	},
	{"v=x; read v <<< 'y'; echo $v", "y\n"},
	{"v=x; v=inline read v <<< 'y'; echo $v", "x\n"},
	{"v=x; v=inline unset v; echo $v", "x\n"},
	{"v=x; echo 'v=y' >f; v=inline source ./f; echo $v", "x\n"},
	{"declare -n v=v2; v=inline true; echo $v $v2", "\n"},
	{"f() { echo $v; }; v=x; v=y f; f", "y\nx\n"},
	{"f() { echo $v; }; v=x; v+=y f; f", "xy\nx\n"},
	{"f() { echo $v; }; declare -n v=v2; v2=x; v=y f; f", "y\nx\n"},
	{"f() { echo ${v[@]}; }; v=(e1 e2); v=y f; f", "y\ne1 e2\n"},

	// special vars
	{"echo $?; false; echo $?", "0\n1\n"},
	{"for i in 1 2; do\necho $LINENO\necho $LINENO\ndone", "2\n3\n2\n3\n"},
	{"[[ -n $$ && $$ -gt 0 ]]", ""},
	{"[[ $$ -eq $PPID ]]", "exit status 1"},
	{"[[ $RANDOM -eq $RANDOM ]]", "exit status 1"},   // 1 in 32k chance of a collision, 0.003%
	{"[[ $SRANDOM -eq $SRANDOM ]]", "exit status 1"}, // 1 in 2**32 chance of a collision,

	// Ensure that we consistently use 64 bits even on 32-bit platforms.
	// Bash doesn't do this, but we do, for portability and consistency.
	{"[[ 1000000000123 -lt 100 ]]", "exit status 1"},
	{"[[ 1000000000123 -eq 1000000000456 ]]", "exit status 1"},
	{"[[ 1000000000123 < 100 ]]", "exit status 1"},
	{"((1000000000123 == 1000000000456))", "exit status 1"},

	// var manipulation
	{"echo ${#a} ${#a[@]}", "0 0\n"},
	{"a=bar; echo ${#a} ${#a[@]}", "3 1\n"},
	{"a=世界; echo ${#a}", "2\n"},
	{"a=(a bcd); echo ${#a} ${#a[@]} ${#a[*]} ${#a[1]}", "1 2 2 3\n"},
	{
		"a=($(echo a bcd)); echo ${#a} ${#a[@]} ${#a[*]} ${#a[1]}",
		"1 2 2 3\n",
	},
	{
		"a=([0]=$(echo a b) $(echo c d)); echo ${#a} ${#a[@]} ${#a[*]} ${#a[0]}",
		"3 3 3 3\n",
	},
	{"set -- a bc; echo ${#@} ${#*} $#", "2 2 2\n"},
	{
		"echo ${!a}; echo more",
		"invalid indirect expansion\nexit status 1 #JUSTERR",
	},
	{
		"a=b; echo ${!a}; b=c; echo ${!a}",
		"\nc\n",
	},
	// An operator after the indirection applies to the target (#277):
	// substitution and trims rewrite the target's value, a slice cuts
	// it, and a default fires on the target being unset. All were
	// silently dropped, so ${!x//c/X} answered the unsubstituted value.
	{`x=var; var=abcde; echo "${!x//c/X}"`, "abXde\n"},
	{`x=var; var=abcde; echo "${!x:1:2}"`, "bc\n"},
	{`x=var; var=abcde; echo "${!x%de}"`, "abc\n"},
	{`x=var; var=abcde; echo "${!x:-def}"`, "abcde\n"},
	{`x=var; unset var; echo "${!x-def}"`, "def\n"},
	// An empty or invalid target name is an error in bash, never a
	// silent empty string — silence made ${!x} with a garbage x read
	// as an unset variable.
	{
		`foo=; echo "${!foo-def}"`,
		"invalid indirect expansion\nexit status 1 #JUSTERR",
	},
	{
		`x='a b'; echo "${!x}"`,
		"invalid indirect expansion\nexit status 1 #JUSTERR",
	},
	{
		"a=foo_very_long; echo ${a:1}; echo ${a: -1}; echo ${a: -10}; echo ${a:5}",
		"oo_very_long\ng\n_very_long\nery_long\n",
	},
	{
		"a=foo_very_long; echo ${a::2}; echo ${a::-1}; echo ${a: -10}; echo ${a::5}",
		"fo\nfoo_very_lon\n_very_long\nfoo_v\n",
	},
	{
		"a=abc; echo ${a:1:1}",
		"b\n",
	},
	{
		`a=héllo; echo "${a:2}" "${a:1:2}" "${a::-3}" "${a: -2}"`,
		"llo él hé lo\n",
	},
	{
		"a=foo; echo ${a/no/x} ${a/o/i} ${a//o/i} ${a/fo/}",
		"foo fio fii o\n",
	},
	{
		"a=foo; echo ${a/*/xx} ${a//?/na} ${a/o*}",
		"xx nanana f\n",
	},
	{
		"a=12345; echo ${a//[42]} ${a//[^42]} ${a//[!42]}",
		"135 24 24\n",
	},
	{"a=0123456789; echo ${a//[1-35-8]}", "049\n"},
	{"a=]abc]; echo ${a//[]b]}", "ac\n"},
	{"a=-abc-; echo ${a//[-b]}", "ac\n"},
	{`a='x\y'; echo ${a//\\}`, "xy\n"},
	{"a=']'; echo ${a//[}", "]\n"},
	{"a=']'; echo ${a//[]}", "]\n"},
	{"a=']'; echo ${a//[]]}", "\n"},
	{"a='['; echo ${a//[[]}", "\n"},
	{"a=']'; echo ${a//[xy}", "]\n"},
	{"a='abc123'; echo ${a//[[:digit:]]}", "abc\n"},
	{"a='[[:wrong:]]'; echo ${a//[[:wrong:]]}", "[[:wrong:]]\n"},
	{"a='[[:wrong:]]'; echo ${a//[[:}", "[[:wrong:]]\n"},
	{"a='abcx1y'; echo ${a//x[[:digit:]]y}", "abc\n"},
	{`a=xyz; echo "${a/y/a  b}"`, "xa  bz\n"},
	{"a='foo/bar'; echo ${a//o*a/}", "fr\n"},
	{"a=foobar; echo ${a//a/} ${a///b} ${a///}", "foobr foobar foobar\n"},
	{
		"echo ${a:-b}; echo $a; a=; echo ${a:-b}; a=c; echo ${a:-b}",
		"b\n\nb\nc\n",
	},
	{
		"echo ${#:-never} ${?:-never} ${LINENO:-never}",
		"0 0 1\n",
	},
	{
		"echo ${1-one} ${2-two} ${3-three}",
		"one two three\n",
	},
	{
		"set -u; echo ${1}",
		"1: unbound variable\nexit status 1 #JUSTERR",
	},
	{
		"echo ${a-b}; echo $a; a=; echo ${a-b}; a=c; echo ${a-b}",
		"b\n\n\nc\n",
	},
	{
		"echo ${a:=b}; echo $a; a=; echo ${a:=b}; a=c; echo ${a:=b}",
		"b\nb\nb\nc\n",
	},
	{
		"echo ${a=b}; echo $a; a=; echo ${a=b}; a=c; echo ${a=b}",
		"b\nb\n\nc\n",
	},
	{
		"echo ${a:+b}; echo $a; a=; echo ${a:+b}; a=c; echo ${a:+b}",
		"\n\n\nb\n",
	},
	{
		"echo ${a+b}; echo $a; a=; echo ${a+b}; a=c; echo ${a+b}",
		"\n\nb\nb\n",
	},
	{
		"a=b; echo ${a:?err1}; a=; echo ${a:?err2}; unset a; echo ${a:?err3}",
		"b\na: err2\nexit status 1 #JUSTERR",
	},
	{
		"a=b; echo ${a?err1}; a=; echo ${a?err2}; unset a; echo ${a?err3}",
		"b\n\na: err3\nexit status 1 #JUSTERR",
	},
	{
		"echo ${a:?%s}",
		"a: %s\nexit status 1 #JUSTERR",
	},
	{
		"x=aaabccc; echo ${x#*a}; echo ${x##*a}",
		"aabccc\nbccc\n",
	},
	{
		"x=(__a _b c_); echo ${x[@]#_}",
		"_a b c_\n",
	},
	{
		"x=(a__ b_ _c); echo ${x[@]%%_}",
		"a_ b _c\n",
	},
	{
		"x=aaabccc; echo ${x%c*}; echo ${x%%c*}",
		"aaabcc\naaab\n",
	},
	{
		"x=aaabccc; echo ${x%%[bc}",
		"aaabccc\n",
	},
	{
		"a='àÉñ bAr'; echo ${a^}; echo ${a^^}",
		"ÀÉñ bAr\nÀÉÑ BAR\n",
	},
	{
		"a='àÉñ bAr'; echo ${a,}; echo ${a,,}",
		"àÉñ bAr\nàéñ bar\n",
	},
	{
		"a='àÉñ bAr'; echo ${a^?}; echo ${a^^[br]}",
		"ÀÉñ bAr\nàÉñ BAR\n",
	},
	{
		"a='àÉñ bAr'; echo ${a,?}; echo ${a,,[br]}",
		"àÉñ bAr\nàÉñ bAr\n",
	},
	{
		"a=foo; echo ${a^o} ${a^f}; a=OOF; echo ${a,O} ${a,,O} ${a,o}",
		"foo Foo\noOF ooF OOF\n",
	},
	{
		"a=(àÉñ bAr); echo ${a[@]^}; echo ${a[*],,}",
		"ÀÉñ BAr\nàéñ bar\n",
	},
	{
		`a=(foo boo); printf '[%s]' "${a[@]%o}"; echo; printf '[%s]' "${a[@]/o/O}"; echo; printf '[%s]' "${a[@]^}"; echo`,
		"[fo][bo]\n[fOo][bOo]\n[Foo][Boo]\n",
	},
	{
		`set -- foo boo; printf '[%s]' "${@#?}"; echo; IFS=,; echo "${*%o}"`,
		"[oo][oo]\nfo,bo\n",
	},
	{
		`a=(foo boo); IFS=,; echo "${a[*]%o}"`,
		"fo,bo\n",
	},
	{
		`a=(aax abx); echo ${a[@]/x/}; b=("${a[@]/a/z}"); echo "${b[0]}" "${b[1]}"`,
		"aa ab\nzax zbx\n",
	},
	{
		"a=(foo boo); echo ${a[@]%o}; echo ${a[@]}",
		"fo bo\nfoo boo\n",
	},
	{
		"INTERP_X_1=a INTERP_X_2=b; echo ${!INTERP_X_*}",
		"INTERP_X_1 INTERP_X_2\n",
	},
	{
		"INTERP_X_2=b INTERP_X_1=a; echo ${!INTERP_*}",
		"INTERP_GLOBAL INTERP_X_1 INTERP_X_2\n",
	},
	{
		`INTERP_X_2=b INTERP_X_1=a; set -- ${!INTERP_*}; echo $#`,
		"3\n",
	},
	{
		`INTERP_X_2=b INTERP_X_1=a; set -- "${!INTERP_*}"; echo $#`,
		"1\n",
	},
	{
		`INTERP_X_2=b INTERP_X_1=a; set -- ${!INTERP_@}; echo $#`,
		"3\n",
	},
	{
		`INTERP_X_2=b INTERP_X_1=a; set -- "${!INTERP_@}"; echo $#`,
		"3\n",
	},
	{
		`a='b  c'; eval "echo -n ${a} ${a@Q}"`,
		`b c b  c`,
	},
	{
		`a='"\n'; printf "%s %s" "${a}" "${a@E}"`,
		"\"\\n \"\n",
	},

	// ${var@a} and ${var@A}
	{
		`a=foo; echo "<${a@a}>"`,
		"<>\n",
	},
	{
		`declare -a arr=(1 2 3); echo "${arr@a}"`,
		"a\n",
	},
	{
		`declare -A map=([k]=v); echo "${map@a}"`,
		"A\n",
	},
	{
		`export e=1; echo "${e@a}"`,
		"x\n",
	},
	{
		`readonly ro=1; echo "${ro@a}"`,
		"r\n",
	},
	{
		`declare -a arr=(1); export arr; echo "${arr@a}"`,
		"ax\n",
	},
	{
		`a=hello; echo "${a@A}"`,
		"a=hello\n #IGNORE bash always single-quotes",
	},
	{
		`export e=1; echo "${e@A}"`,
		"declare -x e=1\n #IGNORE bash always single-quotes",
	},
	{
		`a=Hello; echo "${a@U}"`,
		"HELLO\n",
	},
	{
		`a=hello; echo "${a@u}"`,
		"Hello\n",
	},
	{
		`a=HELLO; echo "${a@L}"`,
		"hello\n",
	},
	{
		`a=foo; echo "<${a@K}><${a@k}>"`,
		"<foo><foo>\n #IGNORE not implemented; must not panic",
	},
	{
		"declare a; a+=(b); echo ${a[@]} ${#a[@]}",
		"b 1\n",
	},
	{
		`a=""; a+=(b); echo ${a[@]} ${#a[@]}`,
		"b 2\n",
	},
	{
		"f() { local a; a=bad; a=good; echo $a; }; f",
		"good\n",
	},
	{
		`declare x; [[ -v x ]] && echo set || echo unset`,
		"unset\n",
	},
	{
		`declare x=; [[ -v x ]] && echo set || echo unset`,
		"set\n",
	},
	{
		`declare -a x; [[ -v x ]] && echo set || echo unset`,
		"unset\n",
	},
	{
		`declare -A x; [[ -v x ]] && echo set || echo unset`,
		"unset\n",
	},
	{
		`declare -r -x x; [[ -v x ]] && echo set || echo unset`,
		"unset\n",
	},
	{
		`declare -n x; [[ -v x ]] && echo set || echo unset`,
		"unset\n",
	},

	// compgen
	{
		`g() { :; }; a() { :; }; compgen -A function`,
		"a\ng\n",
	},
	{
		// a word operand is a prefix to match
		`foo() { :; }; fob() { :; }; bar() { :; }; compgen -A function fo`,
		"fob\nfoo\n",
	},
	{
		// no matches is a non-zero status, as bash reports it
		`compgen -A function; echo "st=$?"`,
		"st=1\n",
	},
	{
		`foo() { :; }; compgen -A function zz; echo "st=$?"`,
		"st=1\n",
	},
	{
		`alias foo=bar; compgen -A alias`,
		"foo\n",
	},
	{
		`alias zz=ls; compgen -a`,
		"zz\n",
	},
	{
		`xyzzy=1; compgen -A variable xyzzy`,
		"xyzzy\n",
	},
	{
		`xyzzy=1; compgen -v xyzzy`,
		"xyzzy\n",
	},
	{
		// the actions which are not implemented are refused rather than
		// answering nothing, which for compgen would be indistinguishable
		// from a correct empty answer
		`compgen -A directory`,
		"compgen: -A \"directory\": NOT IMPLEMENTED action\nexit status 2 #IGNORE bash implements this action",
	},

	// FUNCNEST bounds function nesting (#349). The violation unwinds the
	// whole function stack and the top level resumes: the rest of the
	// violating line is lost, the next line runs with status 1.
	{
		"FUNCNEST=2; f(){ echo f; g; }; g(){ echo g; h; }; h(){ echo h; }; f; echo never",
		"f\ng\nh: maximum function nesting level exceeded (2)\nexit status 1 #JUSTERR",
	},
	{
		"FUNCNEST=1\nf(){ g; echo never; }\ng(){ :; }\nf\necho st=$?",
		"g: maximum function nesting level exceeded (1)\nst=1\n #JUSTERR",
	},
	{
		// A runaway f(){ f; } dies cleanly instead of hanging the shell.
		"FUNCNEST=20\nx=0\nf(){ x=$((x+1)); f; }\nf\necho x=$x st=$?",
		"f: maximum function nesting level exceeded (20)\nx=20 st=1\n #JUSTERR",
	},
	{
		// Zero and non-numeric values do not bind.
		"FUNCNEST=0; n=0; f(){ n=$((n+1)); [ $n -lt 3 ] && f; }; f; echo n=$n",
		"n=3\n",
	},
	{
		"FUNCNEST=abc; n=0; f(){ n=$((n+1)); [ $n -lt 3 ] && f; }; f; echo n=$n",
		"n=3\n",
	},

	// FUNCNAME
	{
		`f() { echo "[${FUNCNAME[0]:-MISSING}]"; }; f`,
		"[f]\n",
	},
	{
		// innermost first, like bash
		`g() { echo "[${FUNCNAME[@]}]"; }; f() { g; }; f`,
		"[g f]\n",
	},
	{
		`g() { echo "[${FUNCNAME[1]}]"; }; f() { g; }; f`,
		"[f]\n",
	},
	{
		// unset at the top level, and again once the call returns
		`echo "[${FUNCNAME[@]:-EMPTY}] n=${#FUNCNAME[@]}"`,
		"[EMPTY] n=0\n",
	},
	{
		`f() { :; }; f; echo "[${FUNCNAME[@]:-EMPTY}]"`,
		"[EMPTY]\n",
	},
	{
		`g() { :; }; f() { g; echo "[${FUNCNAME[@]}]"; }; f`,
		"[f]\n",
	},
	{
		// a subshell inside a function is still inside it
		`f() { ( echo "[${FUNCNAME[0]}]" ); }; f`,
		"[f]\n",
	},

	// declare -i
	{
		`declare -i n; n=1+1; echo "[$n]"`,
		"[2]\n",
	},
	{
		`declare -i n=2+3; echo "[$n]"`,
		"[5]\n",
	},
	{
		// a name which is not set is zero in arithmetic, so this is not an error
		`declare -i n; n=abc; echo "[$n]"`,
		"[0]\n",
	},
	{
		`declare -i n; n=; echo "[$n]"`,
		"[0]\n",
	},
	{
		// += adds rather than concatenating
		`declare -i n=1; n+=2; echo "[$n]"`,
		"[3]\n",
	},
	{
		`declare -i n; m=3; n=m*2; echo "[$n]"`,
		"[6]\n",
	},
	{
		`declare -i n; n=7/2; echo "[$n]"`,
		"[3]\n",
	},
	{
		// the attribute does not re-evaluate what is already there
		`x=abc; declare -i x; echo "[$x]"`,
		"[abc]\n",
	},
	{
		`declare -i n=1; declare +i n; n=1+1; echo "[$n]"`,
		"[1+1]\n",
	},
	{
		`f() { local -i n; n=2+2; echo "[$n]"; }; f`,
		"[4]\n",
	},
	{
		`declare -ia a; a[0]=1+1; echo "[${a[0]}]"`,
		"[2]\n",
	},
	{
		`declare -i n=2; declare -p n`,
		`declare -i n="2"` + "\n",
	},
	{
		// every flag clustered into one argument applies, not just the first
		`declare -ri n=1+1; declare -p n`,
		`declare -ir n="2"` + "\n",
	},
	{
		`declare -ix n=1+1; declare -p n`,
		`declare -ix n="2"` + "\n",
	},
	{
		`declare -rx v=1; declare -p v`,
		`declare -rx v="1"` + "\n",
	},
	{
		`declare -ia a=(1); declare -p a`,
		`declare -ai a=([0]="1")` + "\n",
	},

	// The array attribute is sticky (#378): a naked declare -a/-A
	// declares an unset array that prints bare, a later scalar
	// assignment fills element 0 instead of flattening to a scalar,
	// converting a scalar keeps its value at element 0, and converting
	// one array kind to the other is an error with the data kept.
	{
		`declare -a c; declare -p c`,
		"declare -a c\n",
	},
	{
		`declare -A m; declare -p m`,
		"declare -A m\n",
	},
	{
		`declare -a c; c=4; declare -p c`,
		`declare -a c=([0]="4")` + "\n",
	},
	{
		`declare -a r; r="(5)"; declare -p r`,
		`declare -a r=([0]="(5)")` + "\n",
	},
	{
		`x=5; declare -a x; declare -p x`,
		`declare -a x=([0]="5")` + "\n",
	},
	{
		`x=5; declare -A x; declare -p x`,
		`declare -A x=([0]="5" )` + "\n",
	},
	{
		`declare -a q=5; declare -p q`,
		`declare -a q=([0]="5")` + "\n",
	},
	{
		`declare -a x=(1); declare -A x; echo rc=$?; declare -p x`,
		"declare: x: cannot convert indexed to associative array\nrc=1\n" +
			`declare -a x=([0]="1")` + "\n #JUSTERR",
	},
	{
		`declare -A m=([k]=v); declare -a m; echo rc=$?; declare -p m`,
		"declare: m: cannot convert associative to indexed array\nrc=1\n" +
			`declare -A m=([k]="v" )` + "\n #JUSTERR",
	},
	{
		`declare -ia n; n=2+3; declare -p n`,
		`declare -ai n=([0]="5")` + "\n",
	},
	{
		`declare -a d; d[3]=x; declare -p d`,
		`declare -a d=([3]="x")` + "\n",
	},
	{
		`declare -a e; e+=(z); declare -p e`,
		`declare -a e=([0]="z")` + "\n",
	},
	{
		`a=(1 2); export a=5; declare -p a`,
		`declare -ax a=([0]="5" [1]="2")` + "\n",
	},
	{
		`declare -A f=([a]=b); declare f[qux]=assigned; echo "${f[qux]}-${f[a]}"`,
		"assigned-b\n",
	},
	{
		`declare -A m=([a]=b); m=x; echo "${m[0]}-${m[a]}"`,
		"x-b\n",
	},

	// A temp-env assignment before a declaration utility (#380): the
	// binding is what the utility sees, and when the utility declares
	// the name — not merely queries it — the binding is promoted in
	// its scope instead of unwound; a function-local declaration
	// shadows the promoted scope, so the unwind lands underneath it.
	{
		`func(){ var=value declare -x var; echo -n "inside: "; declare -p var; }; var=one; func; echo -n "outside: "; declare -p var`,
		`inside: declare -x var="value"` + "\noutside: " + `declare -- var="one"` + "\n",
	},
	{
		`foo="" export foo; declare -p foo`,
		`declare -x foo=""` + "\n",
	},
	{
		`foo=bar declare -p foo; echo after: ${foo-unset}`,
		`declare -x foo="bar"` + "\nafter: unset\n",
	},
	{
		`tempvar1=foo declare -r tempvar1; declare -p tempvar1`,
		`declare -rx tempvar1="foo"` + "\n",
	},
	{
		`v=base; f(){ local v=fl; g; echo f:$v; }; g(){ v=temp declare -x v; echo g:$v; }; f; echo out:$v`,
		"g:temp\nf:fl\nout:base\n",
	},
	{
		`v=base; f(){ local v=fl; g; echo f:$v; }; g(){ v=temp export v; echo g:$v; }; f; echo out:$v`,
		"g:temp\nf:temp\nout:base\n",
	},
	{
		`v=base; f(){ local v=fl; g; echo f:$v; }; g(){ v=temp true; echo g:$v; }; f; echo out:$v`,
		"g:fl\nf:fl\nout:base\n",
	},
	{
		`ref=xxx typeset -p ref; echo ${ref-unset}`,
		`declare -x ref="xxx"` + "\nunset\n",
	},
	{
		`foo=bar :; echo colon:${foo-unset}`,
		"colon:unset\n",
	},

	// A new local starts unset rather than inheriting the outer
	// variable (#381): only the export attribute carries over, a
	// readonly outer refuses the declaration, and a second `local` in
	// the same scope keeps what the first one holds. `typeset` is
	// declare's synonym and localizes with it (#382).
	{
		`V=abc; f(){ local V; echo "${V-unset}"; }; f`,
		"unset\n",
	},
	{
		`V=abc; f(){ declare V; echo "${V-unset}"; declare -p V; }; f`,
		"unset\ndeclare -- V\n",
	},
	{
		`f() { typeset v=inner; :; }; v=outer; f; echo "v=$v"`,
		"v=outer\n",
	},
	{
		`V=abc; f(){ typeset V; echo "${V-unset}"; }; f; echo $V`,
		"unset\nabc\n",
	},
	{
		// A leaked `typeset IFS=:` poisons every later expansion in
		// the file, which is how ifs.tests found this.
		`f(){ typeset IFS=:; }; f; x="a b"; set -- $x; echo $#`,
		"2\n",
	},
	{
		`f(){ local V=1; local V; echo "${V-unset}"; }; f`,
		"1\n",
	},
	{
		`V=out; f(){ local V=fl; g; }; g(){ local V; echo "${V-unset}"; }; f`,
		"unset\n",
	},
	{
		`declare -x V=abc; f(){ local V; declare -p V; }; f`,
		"declare -x V\n",
	},
	{
		`declare -i V=5; f(){ local V; declare -p V; V=2+2; declare -p V; }; f`,
		"declare -- V\n" + `declare -- V="2+2"` + "\n",
	},
	{
		`declare -a V=(1); f(){ local V; declare -p V; }; f`,
		"declare -- V\n",
	},
	{
		`declare -r V=5; f(){ local V W=ok; declare -p W; }; f`,
		"local: V: readonly variable\n" + `declare -- W="ok"` + "\n #JUSTERR",
	},
	{
		`shopt -s localvar_inherit; V=abc; f(){ local V; echo "${V-unset}"; }; f`,
		"abc\n",
	},
	{
		`shopt -s localvar_inherit; declare -x V=abc; f(){ local -x V; declare -p V; }; f`,
		`declare -x V="abc"` + "\n",
	},

	// declare -g writes the global scope through any local shadowing
	// the name (#379); reads stay dynamically scoped, so both
	// functions still see the local.
	{
		`f(){ local v; g; echo f:$v; }; g(){ declare -g v=two; echo g:$v; }; f; echo FIN:$v`,
		"g:\nf:\nFIN:two\n",
	},
	{
		`f(){ local v=one; declare -g v=two; echo in:$v; }; f; echo out:$v`,
		"in:one\nout:two\n",
	},
	{
		`v=g0; f(){ local v=one; g; }; g(){ declare -g v; v=three; }; f; echo $v`,
		"g0\n",
	},
	{
		`f(){ declare -ga arr=(1 2); }; f; declare -p arr`,
		`declare -a arr=([0]="1" [1]="2")` + "\n",
	},
	// The string form of a compound assignment (#379): parsed as an
	// array literal — its elements expanded — only under an explicit
	// -a/-A or an existing array; the bare form stays a literal
	// string (bash 5.1), and so does an unbalanced "(".
	{
		`aux=v; declare -ga "$aux=( a b )"; declare -p v`,
		`declare -a v=([0]="a" [1]="b")` + "\n",
	},
	{
		`aux="v=( a b )"; declare "$aux"; declare -p v`,
		`declare -- v="( a b )"` + "\n",
	},
	{
		`v=(1); declare "v=( new )"; declare -p v`,
		`declare -a v=([0]="new")` + "\n",
	},
	{
		`x="\$y"; y=z; declare -a v="( $x )"; declare -p v`,
		`declare -a v=([0]="z")` + "\n",
	},
	{
		`aux="( a b )"; declare -a v=$aux; declare -p v`,
		`declare -a v=([0]="a" [1]="b")` + "\n",
	},
	{
		`aux="( a b )"; declare -a v=("$aux"); declare -p v`,
		`declare -a v=([0]="( a b )")` + "\n",
	},
	{
		`declare -a "w=( a b"; echo rc=$?; declare -p w`,
		"rc=0\n" + `declare -a w=([0]="( a b")` + "\n",
	},
	{
		`declare -a "w=()"; declare -p w`,
		"declare -a w=()\n",
	},

	// test -v on arrays (#378): a bare array name tests element 0, a
	// subscript tests that element (@/* meaning any, except that they
	// are ordinary keys for an associative array), and a scalar is
	// element 0 of itself.
	{
		`typeset -A A; A[a]=1; [ -v A ] && echo set || echo unset`,
		"unset\n",
	},
	{
		`a[1]=1; [ -v a ] || echo unset; [ -v "a[1]" ] && echo e1; [ -v "a[@]" ] && echo any`,
		"unset\ne1\nany\n",
	},
	{
		`s=x; [ -v "s[0]" ] && echo s0; [ -v "s[@]" ] && echo sat; [ -v "s[1]" ] || echo no1`,
		"s0\nsat\nno1\n",
	},
	{
		`declare -A B; B[k]=v; [ -v "B[@]" ] || echo nokey; B[@]=x; [ -v "B[@]" ] && echo litkey`,
		"nokey\nlitkey\n",
	},
	{
		`a=(x y); [ -v "a[-1]" ] && echo neg`,
		"neg\n",
	},
	{
		`a=(x y); [ -v "a[-5]" ]`,
		"a: bad array subscript\nexit status 1 #JUSTERR",
	},

	// declare -p output has to re-read as the value it printed (#383):
	// a control character forces ANSI-C quoting, and the characters
	// that would still expand inside double quotes are escaped.
	{
		`v=$'a\nb'; declare -p v`,
		`declare -- v=$'a\nb'` + "\n",
	},
	{
		`export FOO='$$'; declare -p FOO`,
		`declare -x FOO="\$\$"` + "\n",
	},
	{
		`v='` + "`c`" + `'; declare -p v`,
		`declare -- v="\` + "`c\\`" + `"` + "\n",
	},
	{
		`v=$'\t\\'; declare -p v`,
		`declare -- v=$'\t\\'` + "\n",
	},
	{
		`v="it's"; declare -p v`,
		`declare -- v="it's"` + "\n",
	},
	{
		`a=(x $'p\nq'); declare -p a`,
		`declare -a a=([0]="x" [1]=$'p\nq')` + "\n",
	},
	{
		`declare -A m=([k$]=$'v\n'); declare -p m`,
		`declare -A m=(["k\$"]=$'v\n' )` + "\n",
	},
	// An attribute flag with no operands lists the variables carrying
	// it (#384); bare declare prints POSIX name=value pairs instead.
	{
		`declare -A f; f[a]=1; declare -A | grep '^declare -A f'`,
		`declare -A f=([a]="1" )` + "\n",
	},
	{
		`declare -i n=1; declare -i | grep ' n='`,
		`declare -i n="1"` + "\n",
	},
	{
		`zz=1; declare | grep '^zz'`,
		"zz=1\n",
	},
	{
		`zz=1; declare -- | grep '^zz'`,
		"zz=1\n",
	},
	{
		`zz=1; declare -p | grep ' zz='`,
		`declare -- zz="1"` + "\n",
	},
	{
		`declare -- v=1; declare -p v`,
		`declare -- v="1"` + "\n",
	},

	// declare's remaining option surface (#385): -u/-l/-c transform on
	// every assignment, two of them cancel rather than stack, +x/+r/+t
	// remove attributes (readonly refusing to be removed), -I inherits
	// the enclosing scope, and `local -` restores the shell options
	// when the function returns.
	{
		`declare -u u; u=abc; echo $u; declare -p u`,
		"ABC\n" + `declare -u u="ABC"` + "\n",
	},
	{
		`declare -l l=ABC; echo $l; declare -p l`,
		"abc\n" + `declare -l l="abc"` + "\n",
	},
	{
		`declare -c c=hello_world; echo $c; declare -c d="hello world"; echo $d`,
		"Hello_world\nHello world\n",
	},
	{
		`declare -u a=(x y); declare -p a`,
		`declare -au a=([0]="X" [1]="Y")` + "\n",
	},
	{
		`declare -A m; declare -u m; m[k]=vv; declare -p m`,
		`declare -Au m=([k]="VV" )` + "\n",
	},
	{
		`declare -u u=abc; u+=def; echo $u`,
		"ABCDEF\n",
	},
	{
		`declare -u u; u=abc; declare +u u; echo $u; u=xyz; echo $u`,
		"ABC\nxyz\n",
	},
	{
		`declare -ul x=ABC; declare -p x`,
		`declare -- x="ABC"` + "\n",
	},
	{
		`declare -u u=a; declare -l u; declare -p u; u=QQ; echo $u`,
		`declare -l u="A"` + "\nqq\n",
	},
	{
		`export V=1; declare +x V; declare -p V`,
		`declare -- V="1"` + "\n",
	},
	{
		`readonly V=1; declare +r V; declare -p V`,
		"declare: V: readonly variable\n" + `declare -r V="1"` + "\n #JUSTERR",
	},
	{
		`declare -tux w=v; declare -p w`,
		`declare -txu w="V"` + "\n",
	},
	{
		`declare -irtx z=5; declare -p z`,
		`declare -irtx z="5"` + "\n",
	},
	{
		`V=out; f(){ local V=in; g; }; g(){ local -I V; echo "${V-unset}"; }; f`,
		"in\n",
	},
	{
		`unset Z; f(){ local -I Z; echo "${Z-unset}"; }; f`,
		"unset\n",
	},
	{
		`set -e; f(){ local -; set +e; case $- in *e*) echo in-e;; *) echo in-noe;; esac; }; f; case $- in *e*) echo out-e;; esac`,
		"in-noe\nout-e\n",
	},
	{
		`f(){ local -; set -u; case $- in *u*) echo in-u;; esac; }; f; case $- in *u*) echo out-u;; *) echo no-u;; esac`,
		"in-u\nno-u\n",
	},

	// export -f marks a function for export rather than printing it
	// (#387), and an -x listing is filtered to the exported ones
	// (#388) where koi listed every function it had.
	{
		`a(){ :; }; b(){ :; }; export -f a; declare -xF; echo end`,
		"declare -fx a\nend\n",
	},
	{
		`a(){ :; }; b(){ :; }; declare -xF; echo end`,
		"end\n",
	},
	{
		`f(){ :; }; export -f f; export -nf f; declare -xF; echo end`,
		"end\n",
	},
	{
		`f(){ echo body; }; declare -xf f; echo ---; declare -xF`,
		"---\ndeclare -fx f\n",
	},
	{
		`export -f nope`,
		"export: nope: not a function\nexit status 1 #JUSTERR",
	},
	{
		// A function name is not a variable name, so a dashed name
		// exports rather than being refused.
		`foo-bar(){ :; }; export -f foo-bar; echo rc=$?`,
		"rc=0\n",
	},
	{
		// `export -n` removes the export attribute; it is a nameref
		// only for declare/local/typeset.
		`V=1; export V; export -n V; declare -p V`,
		`declare -- V="1"` + "\n",
	},

	// declare -F
	{
		`f() { :; }; declare -F f; echo "st=$?"`,
		"f\nst=0\n",
	},
	{
		`declare -F nope; echo "st=$?"`,
		"st=1\n",
	},
	{
		// a missing name does not stop the ones which follow
		`f() { :; }; declare -F f nope; echo "st=$?"`,
		"f\nst=1\n",
	},
	{
		// with no names it lists every function, sorted
		`zeta() { :; }; alpha() { :; }; mid() { :; }; declare -F`,
		"declare -f alpha\ndeclare -f mid\ndeclare -f zeta\n",
	},
	{
		`declare -F; echo "st=$?"`,
		"st=0\n",
	},
	{
		`f() { :; }; typeset -F f`,
		"f\n",
	},
	{
		// the flag is the interpreter's, so it survives a re-parse
		`f() { :; }; eval "declare -F f"; echo "st=$?"`,
		"f\nst=0\n",
	},

	// declare -f and declare -p
	{
		`f() { echo hello; }; declare -f f`,
		"f()\n{ echo hello; }\n #IGNORE output format differs from bash",
	},
	{
		`declare -f nonexistent 2>/dev/null; echo "exit: $?"`,
		"exit: 1\n",
	},
	{
		`f() { echo hello; }; declare -f f >/dev/null && echo "f exists"`,
		"f exists\n",
	},
	{
		`a=hello; declare -p a`,
		"declare -- a=\"hello\"\n",
	},
	{
		`declare -a arr=(1 2 3); declare -p arr`,
		"declare -a arr=([0]=\"1\" [1]=\"2\" [2]=\"3\")\n",
	},
	{
		`export e=1; declare -p e`,
		"declare -x e=\"1\"\n",
	},
	{
		`readonly c=immutable; declare -p c`,
		"declare -r c=\"immutable\"\n",
	},
	{
		`declare -p nonexistent 2>/dev/null; echo "exit: $?"`,
		"exit: 1\n",
	},

	// if
	{
		"if true; then echo foo; fi",
		"foo\n",
	},
	{
		"if false; then echo foo; fi",
		"",
	},
	{
		"if GOSH_CMD=print_fail $GOSH_PROG; then echo foo; fi",
		"exec fail\n",
	},
	{
		"if true; then echo foo; else echo bar; fi",
		"foo\n",
	},
	{
		"if false; then echo foo; else echo bar; fi",
		"bar\n",
	},
	{
		"if true; then false; fi",
		"exit status 1",
	},
	{
		"if false; then :; else false; fi",
		"exit status 1",
	},
	{
		"if false; then :; elif true; then echo foo; fi",
		"foo\n",
	},
	{
		"if false; then :; elif false; then :; elif true; then echo foo; fi",
		"foo\n",
	},
	{
		"if false; then :; elif false; then :; else echo foo; fi",
		"foo\n",
	},

	// while
	{
		"while false; do echo foo; done",
		"",
	},
	{
		"while GOSH_CMD=print_fail $GOSH_PROG; do echo foo; done",
		"exec fail\n",
	},
	{
		"while true; do exit 1; done",
		"exit status 1",
	},
	{
		"while true; do break; done",
		"",
	},
	{
		"while true; do while true; do break 2; done; done",
		"",
	},

	// until
	{
		"until true; do echo foo; done",
		"",
	},
	{
		"until false; do exit 1; done",
		"exit status 1",
	},
	{
		"until false; do break; done",
		"",
	},

	// for
	{
		"for i in 1 2 3; do echo $i; done",
		"1\n2\n3\n",
	},
	{
		"for i in 1 2 3; do echo $i; exit; done",
		"1\n",
	},
	{
		"for i in 1 2 3; do echo $i; false; done",
		"1\n2\n3\nexit status 1",
	},
	{
		"for i in 1 2 3; do echo $i; break; done",
		"1\n",
	},
	{
		"for i in 1 2 3; do echo $i; continue; echo foo; done",
		"1\n2\n3\n",
	},
	{
		"for i in 1 2; do for j in a b; do echo $i $j; continue 2; done; done",
		"1 a\n2 a\n",
	},
	{
		"for ((i=0; i<3; i++)); do echo $i; done",
		"0\n1\n2\n",
	},
	// for, with missing Init, Cond, Post
	{
		"i=0; for ((; i<3; i++)); do echo $i; done",
		"0\n1\n2\n",
	},
	{
		"for ((i=0;; i++)); do if [ $i -ge 3 ]; then break; fi; echo $i; done",
		"0\n1\n2\n",
	},
	{
		"for ((i=0; i<3;)); do echo $i; i=$((i+1)); done",
		"0\n1\n2\n",
	},
	{
		"i=0; for ((;;)); do if [ $i -ge 3 ]; then break; fi; echo $i; i=$((i+1)); done",
		"0\n1\n2\n",
	},
	// TODO: uncomment once expandEnv.Set starts returning errors
	// {
	// 	"readonly i; for ((i=0; i<3; i++)); do echo $i; done",
	// 	"0\n1\n2\n",
	// },
	{
		"for ((i=5; i>0; i--)); do echo $i; break; done",
		"5\n",
	},
	{
		"for i in 1 2; do for j in a b; do echo $i $j; done; break; done",
		"1 a\n1 b\n",
	},
	{
		"for i in 1 2 3; do :; done; echo $i",
		"3\n",
	},
	{
		"for ((i=0; i<3; i++)); do :; done; echo $i",
		"3\n",
	},
	{
		"set -- a 'b c'; for i in; do echo $i; done",
		"",
	},
	{
		"set -- a 'b c'; for i; do echo $i; done",
		"a\nb c\n",
	},

	// block
	{
		"{ echo foo; }",
		"foo\n",
	},
	{
		"{ false; }",
		"exit status 1",
	},

	// subshell
	{
		"(echo foo)",
		"foo\n",
	},
	{
		"(false)",
		"exit status 1",
	},
	{
		"(exit 1)",
		"exit status 1",
	},
	{
		"(false); echo foo",
		"foo\n",
	},
	{
		"(exit 0); echo foo",
		"foo\n",
	},
	{
		"(exit 1); echo foo",
		"foo\n",
	},
	{
		"(foo=bar; echo $foo); echo $foo",
		"bar\n\n",
	},
	{
		"(echo() { printf 'bar\n'; }; echo); echo",
		"bar\n\n",
	},
	{
		"unset INTERP_GLOBAL & echo $INTERP_GLOBAL",
		"value\n",
	},
	{
		"(fn() { :; }) & pwd >/dev/null",
		"",
	},
	{
		"x[0]=x; (echo ${x[0]}; x[0]=y; echo ${x[0]}); echo ${x[0]}",
		"x\ny\nx\n",
	},
	{
		`x[3]=x; (x[3]=y); echo ${x[3]}`,
		"x\n",
	},
	{
		"shopt -s expand_aliases; alias f='echo x'\nf\n(f\nalias f='echo y'\neval f\n)\nf\n",
		"x\nx\ny\nx\n",
	},
	{
		"set -- a; echo $1; (echo $1; set -- b; echo $1); echo $1",
		"a\na\nb\na\n",
	},
	{"false; ( echo $? )", "1\n"},

	// cd/pwd
	{"[[ fo~ == 'fo~' ]]", ""},
	{`[[ 'ab\c' == *\\* ]]`, ""},
	{`[[ foo/bar == foo* ]]`, ""},
	{"[[ a == [ab ]]", "exit status 1"},
	{`HOME='/*'; echo ~; echo "$HOME"`, "/*\n/*\n"},
	{`test -d ~`, ""},
	{
		`for flag in b c d e f g h k L p r s S u w x; do test -$flag ""; echo -n "$flag$? "; done`,
		`b1 c1 d1 e1 f1 g1 h1 k1 L1 p1 r1 s1 S1 u1 w1 x1 `,
	},
	{`foo=~; test -d $foo`, ""},
	{`foo=~; test -d "$foo"`, ""},
	{`foo='~'; test -d $foo`, "exit status 1"},
	{`foo='~'; [ $foo == '~' ]`, ""},
	{
		`[[ ~ == "$HOME" ]] && [[ ~/foo == "$HOME/foo" ]]`,
		"",
	},
	{
		`HOME=$PWD/home; mkdir home; touch home/f; [[ -e ~/f ]]`,
		"",
	},
	{
		`HOME=$PWD/home; mkdir home; touch home/f; [[ ~/f -ef $HOME/f ]]`,
		"",
	},
	{
		"[[ ~noexist == '~noexist' ]]",
		"",
	},
	{
		`w="$HOME"; cd; [[ $PWD == "$w" ]]`,
		"",
	},
	{
		`cd ''`,
		"cd: empty directory path\nexit status 1 #JUSTERR",
	},
	{
		`HOME=/foo; echo $HOME`,
		"/foo\n",
	},
	{
		"cd noexist",
		"cd: no such file or directory: \"noexist\"\nexit status 1 #JUSTERR",
	},
	{
		"mkdir -p a/b && cd a && cd b && cd ../..",
		"",
	},
	{
		">a && cd a",
		"cd: no such file or directory: \"a\"\nexit status 1 #JUSTERR",
	},
	{
		`[[ $PWD == "$(pwd)" ]]`,
		"",
	},
	{
		"PWD=changed; [[ $PWD == changed ]]",
		"",
	},
	{
		"PWD=changed; mkdir a; cd a; [[ $PWD == changed ]]",
		"exit status 1",
	},
	{
		`mkdir %s; old="$PWD"; cd %s; [[ $old == "$PWD" ]]`,
		"exit status 1",
	},
	{
		`old="$PWD"; mkdir a; cd a; cd ..; [[ $old == "$PWD" ]]`,
		"",
	},
	{
		`[[ $PWD == "$OLDPWD" ]]`,
		"exit status 1",
	},
	{
		`old="$PWD"; mkdir a; cd a; [[ $old == "$OLDPWD" ]]`,
		"",
	},
	{
		`mkdir a; ln -s a b; [[ $(cd a && pwd) == "$(cd b && pwd)" ]]; echo $?`,
		"1\n",
	},
	{
		`pwd -a`,
		"invalid option: \"-a\"\nexit status 2 #JUSTERR",
	},
	{
		`pwd -L -P -a`,
		"invalid option: \"-a\"\nexit status 2 #JUSTERR",
	},
	{
		`mkdir a; ln -s a b; [[ "$(cd a && pwd -P)" == "$(cd b && pwd -P)" ]]`,
		"",
	},
	{
		`mkdir a; ln -s a b; [[ "$(cd a && pwd -P)" == "$(cd b && pwd -L)" ]]; echo $?`,
		"1\n",
	},
	{
		`orig="$PWD"; mkdir a; cd a; cd - >/dev/null; [[ "$PWD" == "$orig" ]]`,
		"",
	},
	{
		`orig="$PWD"; mkdir a; cd a; [[ $(cd -) == "$orig" ]]`,
		"",
	},

	// dirs/pushd/popd
	{"set -- $(dirs); echo $# ${#DIRSTACK[@]}", "1 1\n"},
	{"pushd", "pushd: no other directory\nexit status 1 #JUSTERR"},
	{"pushd -n", ""},
	{"pushd foo bar", "pushd: too many arguments\nexit status 2 #JUSTERR"},
	{"pushd does-not-exist; set -- $(dirs); echo $#", "pushd: no such file or directory: \"does-not-exist\"\n1\n #IGNORE"},
	{"mkdir a; pushd a >/dev/null; set -- $(dirs); echo $#", "2\n"},
	{"mkdir a; set -- $(pushd a); echo $#", "2\n"},
	{
		`mkdir a; pushd a >/dev/null; set -- $(dirs); [[ $1 == "$HOME" ]]`,
		"exit status 1",
	},
	{
		`mkdir a; pushd a >/dev/null; [[ ${DIRSTACK[0]} == "$HOME" ]]`,
		"exit status 1",
	},
	{
		`old=$(dirs); mkdir a; pushd a >/dev/null; pushd >/dev/null; set -- $(dirs); [[ $1 == "$old" ]]`,
		"",
	},
	{
		`old=$(dirs); mkdir a; pushd a >/dev/null; pushd -n >/dev/null; set -- $(dirs); [[ $1 == "$old" ]]`,
		"exit status 1",
	},
	{
		"mkdir a; pushd a >/dev/null; pushd >/dev/null; rm -r a; pushd",
		"pushd: no such file or directory: ABS_PATH_A\nexit status 1 #JUSTERR",
	},
	{
		`old=$(dirs); mkdir a; pushd -n a >/dev/null; set -- $(dirs); [[ $1 == "$old" ]]`,
		"",
	},
	{
		`old=$(dirs); mkdir a; pushd -n a >/dev/null; pushd >/dev/null; set -- $(dirs); [[ $1 == "$old" ]]`,
		"exit status 1",
	},
	{"popd", "popd: directory stack empty\nexit status 1 #JUSTERR"},
	{"popd -n", "popd: directory stack empty\nexit status 1 #JUSTERR"},
	{"popd foo", "popd: invalid argument\nexit status 2 #JUSTERR"},
	{"old=$(dirs); mkdir a; pushd a >/dev/null; set -- $(popd); echo $#", "1\n"},
	{
		`old=$(dirs); mkdir a; pushd a >/dev/null; popd >/dev/null; [[ $(dirs) == "$old" ]]`,
		"",
	},
	{"old=$(dirs); mkdir a; pushd a >/dev/null; set -- $(popd -n); echo $#", "1\n"},
	{
		`old=$(dirs); mkdir a; pushd a >/dev/null; popd -n >/dev/null; [[ $(dirs) == "$old" ]]`,
		"exit status 1",
	},
	{
		"mkdir a; pushd a >/dev/null; pushd >/dev/null; rm -r a; popd",
		"popd: no such file or directory: ABS_PATH_A\nexit status 1 #JUSTERR",
	},

	// binary cmd
	{
		"true && echo foo || echo bar",
		"foo\n",
	},
	{
		"false && echo foo || echo bar",
		"bar\n",
	},

	// func
	{
		"foo() { echo bar; }; foo",
		"bar\n",
	},
	{
		"foo() { echo $1; }; foo",
		"\n",
	},
	{
		"foo() { echo $1; }; foo a b",
		"a\n",
	},
	{
		"foo() { echo $1; bar c d; echo $2; }; bar() { echo $2; }; foo a b",
		"a\nd\nb\n",
	},
	{
		`foo() { echo $#; }; foo; foo 1 2 3; foo "a b"; echo $#`,
		"0\n3\n1\n0\n",
	},
	{
		`foo() { for a in $*; do echo "$a"; done }; foo 'a  1' 'b  2'`,
		"a\n1\nb\n2\n",
	},
	{
		`foo() { for a in "$*"; do echo "$a"; done }; foo 'a  1' 'b  2'`,
		"a  1 b  2\n",
	},
	{
		`foo() { for a in "foo$*"; do echo "$a"; done }; foo 'a  1' 'b  2'`,
		"fooa  1 b  2\n",
	},
	{
		`foo() { for a in $@; do echo "$a"; done }; foo 'a  1' 'b  2'`,
		"a\n1\nb\n2\n",
	},
	{
		`foo() { for a in "$@"; do echo "$a"; done }; foo 'a  1' 'b  2'`,
		"a  1\nb  2\n",
	},

	// alias (note the input newlines)
	{
		"alias foo; alias foo=echo; alias foo; alias foo=; alias foo",
		"alias: \"foo\" not found\nalias foo='echo'\nalias foo=''\n #IGNORE",
	},
	{
		"shopt -s expand_aliases; alias foo=echo\nfoo foo; foo bar",
		"foo\nbar\n",
	},
	{
		"shopt -s expand_aliases; alias true=echo\ntrue foo; unalias true\ntrue bar",
		"foo\n",
	},
	{
		"shopt -s expand_aliases; alias echo='echo a'\necho b c",
		"a b c\n",
	},
	{
		"shopt -s expand_aliases; alias foo='echo '\nfoo foo; foo bar",
		"echo\nbar\n",
	},

	// case
	{
		"case b in x) echo foo ;; a|b) echo bar ;; esac",
		"bar\n",
	},
	{
		// ';&' runs the next item's statements without testing its patterns
		"case a in a) echo A ;& b) echo B ;; esac",
		"A\nB\n",
	},
	{
		"case a in a) echo A ;& z) echo Z ;; esac",
		"A\nZ\n",
	},
	{
		"case a in a) ;& b) echo B ;; esac",
		"B\n",
	},
	{
		// ';;&' carries on testing the patterns which follow
		"case a in a) echo A ;;& a*) echo A2 ;; esac",
		"A\nA2\n",
	},
	{
		"case a in a) echo A ;;& z) echo Z ;; esac",
		"A\n",
	},
	{
		"case ab in a*) echo 1 ;;& *b) echo 2 ;;& ab) echo 3 ;; esac",
		"1\n2\n3\n",
	},
	{
		"case a in z) echo Z ;;& a) echo A ;; esac",
		"A\n",
	},
	{
		// the two mix, and a plain ';;' still stops
		"case a in a) echo 1 ;& z) echo 2 ;;& *) echo 3 ;; esac",
		"1\n2\n3\n",
	},
	{
		"case a in a) echo A ;; a*) echo A2 ;; esac",
		"A\n",
	},
	{
		// the status is that of the last body which ran
		`case a in a) false ;& b) true ;; esac; echo "st=$?"`,
		"st=0\n",
	},
	{
		`case a in a) true ;& b) false ;; esac; echo "st=$?"`,
		"st=1\n",
	},
	{
		"case b in x) echo foo ;; y|z) echo bar ;; esac",
		"",
	},
	{
		"case foo in bar) echo foo ;; *) echo bar ;; esac",
		"bar\n",
	},
	{
		"case foo in *o*) echo bar ;; esac",
		"bar\n",
	},
	{
		"case foo in '*') echo x ;; f*) echo y ;; esac",
		"y\n",
	},
	{
		`case 0 in [\0]) echo bar ;; esac`,
		"bar\n",
	},
	{
		`case d in [\d]) echo bar ;; esac`,
		"bar\n",
	},
	{
		`case '[' in [) echo match ;; *) echo miss ;; esac`,
		"match\n",
	},
	{
		`case '[abc' in [a*) echo match ;; *) echo miss ;; esac`,
		"match\n",
	},
	{
		`touch a b; x=']'; echo [ab$x`,
		"a b\n",
	},

	// exec
	{
		"$GOSH_PROG 'echo foo'",
		"foo\n",
	},
	{
		"$GOSH_PROG 'echo foo >&2' >/dev/null",
		"foo\n",
	},
	{
		"echo foo | $GOSH_PROG 'cat >&2' >/dev/null",
		"foo\n",
	},
	{
		"$GOSH_PROG 'exit 1'",
		"exit status 1",
	},
	{
		"exec >/dev/null; echo foo",
		"",
	},
	{
		"exec >a; echo foo; cat a >&2",
		"foo\n",
	},
	{
		"exec >a; echo one >b; echo two; cat a b >&2",
		"two\none\n",
	},
	{
		"{ exec >a; echo in; } >b; echo out; cat a b >&2",
		"out\nin\n",
	},

	// return
	{"return", "return: can only be done from a func or sourced script\nexit status 1 #JUSTERR"},
	{"f() { return; }; f", ""},
	{"f() { return 2; }; f", "exit status 2"},
	{"f() { echo foo; return; echo bar; }; f", "foo\n"},
	{"f1() { :; }; f2() { f1; return; }; f2", ""},
	{"echo 'return' >a; source ./a", ""},
	{"echo 'return' >a; source ./a; return", "return: can only be done from a func or sourced script\nexit status 1 #JUSTERR"},
	{"echo 'return 2' >a; source ./a", "exit status 2"},
	{"echo 'echo foo; return; echo bar' >a; source ./a", "foo\n"},

	// command
	{"command", ""},
	{"command -o echo", "command: invalid option \"-o\"\nexit status 2 #JUSTERR"},
	{"command -vo echo", "command: invalid option \"-o\"\nexit status 2 #JUSTERR"},
	{"echo() { :; }; echo foo", ""},
	{"echo() { :; }; command echo foo", "foo\n"},
	{"command -v does-not-exist", "exit status 1"},
	{"foo() { :; }; command -v foo", "foo\n"},
	{"foo() { :; }; command -v does-not-exist foo", "foo\n"},
	{"command -v echo", "echo\n"},
	{"[[ $(command -v $PATH_PROG) == $PATH_PROG ]]", "exit status 1"},

	// cmd substitution
	{
		"echo foo $(printf bar)",
		"foo bar\n",
	},
	{
		"echo foo $(echo bar)",
		"foo bar\n",
	},
	{
		"$(echo echo foo bar)",
		"foo bar\n",
	},
	{
		"for i in 1 $(echo 2 3) 4; do echo $i; done",
		"1\n2\n3\n4\n",
	},
	{
		"echo 1$(echo 2 3)4",
		"12 34\n",
	},
	{
		`mkdir d; [[ $(cd d && pwd) == "$(pwd)" ]]`,
		"exit status 1",
	},
	{
		"a=sub true & { a=main $ENV_PROG | grep '^a='; }",
		"a=main\n",
	},
	{
		"echo foo >f; echo $(cat f); echo $(<f)",
		"foo\nfoo\n",
	},
	{
		"echo foo >f; echo $(<f; echo bar)",
		"bar\n",
	},
	{
		"$(false); echo $?; $(exit 3); echo $?; $(true); echo $?",
		"1\n3\n0\n",
	},
	{
		"foo=$(false); echo $?; echo foo $(false); echo $?",
		"1\nfoo\n0\n",
	},
	{
		"$(false) $(true); echo $?; $(true) $(false); echo $?",
		"0\n1\n",
	},
	{
		"foo=$(false) $(true); echo $?; foo=$(true) $(false); echo $?",
		"1\n0\n",
	},

	// pipes
	{
		"echo foo | sed 's/o/a/g'",
		"faa\n",
	},
	{
		"echo foo | false | true",
		"",
	},
	{
		"true $(true) | true", // used to panic
		"",
	},
	{
		// The first command in the block used to consume stdin, even
		// though it shouldn't be. We just want to run any arbitrary
		// non-builtin program that doesn't consume stdin.
		"echo foo | { $ENV_PROG >/dev/null; cat; }",
		"foo\n",
	},

	// redirects
	{
		"echo foo >&1 | sed 's/o/a/g'",
		"faa\n",
	},
	{
		"echo foo >&2 | sed 's/o/a/g'",
		"foo\n",
	},
	{
		// TODO: why does bash need a block here?
		"{ echo foo >&2; } |& sed 's/o/a/g'",
		"faa\n",
	},
	{
		"echo foo >/dev/null; echo bar",
		"bar\n",
	},
	{
		">a; echo foo >>b; wc -c <a >>b; cat b | tr -d ' '",
		"foo\n0\n",
	},
	{
		"echo foo >a; <a",
		"",
	},
	{
		"echo foo >a; mkdir b; cd b; cat <../a",
		"foo\n",
	},
	{
		"echo foo >a; wc -c <a | tr -d ' '",
		"4\n",
	},
	{
		"echo foo >>a; echo bar &>>a; wc -c <a | tr -d ' '",
		"8\n",
	},
	{
		"{ echo a; echo b >&2; } &>/dev/null",
		"",
	},
	{
		"sed 's/o/a/g' <<EOF\nfoo$foo\nEOF",
		"faa\n",
	},
	{
		"sed 's/o/a/g' <<'EOF'\nfoo$foo\nEOF",
		"faa$faa\n",
	},
	{
		"sed 's/o/a/g' <<EOF\n\tfoo\nEOF",
		"\tfaa\n",
	},
	{
		"sed 's/o/a/g' <<EOF\nfoo\nEOF",
		"faa\n",
	},
	{
		"cat <<EOF\n~/foo\nEOF",
		"~/foo\n",
	},
	{
		"sed 's/o/a/g' <<<foo$foo",
		"faa\n",
	},
	{
		"cat <<-EOF\n\tfoo\nEOF",
		"foo\n",
	},
	{
		"cat <<-EOF\n\tfoo\n\nEOF",
		"foo\n\n",
	},
	{
		"cat <<EOF\nfoo\\\nbar\nEOF",
		"foobar\n",
	},
	{
		"cat <<'EOF'\nfoo\\\nbar\nEOF",
		"foo\\\nbar\n",
	},
	// `read -t` and `read -u` (#267). Both used to be refused with exit 2,
	// which nothing calling `read` checks — so in practice the variable
	// stayed empty and the loop body never ran.
	{
		"read -r -t 1 x <<< hi; echo \"st=$? x=[$x]\"",
		"st=0 x=[hi]\n",
	},
	{
		"read -r -t 0.2 x <<< hi; echo \"st=$? x=[$x]\"",
		"st=0 x=[hi]\n",
	},
	{
		// A timeout is reported as a status above 128, the way a signal is.
		"{ sleep 0.5; } | { read -r -t 0.1 x; echo \"st=$? x=[$x]\"; }",
		"st=142 x=[]\n",
	},
	{
		// Whatever arrived before the timeout is still assigned: only the
		// status says the read was cut short.
		"{ printf par; sleep 0.5; } | { read -r -t 0.1 x; echo \"st=$? x=[$x]\"; }",
		"st=142 x=[par]\n",
	},
	{
		// A regular file cannot take a deadline and does not need one.
		"printf 'hi\\n' > f; read -r -t 1 x < f; echo \"st=$? x=[$x]\"",
		"st=0 x=[hi]\n",
	},
	{
		"read -r -t zz x 2>/dev/null; echo \"st=$?\"",
		"st=1\n",
	},
	{
		"printf 'hi\\n' > f; exec 3< f; read -r -u 3 x; echo \"st=$? x=[$x]\"",
		"st=0 x=[hi]\n",
	},
	{
		// The descriptor keeps its position between reads, which is what a
		// `while read -u "$fd"` loop depends on.
		"printf 'a\\nb\\n' > f; exec 3< f; read -r -u 3 x; read -r -u 3 y; echo \"[$x][$y]\"",
		"[a][b]\n",
	},
	{
		"printf 'hi\\n' > f; exec 3< f; read -r -N 1 -u 3 x; echo \"[$x]\"",
		"[h]\n",
	},
	{
		"read -r -u 7 x 2>/dev/null; echo \"st=$?\"",
		"st=1\n",
	},
	{
		"read -r -u zz x 2>/dev/null; echo \"st=$?\"",
		"st=1\n",
	},
	{
		"printf 'hi\\n' > f; exec 3< f; read -r -t 1 -u 3 x; echo \"st=$? x=[$x]\"",
		"st=0 x=[hi]\n",
	},
	{
		// `-t 0` asks whether input is waiting and reads nothing at all.
		"printf 'hi\\n' > f; read -r -t 0 x < f; echo \"st=$? x=[$x]\"",
		"st=0 x=[]\n",
	},
	// The call-frame stack: FUNCNAME, BASH_SOURCE, BASH_LINENO and the
	// `caller` builtin, which are four views of one thing (#266, #250).
	//
	// None of these print a *file*, deliberately: this harness runs bash
	// with the script on stdin and koi in process with no parse name, so
	// the two disagree about what to call the input — a divergence about
	// $0 (#120) rather than about frames. The file is checked end to end
	// in cmd/koi, where both shells are given a real script.
	{
		"g(){ echo \"(${FUNCNAME[@]})\"; }; f(){ g; }; f",
		"(g f)\n",
	},
	{
		"echo \"[${FUNCNAME[0]-unset}]\"",
		"[unset]\n",
	},
	{
		// The line the *caller* is on, which is the half of a `die` helper
		// that says where to look.
		"g(){ echo \"${BASH_LINENO[0]}\"; }; f(){ g; }; f",
		"1\n",
	},
	{
		"g(){ echo \"${BASH_LINENO[0]}\"; }\nf(){\n  g\n}\nf",
		"3\n",
	},
	{
		"g(){ echo \"${#BASH_SOURCE[@]}\"; }; f(){ g; }; f",
		"2\n",
	},
	{
		// Unset at the top level of a command string, exactly as bash
		// leaves it — a script file is the case where it is set.
		"echo \"[${BASH_SOURCE[0]-unset}]\"",
		"[unset]\n",
	},
	{
		// `caller` answers by status when there is no frame to name, which
		// is what an error helper branches on.
		"caller; echo \"rc=$?\"",
		"rc=1\n",
	},
	{
		"f(){ caller 0; echo \"rc=$?\"; }; f",
		"rc=1\n",
	},
	{
		// Bare `caller` needs no frame above and prints bash's literal
		// NULL for a context with no file.
		"f(){ caller; }; f",
		"1 NULL\n",
	},
	{
		"g(){ caller 0 | cut -d' ' -f1-2; }; f(){ g; }; f",
		"1 f\n",
	},
	{
		// The diagnostic itself is koi-shaped (#120), so only the status is
		// compared — which is what a caller branches on anyway.
		"f(){ caller zz 2>/dev/null; echo \"rc=$?\"; }; f",
		"rc=2\n",
	},
	// The DEBUG trap, and BASH_COMMAND with it (#268). A DEBUG trap used
	// to be refused here and intercepted a layer up, which left a script's
	// `trap … DEBUG` recorded and never fired — accepted, silent, exit 0.
	{
		"trap 'echo D:$BASH_COMMAND' DEBUG; echo a; echo b",
		"D:echo a\na\nD:echo b\nb\n",
	},
	{
		// BASH_COMMAND is the source text, not the expansion: the trap
		// runs before the words are expanded, which is the whole reason a
		// tracer can print what was written.
		"trap 'echo D:$BASH_COMMAND' DEBUG; x=1; echo $x",
		"D:x=1\nD:echo $x\n1\n",
	},
	{
		// The far more common reader of BASH_COMMAND: an ERR trap saying
		// which command failed. It was reporting an empty string.
		"trap 'echo failed: $BASH_COMMAND' ERR; false; echo done",
		"failed: false\ndone\n",
	},
	{
		// Redirections are part of the command text (#445). koi rendered
		// st.Cmd alone, so a trap matching on BASH_COMMAND saw a
		// different string than bash's.
		"trap 'echo D:[$BASH_COMMAND]' DEBUG; echo a >/dev/null",
		"D:[echo a > /dev/null]\n",
	},
	{
		// bash normalizes the spacing rather than quoting the source:
		// both of these answer "> /dev/null".
		"trap 'echo D:[$BASH_COMMAND]' DEBUG; echo b   >   /dev/null",
		"D:[echo b > /dev/null]\n",
	},
	{
		// A dup stays tight where a target takes a space, and several
		// redirections keep their order.
		"trap 'echo D:[$BASH_COMMAND]' DEBUG; echo c 2>&1 >/dev/null",
		"D:[echo c 2>&1 > /dev/null]\n",
	},
	{
		// The clobber and all-streams forms take a space too.
		"trap 'echo D:[$BASH_COMMAND]' DEBUG; echo d >| /dev/null",
		"D:[echo d >| /dev/null]\n",
	},
	{
		// An fd number prefixes the operator, and a close is tight.
		"trap 'echo D:[$BASH_COMMAND]' DEBUG; exec 4>&-",
		"D:[exec 4>&-]\n",
	},
	{
		// A here-string is a word, so it takes the space.
		"trap 'echo D:[$BASH_COMMAND]' DEBUG; cat <<<hs >/dev/null",
		"D:[cat <<< hs > /dev/null]\n",
	},
	{
		// A here-document keeps its body and its terminator, with real
		// newlines — measured, where the obvious guess is that bash
		// prints the operator alone.
		//
		// The expansion is quoted here and unquoted above on purpose: an
		// unquoted $BASH_COMMAND word-splits, so the newlines collapse to
		// spaces and the case would pass against a rendering that never
		// produced them.
		"trap 'echo \"D:[$BASH_COMMAND]\"' DEBUG; cat <<EOF >/dev/null\nbody line\nEOF\n",
		"D:[cat <<EOF > /dev/null\nbody line\nEOF\n]\n",
	},
	{
		// A function body is not traced without "functrace"; the call is.
		"trap 'echo D' DEBUG; f() { echo in; }; f",
		"D\nin\n",
	},
	{
		"trap 'echo D' DEBUG; ( echo sub )",
		"sub\n",
	},
	{
		// The trap fires for the command that removes it, which is the
		// order bash runs them in rather than an accident.
		"trap 'echo D' DEBUG; trap - DEBUG; echo after",
		"D\nafter\n",
	},
	{
		// The trap's own status must not become the command's.
		"trap 'true' DEBUG; false; echo $?",
		"1\n",
	},
	{
		"trap 'echo x' EXIT; trap -p",
		"trap -- 'echo x' EXIT\nx\n",
	},

	// RETURN (#295). The frame rules are covered end to end against real
	// bash in cmd/koi/trapreturn_test.go, including `source`, which needs
	// a file this table has no way to write. These are the in-package
	// core: that it fires, that it does not eat the return status, and
	// the two inheritance rules that decide everything else.
	{
		"f(){ trap 'echo left' RETURN; echo in; }; f",
		"in\nleft\n",
	},
	{
		"f(){ trap 'echo left' RETURN; return 5; }; f; echo rc=$?",
		"left\nrc=5\n",
	},
	{
		// FUNCNAME inside the trap is still the returning function — a
		// cleanup handler reads it to name what it is cleaning up after.
		"f(){ trap 'echo left:$FUNCNAME' RETURN; :; }; f",
		"left:f\n",
	},
	{
		// A function does not inherit RETURN...
		"trap 'echo R' RETURN; f(){ echo in; }; f; echo done",
		"in\ndone\n",
	},
	{
		// ...until functrace says so.
		"set -T; trap 'echo R' RETURN; f(){ echo in; }; f",
		"in\nR\n",
	},
	{
		// A nested call turning inheritance off must not silence the
		// caller's own return.
		"f(){ trap 'echo R' RETURN; g; }; g(){ echo g; }; f",
		"g\nR\n",
	},
	{
		"f(){ trap 'echo T' RETURN; :; }; f; trap -p RETURN",
		"T\ntrap -- 'echo T' RETURN\n",
	},
	{
		// bare `trap` prints the same listing as `trap -p`.
		"trap 'echo x' EXIT; trap",
		"trap -- 'echo x' EXIT\nx\n",
	},
	{
		"trap 'echo e' ERR; trap 'echo x' EXIT; trap -p",
		"trap -- 'echo x' EXIT\ntrap -- 'echo e' ERR\nx\n",
	},
	{
		"trap 'echo e' ERR; trap 'echo x' EXIT; trap -p ERR",
		"trap -- 'echo e' ERR\nx\n",
	},
	{
		"trap -p; echo none",
		"none\n",
	},
	{
		// The reason `-p` exists: save a handler, do something that needs
		// it gone, put it back. It runs in a command substitution, and a
		// subshell that reported its own empty set would hand back an
		// empty string — losing the handler silently, which is worse than
		// `-p` never having worked.
		"trap 'echo cleanup' EXIT\nsaved=$(trap -p EXIT)\ntrap - EXIT\neval \"$saved\"\necho body",
		"body\ncleanup\n",
	},
	{
		"trap 'echo bye' EXIT; ( trap -p EXIT )",
		"trap -- 'echo bye' EXIT\nbye\n",
	},
	{
		// The listing is what restores the handler, so it has to be
		// shell-quoted rather than Go-quoted — including the awkward case.
		"trap \"echo \\\"it's fine\\\"\" EXIT; trap -p",
		"trap -- 'echo \"it'\\''s fine\"' EXIT\nit's fine\n",
	},
	// `$-` tracks the options that are set (#265). The letters themselves
	// are not compared, because bash reports `h` for hashing and an
	// embedder contributes letters this package cannot know about; what is
	// compared is the answer the idiom asks for, which is all a caller
	// ever acts on.
	{
		"set -e; case $- in *e*) echo on;; *) echo off;; esac",
		"on\n",
	},
	{
		"set -e; set +e; case $- in *e*) echo on;; *) echo off;; esac",
		"off\n",
	},
	{
		"set -uf; case $- in *u*) echo u;; esac; case $- in *f*) echo f;; esac",
		"u\nf\n",
	},
	{
		"f() { set -e; case $- in *e*) echo on;; esac; }; f",
		"on\n",
	},
	{
		"( set -u; case $- in *u*) echo sub;; esac ); case $- in *u*) echo outer;; *) echo clean;; esac",
		"sub\nclean\n",
	},
	{
		"eval 'set -f; case $- in *f*) echo yes;; esac'",
		"yes\n",
	},
	{
		// `set -o pipefail` has no letter, in bash exactly as here.
		"set -o pipefail; case $- in *o*) echo letter;; *) echo none;; esac",
		"none\n",
	},
	{
		// `$-` is always set, so `set -u` must not make reading it fatal.
		"set -u; echo \"${-+present}\"",
		"present\n",
	},
	// A quoted delimiter makes the body literal, and that includes escape
	// processing — not only expansion (#244). The cases below were the
	// whole bug: `\\`, `\$` and an escaped backquote were the one set
	// still being unescaped, which is the *unquoted* form's rule.
	{
		"cat <<'EOF'\nre=\\\\d+\nEOF",
		"re=\\\\d+\n",
	},
	{
		"cat <<'EOF'\ncost=\\$5\nEOF",
		"cost=\\$5\n",
	},
	{
		"cat <<'EOF'\ncmd=\\`id`\nEOF",
		"cmd=\\`id`\n",
	},
	{
		"cat <<\"EOF\"\nre=\\\\d+\nEOF",
		"re=\\\\d+\n",
	},
	{
		"cat <<\\EOF\nre=\\\\d+\nEOF",
		"re=\\\\d+\n",
	},
	{
		"cat <<-'EOF'\n\tre=\\\\d+\n\t\tindented\n\tEOF",
		"re=\\\\d+\nindented\n",
	},
	{
		"cat <<-'EOF'\n\t  re=\\\\d+\n\tEOF",
		"  re=\\\\d+\n",
	},
	// `eval` and `source` re-parse inside the interpreter, so a fix that
	// rewrites the tree koi parses cannot reach them (#259). The tests
	// above pass either way and would not have noticed.
	{
		"eval 'cat <<'\\''EOF'\\''\nre=\\\\d+\nEOF'",
		"re=\\\\d+\n",
	},
	{
		"cat <<EOF\nfoo\\\"bar\\baz\nEOF",
		"foo\\\"bar\\baz\n",
	},
	{
		"cat <<EOF\n \\\\ \\$ \\` \nEOF",
		" \\ $ ` \n",
	},
	{
		"mkdir a; echo foo >a |& grep -q 'is a directory'",
		" #IGNORE bash prints a warning",
	},
	{
		"echo foo 1>&1 | sed 's/o/a/g'",
		"faa\n",
	},
	{
		"echo foo 2>&2 |& sed 's/o/a/g'",
		"faa\n",
	},
	{
		"printf 2>&1 | sed 's/.*usage.*/foo/'",
		"foo\n",
	},
	{
		"mkdir a && cd a && echo foo >b && cd .. && cat a/b",
		"foo\n",
	},
	{
		"echo foo 2>&-; :",
		"foo\n",
	},
	{
		// `>&-` closes stdout or stderr. Note that any writes result in errors.
		"echo foo >&- 2>&-; :",
		"",
	},
	{
		// `>|` overwrites the file whether or not noclobber is set
		"echo old >f; echo new >|f; cat f",
		"new\n",
	},
	{
		// `<>` opens for reading and writing without truncating
		"echo hi >f; read -r l <>f; echo \"[$l]\"; cat f",
		"[hi]\nhi\n",
	},

	// file descriptors above 2
	{
		`exec 3>f; echo hi >&3; exec 3>&-; cat f`,
		"hi\n",
	},
	{
		`echo hi 3>f >&3; cat f`,
		"hi\n",
	},
	{
		`printf 'line\n' >f; exec 3<f; read -r l <&3; echo "[$l]"`,
		"[line]\n",
	},
	{
		`exec 3>a 4>b; echo A >&3; echo B >&4; exec 3>&- 4>&-; cat a b`,
		"A\nB\n",
	},
	{
		`echo one >f; exec 3>>f; echo two >&3; exec 3>&-; cat f`,
		"one\ntwo\n",
	},
	{
		// a descriptor can be duplicated from another
		`exec 3>&1; echo hi >&3`,
		"hi\n",
	},
	{
		`exec 3>f; echo hi >&2 2>&3; exec 3>&-; cat f`,
		"hi\n",
	},
	{
		// a redirection on one statement does not outlive it, unlike exec's
		`exec 3>a; { echo inner >&3; } 3>b; echo outer >&3; exec 3>&-; echo "a=[$(cat a)] b=[$(cat b)]"`,
		"a=[outer] b=[inner]\n",
	},
	{
		// "{name}>" allocates one and says which
		`exec {v}>f; echo hi >&$v; exec {v}>&-; cat f; [ "$v" -ge 10 ] && echo high`,
		"hi\nhigh\n",
	},
	{
		`printf 'a\nb\n' >f; exec 3<f; read -r x <&3; read -r y <&3; echo "[$x][$y]"`,
		"[a][b]\n",
	},
	{
		`echo hi >f; exec 3<>f; read -r l <&3; echo "[$l]"`,
		"[hi]\n",
	},
	{
		// a subshell inherits them
		`exec 3>f; ( echo sub >&3 ); exec 3>&-; cat f`,
		"sub\n",
	},
	{
		`echo hi >&9`,
		"9: Bad file descriptor\nexit status 1 #JUSTERR",
	},
	{
		`read -r l <&9`,
		"9: Bad file descriptor\nexit status 1 #JUSTERR",
	},

	// noclobber
	{
		`set -C; echo old >f; echo new >f; echo "st=$?"; cat f`,
		"f: cannot overwrite existing file\nst=1\nold\n #IGNORE bash prefixes its diagnostics",
	},
	{
		// the same without the diagnostic, so that bash confirms the behavior
		`set -C; echo old >f; echo new 2>/dev/null >f; echo "st=$?"; cat f`,
		"st=1\nold\n",
	},
	{
		// writing to a file which does not exist yet is allowed
		`set -C; echo new >f; cat f`,
		"new\n",
	},
	{
		`set -C; echo old >f; echo new >|f; cat f`,
		"new\n",
	},
	{
		`set -C; echo old >f; echo new >>f; cat f`,
		"old\nnew\n",
	},
	{
		`set -C; echo old >f; echo new 2>/dev/null &>f; echo "st=$?"; cat f`,
		"st=1\nold\n",
	},
	{
		// only regular files are protected
		`set -C; echo x >/dev/null; echo "st=$?"`,
		"st=0\n",
	},
	{
		`set -C; set +C; echo old >f; echo new >f; cat f`,
		"new\n",
	},
	{
		`set -o noclobber; echo old >f; echo new 2>/dev/null >f; echo "st=$?"`,
		"st=1\n",
	},
	{
		"echo foo | sed $(read line 2>/dev/null; echo 's/o/a/g')",
		"",
	},
	{
		// `<&-` closes stdin, to e.g. ensure that a subshell does not consume
		// the standard input shared with the parent shell.
		// Note that any reads result in errors.
		"echo foo | sed $(exec <&-; read line 2>/dev/null; echo 's/o/a/g')",
		"faa\n",
	},
	{
		// Concurrent pipe commands used to cause races when modifying the environment.
		"a=1 b=2 c=3 d=4 e=5 : | a=1 b=2 c=3 d=4 e=5 : | a=1 b=2 c=3 d=4 e=5 : | a=1 b=2 c=3 d=4 e=5 :",
		"",
	},

	// background/wait
	{"wait", ""},
	{"wait foo", "wait: pid foo is not a child of this shell\nexit status 1 #JUSTERR"},

	// `wait -n` (#287). Every expectation here was taken from real bash
	// rather than reasoned about: the status is the finished job's, an
	// already-finished job satisfies it without blocking, each job is
	// handed back once, and 127 means there is nothing left — which is
	// how a drain loop knows to stop.
	{"wait -n; echo $?", "127\n"},
	{"(exit 3) & wait -n; echo $?", "3\n"},
	// Ordered with a sleep rather than left to the scheduler: two jobs
	// that both exit immediately have no defined finishing order, so an
	// expectation about which -n returns first would be a coin flip
	// dressed up as a test.
	{"(exit 3) & (sleep 0.1; exit 4) & wait -n; echo $?", "3\n"},
	{"(sleep 0.1; exit 3) & (exit 4) & wait -n; echo $?", "4\n"},
	{"(exit 3) & (sleep 0.1; exit 4) & wait -n; wait -n; echo $?", "4\n"},
	{"(exit 3) & wait -n; wait -n; echo $?", "127\n"},
	// Reaping is not a -n-only notion: bash's plain `wait` collects the
	// jobs too, so nothing is left for a following -n.
	{"(exit 3) & wait; wait -n; echo $?", "127\n"},
	{"(exit 3) & p=$!; wait $p; wait -n; echo $?", "127\n"},
	{"wait -n foo", "wait: pid foo is not a child of this shell\nexit status 1 #JUSTERR"},
	// -p records *which* job answered, so a script can tell "job N
	// finished" from "there was nothing left" without reading $? twice.
	// bash leaves it unset in the second case, and so does koi.
	// The pid's *value* is deliberately not compared: koi's jobs are
	// goroutines and $! spells them "gN", where bash has a real process
	// id. What has to agree is whether the variable was set, and that it
	// carries the same spelling the shell itself hands out.
	{"(exit 3) & wait -n -p v; echo \"$?/${v:+set}\"", "3/set\n"},
	{"wait -n -p v; echo \"$?/${v-unset}\"", "127/unset\n"},
	{"(exit 3) & p=$!; wait -p v $p; [ \"$v\" = \"$p\" ] && echo matches", "matches\n"},

	// coproc (#287): the clause parsed and the executor dropped it.
	{
		"coproc C { read -r l; echo \"got:$l\"; }; echo hi >&\"${C[1]}\"; read -r r <&\"${C[0]}\"; echo \"$r\"",
		"got:hi\n",
	},
	// No name means COPROC, which is also bash's rule for a simple command.
	{"coproc { echo named-by-default; }; read -r r <&\"${COPROC[0]}\"; echo \"$r\"", "named-by-default\n"},
	// NAME_PID spells the job the way $! does, so `wait` can find it.
	{"coproc C { exit 4; }; wait \"$C_PID\"; echo $?", "4\n"},
	{"coproc 1bad { :; }", "coproc: \"1bad\": not a valid name\nexit status 1 #JUSTERR"},
	{"{ true; } & wait", ""},
	{"{ false; } & wait", ""},
	{"{ sleep 0.01; true; } & wait", ""},
	{"{ sleep 0.01; false; } & wait", ""},
	{
		"{ echo foo; } & wait; echo bar",
		"foo\nbar\n",
	},
	{
		"{ echo foo & wait; } & wait; echo bar",
		"foo\nbar\n",
	},
	{`mkdir d; old=$PWD; cd d & wait; [[ $old == "$PWD" ]]`, ""},
	{
		"f() { echo 1; }; { sleep 0.01; f; } & f() { echo 2; }; wait",
		"1\n",
	},
	{"[[ -n $! ]]", "exit status 1"},
	{"true & [[ -n $! ]]", ""},
	{"true & true;  [[ -n $! ]]", ""},
	{"true & pid=$!; wait $pid", ""},
	{"false & pid=$!; wait $pid", "exit status 1"},
	{"{ sleep 0.01; true; } & pid=$!; wait $pid", ""},
	{"{ sleep 0.01; false; } & pid=$!; wait $pid", "exit status 1"},
	{"(true) & ok=$!; (false) & fail=$!; wait $ok $fail", "exit status 1"},
	{"(true) & ok=$!; (false) & ignore=$!; wait $ok", ""},
	{"echo foo | true | false & wait $!", "exit status 1"},
	{"echo foo | false | true & wait $!", ""},
	{"f() { false & true; }; f; wait $!", "exit status 1"},
	// The parent and child shells should not cause data races when setting env vars.
	// Note that we can't use `echo $var`, as it seems to write newlines separately,
	// which can cause them to get mixed up between concurrent subshells.
	{
		"{ for n in {0..9}; do { echo -n $n$'\n'; } & done; wait; } | sort",
		"0\n1\n2\n3\n4\n5\n6\n7\n8\n9\n",
	},
	{
		"outer=val; for n in {0..9}; do { echo -n $outer$'\n'; } & outer=val; done; wait",
		"val\nval\nval\nval\nval\nval\nval\nval\nval\nval\n",
	},
	{
		"for n in {0..9}; do { inner=val; } & echo $inner; done",
		"\n\n\n\n\n\n\n\n\n\n",
	},
	{
		"exit 2 & bg1=$!; exit 0 & bg2=$!; wait $bg1 $bg2; echo $?",
		"0\n",
	},
	{
		"exit 2 & bg1=$!; exit 4 & bg2=$!; wait $bg1 $bg2; echo $?",
		"4\n",
	},

	// bash test
	{
		"[[ a ]]",
		"",
	},
	{
		"[[ '' ]]",
		"exit status 1",
	},
	{
		"[[ '' ]]; [[ a ]]",
		"",
	},
	{
		"[[ ! (a == b) ]]",
		"",
	},
	{
		"[[ a != b ]]",
		"",
	},
	{
		"[[ a && '' ]]",
		"exit status 1",
	},
	{
		"[[ a || '' ]]",
		"",
	},
	{
		"[[ a > 3 ]]",
		"",
	},
	{
		"[[ a < 3 ]]",
		"exit status 1",
	},
	{
		"[[ 3 == 03 ]]",
		"exit status 1",
	},
	{
		"[[ a -eq b ]]",
		"",
	},
	{
		"[[ 3 -eq 03 ]]",
		"",
	},
	{
		"[[ 3 -ne 4 ]]",
		"",
	},
	{
		"[[ 3 -le 4 ]]",
		"",
	},
	{
		"[[ 3 -ge 4 ]]",
		"exit status 1",
	},
	{
		"[[ 3 -ge 3 ]]",
		"",
	},
	{
		"[[ 3 -lt 4 ]]",
		"",
	},
	{
		"[[ ' 3' -lt '4 ' ]]",
		"",
	},
	{
		"[[ 3 -gt 4 ]]",
		"exit status 1",
	},
	{
		"[[ 3 -gt 3 ]]",
		"exit status 1",
	},
	{
		"[[ a -nt a || a -ot a ]]",
		"exit status 1",
	},
	{
		"touch -t 202111050000.30 a b; [[ a -nt b || a -ot b ]]",
		"exit status 1",
	},
	{
		"touch -t 202111050200.00 a; touch -t 202111060100.00 b; [[ a -nt b ]]",
		"exit status 1",
	},
	{
		"touch -t 202111050000.00 a; touch -t 202111060000.00 b; [[ a -ot b ]]",
		"",
	},
	{
		">a; [[ a -nt b ]]",
		"",
	},
	{
		">a; [[ a -ot b ]]",
		"exit status 1",
	},
	{
		">b; [[ a -nt b ]]",
		"exit status 1",
	},
	{
		">b; [[ a -ot b ]]",
		"",
	},
	{
		"[[ a -ef b ]]",
		"exit status 1",
	},
	{
		">a >b; [[ a -ef b ]]",
		"exit status 1",
	},
	{
		">a; [[ a -ef a ]]",
		"",
	},
	{
		">a; ln a b; [[ a -ef b ]]",
		"",
	},
	{
		">a; ln -s a b; [[ a -ef b ]]",
		"",
	},
	{
		"[[ -z 'foo' || -n '' ]]",
		"exit status 1",
	},
	{
		"[[ -z '' && -n 'foo' ]]",
		"",
	},
	{
		"a=x b=''; [[ -v a && -v b && ! -v c ]]",
		"",
	},
	{
		"[[ abc == *b* ]]",
		"",
	},
	{
		"[[ abc != *b* ]]",
		"exit status 1",
	},
	{
		"[[ *b = '*b' ]]",
		"",
	},
	{
		"[[ ab == a. ]]",
		"exit status 1",
	},
	{
		`x='*b*'; [[ abc == $x ]]`,
		"",
	},
	{
		`x='*b*'; [[ abc == "$x" ]]`,
		"exit status 1",
	},
	{
		`[[ abc == \a\bc ]]`,
		"",
	},
	{
		"[[ abc != *b'*' ]]",
		"",
	},
	{
		"[[ a =~ b ]]",
		"exit status 1",
	},
	{
		"[[ foo =~ foo && foo =~ .* && foo =~ f.o ]]",
		"",
	},
	{
		"[[ foo =~ oo ]] && echo foo; [[ foo =~ ^oo$ ]] && echo bar || true",
		"foo\n",
	},
	{
		"[[ a =~ [ ]]",
		"exit status 2 #JUSTERR",
	},
	{
		"[[ a__b__c =~ _*(b_*) ]]; echo ${BASH_REMATCH[0]}; echo ${BASH_REMATCH[1]}",
		"__b__\nb__\n",
	},
	{
		"[[ -e a ]] && echo x; >a; [[ -e a ]] && echo y",
		"y\n",
	},
	{
		"ln -s b a; [[ -e a ]] && echo x; >b; [[ -e a ]] && echo y",
		"y\n",
	},
	{
		"[[ -f a ]] && echo x; >a; [[ -f a ]] && echo y",
		"y\n",
	},
	{
		"[[ -e a ]] && echo x; mkdir a; [[ -e a ]] && echo y",
		"y\n",
	},
	{
		"[[ -d a ]] && echo x; mkdir a; [[ -d a ]] && echo y",
		"y\n",
	},
	{
		"[[ -r a ]] && echo x; >a; [[ -r a ]] && echo y",
		"y\n",
	},
	{
		"[[ -w a ]] && echo x; >a; [[ -w a ]] && echo y",
		"y\n",
	},
	{
		"[[ -s a ]] && echo x; echo body >a; [[ -s a ]] && echo y",
		"y\n",
	},
	{
		"[[ -L a ]] && echo x; ln -s b a; [[ -L a ]] && echo y;",
		"y\n",
	},
	{
		"[[ \"multiline\ntext\" == *text* ]] && echo x; [[ \"multiline\ntext\" == *multiline* ]] && echo y",
		"x\ny\n",
	},
	// * should match a newline
	{
		"[[ \"multiline\ntext\" == multiline*text ]] && echo x",
		"x\n",
	},
	{
		"[[ \"multiline\ntext\" == text ]]",
		"exit status 1",
	},
	{
		`case $'a\nb' in a*b) echo match ;; esac`,
		"match\n",
	},
	{
		`a=$'a\nb'; echo "${a/a*b/sub}"`,
		"sub\n",
	},
	{
		"mkdir a; cd a; test -f b && echo x; >b; test -f b && echo y",
		"y\n",
	},
	{
		">a; [[ -b a ]] && echo block; [[ -c a ]] && echo char; true",
		"",
	},
	{
		"[[ -e /dev/sda ]] || { echo block; exit; }; [[ -b /dev/sda ]] && echo block; [[ -c /dev/sda ]] && echo char; true",
		"block\n",
	},
	{
		"[[ -e /dev/nvme0n1 ]] || { echo block; exit; }; [[ -b /dev/nvme0n1 ]] && echo block; [[ -c /dev/nvme0n1 ]] && echo char; true",
		"block\n",
	},
	{
		"[[ -e /dev/tty ]] || { echo char; exit; }; [[ -b /dev/tty ]] && echo block; [[ -c /dev/tty ]] && echo char; true",
		"char\n",
	},
	{"[[ -t 1 ]]", "exit status 1"},
	{"[[ -t 1234 ]]", "exit status 1"},
	{"[[ -o wrong ]]", "exit status 1"},
	{"[[ -o errexit ]]", "exit status 1"},
	{"set -e; [[ -o errexit ]]", ""},
	{"[[ -o noglob ]]", "exit status 1"},
	{"set -f; [[ -o noglob ]]", ""},
	{"[[ -o allexport ]]", "exit status 1"},
	{"set -a; [[ -o allexport ]]", ""},
	{"[[ -o nounset ]]", "exit status 1"},
	{"set -u; [[ -o nounset ]]", ""},
	{"[[ -o noexec ]]", "exit status 1"},
	{"set -n; [[ -o noexec ]]", ""}, // actually does nothing, but oh well
	{"[[ -o pipefail ]]", "exit status 1"},
	{"set -o pipefail; [[ -o pipefail ]]", ""},
	// TODO: we don't implement precedence of && over ||.
	// {"[[ a == x && b == x || c == c ]]", ""},
	{"[[ (a == x && b == x) || c == c ]]", ""},
	{"[[ a == x && (b == x || c == c) ]]", "exit status 1"},

	// classic test
	{
		"[",
		"1:1: [: missing matching ]\nexit status 2 #JUSTERR",
	},
	{
		"[ a",
		"1:1: [: missing matching ]\nexit status 2 #JUSTERR",
	},
	{
		"[ a b c ]",
		"1:1: not a valid test operator: `b`\nexit status 2 #JUSTERR",
	},
	{
		"[ a -a ]",
		"1:1: -a must be followed by an expression\nexit status 2 #JUSTERR",
	},
	{"[ a ]", ""},
	{"[ -n ]", ""},
	{"[ '-n' ]", ""},
	{"[ -z ]", ""},
	{"[ ! ]", ""},
	{"[ a != b ]", ""},
	{"[ ! a '==' a ]", "exit status 1"},
	{"[ a -a 0 -gt 1 ]", "exit status 1"},
	{"[ 0 -gt 1 -o 1 -gt 0 ]", ""},
	{"[ 3 -gt 4 ]", "exit status 1"},
	{"[ 3 -lt 4 ]", ""},
	{"[ ' 3' -lt '4 ' ]", ""},
	{
		"[ -e a ] && echo x; >a; [ -e a ] && echo y",
		"y\n",
	},
	{
		"test 3 -gt 4",
		"exit status 1",
	},
	{
		"test 3 -lt 4",
		"",
	},
	{
		"test 3 -lt",
		"1:1: -lt must be followed by a word\nexit status 2 #JUSTERR",
	},
	{
		"touch -t 202111050000.00 a; touch -t 202111060000.00 b; [ a -nt b ]",
		"exit status 1",
	},
	{
		"touch -t 202111050000.00 a; touch -t 202111060000.00 b; [ a -ot b ]",
		"",
	},
	{
		">a; [ a -nt b ]",
		"",
	},
	{
		">b; [ a -ot b ]",
		"",
	},
	{
		"[ a -nt b ]",
		"exit status 1",
	},
	{
		">a; [ a -ef a ]",
		"",
	},
	{"[ 3 -eq 04 ]", "exit status 1"},
	{"[ 3 -eq 03 ]", ""},
	{"[ 3 -ne 03 ]", "exit status 1"},
	{"[ 3 -le 4 ]", ""},
	{"[ 3 -ge 4 ]", "exit status 1"},
	{
		"[ -d a ] && echo x; mkdir a; [ -d a ] && echo y",
		"y\n",
	},
	{
		"[ -r a ] && echo x; >a; [ -r a ] && echo y",
		"y\n",
	},
	{
		"[ -w a ] && echo x; >a; [ -w a ] && echo y",
		"y\n",
	},
	{
		// A directory is readable, writable, and executable.
		"mkdir d; [ -r d ] && echo r; [ -w d ] && echo w; [ -x d ] && echo x",
		"r\nw\nx\n",
	},
	{
		"test -? a",
		// TODO: this error message should refer to `-?`
		"1:1: not a valid test operator: `a`\n1:1: a must be followed by a word\nexit status 2 #JUSTERR",
	},
	{
		"[ -s a ] && echo x; echo body >a; [ -s a ] && echo y",
		"y\n",
	},
	{
		"[ -L a ] && echo x; ln -s b a; [ -L a ] && echo y;",
		"y\n",
	},
	{
		">a; [ -b a ] && echo block; [ -c a ] && echo char; true",
		"",
	},
	{"[ -t 1 ]", "exit status 1"},
	{"[ -t 1234 ]", "exit status 1"},
	{"[ -o wrong ]", "exit status 1"},
	{"[ -o errexit ]", "exit status 1"},
	{"set -e; [ -o errexit ]", ""},
	{"a=x b=''; [ -v a -a -v b -a ! -v c ]", ""},
	{"[ a = a ]", ""},
	{"[ a != a ]", "exit status 1"},
	{"[ abc = ab* ]", "exit status 1"},
	{"[ abc != ab* ]", ""},
	// TODO: we don't implement precedence of -a over -o.
	// {"[ a = x -a b = x -o c = c ]", ""},
	{`[ \( a = x -a b = x \) -o c = c ]`, ""},
	{`[ a = x -a \( b = x -o c = c \) ]`, "exit status 1"},

	// arithm
	{
		"echo $((1 == +1))",
		"1\n",
	},
	{
		"echo $((!0))",
		"1\n",
	},
	{
		"echo $((!3))",
		"0\n",
	},
	{
		"echo $((~0))",
		"-1\n",
	},
	{
		"echo $((~3))",
		"-4\n",
	},
	{
		"echo $((1 + 2 - 3))",
		"0\n",
	},
	{
		"echo $((-1 * 6 / 2))",
		"-3\n",
	},
	{
		"a=2; echo $(( a + $a + c ))",
		"4\n",
	},
	{
		"a=b; b=c; c=5; echo $((a % 3))",
		"2\n",
	},
	{
		"echo $((2 > 2 || 2 < 2))",
		"0\n",
	},
	{
		"echo $((2 >= 2 && 2 <= 2))",
		"1\n",
	},
	{
		"x=0; echo $((0 && (x = 1))) $x",
		"0 0\n",
	},
	{
		"x=0; echo $((1 || (x = 1))) $x",
		"1 0\n",
	},
	{
		"x=0; echo $((0 && x++)) $x $((1 || x++)) $x",
		"0 0 1 0\n",
	},
	{
		"x=0; echo $((1 && (x = 1))) $x",
		"1 1\n",
	},
	{
		"x=0; echo $((0 || (x = 2))) $x",
		"1 2\n",
	},
	{
		"echo $((0 && 1/0)) $((1 || 1/0))",
		"0 1\n",
	},
	{
		"x=0; y=0; echo $((0 && (x = 1) || (y = 2))) $x $y",
		"1 0 2\n",
	},
	{
		"x=0; echo $((1/0 && x++)); echo $x",
		"division by zero\n0\n #JUSTERR",
	},
	{
		"echo $(((1 & 2) != (1 | 2)))",
		"1\n",
	},
	{
		"echo $a; echo $((a = 3 ^ 2)); echo $a",
		"\n1\n1\n",
	},
	{
		"echo $((a += 1, a *= 2, a <<= 2, a >> 1))",
		"4\n",
	},
	{
		"echo $((a -= 10, a /= 2, a >>= 1, a << 1))",
		"-6\n",
	},
	{
		"echo $((a |= 3, a &= 1, a ^= 8, a %= 5, a))",
		"4\n",
	},
	{
		"echo $((a = 3, ++a, a--))",
		"4\n",
	},
	{
		"echo $((2 ** 3)) $((1234 ** 4567))",
		"8 0\n",
	},
	{
		"echo $((2 ** -1)); let x=2**-1",
		"exponent less than 0\nexponent less than 0\nexit status 1 #JUSTERR",
	},
	{
		"echo $((1 ? 2 : 3)) $((0 ? 2 : 3))",
		"2 3\n",
	},
	{
		"echo $((2 ? 3 : 4)) $((-1 ? 3 : 4))",
		"3 3\n",
	},
	{
		"echo $((255+1))",
		"256\n",
	},
	{
		"echo $((0xff+1))",
		"256\n",
	},
	{
		"echo $((0377+1))",
		"256\n",
	},
	{
		"echo $((10#255+1))",
		"256\n",
	},
	{
		"echo $((16#ff+1))",
		"256\n",
	},
	{
		"echo $((2#11111111+1))",
		"256\n",
	},
	// TODO: Enable this test once integer bit widths are
	// handled in a consistent manner throughout the library.
	//{
	//	"echo $((16#badc0ffee+1))",
	//	"50159747055\n",
	//},
	{
		"echo $((16#cafe+1))",
		"51967\n",
	},
	{
		"x=-010 y=+010 z=-0x10; echo $((x)) $((y)) $((z))",
		"-8 8 -16\n",
	},
	{
		"echo $((64#z)) $((64#Z)) $((40#A)) $((64#10)) $((36#z))",
		"35 61 36 64 35\n",
	},
	{
		"a=64#@ b=64#_ c=64#1_; echo $((a)) $((b)) $((c))",
		"62 63 127\n",
	},
	{
		"echo $((nope+1))",
		"1\n", // Yes, this is what bash does.
	},
	{
		"((1))",
		"",
	},
	{
		"((3 == 4))",
		"exit status 1",
	},
	{
		"let i=(3+4); let i++; echo $i; let i--; echo $i",
		"8\n7\n",
	},
	{
		"let 3==4",
		"exit status 1",
	},
	{
		"a=1; let a++; echo $a",
		"2\n",
	},
	{
		"a=$((1 + 2)); echo $a",
		"3\n",
	},
	{
		"x=3; echo $(($x)) $((x))",
		"3 3\n",
	},
	{
		"set -- 1; echo $(($@))",
		"1\n",
	},
	{
		// A name chase that cycles is an error, as 5.3's is; a chase
		// that dead-ends stays 0.
		"a=b b=a; echo $(($a)); echo next",
		"b: expression recursion level exceeded\nexit status 1 #JUSTERR",
	},
	{
		"a=b; echo $(($a))",
		"0\n",
	},
	// Bracket-expression forms (#374): single-character collating
	// symbols and equivalence classes resolve to the character — early
	// enough to anchor a range — a closed invalid class contributes
	// nothing, and an unclosed [: degrades to literal members.
	{`case a in [[.a.]]) echo y;; *) echo n;; esac`, "y\n"},
	{`case b in [[=b=]]) echo y;; *) echo n;; esac`, "y\n"},
	{`case a in [[:alpha]) echo y;; *) echo n;; esac`, "y\n"},
	{`case "[" in [[:alpha]) echo y;; *) echo n;; esac`, "y\n"},
	{`case a in [abc[:foo:]]) echo y;; *) echo n;; esac`, "y\n"},
	{`case : in [[:foo:]]) echo y;; *) echo n;; esac`, "n\n"},
	{`case a in [[.ab.]]) echo y;; *) echo n;; esac`, "n\n"},
	{`case c in [[.a.]-z]) echo y;; *) echo n;; esac`, "y\n"},

	// Backslash quoting reaches the pattern matcher (#372): an escaped
	// metacharacter never globs, an escaped name component resolves to
	// the real file, and a trailing lone backslash is a literal one.
	{"touch a abc; echo \\* a\\*", "* a*\n"},
	{`var="ab\\"; [[ $var = $var ]] && echo true || echo false`, "true\n"},
	{`var="ab\\"; case $var in $var) echo m;; *) echo n;; esac`, "m\n"},
	{"mkdir 's*d'; touch 's*d/f'; echo s\\*d/* | sed 's@\\\\@/@g'", "s*d/f\n"},

	// A readonly violation inside an arithmetic expansion is fatal to
	// the input unit (#370): the command aborts with status 1 and a
	// script continues at its next line.
	{
		"readonly xx=1\ncase 1 in $((xx++)) ) echo hi1 ;; *) echo hi2; esac\necho ${xx}.$?",
		"xx: readonly variable\n1.1\n #JUSTERR",
	},
	{
		"readonly xx=1; echo $((xx++)); echo same-line",
		"xx: readonly variable\nexit status 1 #JUSTERR",
	},

	// A failing body command does not end a C-style loop (#369), and an
	// empty update section is fine — the body's own ((i++)) answers
	// status 1 on its first step, which used to stop the loop.
	{`for (( i=0; i<3; )); do echo $i; ((i++)); done; echo st=$?`, "0\n1\n2\nst=0\n"},
	{`for (( f=0; f<3; f++ )); do printf "%d " $f; false; done; echo end $?`, "0 1 2 end 1\n"},

	// declare -i survives arithmetic assignment, and compound element
	// values under it evaluate (#368).
	{`declare -i j=8; let j=j+1; declare -p j`, "declare -i j=\"9\"\n"},
	{`declare -i j=8; ((j=j+2)); declare -p j; j="j+5"; declare -p j`, "declare -i j=\"10\"\ndeclare -i j=\"15\"\n"},
	{`declare -ix e=3; let e=e+1; declare -p e`, "declare -ix e=\"4\"\n"},
	{`typeset -i x; x=([0]=7+11); echo ${x[@]}`, "18\n"},
	{`typeset -i y; y=(1+1 2+2); echo "${y[@]}"`, "2 4\n"},

	// A word in arithmetic context evaluates its string (#367): quoting
	// no longer disables let and ((...)), and a value that is itself an
	// expression evaluates through.
	{`x=5; let "x *= 2"; echo "$? $x"`, "0 10\n"},
	{`let "x=5+2"; echo "$? $x"`, "0 7\n"},
	{`i=0; (( "i < 3" )); echo $?`, "0\n"},
	{`i=0; n=0; for (( ; "i < 3" ; i++ )); do n=$((n+1)); done; echo $n`, "3\n"},
	{`y=1+1; echo $((y))`, "2\n"},
	// An invalid ${var:offset} is an arithmetic error that abandons the
	// input unit, not a silent slice from 0 (#366).
	{
		"HOME2=/x; echo \"${HOME2:`echo \\}`}\"; echo never",
		"}: arithmetic syntax error\nexit status 1 #JUSTERR",
	},
	{
		"let x=3; let 3/0; ((3/0)); echo $((x/y)); let x/=0",
		"division by zero\ndivision by zero\ndivision by zero\ndivision by zero\nexit status 1 #JUSTERR",
	},
	{
		"let x=3; let 3%0; ((3%0)); echo $((x%y)); let x%=0",
		"division by zero\ndivision by zero\ndivision by zero\ndivision by zero\nexit status 1 #JUSTERR",
	},
	{
		"let x=' 3'; echo $x",
		"3\n",
	},
	{
		"x=' 3'; let x++; echo \"$x\"",
		"4\n",
	},

	// set/shift
	{
		"echo $#; set foo bar; echo $#",
		"0\n2\n",
	},
	{
		"shift; set a b c; shift; echo $@",
		"b c\n",
	},
	{
		"shift 2; set a b c; shift 2; echo $@",
		"c\n",
	},
	{
		`echo $#; set '' ""; echo $#`,
		"0\n2\n",
	},
	{
		"set -- a b; echo $#",
		"2\n",
	},
	{
		"set +; echo $#",
		"0\n",
	},
	{
		"set + a b; echo $# $1 $2",
		"2 a b\n",
	},
	{
		"set -U",
		"set: invalid option: \"-U\"\nexit status 2 #JUSTERR",
	},
	{
		"set -e; false; echo foo",
		"exit status 1",
	},
	{
		"set -e; shouldnotexist; echo foo",
		"\"shouldnotexist\": executable file not found in $PATH\nexit status 127 #JUSTERR",
	},
	{
		"set -e; set +e; false; echo foo",
		"foo\n",
	},
	{
		"set -e; ! false; echo foo",
		"foo\n",
	},
	{
		"set -e; ! true; echo foo",
		"foo\n",
	},
	{
		"set -e; if false; then echo never; fi; echo foo",
		"foo\n",
	},
	{
		"set -e; while false; do echo never; done; echo foo",
		"foo\n",
	},
	{
		"set -e; false || true; echo foo",
		"foo\n",
	},
	{
		"set -e; false && true; echo foo",
		"foo\n",
	},
	{
		"set -e; true && false; echo foo",
		"exit status 1",
	},
	{
		"false | :",
		"",
	},
	{
		// Important that we don't print in these, as otherwise we get "broken pipe" errors.
		"GOSH_CMD=exit_5 $GOSH_PROG | GOSH_CMD=exit_0 $GOSH_PROG",
		"",
	},
	{
		"set -o pipefail; false | :",
		"exit status 1",
	},
	{
		"set -o pipefail; GOSH_CMD=exit_5 $GOSH_PROG | GOSH_CMD=exit_0 $GOSH_PROG",
		"exit status 5",
	},
	{
		"set -o pipefail; true | false | true | :",
		"exit status 1",
	},
	{
		"set -o pipefail; set -M 2>/dev/null | false",
		"exit status 1",
	},
	{
		"set -o pipefail; false | :; echo next",
		"next\n",
	},
	{
		"set -o pipefail; exit 0 | :; echo next",
		"next\n",
	},
	{
		"set -o pipefail; exit 1 | :; echo next",
		"next\n",
	},
	{
		"set -e -o pipefail; false | :; echo next",
		"exit status 1",
	},
	{
		"exit 0 && true; echo foo",
		"",
	},
	{
		"exit 1 && true; echo foo",
		"exit status 1",
	},
	{
		"set -f; >a.x; echo *.x;",
		"*.x\n",
	},
	{
		"set -f; set +f; >a.x; echo *.x;",
		"a.x\n",
	},
	{
		"set -a; foo=bar; $ENV_PROG | grep ^foo=",
		"foo=bar\n",
	},
	{
		"set -a; foo=(b a r); $ENV_PROG | grep ^foo=",
		"exit status 1",
	},
	{
		"foo=bar; set -a; $ENV_PROG | grep ^foo=",
		"exit status 1",
	},
	{
		"a=b; echo $a; set -u; echo $a",
		"b\nb\n",
	},
	{
		"echo $a; set -u; echo $a; echo extra",
		"\na: unbound variable\nexit status 1 #JUSTERR",
	},
	{
		"foo=bar; set -u; echo ${foo/bar/}",
		"\n",
	},
	{
		"foo=bar; set -u; echo ${foo#bar}",
		"\n",
	},
	{
		"set -u; echo ${foo/bar/}",
		"foo: unbound variable\nexit status 1 #JUSTERR",
	},
	{
		"set -u; echo ${foo#bar}",
		"foo: unbound variable\nexit status 1 #JUSTERR",
	},
	// TODO: detect this case as unset
	// {
	// 	"set -u; foo=(bar); echo $foo; echo ${foo[3]}",
	// 	"bar\nfoo: unbound variable\nexit status 1 #JUSTERR",
	// },
	{
		"set -u; foo=(''); echo ${foo[0]}",
		"\n",
	},
	{
		"set -u; echo ${#foo}",
		"foo: unbound variable\nexit status 1 #JUSTERR",
	},
	{
		"set -u; echo ${foo+bar}",
		"\n",
	},
	{
		"set -u; echo ${foo:+bar}",
		"\n",
	},
	{
		"set -u; echo ${foo-bar}",
		"bar\n",
	},
	{
		"set -u; echo ${foo:-bar}",
		"bar\n",
	},
	{
		"set -u; echo ${foo=bar}",
		"bar\n",
	},
	{
		"set -u; echo ${foo:=bar}",
		"bar\n",
	},
	{
		"set -u; echo ${foo?bar}",
		"foo: bar\nexit status 1 #JUSTERR",
	},
	{
		"set -u; echo ${foo:?bar}",
		"foo: bar\nexit status 1 #JUSTERR",
	},
	{
		"set -ue; set -ueo pipefail",
		"",
	},
	{"set -n; echo foo", ""},
	{"set -n; [ wrong", ""},
	{"set -n; set +n; echo foo", ""},
	{
		"set -o foobar",
		"set: invalid option: \"foobar\"\nexit status 2 #JUSTERR",
	},
	{"set -o noexec; echo foo", ""},
	{"set +o noexec; echo foo", "foo\n"},
	{"set -e; set -o | grep -E 'errexit|noexec' | wc -l | tr -d ' '", "2\n"},
	{"set -e; set -o | grep -E 'errexit|noexec' | grep 'on$' | wc -l | tr -d ' '", "1\n"},
	{
		"set -a; set +o",
		`set -o allexport
set -o braceexpand
set +o emacs
set +o errexit
set +o errtrace
set +o functrace
set -o hashall
set +o histexpand
set +o history
set +o ignoreeof
set -o interactive-comments
set +o keyword
set +o monitor
set +o noclobber
set +o noexec
set +o noglob
set +o nolog
set +o notify
set +o nounset
set +o onecmd
set +o physical
set +o pipefail
set +o posix
set +o privileged
set +o verbose
set +o vi
set +o xtrace
 #IGNORE`,
	},
	{`set - foobar; echo $@; set -; echo $@`, "foobar\nfoobar\n"},
	// Options koi does not implement but bash starts in a known state:
	// asking for the state they are already in is a no-op in bash and has
	// to be one here, since refusing it is exit 2 and the end of a script
	// running under `set -e` (#245).
	{"set -h; echo ok", "ok\n"},
	{"set +H; echo ok", "ok\n"},
	{"set +m; echo ok", "ok\n"},
	{"set -o hashall; echo ok", "ok\n"},
	{"set +o posix; echo ok", "ok\n"},
	// braceexpand and physical are implemented rather than tolerated, so
	// both directions have to be real.
	{"set +B; echo a{1,2}", "a{1,2}\n"},
	{"set +B; set -B; echo a{1,2}", "a1 a2\n"},
	{"set +o braceexpand; echo x{y,z}", "x{y,z}\n"},

	// unset
	{
		"a=1; echo $a; unset a; echo $a",
		"1\n\n",
	},
	{
		"notinpath() { echo func; }; notinpath; unset -f notinpath; notinpath",
		"func\n\"notinpath\": executable file not found in $PATH\nexit status 127 #JUSTERR",
	},
	{
		"a=1; a() { echo func; }; unset -f a; echo $a",
		"1\n",
	},
	{
		"a=1; a() { echo func; }; unset -v a; a; echo $a",
		"func\n\n",
	},
	{
		"notinpath=1; notinpath() { echo func; }; notinpath; echo $notinpath; unset notinpath; notinpath; echo $notinpath; unset notinpath; notinpath",
		"func\n1\nfunc\n\n\"notinpath\": executable file not found in $PATH\nexit status 127 #JUSTERR",
	},
	{
		"unset PATH; [[ $PATH == '' ]]",
		"",
	},
	{
		"readonly a=1; echo $a; unset a; echo $a",
		"1\na: readonly variable\n1\n #IGNORE bash prints a warning",
	},
	{
		"f() { local a=1; echo $a; unset a; echo $a; }; f",
		"1\n\n",
	},
	{
		`a=b eval 'echo $a; unset a; echo $a'`,
		"b\n\n",
	},
	{
		`$(unset INTERP_GLOBAL); echo $INTERP_GLOBAL; unset INTERP_GLOBAL; echo $INTERP_GLOBAL`,
		"value\n\n",
	},
	{
		`x=orig; f() { local x=local; unset x; x=still_local; }; f; echo $x`,
		"orig\n",
	},
	{
		`x=orig; f() { local x=local; unset x; [[ -v x ]] && echo set || echo unset; }; f`,
		"unset\n",
	},
	{
		`PS3="pick one: "; select opt in foo bar baz; do echo "Selected $opt"; break; done <<< 3`,
		"1) foo\n2) bar\n3) baz\npick one: Selected baz\n",
	},
	{
		`opts=(foo bar baz); select opt in ${opts[@]}; do echo "Selected $opt"; break; done <<< 99`,
		"1) foo\n2) bar\n3) baz\n#? Selected \n",
	},
	{
		`select opt in foo; do
	case $opt in
	foo) echo "option 1"; break;;
	*) echo "invalid option $REPLY"; break;;
	esac
done <<< 2`,
		"1) foo\n#? invalid option 2\n",
	},
	{
		"select opt in a b c; do echo \"got $opt\"; if [[ $REPLY == 2 ]]; then break; fi; done <<< $'1\n2'",
		"1) a\n2) b\n3) c\n#? got a\n#? got b\n",
	},
	{
		"select opt in a b; do break; done </dev/null; echo status $?",
		"1) a\n2) b\n#? \nstatus 1\n",
	},
	{
		"select opt in a b; do echo \"got $opt\"; done <<< 2",
		"1) a\n2) b\n#? got b\n#? \nexit status 1",
	},
	{
		"select opt in a b; do break; done <<< $'\n1'",
		"1) a\n2) b\n#? 1) a\n2) b\n#? ",
	},

	// shopt
	{"set -e; shopt -o | grep -E '^(errexit|noexec)' | wc -l | tr -d ' '", "2\n"},
	{"set -e; shopt -o | grep -E '^(errexit|noexec)' | grep 'on$' | wc -l | tr -d ' '", "1\n"},
	{"set -e; shopt | grep -E '^(errexit|noexec)' | wc -l | tr -d ' '", "0\n"},
	{"shopt -s -o noexec; echo foo", ""},
	{"shopt -so noexec; echo foo", ""},
	{"shopt -u -o noexec; echo foo", "foo\n"},
	{"shopt -u globstar; shopt globstar | grep 'off$' | wc -l | tr -d ' '", "1\n"},
	{"shopt -s globstar; shopt globstar | grep 'off$' | wc -l | tr -d ' '", "0\n"},
	// lastpipe (#277): off by default as in bash, so the last pipeline
	// stage is a subshell like every other stage — before this, koi
	// answered the most famous bash gotcha un-bash-ly and `exit` in a
	// last stage took the whole shell down.
	{"echo foo | read x; echo \"x=[$x]\"", "x=[]\n"},
	{"printf 'a\\nb\\n' | while read l; do n=$((n+1)); done; echo \"n=[$n]\"", "n=[]\n"},
	{"echo | exit 3; echo after=$?", "after=3\n"},
	{"echo | cd /; [ \"$PWD\" = / ] && echo moved || echo stayed", "stayed\n"},
	{"shopt -s lastpipe; echo foo | read x; echo \"x=[$x]\"", "x=[foo]\n"},
	{"shopt -s lastpipe; printf 'a\\nb\\n' | while read l; do n=$((n+1)); done; echo \"n=[$n]\"", "n=[2]\n"},
	{"shopt -s lastpipe; shopt -u lastpipe; echo foo | read x; echo \"x=[$x]\"", "x=[]\n"},
	{"shopt lastpipe | grep 'off$' | wc -l | tr -d ' '", "1\n"},
	{"set -o pipefail; shopt -s lastpipe; false | true; echo st=$?", "st=1\n"},
	// declare/typeset behind a prefix assignment (#277): the prefix keeps
	// the word from being a keyword, so these arrive at the builtin
	// dispatch, which refused them — `ref=xxx typeset -p ref` answered
	// "unsupported builtin" while `typeset -p ref` worked.
	{"x=1 declare -p x", "declare -x x=\"1\"\n"},
	{"x=1 typeset -p x", "declare -x x=\"1\"\n"},
	{"v=1; x=2 declare -p v", "declare -- v=\"1\"\n"},
	{"x=1 declare -x y=2; declare -p y", "declare -x y=\"2\"\n"},
	{"ref=xxx typeset -p nosuch 2>/dev/null; echo st=$?", "st=1\n"},
	{"shopt extglob | grep 'off' | wc -l | tr -d ' '", "1\n"},
	{
		"shopt inherit_errexit",
		"inherit_errexit\ton\t(\"off\" not supported)\n #JUSTERR",
	},
	{
		"shopt -o -s pipefail; shopt -o pipefail | grep -q 'on$'",
		"",
	},
	{
		"shopt -o -u pipefail; shopt -o pipefail | grep -q 'on$'",
		"exit status 1",
	},
	{
		"shopt pipefail",
		"shopt: invalid option name \"pipefail\"\nexit status 1 #JUSTERR",
	},
	{
		"shopt -s pipefail",
		"shopt: invalid option name \"pipefail\"\nexit status 1 #JUSTERR",
	},
	{
		"shopt -o -s extglob",
		"shopt: invalid option name \"extglob\"\nexit status 1 #JUSTERR",
	},
	{
		"shopt -s login_shell",
		"shopt: unsupported option \"login_shell\"\nexit status 1 #IGNORE",
	},
	{
		"shopt -s interactive_comments",
		"shopt: unsupported option \"interactive_comments\"\nexit status 1 #IGNORE",
	},
	{
		"shopt -s nosuchname",
		"shopt: invalid option name \"nosuchname\"\nexit status 1 #JUSTERR",
	},
	{
		"shopt -o -s nosuchname",
		"shopt: invalid option name \"nosuchname\"\nexit status 1 #JUSTERR",
	},
	{
		"touch a .b ..c; shopt -u dotglob; echo *",
		"a\n",
	},
	{
		"touch a .b ..c; shopt -s dotglob; echo *",
		"..c .b a\n",
	},
	{
		"mkdir sub .sub2; touch {sub,.sub2}/{a,.b}; shopt -s globstar; shopt -u dotglob; echo **/* | sed 's@\\\\@/@g'",
		"sub sub/a\n",
	},
	// Adjacent ** collapse instead of cross-multiplying, and the
	// zero-match trailing slash appears only after a literal prefix
	// (#371): a/** answers "a/" where **/a/**, a/**/** and */** answer
	// bare names — with **/a/**'s natural duplicate kept, as bash keeps
	// it.
	{
		"mkdir -p ga/a gb/a; shopt -s globstar; printf '<%s>' **/** | sed 's@\\\\@/@g'; echo",
		"<ga><ga/a><gb><gb/a>\n",
	},
	{
		"mkdir -p ga/a; shopt -s globstar; printf '<%s>' ga/** | sed 's@\\\\@/@g'; echo",
		"<ga/><ga/a>\n",
	},
	{
		"mkdir -p ga/a; shopt -s globstar; printf '<%s>' ga/**/** | sed 's@\\\\@/@g'; echo",
		"<ga><ga/a>\n",
	},
	{
		"mkdir -p a/a b/a; shopt -s globstar; printf '<%s>' **/a/** | sed 's@\\\\@/@g'; echo",
		"<a><a/a><a/a><b/a>\n",
	},
	{
		"mkdir -p ga/a gb/a; shopt -s globstar; printf '<%s>' */** | sed 's@\\\\@/@g'; echo",
		"<ga><ga/a><gb><gb/a>\n",
	},
	{
		"mkdir sub .sub2; touch {sub,.sub2}/{a,.b}; shopt -s globstar; shopt -s dotglob; echo **/* | sed 's@\\\\@/@g'",
		".sub2 .sub2/.b .sub2/a sub sub/.b sub/a\n",
	},
	{
		// Beware that macOS file systems are by default case-preserving but
		// case-insensitive, so e.g. "touch x X" creates only one file.
		"touch a ab Ac Ad; shopt -u nocaseglob; echo a*",
		"a ab\n",
	},
	{
		"touch a ab Ac Ad; shopt -s nocaseglob; echo a*",
		"Ac Ad a ab\n",
	},
	{
		"touch a ab abB Ac Ad; shopt -u nocaseglob; echo *b",
		"ab\n",
	},
	{
		"touch a ab abB Ac Ad; shopt -s nocaseglob; echo *b",
		"ab abB\n",
	},
	{
		"shopt -p",
		"shopt: unsupported option \"-p\"\nexit status 2 #IGNORE",
	},
	{
		"shopt -q",
		"shopt: unsupported option \"-q\"\nexit status 2 #IGNORE",
	},

	// $'\x{...}' and $'\cX' (#365): the brace hex form (closing brace
	// optional, value masked to a byte, empty braces a truncating NUL)
	// and control-char notation. printf's own format keeps its rules.
	{`x=$'ab\x{41}cd'; echo "$x"`, "abAcd\n"},
	{`x=$'\ca'; [ "$x" = "$(printf '\001')" ] && echo ok`, "ok\n"},
	{`x=$'\c?'; [ "$x" = "$(printf '\177')" ] && echo ok`, "ok\n"},
	{`x=$'a\x{}b'; echo "[$x]"`, "[a]\n"},
	{`x=$'a\x{4141}b'; echo "$x"`, "aAb\n"},
	{`x=$'a\x{41 b'; echo "$x"`, "aA b\n"},
	{`x=$'\c'; echo "$x"`, "\\c\n"},
	{`x=$'\u{41}'; echo "$x"`, "\\u{41}\n"},

	// Tilde positions beyond the word start (#364): after each colon in
	// an assignment value, after the = of an assignment-shaped argument,
	// at a colon-terminated prefix — and ~+/~- read PWD/OLDPWD. Results
	// are compared through $HOME so any machine confirms them.
	{`p=/bin:~/bin; [ "$p" = "/bin:$HOME/bin" ] && echo ok`, "ok\n"},
	{`[ "$(echo make FOO=~/mumble)" = "make FOO=$HOME/mumble" ] && echo ok`, "ok\n"},
	{`[ "$(echo a:~)" = "a:~" ] && echo ok`, "ok\n"},
	{`[ "$(echo ~:x)" = "$HOME:x" ] && echo ok`, "ok\n"},
	{`[ "$(echo FOO=~:~)" = "FOO=$HOME:$HOME" ] && echo ok`, "ok\n"},
	{`[ "$(echo 3foo=~)" = "3foo=~" ] && echo ok`, "ok\n"},
	{`cd /usr; cd /tmp; [ "$(echo ~- ~+)" = "/usr $(pwd)" ] && echo ok`, "ok\n"},
	{`p="pre ~"; q=x:$p:~; [ "$q" = "x:pre ~:$HOME" ] && echo ok`, "ok\n"},

	// "$@" splits at element boundaries even with text attached (#361),
	// and a quoted ${x+word} keeps that identity for a "$@" inside the
	// word (#360).
	{`n(){ printf "<%s>" "$@"; echo; }; set -- 1 2; n "x $@ y"`, "<x 1><2 y>\n"},
	{`n(){ printf "<%s>" "$@"; echo; }; set -- 1 2; n "$@$@"`, "<1><21><2>\n"},
	{`n(){ printf "<%s>" "$@"; echo; }; set --; n "x $@ y"`, "<x  y>\n"},
	{`n(){ printf "<%s>" "$@"; echo; }; a=(p q); n "x ${a[@]} y"`, "<x p><q y>\n"},
	{`n(){ printf "<%s>" "$@"; echo; }; set -- "a b" c; n "${1+$@}"`, "<a b><c>\n"},
	{`n(){ printf "<%s>" "$@"; echo; }; set -- "a b" c; n "${1+"$@"}"`, "<a b><c>\n"},
	{`n(){ printf "<%s>" "$@"; echo; }; set -- 1 2; IFS=:; n "x $* y"`, "<x 1:2 y>\n"},
	// A quoted ${x+word} does not tilde-expand; $* and $@ are unset with
	// no positional parameters; empty IFS keeps per-element fields for
	// unquoted list expansions and their per-element operators (#360,
	// #361).
	{`n(){ printf "<%s>" "$@"; echo; }; unset u123; n "${u123:-~}"`, "<~>\n"},
	{`n(){ printf "<%s>" "$@"; echo; }; set --; n ${*-x}`, "<x>\n"},
	{`n(){ printf "<%s>" "$@"; echo; }; set -- " A " " B "; IFS=; n $*`, "< A >< B >\n"},
	{`n(){ printf "<%s>" "$@"; echo; }; set -- " A " " B "; IFS=; n ${*##}`, "< A >< B >\n"},
	{`n(){ printf "<%s>" "$@"; echo; }; unset u; n "${u:-'x'}"`, "<'x'>\n"},

	// Single quotes inside a double-quoted ${x+word} are literal text
	// (#359), and what sits between them still expands; $'..' expands
	// there, a heredoc's ${x+word} keeps the span as written, and a
	// pattern operator's quotes really do quote.
	{`echo "foo ${IFS+'bar'} baz"`, "foo 'bar' baz\n"},
	{`a=foo; echo "${IFS+'$a'}"`, "'foo'\n"},
	{`a=foo; echo "${IFS+'\$a'}"`, "'$a'\n"},
	{`unset u; echo "${u-'x'}"`, "'x'\n"},
	{`a=foo; echo "${a#'f'}"`, "oo\n"},
	{`x=1; echo ${x:+'q'}`, "q\n"},
	{"unset n; cat <<EOF\n${n-a$'\\01'b}\nEOF", "a$'\\01'b\n"},

	// The word in an unquoted ${param op word} expands in the caller's
	// context (#358): quoted nulls make fields, quoted spaces are
	// protected, and an inner "$@" splits into the parameters — the flat
	// string loses all three.
	{`n(){ echo $#; }; x=x; n ${x:+""}`, "1\n"},
	{`n(){ echo $#; }; x=x; n ${x:+'' ''}`, "2\n"},
	{`n(){ echo $#; }; x=x; set -- "" ""; n ${x+"$@"}`, "2\n"},
	{`n(){ echo "$#:$1:$2"; }; unset u; n ${u:-"a b" c}`, "2:a b:c\n"},
	{`n(){ echo $#; }; unset u; n ${u:-}`, "0\n"},
	{`x=x; echo pre${x:+a b}post`, "prea bpost\n"},
	{`n(){ echo "$#:$1"; }; x=x; n "${x:+a b}"`, "1:a b\n"},
	{`IFS=:; x=x; n(){ echo "$#:$1:$2"; }; n ${x:+:a}`, "2::a\n"},

	// Backslash quote removal outside command words (#357): an unquoted
	// \X in an assignment value, a ${...} word, a case subject, or a
	// redirect target reads back as X — while a *pattern*'s backslash
	// survives to mean a literal match.
	{`T=a\;b; echo "$T"`, "a;b\n"},
	{`unset a; printf "[%s]\n" ${a:=a\ b}; echo "a=[$a]"`, "[a]\n[b]\na=[a b]\n"},
	{`v=1; echo ${v/1/\'}`, "'\n"},
	{`case \x in \x) echo m;; *) echo n;; esac`, "m\n"},
	{`case \* in \*) echo star;; *) echo other;; esac`, "star\n"},
	{`case x in \*) echo wrong;; *) echo right;; esac`, "right\n"},
	{`echo hi > a\ b; cat "a b"`, "hi\n"},

	// IFS
	{`echo -n "$IFS"`, " \t\n"},
	{`a="x:y:z"; IFS=:; echo $a`, "x y z\n"},
	// A non-whitespace IFS delimiter delimits *empty* fields (#356):
	// adjacent delimiters do not collapse, a leading one yields an empty
	// first field, and only a trailing one yields nothing.
	{`IFS=:; x=":a::b:"; set -- $x; echo "[$#]($1)($2)($3)($4)"`, "[4]()(a)()(b)\n"},
	{`IFS=:; x=":"; set -- $x; echo "[$#]($1)"`, "[1]()\n"},
	{`IFS=": "; x="a : : b"; set -- $x; echo "[$#]($1)($2)($3)"`, "[3](a)()(b)\n"},
	{`IFS=": " read x y <<< ":a"; echo "($x)($y)"`, "()(a)\n"},
	{`IFS=: read -a A <<< ":a::b:"; echo "n=${#A[@]} [${A[0]}][${A[1]}][${A[2]}][${A[3]}]"`, "n=4 [][a][][b]\n"},
	{`IFS=: read x y z <<< "a::b"; echo "[$x][$y][$z]"`, "[a][][b]\n"},
	// With more fields than names the last name takes the rest of the
	// line as written; with the fields fitting, plain assignment strips
	// the trailing delimiter with the field it closed.
	{`IFS=: read x <<< "a:b:"; echo "[$x]"`, "[a:b:]\n"},
	{`IFS=: read x <<< "a:"; echo "[$x]"`, "[a]\n"},
	{`a=(x y z); IFS=-; echo ${a[*]}`, "x y z\n"},
	{`a=(x y z); IFS=-; echo ${a[@]}`, "x y z\n"},
	{`a=(x y z); IFS=-; echo "${a[*]}"`, "x-y-z\n"},
	{`a=(x y z); IFS=-; echo "${a[@]}"`, "x y z\n"},
	{`a="  x y z"; IFS=; echo $a`, "  x y z\n"},
	{`a=(x y z); IFS=; echo "${a[*]}"`, "xyz\n"},
	{`a=(x y z); IFS=-; echo "${!a[@]}"`, "0 1 2\n"},
	{`set -- x y z; IFS=-; echo $*`, "x y z\n"},
	{`set -- x y z; IFS=-; echo "$*"`, "x-y-z\n"},
	{`set -- x y z; IFS=; echo $*`, "x y z\n"},
	{`set -- x y z; IFS=; echo "$*"`, "xyz\n"},
	{`set -- x y z; IFS=-; a=$*; echo "$a"`, "x-y-z\n"},
	{`set -- x y z; IFS=; a=$*; echo "$a"`, "xyz\n"},
	{`a=(x y z); IFS=; echo ${a[*]}; c=${a[*]}; echo "$c"`, "x y z\nxyz\n"},
	{`a=(x y z); IFS=-; b=${a[*]}; echo "$b"`, "x-y-z\n"},
	{`set -- x y; IFS=éz; a=$*; echo "$a"`, "xéy\n"},
	{`set -- xo yo; IFS=-; a=${*%o}; echo "$a"`, "x-y\n"},
	{`a=(zo wo); IFS=-; b=${a[*]^}; echo "$b"`, "Zo-Wo\n"},
	{`a=(x y z); IFS=-; echo "${!a[*]}"`, "0-1-2\n"},
	{`INTERP_Y_1=a INTERP_Y_2=b; IFS=-; echo "${!INTERP_Y_*}"`, "INTERP_Y_1-INTERP_Y_2\n"},

	// builtin
	{"builtin", ""},
	{"builtin noexist", "exit status 1 #JUSTERR"},
	{"builtin echo foo", "foo\n"},
	{
		"echo() { printf 'bar\n'; }; echo foo; builtin echo foo",
		"bar\nfoo\n",
	},

	// type
	{"type", ""},
	{"type for", "for is a shell keyword\n"},
	{"type echo", "echo is a shell builtin\n"},
	{"echo() { :; }; type echo | grep 'is a function'", "echo is a function\n"},
	{"type $PATH_PROG | grep -q -E ' is (/|[A-Z]:)'", ""},
	{"type noexist", "type: noexist: not found\nexit status 1 #JUSTERR"},
	{"type -o echo", "type: invalid option \"-o\"\nexit status 2 #JUSTERR"},
	{"PATH=/; type $PATH_PROG", "type: " + pathProg + ": not found\nexit status 1 #JUSTERR"},
	{"shopt -s expand_aliases; alias interp_foo='bar baz'\ntype interp_foo", "interp_foo is aliased to `bar baz'\n"},
	{"alias interp_foo='bar baz'\ntype interp_foo", "type: interp_foo: not found\nexit status 1 #JUSTERR"},
	{"type -p $PATH_PROG | grep -q -E '^(/|[A-Z]:)'", ""},
	{"PATH=/; type -p $PATH_PROG", "exit status 1"},
	// TODO: type -P should force PATH lookup even for builtins, unlike type -p.
	{"type -P $PATH_PROG | grep -q -E '^(/|[A-Z]:)'", ""},
	{"PATH=/; type -P $PATH_PROG", "exit status 1"},
	{"shopt -s expand_aliases; alias interp_foo='bar'; type -t interp_foo", "alias\n"},
	{"type -t case", "keyword\n"},
	{"interp_foo(){ :; }; type -t interp_foo", "function\n"},
	{"type -t type", "builtin\n"},
	{"type -t $PATH_PROG", "file\n"},
	{"type -t inexisting_dfgsdgfds", "exit status 1"},

	// hash
	{"hash $PATH_PROG", ""},

	// trap
	{"trap 'echo at_exit' EXIT; true", "at_exit\n"},
	{"trap 'echo on_err' ERR; false; echo FAIL", "on_err\nFAIL\n"},
	{"trap 'echo on_err' ERR; false || true; echo OK", "OK\n"},
	{"trap 'echo at_exit' EXIT; trap - EXIT; echo OK", "OK\n"},
	{"set -e; trap 'echo A' ERR EXIT; false; echo FAIL", "A\nA\nexit status 1"},
	{"trap 'foobar' UNKNOWN", "trap: UNKNOWN: invalid signal specification\nexit status 1 #JUSTERR"},
	// $LINENO inside a trap action (#352): DEBUG and ERR count from the
	// line of the command that triggered them, EXIT from the line the
	// trap was set on, and a multi-line action counts on from its base.
	{
		"trap 'echo L=$LINENO' DEBUG\necho one\necho two",
		"L=2\none\nL=3\ntwo\n",
	},
	{
		"trap 'echo E=$LINENO' ERR\ntrue\nfalse\necho after",
		"E=3\nafter\n",
	},
	{
		"trap 'echo X=$LINENO' EXIT\ntrue\nexit 0",
		"X=1\n",
	},
	{
		// EXIT counts from the trap's own line. bash resets LINENO per
		// input unit on stdin (this harness's mode) and answers 1 here;
		// koi parses its input as one file, where bash answers 3 too.
		"true\ntrue\ntrap 'echo X=$LINENO' EXIT\ntrue\nexit 0",
		"X=3\n #IGNORE bash counts stdin lines per input unit",
	},
	{
		"trap 'echo A=$LINENO\necho B=$LINENO' DEBUG\necho one",
		"A=3\nB=4\none\n",
	},
	// An EXIT trap fired by `exit` inside a function sees that
	// function's FUNCNAME, not an emptied stack (#352).
	{
		"f(){ trap 'echo T:${FUNCNAME[0]:-none}' EXIT; exit 5; }\nf\necho never",
		"T:f\nexit status 5",
	},
	{"trap 'foobar' 99; echo st=$?", "trap: 99: invalid signal specification\nst=1\n #JUSTERR"},
	// TODO: our builtin appears to not receive the piped bytes?
	// {"trap 'echo on_err' ERR; trap | grep -q '.*echo on_err.*'", "trap -- \"echo on_err\" ERR\n"},
	{"trap 'false' ERR EXIT; false", "exit status 1"},
	// extdebug: a DEBUG trap answering nonzero cancels the command, the
	// mechanism a debugger's step/skip is built on (#355). The skipped
	// command leaves $? as 0, and its redirections never open.
	{
		"shopt -s extdebug\nskip(){ return 2; }\ntrap 'skip' DEBUG\nx=2\necho \"x is ${x:-unset}\"",
		"",
	},
	{
		"shopt -s extdebug\nskip(){ case \"$BASH_COMMAND\" in echo\\ skipme*) return 2;; esac; return 0; }\ntrap 'skip' DEBUG\nfalse\necho skipme > x1of\necho \"st=$? file=$([ -e x1of ] && echo yes || echo no)\"",
		"st=0 file=no\n",
	},
	{
		// Without extdebug, a nonzero DEBUG trap changes nothing.
		"skip(){ return 2; }\ntrap 'skip' DEBUG\necho runs",
		"runs\n",
	},
	// A `return` inside a function the trap action calls ends that
	// function — it must not be suppressed until the action runs out of
	// statements, or a trailing `return 0` overwrites the answer.
	{
		"f(){ echo a; return 2; echo b; return 0; }\ntrap 'f' DEBUG\ntrue\ntrap - DEBUG",
		"a\na\n #IGNORE bash also fires DEBUG for the trap builtin itself",
	},
	// An ERR trap set inside a subshell or a function fires for failures
	// in that scope (#354): "not inherited" restricts a parent's trap,
	// never the one the scope itself set.
	{"( trap 'echo e' ERR; false; echo after ); echo main", "e\nafter\nmain\n"},
	{"f(){ trap 'echo e' ERR; false; echo in-f; }; f; false; echo top", "e\nin-f\ne\ntop\n"},
	{"trap 'echo outer' ERR; f(){ false; echo in-f; }; f", "in-f\n"},
	{
		// A compound command whose redirection fails also fires it.
		// 2>/dev/null first: redirections apply left to right, and the
		// failing open's complaint is the shell's, so it must already be
		// redirected when the open fails.
		"( trap 'echo e' ERR; while [ -z x ]; do :; done 2>/dev/null </dev/null >/nonexistent-dir-354/f; echo after: $? )",
		"e\nafter: 1\n #IGNORE the unwritable path differs by platform",
	},
	// An EXIT trap set in a subshell runs when that subshell ends (#353),
	// whether it fell off the end, called exit, ran as a command
	// substitution, a background job, or a pipeline stage — and the
	// parent's EXIT trap never fires inside one.
	{"( trap 'echo sub' EXIT; exit 0 ); echo main", "sub\nmain\n"},
	{"( trap 'echo sub' EXIT; true ); echo main", "sub\nmain\n"},
	{"trap 'echo outer' EXIT; ( true ); echo main; trap - EXIT", "main\n"},
	{"x=$( trap 'echo ce' EXIT; echo body ); echo \"[$x]\"", "[body\nce]\n"},
	{"{ trap 'echo bgx' EXIT; true; } & wait; echo done", "bgx\ndone\n"},
	{"true | { trap 'echo p' EXIT; true; }; echo main", "p\nmain\n"},
	// `exit` inside an EXIT trap replaces the status; an ordinary
	// failing command in the action does not.
	{"( trap 'exit 9' EXIT; exit 3 ); echo st=$?", "st=9\n"},
	{"( trap 'echo t; false' EXIT; exit 3 ); echo st=$?", "t\nst=3\n"},
	{"trap 'exit 9' EXIT; exit 3", "exit status 9"},
	// A parse error in one trap callback must not disable later ones.
	{"trap '(' ERR; false; trap 'echo ok' ERR; false; :", "errortrap: error trap:1:1: `(` must be followed by a statement list\nok\n #IGNORE"},
	// On entry to a trap, "$?" is the status of the command which triggered it.
	{"trap 'echo err $?' ERR; false; echo after $?", "err 1\nafter 1\n"},
	{"trap 'echo exit $?' EXIT; false", "exit 1\nexit status 1"},
	{"trap 'echo exit $?' EXIT; true", "exit 0\n"},
	{"trap 'false; echo next $?' EXIT; true", "next 1\n"},
	{"trap 'echo err $?' ERR; trap 'echo exit $?' EXIT; false; true", "err 1\nexit 0\n"},

	// The ERR trap runs once for the command which failed, not again for each
	// compound command which propagates its status outwards.
	{"trap 'echo T' ERR; { false; }; echo end", "T\nend\n"},
	{"trap 'echo T' ERR; { { false; } }; echo end", "T\nend\n"},
	{"trap 'echo T' ERR; if true; then false; fi; echo end", "T\nend\n"},
	{"trap 'echo T' ERR; for i in 1; do false; done; echo end", "T\nend\n"},
	{"trap 'echo T' ERR; case x in x) false;; esac; echo end", "T\nend\n"},
	{"trap 'echo T' ERR; true | false; echo end", "T\nend\n"},
	{"trap 'echo T' ERR; false; false; echo end", "T\nT\nend\n"},

	// Without -E the trap is not inherited by functions or subshells, so only
	// the call itself runs it, however deeply the failure was nested.
	{"trap 'echo T' ERR; f() { false; }; f; echo end", "T\nend\n"},
	{"trap 'echo T' ERR; f() { false; true; }; f; echo end", "end\n"},
	{"trap 'echo T' ERR; g() { false; }; f() { g; }; f; echo end", "T\nend\n"},
	{"trap 'echo T' ERR; ( false ); echo end", "T\nend\n"},

	// With -E it is inherited, so each level runs it: once inside and once for
	// the call.
	{"set -E; trap 'echo T' ERR; f() { false; }; f; echo end", "T\nT\nend\n"},
	{"set -E; trap 'echo T' ERR; g() { false; }; f() { g; }; f; echo end", "T\nT\nT\nend\n"},
	{"set -E; trap 'echo T' ERR; ( false ); echo end", "T\nT\nend\n"},
	{"set -E; trap 'echo T' ERR; { false; }; echo end", "T\nend\n"},
	{"set -E; set +E; trap 'echo T' ERR; f() { false; }; f; echo end", "T\nend\n"},
	{
		// a condition suppresses the trap inside the function too
		"set -E; trap 'echo T' ERR; f() { false; }; if f; then :; fi; echo end",
		"end\n",
	},

	// -E and -T are what let the usual strict-mode header apply at all; with
	// either one refused, none of -e, -u or -o pipefail took effect.
	{"set -Eeuo pipefail; false; echo REACHED", "exit status 1"},
	{"set -eETuo pipefail; echo ok", "ok\n"},
	{"set -T; echo ok", "ok\n"},
	{"set -o errtrace; echo ok", "ok\n"},

	// PIPESTATUS
	{`false | true; echo "${PIPESTATUS[@]}"`, "1 0\n"},
	{`true | false; echo "${PIPESTATUS[@]}"`, "0 1\n"},
	{`(exit 1) | (exit 2) | (exit 3) | (exit 4); echo "${PIPESTATUS[@]}"`, "1 2 3 4\n"},
	{`false | true; echo "${PIPESTATUS[0]}/${PIPESTATUS[1]}/${#PIPESTATUS[@]}"`, "1/0/2\n"},
	{`true |& false; echo "${PIPESTATUS[@]}"`, "0 1\n"},
	{
		// a command which is not a pipeline still gets the one status
		`false; echo "${PIPESTATUS[@]}"`,
		"1\n",
	},
	{`x=1; echo "${PIPESTATUS[@]}"`, "0\n"},
	{
		// the statuses are those the commands exited with, so negating the
		// pipeline changes $? but not PIPESTATUS
		`! true | false; echo "$? ${PIPESTATUS[@]}"`,
		"0 0 1\n",
	},
	{
		// compound commands propagate the pipeline's statuses rather than
		// replacing them with their own single status
		`{ true | false; }; echo "${PIPESTATUS[@]}"`,
		"0 1\n",
	},
	{`if true | false; then :; fi; echo "${PIPESTATUS[@]}"`, "0 1\n"},
	{`for i in 1; do true | false; done; echo "${PIPESTATUS[@]}"`, "0 1\n"},
	{
		// a function call and a subshell are each one command, so they get one
		// status however the body reached it
		`f() { true | false; }; f; echo "${PIPESTATUS[@]}"`,
		"1\n",
	},
	{`( true | false ); echo "${PIPESTATUS[@]}"`, "1\n"},
	{
		// the ERR trap's own commands must not overwrite it
		`trap 'echo T; true' ERR; true | false; echo "${PIPESTATUS[@]}"`,
		"T\n0 1\n",
	},
	{`set -o pipefail; false | true; echo "$? ${PIPESTATUS[@]}"`, "1 1 0\n"},
	{"false; trap 'echo exit $?' EXIT; true", "exit 0\n"},

	// eval
	{"eval", ""},
	{"eval ''", ""},
	{"eval echo foo", "foo\n"},
	{"eval 'echo foo'", "foo\n"},
	{"eval 'exit 1'", "exit status 1"},
	// koi-local: 2, not upstream's 1. bash answers 2 for a syntax error
	// in every non-interactive form — a script, -c, a sourced file and
	// eval alike — and koi's own `-n` check already said so (#276).
	{"eval '(x'", "eval: 1:1: reached EOF without matching `(` with `)`\nexit status 2 #JUSTERR"},
	{"set a b; eval 'echo $@'", "a b\n"},
	{"eval 'a=foo'; echo $a", "foo\n"},
	{`a=b eval "echo $a"`, "\n"},
	{`a=b eval 'echo $a'`, "b\n"},
	{`eval 'echo "\$a"'`, "$a\n"},
	{`a=b eval 'x=y eval "echo \$a \$x"'`, "b y\n"},
	{`a=b eval 'a=y eval "echo $a \$a"'`, "b y\n"},
	{"a=b eval '(echo $a)'", "b\n"},

	// source
	{
		"source",
		"1:1: source: need filename\nexit status 2 #JUSTERR",
	},
	{
		"echo 'echo foo' >a; source ./a; . ./a",
		"foo\nfoo\n",
	},
	{
		"echo 'echo $@' >a; source ./a; source ./a b c; echo $@",
		"\nb c\n\n",
	},
	{
		"echo 'foo=bar' >a; source ./a; echo $foo",
		"bar\n",
	},

	// source from PATH
	{
		"mkdir test; echo 'echo foo' >test/a; PATH=$PWD/test source a; . test/a",
		"foo\nfoo\n",
	},

	// source with set and shift
	{
		"echo 'set -- d e f' >a; source ./a; echo $@",
		"d e f\n",
	},
	{
		"echo 'echo $@' >a; set -- b c; source ./a; echo $@",
		"b c\nb c\n",
	},
	{
		"echo 'echo $@' >a; set -- b c; source ./a d e; echo $@",
		"d e\nb c\n",
	},
	{
		"echo 'shift; echo $@' >a; set -- b c; source ./a d e; echo $@",
		"e\nb c\n",
	},
	{
		"echo 'shift' >a; set -- b c; source ./a; echo $@",
		"c\n",
	},
	{
		"echo 'shift; set -- $@' >a; set -- b c; source ./a d e; echo $@",
		"e\n",
	},
	{
		"echo 'set -- g f'>b; echo 'set -- d e f; echo $@; source ./b;' >a; source ./a; echo $@",
		"d e f\ng f\n",
	},
	{
		"echo 'set -- g f'>b; echo 'echo $@; set -- d e f; source ./b;' >a; source ./a b c; echo $@",
		"b c\ng f\n",
	},
	{
		"echo 'shift; echo $@' >b; echo 'shift; echo $@; source ./b' >a; source ./a b c d; echo $@",
		"c d\nd\n\n",
	},
	{
		"echo 'set -- b c d' >b; echo 'source ./b' >a; set -- a; source ./a; echo $@",
		"b c d\n",
	},
	{
		"echo 'echo $@' >b; echo 'set -- b c d; source ./b' >a; set -- a; source ./a; echo $@",
		"b c d\nb c d\n",
	},
	{
		"echo 'shift; echo $@' >b; echo 'shift; echo $@; source ./b c d' >a; set -- a b; source ./a; echo $@",
		"b\nd\nb\n",
	},
	{
		"echo 'set -- a b c' >b; echo 'echo $@; source ./b; echo $@' >a; source ./a; echo $@",
		"\na b c\na b c\n",
	},

	// indexed arrays
	{
		"a=foo; echo ${a[0]} ${a[@]} ${a[x]}; echo ${a[1]}",
		"foo foo foo\n\n",
	},
	{
		"a=(); echo ${a[0]} ${a[@]} ${a[x]} ${a[1]}",
		"\n",
	},
	{
		"a=(b c); echo $a; echo ${a[0]}; echo ${a[1]}; echo ${a[x]}",
		"b\nb\nc\nb\n",
	},
	{
		"a=(b c); echo ${a[@]}; echo ${a[*]}",
		"b c\nb c\n",
	},
	{
		"a=(1 2 3); echo ${a[2-1]}; echo $((a[1+1]))",
		"2\n3\n",
	},
	{
		"a=(1 2) x=(); a+=b x+=c; echo ${a[@]}; echo ${x[@]}",
		"1b 2\nc\n",
	},
	{
		"a=(1 2) x=(); a+=(b c) x+=(d e); echo ${a[@]}; echo ${x[@]}",
		"1 2 b c\nd e\n",
	},
	{
		"a=bbb; a+=(c d); echo ${a[@]}",
		"bbb c d\n",
	},
	{
		`a=('a  1' 'b  2'); for e in ${a[@]}; do echo "$e"; done`,
		"a\n1\nb\n2\n",
	},
	{
		`a=('a  1' 'b  2'); for e in "${a[*]}"; do echo "$e"; done`,
		"a  1 b  2\n",
	},
	{
		`a=('a  1' 'b  2'); for e in "${a[@]}"; do echo "$e"; done`,
		"a  1\nb  2\n",
	},
	{
		`declare -a a; a[0]='a  1'; a[1]='b  2'; for e in "${a[@]}"; do echo "$e"; done`,
		"a  1\nb  2\n",
	},
	{
		`a=([1]=y [0]=x); echo ${a[0]}`,
		"x\n",
	},
	{
		`a=(y); a[2]=x; echo ${a[2]}`,
		"x\n",
	},
	{
		`a="y"; a[2]=x; echo ${a[2]}`,
		"x\n",
	},
	{
		`declare -a a=(x y); echo ${a[1]}`,
		"y\n",
	},
	{
		`a=b; echo "${a[@]}"`,
		"b\n",
	},
	{
		`a=(b); echo ${a[3]}`,
		"\n",
	},
	{
		`a=(b); echo ${a[-2]}`,
		"negative array index\n #JUSTERR",
	},
	// TODO: also test with gaps in arrays.
	{
		`a=([0]=' x ' [1]=' y '); for v in "${a[@]}"; do echo "$v"; done`,
		" x \n y \n",
	},
	{
		`a=([0]=' x ' [1]=' y '); for v in "${a[*]}"; do echo "$v"; done`,
		" x   y \n",
	},
	{
		`a=([0]=' x ' [1]=' y '); for v in "${!a[@]}"; do echo "$v"; done`,
		"0\n1\n",
	},
	{
		`a=([0]=' x ' [1]=' y '); for v in "${!a[*]}"; do echo "$v"; done`,
		"0 1\n",
	},

	// associative arrays
	{
		`a=foo; echo ${a[""]} ${a["x"]}`,
		"foo foo\n",
	},
	{
		`declare -A a=(); echo ${a[0]} ${a[@]} ${a[1]} ${a["x"]}`,
		"\n",
	},
	{
		`declare -A a=([x]=b [y]=c); echo $a; echo ${a[0]}; echo ${a["x"]}; echo ${a["_"]}`,
		"\n\nb\n\n",
	},
	{
		`declare -Ag a=([x]=y); echo ${a["x"]}`,
		"y\n",
	},
	{
		`declare -A a=([x]=b [y]=c); for e in ${a[@]}; do echo $e; done | sort`,
		"b\nc\n",
	},
	{
		`declare -A a=([y]=b [x]=c); for e in ${a[*]}; do echo $e; done | sort`,
		"b\nc\n",
	},
	{
		`declare -A a=([x]=a); a["y"]=d; a["x"]=c; for e in ${a[@]}; do echo $e; done | sort`,
		"c\nd\n",
	},
	{
		`declare -A a=([x]=a); a[y]=d; a[x]=c; for e in ${a[@]}; do echo $e; done | sort`,
		"c\nd\n",
	},
	{
		// cheating a little; bash just did a=c
		`a=(["x"]=b ["y"]=c); echo ${a["y"]}`,
		"c\n",
	},
	{
		`declare -A a=(['x']=b); echo ${a['x']} ${a[$'x']} ${a[$"x"]}`,
		"b b b\n",
	},
	{
		// bash 5.1+: bare words pair up as key/value.
		`declare -A a=(one 1 two 2); for e in "${!a[@]}"; do echo "$e=${a[$e]}"; done | sort`,
		"one=1\ntwo=2\n",
	},
	{
		// An odd word out keys the empty string.
		`declare -A a=(one 1 two); echo "${a[one]}-${a[two]}."`,
		"1-.\n",
	},
	{
		`declare -A a=([k]=v); a+=(b c d e); for e in "${!a[@]}"; do echo "$e=${a[$e]}"; done | sort`,
		"b=c\nd=e\nk=v\n",
	},
	{
		`declare -A a=(one 1); a+=([one]=x [two]=y); echo ${a[one]} ${a[two]}`,
		"x y\n",
	},
	{
		`declare -A a=(one 1); a=(two 2); echo "${a[one]}${a[two]}"`,
		"2\n",
	},
	{
		// A bare word after a subscripted element is a fatal assignment error.
		`declare -A a=([k]=v one 1); echo after`,
		"a: one: must use subscript when assigning associative array\nexit status 1 #JUSTERR",
	},
	{
		// An empty key is skipped with a complaint; the rest still lands.
		`declare -A a=("" e one 1); echo st=$?; echo ${a[one]}`,
		"'': bad array subscript\nst=0\n1\n #IGNORE bash prints the error but continues identically",
	},
	{
		`a=(['x']=b); echo ${a['y']}`,
		"\n #IGNORE bash requires -A",
	},
	{
		`declare -A a=(['a  1']=' x ' ['b  2']=' y '); for v in "${a[@]}"; do echo "$v"; done | sort`,
		" x \n y \n",
	},
	{
		`declare -A a=(['a  1']=' x ' ['b  2']=' y '); for v in "${a[*]}"; do echo "$v"; done`,
		" x   y \n",
	},
	{
		`declare -A a=(['a  1']=' x ' ['b  2']=' y '); for v in "${!a[@]}"; do echo "$v"; done | sort`,
		"a  1\nb  2\n",
	},
	{
		`declare -A a=(['a  1']=' x ' ['b  2']=' y '); for v in "${!a[*]}"; do echo "$v"; done`,
		"a  1 b  2\n",
	},
	{
		`declare -A a; a[a]=x; a[b]=y; for v in "${!a[@]}"; do echo "$v"; done | sort`,
		"a\nb\n",
	},
	{
		`declare -A a; a[a]=x; a[b]=y; declare -A a; for v in "${!a[@]}"; do echo "$v"; done | sort`,
		"a\nb\n",
	},
	// weird assignments
	{"a=b; a=(c d); echo ${a[@]}", "c d\n"},
	{"a=(b c); a=d; echo ${a[@]}", "d c\n"},
	{"declare -A a=([x]=b [y]=c); a=d; for e in ${a[@]}; do echo $e; done | sort", "b\nc\nd\n"},
	{"i=3; a=b; a[i]=x; echo ${a[@]}", "b x\n"},
	{"i=3; declare a=(b); a[i]=x; echo ${!a[@]}", "0 3\n"},
	{`a=(x "" y); echo ${!a[@]}; echo "${!a[@]}"`, "0 1 2\n0 1 2\n"},
	{"a=(0 1 2 3 4 5 6 7 8 9 10); echo ${!a[@]}", "0 1 2 3 4 5 6 7 8 9 10\n"},
	{"i=3; declare -A a=(['x']=b); a[i]=x; for e in ${!a[@]}; do echo $e; done | sort", "i\nx\n"},

	// sparse indexed arrays
	{"a[5]=x; echo ${#a[@]} ${a[@]} ${!a[@]}", "1 x 5\n"},
	{"a=([5]=x [2]=y); echo ${!a[@]}; echo ${a[@]}", "2 5\ny x\n"},
	{"a=([5]=x y z); echo ${!a[@]}", "5 6 7\n"},
	{"a[5]=x; a[2]=y; declare -p a", "declare -a a=([2]=\"y\" [5]=\"x\")\n"},
	{"a[5]=x; echo ${a[0]-unset} ${a[5]}; echo \"${a[@]}\"", "unset x\nx\n"},
	{"a=(x y z); unset 'a[1]'; echo ${#a[@]} ${!a[@]} ${a[@]}", "2 0 2 x z\n"},
	{"a=(x y z); unset 'a[2]'; echo ${#a[@]} ${!a[@]} ${a[@]}", "2 0 1 x y\n"},
	{"a=(x y z); unset 'a[1]'; a+=(w); echo ${!a[@]}", "0 2 3\n"},
	{"a=(x y z); unset 'a[@]'; echo ${#a[@]}", "0\n"},
	{"a=(w x y z); i=2; unset \"a[i+1]\"; echo ${!a[@]}", "0 1 2\n"},
	{"declare -A a=([x]=1 [y]=2); unset 'a[x]'; echo ${!a[@]}", "y\n"},
	{"a=(1 2 3); a[-1]=x; echo ${a[@]}", "1 2 x\n"},
	{"a=(x); a+=([5]=z w); echo ${!a[@]}; echo ${a[@]}", "0 5 6\nx z w\n"},
	{"a=s; a+=([0]=x); echo ${a[@]}", "x\n"},
	{"a=([5]=x); a+=s; echo ${!a[@]}; echo ${a[@]}", "0 5\ns x\n"},
	{"a=([1]=one [5]=five [10]=ten); echo ${a[@]:2:2}; echo ${a[@]:5}; echo ${a[@]: -1}", "five ten\nfive ten\nten\n"},
	{"a=([2]=x [5]=y); echo \"${a[@]::1}\" \"${a[@]:0}\"", "x x y\n"},
	{"a=([2]=x [5]=y); echo $a ${a[0]-unset}", "unset\n"},
	{"a=([0]=x [5]=y); echo $a", "x\n"},
	{"a=([5]=x); echo ${a+set} ${a-unset}", "unset\n"},
	{"a=(x y); : \"${a[5]=z}\"; declare -p a", "declare -a a=([0]=\"x\" [1]=\"y\" [5]=\"z\")\n"},
	{"s=x; : \"${s[1]=z}\"; declare -p s", "declare -a s=([0]=\"x\" [1]=\"z\")\n"},
	{"declare -A m=([k]=v); : \"${m[j]=z}\"; echo ${m[j]} ${m[k]}", "z v\n"},
	{"a=([5]=b [-1]=c d); declare -p a", "declare -a a=([5]=\"c\" [6]=\"d\")\n"},
	{"a=(1 2 3); echo ${a[-1]} ${a[-3]}", "3 1\n"},
	{"a=(x); unset 'a[]'; echo $?; declare -p a", "0\ndeclare -a a=([0]=\"x\")\n"},
	{"s=x; unset 's[0]'; echo ${s-unset}", "unset\n"},
	{"s=x; unset 's[5]'; echo $s", "unset: s: not an array variable\nx\n #JUSTERR"},
	{"a=([5]=x); (a[2]=y; echo ${!a[@]}); echo ${!a[@]}", "2 5\n5\n"},
	{"a=([1]=y [0]=x); declare -p a", "declare -a a=([0]=\"x\" [1]=\"y\")\n"},
	{"declare -n r=a; a=(1 2 3); unset 'r[1]'; echo ${!a[@]}", "0 2\n"},

	// declare
	{"declare -B foo", "declare: invalid option \"-B\"\nexit status 2 #JUSTERR"},
	{"a=b; declare a; echo $a; declare a=; echo $a", "b\n\n"},
	{"a=b; declare a; echo $a", "b\n"},
	{
		"declare a=b c=(1 2); echo $a; echo ${c[@]}",
		"b\n1 2\n",
	},
	{"a=x; declare $a; echo $a $x", "x\n"},
	{"a=x=y; declare $a; echo $a $x", "x=y y\n"},
	{"a='x=(y)'; declare $a; echo $a $x", "x=(y) (y)\n"},
	{"a='x=b y=c'; declare $a; echo $x $y", "b c\n"},
	{"declare =bar", "declare: invalid name \"\"\nexit status 1 #JUSTERR"},
	{"declare $unset=$unset", "declare: invalid name \"\"\nexit status 1 #JUSTERR"},

	// export
	{"declare foo=bar; $ENV_PROG | grep '^foo='", "exit status 1"},
	{"declare -x foo=bar; $ENV_PROG | grep '^foo='", "foo=bar\n"},
	{"export foo=bar; $ENV_PROG | grep '^foo='", "foo=bar\n"},
	{"foo=bar; export foo; $ENV_PROG | grep '^foo='", "foo=bar\n"},
	{"export foo=bar; foo=baz; $ENV_PROG | grep '^foo='", "foo=baz\n"},
	{"export foo=bar; readonly foo=baz; $ENV_PROG | grep '^foo='", "foo=baz\n"},
	{"export foo=(1 2); $ENV_PROG | grep '^foo='", "exit status 1"},
	{"declare -A foo=([a]=b); export foo; $ENV_PROG | grep '^foo='", "exit status 1"},
	{"export foo=(b c); foo=x; $ENV_PROG | grep '^foo='", "exit status 1"},
	{"foo() { bar=foo; export bar; }; foo; $ENV_PROG | grep ^bar=", "bar=foo\n"},
	{"foo() { export bar; }; bar=foo; foo; $ENV_PROG | grep ^bar=", "bar=foo\n"},
	{"foo() { export bar; }; foo; bar=foo; $ENV_PROG | grep ^bar=", "bar=foo\n"},
	{"foo() { export bar=foo; }; foo; readonly bar; $ENV_PROG | grep ^bar=", "bar=foo\n"},

	// local
	{
		"local a=b",
		"local: can only be used in a function\nexit status 1 #JUSTERR",
	},
	{
		"local a=b 2>/dev/null; echo $a",
		"\n",
	},
	{
		"{ local a=b; }",
		"local: can only be used in a function\nexit status 1 #JUSTERR",
	},
	{
		"echo 'local a=b' >a; source ./a",
		"local: can only be used in a function\nexit status 1 #JUSTERR",
	},
	{
		"echo 'local a=b' >a; f() { source ./a; }; f; echo $a",
		"\n",
	},
	{
		"f() { local a=b; }; f; echo $a",
		"\n",
	},
	{
		"a=x; f() { local a=b; }; f; echo $a",
		"x\n",
	},
	{
		"a=x; f() { echo $a; local a=b; echo $a; }; f",
		"x\nb\n",
	},
	{
		"f1() { local a=b; }; f2() { f1; echo $a; }; f2",
		"\n",
	},
	{
		"f() { a=1; declare b=2; export c=3; readonly d=4; declare -g e=5; }; f; echo $a $b $c $d $e",
		"1 3 4 5\n",
	},
	{
		`f() { local x; [[ -v x ]] && echo set || echo unset; }; f`,
		"unset\n",
	},
	{
		`f() { local x=; [[ -v x ]] && echo set || echo unset; }; f`,
		"set\n",
	},
	{
		`export x=before; f() { local x; export x=after; $ENV_PROG | grep '^x='; }; f; echo $x`,
		"x=after\nbefore\n",
	},
	{
		"getx() { echo $X; }; f() { local X=Y; getx; echo $X; }; f",
		"Y\nY\n",
	},
	{
		"setx() { X=Y; }; f() { local X; setx; echo $X; }; f",
		"Y\n",
	},
	{
		"setx() { local X=Y; }; f() { local X; setx; echo $X; }; f",
		"\n",
	},
	{
		"setx() { declare X=Y; }; f() { local X; setx; echo $X; }; f",
		"\n",
	},
	{
		"setx() { X=Y :; }; f() { local X; setx; echo $X; }; f",
		"\n",
	},

	// unset global from inside function
	{"f() { unset foo; echo $foo; }; foo=bar; f", "\n"},
	{"f() { unset foo; }; foo=bar; f; echo $foo", "\n"},

	// name references
	// Writes through a nameref update the *target* (#277): before the
	// assignment path resolved prev, an indexed write started from the
	// nameref's own empty value and replaced the whole target array with
	// one element, and += appended to nothing.
	{`a=(1 3 5 7 9); declare -n r=a; r[2]=42; echo "${a[@]}"`, "1 3 42 7 9\n"},
	{`a=hello; declare -n r=a; r+=X; echo "$a"`, "helloX\n"},
	{`a=(1 2); declare -n r=a; r+=(3); echo "${a[@]}"`, "1 2 3\n"},
	{`declare -A m=([k]=v); declare -n r=m; r[j]=w; echo "${m[j]}"`, "w\n"},
	{`a=(1 2 3); declare -n r=a; unset "r[1]"; echo "${a[@]}"`, "1 3\n"},
	{`declare -n r=newvar; r=5; echo "$newvar"`, "5\n"},
	{"declare -n foo=bar; bar=etc; [[ -R foo ]]", ""},
	{"declare -n foo=bar; bar=etc; [ -R foo ]", ""},
	{"nameref foo=bar; bar=etc; [[ -R foo ]]", " #IGNORE"},
	{"declare foo=bar; bar=etc; [[ -R foo ]]", "exit status 1"},
	{
		"declare -n foo=bar; bar=etc; echo $foo; bar=zzz; echo $foo",
		"etc\nzzz\n",
	},
	{
		"declare -n foo=bar; bar=(x y); echo ${foo[1]}; bar=(a b); echo ${foo[1]}",
		"y\nb\n",
	},
	{
		"declare -n foo=bar; bar=etc; echo $foo; unset bar; echo $foo",
		"etc\n\n",
	},
	{
		"declare -n a1=a2 a2=a3 a3=a4; a4=x; echo $a1 $a3",
		"x x\n",
	},
	{
		"declare -n foo=bar bar=foo; echo $foo",
		"\n #IGNORE",
	},
	{
		"declare -n foo=bar; echo $foo",
		"\n",
	},
	{
		"declare -n foo=bar; echo ${!foo}",
		"bar\n",
	},
	{
		"declare -n foo=bar; bar=etc; echo $foo; echo ${!foo}",
		"etc\nbar\n",
	},
	{
		"declare -n foo=bar; bar=etc; foo=value; echo $foo; echo $bar",
		"value\nvalue\n",
	},
	{
		"declare -n foo=bar; foo=value; echo $foo; echo $bar",
		"value\nvalue\n",
	},
	{
		"declare -n foo=bar; declare foo=value; echo $foo; echo $bar",
		"value\nvalue\n",
	},
	{
		"declare -n foo=bar bar=baz; foo=value; echo $foo; echo $bar; echo $baz",
		"value\nvalue\nvalue\n",
	},
	{
		"declare -n foo=bar; set -u; echo ${foo}",
		"foo: unbound variable\nexit status 1 #JUSTERR",
	},
	{
		"declare -n foo=bar; set -u; echo ${foo:=value}; echo $foo; echo $bar",
		"value\nvalue\nvalue\n",
	},
	{
		"declare -n foo=bar; foo=value $ENV_PROG | grep '^bar='",
		"bar=value\n",
	},
	{
		"echo ${!@}-${!*}; set -- foo; echo ${!@}-${!*}-${!1}; foo=value; echo ${!@}-${!*}-${!1}",
		"-\n--\nvalue-value-value\n",
	},
	{
		"declare -n ref=arr; ref+=(x y); echo ${ref[@]} ${arr[@]}",
		"x y x y\n",
	},

	// read-only vars
	{"declare -r foo=bar; echo $foo", "bar\n"},
	{"readonly foo=bar; echo $foo", "bar\n"},
	{
		// a plain assignment to a readonly variable is fatal in bash, so
		// nothing after it runs
		`readonly v=1; v=2; echo REACHED`,
		"v: readonly variable\nexit status 1 #JUSTERR",
	},
	{
		`readonly v=1; v+=2; echo REACHED`,
		"v: readonly variable\nexit status 1 #JUSTERR",
	},
	{
		// including from inside a function, which ends the whole script
		`f() { v=2; echo IN; }; readonly v=1; f; echo OUT`,
		"v: readonly variable\nexit status 1 #JUSTERR",
	},
	{
		// in a subshell it ends the subshell only
		`readonly v=1; ( v=2; echo INSUB ) 2>/dev/null; echo OUT`,
		"OUT\n",
	},
	{
		// the same error from a command prefix is not fatal, and is reported
		// once rather than again while restoring
		`readonly v=1; { v=2 true; } 2>/dev/null; echo REACHED`,
		"REACHED\n",
	},
	{
		`readonly v=1; export v=2 2>/dev/null; echo REACHED`,
		"REACHED\n",
	},
	{
		`readonly v=1; echo "[$v]"`,
		"[1]\n",
	},
	{"readonly foo=bar; export foo; echo $foo", "bar\n"},
	{"readonly foo=bar; readonly bar=foo; export foo bar; echo $bar", "foo\n"},
	{
		"a=b; a=c; echo $a; readonly a; a=d",
		"c\na: readonly variable\nexit status 1 #JUSTERR",
	},
	{
		"declare -r foo=bar; foo=etc",
		"foo: readonly variable\nexit status 1 #JUSTERR",
	},
	{
		"declare -r foo=bar; export foo=",
		"foo: readonly variable\nexit status 1 #JUSTERR",
	},
	{
		"readonly foo=bar; foo=etc",
		"foo: readonly variable\nexit status 1 #JUSTERR",
	},
	{
		"foo() { bar=foo; readonly bar; }; foo; bar=bar",
		"bar: readonly variable\nexit status 1 #JUSTERR",
	},
	{
		"foo() { readonly bar; }; foo; bar=foo",
		"bar: readonly variable\nexit status 1 #JUSTERR",
	},
	{
		"foo() { readonly bar=foo; }; foo; export bar; $ENV_PROG | grep '^bar='",
		"bar=foo\n",
	},

	// multiple var modes at once
	{
		"declare -r -x foo=bar; $ENV_PROG | grep '^foo='",
		"foo=bar\n",
	},
	{
		"declare -r -x foo=bar; foo=x",
		"foo: readonly variable\nexit status 1 #JUSTERR",
	},

	// globbing
	{"echo .", ".\n"},
	{"echo ..", "..\n"},
	{"echo ./.", "./.\n"},
	{
		">a.x >b.x >c.x; echo *.x; rm a.x b.x c.x",
		"a.x b.x c.x\n",
	},
	{
		`>a.x; echo '*.x' "*.x"; rm a.x`,
		"*.x *.x\n",
	},
	{
		`>a.x >b.y; echo *'.'x; rm a.x`,
		"a.x\n",
	},
	{
		`>a.x; echo *'.x' "a."* '*'.x; rm a.x`,
		"a.x a.x *.x\n",
	},
	{
		"echo *.x; echo foo *.y bar",
		"*.x\nfoo *.y bar\n",
	},
	{
		`>a.x >b.x >c.x; a=*.x; echo $a; echo "$a"`,
		"a.x b.x c.x\n*.x\n",
	},
	{
		`>a.x >b.x >c.x; a=(*.x); echo "${a[@]}"; echo ${a[1]}`,
		"a.x b.x c.x\nb.x\n",
	},
	{
		"mkdir a; >a/b.x; echo */*.x | sed 's@\\\\@/@g'; cd a; echo *.x",
		"a/b.x\nb.x\n",
	},
	{
		"mkdir -p a/b/c; echo a/* | sed 's@\\\\@/@g'",
		"a/b\n",
	},
	{
		">.hidden >a; echo *; echo .h*; rm .hidden a",
		"a\n.hidden\n",
	},
	{
		`mkdir d; >d/.hidden >d/a; set -- "$(echo d/*)" "$(echo d/.h*)"; echo ${#1} ${#2}; rm -r d`,
		"3 9\n",
	},
	{
		"mkdir -p a/b/c; echo a/** | sed 's@\\\\@/@g'",
		"a/b\n",
	},
	{
		"shopt -s globstar; mkdir -p a/b/c; echo a/** | sed 's@\\\\@/@g'",
		"a/ a/b a/b/c\n",
	},
	{
		"shopt -s globstar; mkdir -p a/b/c; echo **/c | sed 's@\\\\@/@g'",
		"a/b/c\n",
	},
	{
		"shopt -s globstar; mkdir -p a/b; touch c; echo ** | sed 's@\\\\@/@g'",
		"a a/b c\n",
	},
	{
		"shopt -s globstar; mkdir -p a/b; touch c; echo **/ | sed 's@\\\\@/@g'",
		"a/ a/b/\n",
	},
	{
		"shopt -s globstar; mkdir -p a/b/c a/d; echo ** | sed 's@\\\\@/@g'",
		"a a/b a/b/c a/d\n",
	},
	{
		"shopt -s globstar; mkdir -p a.x a/b.x a/b/c.x; echo **.x ./**.x | sed 's@\\\\@/@g'",
		"a.x ./a.x\n",
	},
	{
		"shopt -s globstar; mkdir -p a/b; touch a/b/c; echo **/* | sed 's@\\\\@/@g'",
		"a a/b a/b/c\n",
	},
	{
		"shopt -s globstar; mkdir -p b; touch x2 a b/c d x1; echo **/* | sed 's@\\\\@/@g'",
		"a b b/c d x1 x2\n",
	},
	{
		"mkdir foo; touch foo/bar; echo */bar */bar/ | sed 's@\\\\@/@g'",
		"foo/bar */bar/\n",
	},
	{
		"shopt -s nullglob; touch existing-1; echo missing-* existing-*",
		"existing-1\n",
	},
	{
		"touch ŀfoo; echo ŀ*",
		"ŀfoo\n",
	},

	// failglob aborts the input unit on a matchless pattern (#375): the
	// -c string loses its remainder and exits 1, and it outranks
	// nullglob; an invalid pattern stays a literal word instead.
	{
		"shopt -s failglob; echo missing-*; echo never",
		"no match: missing-*\nexit status 1 #JUSTERR",
	},
	{
		"shopt -s nullglob failglob; echo missing-* end; echo never",
		"no match: missing-*\nexit status 1 #JUSTERR",
	},
	{
		"shopt -s failglob; echo [x; echo after",
		"[x\nafter\n",
	},
	// GLOBIGNORE filters glob results and implies dotglob while set
	// (#375); patterns match the produced path string verbatim, so a
	// basename ignore does not reach into subdirectories.
	{
		"touch a.h a.c; GLOBIGNORE='*.h'; echo *",
		"a.c\n",
	},
	// Assigning a non-null GLOBIGNORE turns the real dotglob option on
	// — shopt reports it and shopt -u undoes it — while unsetting
	// GLOBIGNORE turns dotglob off even when it was set by hand.
	{
		"touch .h a.c; GLOBIGNORE=zz; echo *; unset GLOBIGNORE; echo *",
		".h a.c\na.c\n",
	},
	{
		"touch .h a.c; GLOBIGNORE=zz; shopt -u dotglob; echo *",
		"a.c\n",
	},
	{
		"touch .h a.c; GLOBIGNORE=zz; shopt dotglob | sed 's/[\t ][\t ]*/ /g'",
		"dotglob on\n",
	},
	{
		"touch .h a.c; shopt -s dotglob; unset GLOBIGNORE; echo *",
		"a.c\n",
	},
	{
		"mkdir d; touch d/b.h; GLOBIGNORE='*.h'; echo d/* | sed 's@\\\\@/@g'",
		"d/b.h\n",
	},
	{
		"mkdir d; touch d/b.h; GLOBIGNORE='*/*.h'; echo d/* | sed 's@\\\\@/@g'",
		"d/*\n",
	},
	// GLOBSORT reorders glob results (#375): - reverses, ties fall back
	// to name order inside the reversal, whole-string numbers sort
	// numerically ahead of everything else, and an unrecognized key —
	// its sign included — is a plain forward name sort.
	{
		"touch ga gb gc; GLOBSORT=-name; echo g*",
		"gc gb ga\n",
	},
	{
		"printf x >s1; printf xxx >s2; printf xx >s3; GLOBSORT=size; echo s*; GLOBSORT=-size; echo s*",
		"s1 s3 s2\ns2 s3 s1\n",
	},
	{
		"touch ga gb; GLOBSORT=-nonsense; echo g*",
		"ga gb\n",
	},
	{
		"touch 10 9 2x; GLOBSORT=numeric; echo *; GLOBSORT=-numeric; echo *",
		"9 10 2x\n2x 10 9\n",
	},
	{
		"touch za zb zc; GLOBSORT=size; echo z?; GLOBSORT=-size; echo z?",
		"za zb zc\nzc zb za\n",
	},
	// A leading dot is only matched by a literal dot (#376): a bracket
	// class never matches it, dotglob and GLOBIGNORE lift that.
	{
		"touch .h b; echo [!a]*",
		"b\n",
	},
	{
		"touch .h b; shopt -s dotglob; echo [!a]*",
		".h b\n",
	},
	{
		"touch .ha; echo .[gh]*",
		".ha\n",
	},
	// A bracket expression broken by an unescaped slash makes the word
	// not a pattern at all (#376, POSIX 2.13.3): it prints literally
	// even under nullglob, where an escaped slash keeps the bracket
	// valid — and unmatchable.
	{
		"shopt -s nullglob; echo [q/w] end",
		"[q/w] end\n",
	},
	{
		"shopt -s nullglob; echo [q\\/w] end",
		"end\n",
	},
	// An extglob group is a pattern even with no *?[ in sight (#375).
	{
		"shopt -s extglob\ntouch ea eb; echo @(ea|zz); echo +(e)b",
		"ea\neb\n",
	},

	// Extended globbing via the extglob option.
	// Note how extglob affects Bash's own line-by-line parsing, so we set the option before a newline.
	{
		"shopt -s extglob\necho invalid-?([)",
		"invalid-?([)\n",
	},
	{
		"touch az a1z a12z a123z; echo a?([0-9])z",
		"extended globbing operator used without the \"extglob\" option set\n #JUSTERR",
	},
	{
		"shopt -s extglob\ntouch az a1z a12z a123z; echo a?([0-9])z",
		"a1z az\n",
	},
	{
		"shopt -s extglob\ntouch az a1z a12z a123z; echo a*([0-9])z",
		"a123z a12z a1z az\n",
	},
	{
		"shopt -s extglob\ntouch az a1z a12z a123z; echo a+([0-9])z",
		"a123z a12z a1z\n",
	},
	{
		"shopt -s extglob\ntouch az a1z a12z a123z; echo a@([0-9])z",
		"a1z\n",
	},
	{
		"shopt -s extglob\ntouch a{1..9}0z; echo a+(0|[1-2]|8)z",
		"a10z a20z a80z\n",
	},
	{
		"shopt -s extglob\ntouch az a1z a12z a123z; echo a!([0-9])z",
		"a123z a12z az\n",
	},
	// !(pattern) extglob negation in case and [[ ]] matching
	{
		"shopt -s extglob\ncase \"bar\" in !(foo)) echo match;; esac",
		"match\n",
	},
	{
		"shopt -s extglob\ncase \"foo\" in !(foo)) echo match;; esac",
		"",
	},
	{
		"shopt -s extglob\ncase \"\" in !(foo)) echo match;; esac",
		"match\n",
	},
	{
		"shopt -s extglob\ncase \"baz\" in !(foo|bar)) echo match;; esac",
		"match\n",
	},
	{
		"shopt -s extglob\ncase \"file.tar.gz\" in !(*.sig)) echo match;; esac",
		"match\n",
	},
	{
		"shopt -s extglob\ncase \"file.sig\" in !(*.sig)) echo match;; esac",
		"",
	},
	{
		"shopt -s extglob\ncase \"foo_xxx_baz\" in foo_!(bar)_baz) echo match;; esac",
		"match\n",
	},
	{
		"shopt -s extglob\ncase \"foo_bar_baz\" in foo_!(bar)_baz) echo match;; esac",
		"",
	},
	{
		"shopt -s extglob\n[[ \"bar\" == !(foo) ]] && echo match",
		"match\n",
	},
	// !(...) composed with prefixes, suffixes, and other groups (#373):
	// the backtracking matcher handles what a lookahead-free regexp
	// cannot.
	{
		"shopt -s extglob\ncase \"xabab\" in *a!(b)) echo match;; esac",
		"match\n",
	},
	{
		"shopt -s extglob\ncase \"baz\" in !(foo)!(bar)) echo match;; esac",
		"match\n",
	},
	{
		"shopt -s extglob\ncase \".bar\" in .*!(foo)) echo match;; esac",
		"match\n",
	},
	{
		"shopt -s extglob\ncase \".foo\" in .*!(foo)) echo match;; esac",
		"match\n",
	},
	{
		"shopt -s extglob\ncase \"bar\" in .*!(foo)) echo match;; esac",
		"",
	},
	{"shopt -s extglob\n[[ foo = !(x)* ]]; echo $?", "0\n"},
	{"shopt -s extglob\n[[ fff = *(!(f)) ]]; echo $?", "0\n"},
	{"shopt -s extglob\n[[ a.b = !(*.*).!(*.*) ]]; echo $?", "0\n"},
	{"shopt -s extglob\n[[ a.b.c = !(*.*).!(*.*) ]]; echo $?", "1\n"},
	{"shopt -s extglob\n[[ foo = +(!(f)o) ]]; echo $?", "0\n"},
	// An invalid pattern compares as its literal self.
	{`shopt -s extglob
[[ "+(a|b[" == "+(a|b[" ]] && echo eq
case "+(a|b[" in "+(a|b[") echo m;; esac`, "eq\nm\n"},
	{
		// Extended pattern matching is always available outside of pathname expansions (globbing).
		"[[ a123z == a@([0-9])z ]]; echo $?; [[ a123z == a+([0-9])z ]]; echo $?",
		"1\n0\n",
	},
	// Ensure that setting nullglob does not return invalid globs as null
	// strings.
	{
		"shopt -s nullglob; [ -n butter ] && echo bubbles",
		"bubbles\n",
	},
	{
		"cat <<EOF\n{foo,bar}\nEOF",
		"{foo,bar}\n",
	},
	{
		"cat <<EOF\n*.go\nEOF",
		"*.go\n",
	},
	{
		"mkdir -p a/b a/c; echo ./a/* | sed 's@\\\\@/@g'",
		"./a/b ./a/c\n",
	},
	{
		"mkdir -p a/b a/c d; cd d; echo ../a/* | sed 's@\\\\@/@g'",
		"../a/b ../a/c\n",
	},
	{
		"mkdir x-d1 x-d2; >x-f; echo x-*/ | sed 's@\\\\@/@g'",
		"x-d1/ x-d2/\n",
	},
	{
		"mkdir x-d1 x-d2; >x-f; echo ././x-*/// | sed 's@\\\\@/@g'",
		"././x-d1/ ././x-d2/\n",
	},
	{
		"mkdir -p x-d1/a x-d2/b; >x-f; echo x-*/* | sed 's@\\\\@/@g'",
		"x-d1/a x-d2/b\n",
	},
	{
		"mkdir -p foo/bar; ln -s foo sym; echo sy*/; echo sym/b*",
		"sym/\nsym/bar\n",
	},
	{
		">foo; ln -s foo sym; echo sy*; echo sy*/",
		"sym\nsy*/\n",
	},
	{
		"mkdir x-d; >x-f; test -d $PWD/x-*/",
		"",
	},
	{
		"mkdir dir; >dir/x-f; ln -s dir sym; cd sym; test -f $PWD/x-*",
		"",
	},

	// brace expansion; there are also some tests in the expand package
	// Braces expand before parameters, textually (#363): a brace suffix
	// on a short-form $var extends the variable's name, while ${var}
	// keeps its boundary and $1 was never a name to extend.
	{"var=baz varx=vx vary=vy; echo $var{x,y}", "vx vy\n"},
	{"var=baz; echo ${var}{x,y}", "bazx bazy\n"},
	{"a1=one a2=two; echo $a{1,2}", "one two\n"},
	{"set -- p; echo $1{x,y}", "px py\n"},
	{"vx_q=deep; v=top; echo $v{x_q,}", "deep top\n"},
	{"echo a}b", "a}b\n"},
	{"echo {a,b{c,d}", "{a,bc {a,bd\n"},
	{"echo a{b}", "a{b}\n"},
	{"echo a{à,世界}", "aà a世界\n"},
	{"echo a{b,c}d{e,f}g", "abdeg abdfg acdeg acdfg\n"},
	{"echo a{b{x,y},c}d", "abxd abyd acd\n"},
	{"echo a{1..", "a{1..\n"},
	{
		"echo {00..2}; echo {01..10}; echo {1..10..-2}; echo {10..1..2}; echo {-03..3}",
		"00 01 02\n01 02 03 04 05 06 07 08 09 10\n1 3 5 7 9\n10 8 6 4 2\n-03 -02 -01 000 001 002 003\n",
	},
	{"echo a{1..2}b{4..5}c", "a1b4c a1b5c a2b4c a2b5c\n"},
	{"echo a{c..f}", "ac ad ae af\n"},
	{"echo a{4..1..1}", "a4 a3 a2 a1\n"},
	{"b=c; echo ${b}a{4..1..1}", "ca4 ca3 ca2 ca1\n"},
	{"b=c; echo a{1,2}$b", "a1c a2c\n"},
	{"echo a{1,2}'bc'", "a1bc a2bc\n"},
	{`echo a\{1,2}b`, "a{1,2}b\n"},
	{`echo a{1,2\`, "a{1,2\\\n"},
	{`echo a{1,2\}b`, "a{1,2}b\n"},
	{`echo a{1\,2,3}b`, "a1,2b a3b\n"},
	{`echo a{1\}2,3}b`, "a1}2b a3b\n"},
	{`echo a{1\..2}b`, "a{1..2}b\n"},
	{`echo \{\{iriname\}\}`, "{{iriname}}\n"},
	{
		"echo {1..100000}",
		"brace expansion would exceed 16384 elements\n #IGNORE bash has no defensive limit below MaxInt",
	},
	{
		"echo a{0..9999999999}b",
		"brace expansion would exceed 16384 elements\n #JUSTERR bash errors with a different message",
	},

	// brace expansion in declarations
	{"declare {A,B}_VAR=1; echo $A_VAR $B_VAR", "1 1\n"},
	{"declare {x,y}=val; echo $x $y", "val val\n"},
	{"declare -x RUN_{VERY_,}EXPENSIVE_TESTS=yes; echo $RUN_EXPENSIVE_TESTS", "yes\n"},
	{"declare {A,B}_VAR; A_VAR=1; B_VAR=2; echo $A_VAR $B_VAR", "1 2\n"},
	{"declare {foo=x,bar=y}; echo $foo $bar", "x y\n"},
	{`declare foo{bar=baz`, "declare: invalid name \"foo{bar\"\nexit status 1 #JUSTERR"},
	{"{a,b}=value", "\"a=value\": executable file not found in $PATH\nexit status 127 #JUSTERR"},

	// tilde expansion
	{
		"[[ '~/foo' == ~/foo ]] || [[ ~/foo == '~/foo' ]]",
		"exit status 1",
	},
	{
		"case '~/foo' in ~/foo) echo match ;; esac",
		"",
	},
	{
		"a=~/foo; [[ $a == '~/foo' ]]",
		"exit status 1",
	},
	{
		`a=$(echo "~/foo"); [[ $a == '~/foo' ]]`,
		"",
	},
	{
		`HOME=/foo; rel=/bar; echo ~/bar ~/'bar' ~/"bar" ~/$rel ~/"$rel"`,
		"/foo/bar /foo/bar /foo/bar /foo//bar /foo//bar\n",
	},
	{
		`HOME=/foo; rel=/bar; echo ~'/bar' ~"/bar" ~$rel ~"/$rel"`,
		"~/bar ~/bar ~/bar ~//bar\n",
	},
	{
		`HOME=/foo; echo ~ ~/ ~/'' ~'' ~""`,
		"/foo /foo/ /foo/ ~ ~\n",
	},

	// /dev/null
	{"echo foo >/dev/null", ""},
	{"cat </dev/null", ""},

	// time - real would be slow and flaky; see TestElapsedString
	{"{ time; } |& wc | tr -s ' '", " 4 6 42\n"},
	{"{ time echo -n; } |& wc | tr -s ' '", " 4 6 42\n"},
	{"{ time -p; } |& wc | tr -s ' '", " 3 6 29\n"},
	{"{ time -p echo -n; } |& wc | tr -s ' '", " 3 6 29\n"},

	// exec
	{"exec", ""},
	{
		"exec builtin echo foo",
		"\"builtin\": executable file not found in $PATH\nexit status 127 #JUSTERR",
	},
	{
		"exec $GOSH_PROG 'echo foo'; echo bar",
		"foo\n",
	},
	{
		"exec -a",
		"exec: -a: option requires an argument\nexit status 2 #JUSTERR",
	},
	{
		"exec -q foo",
		"exec: invalid option \"-q\"\nexit status 2 #JUSTERR",
	},
	{
		// Flags with no command to apply to still keep this statement's
		// redirections open, as bare "exec" does.
		"exec -a name >/dev/null; echo foo",
		"",
	},

	// read
	{
		"read </dev/null",
		"exit status 1",
	},
	{
		"read 1</dev/null",
		"exit status 1",
	},
	{
		"read -X",
		"read: invalid option \"-X\"\nexit status 2 #JUSTERR",
	},
	{
		"read -rX",
		"read: invalid option \"-X\"\nexit status 2 #JUSTERR",
	},
	{
		"read 0ab",
		"read: invalid identifier \"0ab\"\nexit status 2 #JUSTERR",
	},
	{
		"read <<< foo; echo $REPLY",
		"foo\n",
	},
	{
		"read <<<'  a  b  c  '; echo \"$REPLY\"",
		"  a  b  c  \n",
	},
	{
		"read <<< 'y\nn\n'; echo $REPLY",
		"y\n",
	},
	{
		"read a_0 <<< foo; echo $a_0",
		"foo\n",
	},
	{
		"read a b <<< 'foo  bar  baz  '; echo \"$a\"; echo \"$b\"",
		"foo\nbar  baz\n",
	},
	{
		"while read a; do echo $a; done <<< 'a\nb\nc'",
		"a\nb\nc\n",
	},
	{
		"while read a b; do echo -e \"$a\n$b\"; done <<< '1 2\n3'",
		"1\n2\n3\n\n",
	},
	{
		`read a <<< '\\'; echo "$a"`,
		"\\\n",
	},
	{
		`read a <<< '\a\b\c'; echo "$a"`,
		"abc\n",
	},
	{
		"read -r a b <<< '1\\\t2'; echo $a; echo $b;",
		"1\\\n2\n",
	},
	{
		"echo line\\\ncontinuation | while read a; do echo $a; done",
		"linecontinuation\n",
	},
	{
		"read x <<< $'foo\\\\\nbar'; echo \"$x\"",
		"foobar\n",
	},
	{
		"read x <<< $'a\\\\\nb\\\\\nc'; echo \"$x\"",
		"abc\n",
	},
	{
		"read -r x <<< $'foo\\\\\nbar'; echo \"$x\"",
		"foo\\\n",
	},
	{
		"while read a; do echo $a; GOSH_CMD=print_ok $GOSH_PROG; done <<< 'a\nb\nc'",
		"a\nexec ok\nb\nexec ok\nc\nexec ok\n",
	},
	{
		"while read a; do echo $a; GOSH_CMD=print_ok $GOSH_PROG; done <<EOF\na\nb\nc\nEOF",
		"a\nexec ok\nb\nexec ok\nc\nexec ok\n",
	},
	{
		"echo file1 >f; echo file2 >>f; while read a; do echo $a; done <f",
		"file1\nfile2\n",
	},
	// TODO: our final exit status here isn't right.
	// {
	// 	"while read a; do echo $a; GOSH_CMD=print_fail $GOSH_PROG; done <<< 'a\nb\nc'",
	// 	"a\nexec fail\nb\nexec fail\nc\nexec fail\nexit status 1",
	// },
	{
		`read -r a <<< '\\'; echo "$a"`,
		"\\\\\n",
	},
	{
		"read -r a <<< '\\a\\b\\c'; echo $a",
		"\\a\\b\\c\n",
	},
	{
		"IFS=: read a b c <<< '1:2:3'; echo $a; echo $b; echo $c",
		"1\n2\n3\n",
	},
	{
		"IFS=: read a b c <<< '1\\:2:3'; echo \"$a\"; echo $b; echo $c",
		"1:2\n3\n\n",
	},
	{
		`read x <<< '  a  b  '; echo "[$x]"`,
		"[a  b]\n",
	},
	{
		`IFS=' :' read x <<< ' :a b: '; echo "[$x]"`,
		"[:a b:]\n",
	},
	{
		`IFS=: read x <<< ':a:b:'; echo "[$x]"`,
		"[:a:b:]\n",
	},
	{
		`read <<< '  a \b  '; echo "[$REPLY]"; read -r <<< ' a\b '; echo "[$REPLY]"`,
		"[  a b  ]\n[ a\\b ]\n",
	},
	{
		"read -p",
		"read: -p: option requires an argument\nexit status 2 #JUSTERR",
	},
	{
		"read -X -p",
		"read: invalid option \"-X\"\nexit status 2 #JUSTERR",
	},
	{
		"read -p 'Display me as a prompt. Continue? (y/n) ' choice <<< 'y'; echo $choice",
		"Display me as a prompt. Continue? (y/n) y\n #IGNORE bash requires a terminal",
	},
	{
		"read -r -p 'Prompt and raw flag together: ' a <<< '\\a\\b\\c'; echo $a",
		"Prompt and raw flag together: \\a\\b\\c\n #IGNORE bash requires a terminal",
	},

	// read -a
	{
		`echo "1 2 3" | { read -a arr; echo "${arr[0]} ${arr[1]} ${arr[2]}"; }`,
		"1 2 3\n",
	},
	{
		`echo "a b c" | { read -a arr; echo "${#arr[@]}"; }`,
		"3\n",
	},
	{
		`echo "" | { read -a arr; echo "${#arr[@]}"; }`,
		"0\n",
	},
	{
		`echo 'a\tb' | { read -ra arr; echo "${#arr[@]} ${arr[0]}"; }`,
		"1 a\\tb\n",
	},
	{
		"read -a",
		"read: -a: option requires an argument\nexit status 2 #JUSTERR",
	},

	// read -d
	{
		`printf 'a:b:' | { read -r -d : x; echo "[$x]"; read -r -d : y; echo "[$y]"; }`,
		"[a]\n[b]\n",
	},
	{
		// reaching the end of the input without the delimiter still assigns
		`printf 'ab' | { read -r -d : x; echo "$? [$x]"; }`,
		"1 [ab]\n",
	},
	{
		// an empty delimiter means an ASCII NUL, as used with "find -print0"
		`printf 'a\0b\0' | while read -r -d '' f; do echo "[$f]"; done`,
		"[a]\n[b]\n",
	},
	{
		// an escaped delimiter is a literal character, so it doesn't end the line
		`printf 'a\\:b:' | { read -d : x; echo "[$x]"; }`,
		"[a:b]\n",
	},
	{
		`printf 'a b:' | { read -r -a arr -d :; echo "${#arr[@]} [${arr[0]}] [${arr[1]}]"; }`,
		"2 [a] [b]\n",
	},
	{
		"read -d",
		"read: -d: option requires an argument\nexit status 2 #JUSTERR",
	},

	// read -n and read -N
	{
		`printf 'abcd\n' | { read -r -n 2 x; echo "$? [$x]"; read -r rest; echo "[$rest]"; }`,
		"0 [ab]\n[cd]\n",
	},
	{
		`printf 'ab' | { read -r -N 3 x; echo "$? [$x]"; }`,
		"1 [ab]\n",
	},
	{
		// -N reads a fixed number of characters, ignoring the delimiter
		`printf 'ab:cd' | { read -r -N 4 -d : x; echo "[$x]"; }`,
		"[ab:c]\n",
	},
	{
		// -N does no field splitting nor trimming, unlike -n
		`printf '  a b\n' | { read -N 5 x y; echo "[$x] [$y]"; }`,
		"[  a b] []\n",
	},
	{
		`printf '  a b\n' | { read -n 5 x y; echo "[$x] [$y]"; }`,
		"[a] [b]\n",
	},
	{
		// -N still counts the characters after the escapes are dropped
		`printf 'a\\bc' | { read -N 3 x; echo "[$x]"; }`,
		"[abc]\n",
	},
	// Byte-cleanliness (#377): -n and -N count characters, not bytes; a
	// byte that is not valid UTF-8 survives read and field splitting
	// untouched; a high byte works as -d's delimiter; and printf's
	// octal escapes are bytes on the wire, with overflow wrapping mod
	// 256 the way bash's do.
	{
		`read -n 5 foo <<< "абвгдежз"; echo "$foo"`,
		"абвгд\n",
	},
	{
		`read -N 3 foo <<< "абвгд"; echo "$foo"`,
		"абв\n",
	},
	{
		`printf 'B\315\n' | { IFS= read -r f; printf '%s' "$f" | wc -c | tr -d ' '; }`,
		"2\n",
	},
	{
		`printf 'x B\315 y\n' | { read -r a f b; printf '%s' "$f" | wc -c | tr -d ' '; }`,
		"2\n",
	},
	{
		`printf 'ab\200cd' | { read -rd "$(printf '\200')" s; echo "$s"; }`,
		"ab\n",
	},
	{
		`printf '\303\251' | wc -c | tr -d ' '`,
		"2\n",
	},
	{
		`[ "$(printf '\401')" = "$(printf '\001')" ] && echo wraps`,
		"wraps\n",
	},
	{
		`printf 'abc\n' | { read -r -n 0 x; echo "$? [$x]"; }`,
		"0 []\n",
	},
	{
		"read -n",
		"read: -n: option requires an argument\nexit status 2 #JUSTERR",
	},
	{
		"read -n abc",
		"read: abc: invalid number\nexit status 1 #JUSTERR",
	},
	{
		"read -N -1",
		"read: -1: invalid number\nexit status 1 #JUSTERR",
	},

	// read -s reads from the shell's stdin, which is not the process's stdin
	// under a redirect; there is no echo to suppress when it isn't a terminal.
	{
		`printf 'hi\n' | { read -r -s x; echo "[$x]"; }`,
		"[hi]\n",
	},
	{
		`a=a; echo | (read a; echo -n "$a")`,
		"",
	},
	{
		`a=b; read a < /dev/null; echo -n "$a"`,
		"",
	},
	{
		"a=c; echo x | (read a; echo -n $a)",
		"x",
	},
	{
		"a=d; echo -n y | (read a; echo -n $a)",
		"y",
	},

	// getopts
	{
		"getopts",
		"getopts: usage: getopts optstring name [arg ...]\nexit status 2",
	},
	{
		"getopts a a:b",
		"getopts: invalid identifier: \"a:b\"\nexit status 2 #JUSTERR",
	},
	{
		"getopts abc opt -a; echo $opt; $optarg",
		"a\n",
	},
	{
		"getopts abc opt -z",
		"getopts: illegal option -- \"z\"\n #IGNORE",
	},
	{
		"getopts a: opt -a",
		"getopts: option requires an argument -- \"a\"\n #IGNORE",
	},
	{
		"getopts :abc opt -z; echo $opt; echo $OPTARG",
		"?\nz\n",
	},
	{
		"getopts :a: opt -a; echo $opt; echo $OPTARG",
		":\na\n",
	},
	{
		"getopts abc opt foo -a; echo $opt; echo $OPTIND",
		"?\n1\n",
	},
	{
		"getopts abc opt -a foo; echo $opt; echo $OPTIND",
		"a\n2\n",
	},
	{
		"OPTIND=3; getopts abc opt -a -b -c; echo $opt;",
		"c\n",
	},
	{
		"OPTIND=100; getopts abc opt -a -b -c; echo $opt;",
		"?\n",
	},
	{
		"OPTIND=foo; getopts abc opt -a -b -c; echo $opt;",
		"a\n",
	},
	{
		"while getopts ab:c opt -c -b arg -a foo; do echo $opt $OPTARG $OPTIND; done",
		"c 2\nb arg 4\na 5\n",
	},
	{
		"while getopts abc opt -ba -c foo; do echo $opt $OPTARG $OPTIND; done",
		"b 1\na 2\nc 3\n",
	},
	{
		"while getopts ab: opt -a -bval -a; do echo $opt $OPTARG $OPTIND; done",
		"a 2\nb val 3\na 4\n",
	},
	{
		"while getopts b: opt -bval foo; do echo $opt $OPTARG $OPTIND; done",
		"b val 2\n",
	},
	{
		"while getopts ab: opt -ab val; do echo $opt $OPTARG $OPTIND; done",
		"a 1\nb val 3\n",
	},
	{
		"a() { while getopts abc: opt; do echo $opt $OPTARG; done }; a -a -b -c arg",
		"a\nb\nc arg\n",
	},
	// mapfile
	{
		"mapfile <<EOF\na\nb\nc\nEOF\n" + `for x in "${MAPFILE[@]}"; do echo "$x"; done`,
		"a\n\nb\n\nc\n\n",
	},
	{
		"mapfile -t <<EOF\na\nb\nc\nEOF\n" + `for x in "${MAPFILE[@]}"; do echo "$x"; done`,
		"a\nb\nc\n",
	},
	{
		"mapfile -t -d b <<EOF\nabc\nEOF\n" + `for x in "${MAPFILE[@]}"; do echo "$x"; done`,
		"a\nc\n\n",
	},
	{
		"mapfile -t butter <<EOF\na\nb\nc\nEOF\n" + `for x in "${butter[@]}"; do echo "$x"; done`,
		"a\nb\nc\n",
	},
}

var runTestsUnix = []runTest{
	{"[[ -n $PPID && $PPID -ge 0 ]]", ""}, // can be 0 if running as the init process

	// exec's flags, which need a program that reports its own argv[0] and
	// environment. gosh sets $0 itself, so /bin/sh is the observer here.
	{
		// -a runs one file while telling it a different argv[0], which is how
		// a multi-call binary is dispatched without a wrapper.
		"(exec -a argv0name /bin/sh -c 'echo $0')",
		"argv0name\n",
	},
	{
		// -l marks a login shell by prefixing argv[0] with a dash.
		"(exec -l /bin/sh -c 'case $0 in -*) echo dashed ;; *) echo plain ;; esac')",
		"dashed\n",
	},
	{
		// -a and -l compose: the dash goes on the overridden name.
		"(exec -l -a argv0name /bin/sh -c 'echo $0')",
		"-argv0name\n",
	},
	{
		// -c runs the command with an empty environment.
		"export FOO=bar; (exec -c /bin/sh -c 'echo ${FOO-unset}')",
		"unset\n",
	},
	{
		"export FOO=bar; (exec /bin/sh -c 'echo ${FOO-unset}')",
		"bar\n",
	},
	{
		// no root user on windows
		"[[ ~root == '~root' ]]",
		"exit status 1",
	},

	// windows does not support paths with '*'
	{
		"mkdir -p '*/a.z' 'b/a.z'; cd '*'; set -- *.z; echo $#",
		"1\n",
	},
	{
		"mkdir -p 'a-*/d'; test -d $PWD/a-*/*",
		"",
	},

	// windows does not reliably track last-access time, so -N is unix-only
	{
		">a; cat a; sleep 0.01; echo 'Hello' >> a; test -N a && echo yes",
		"yes\n",
	},
	{
		"test -N nonexistent",
		"exit status 1",
	},
	{
		">a; sleep 0.01; cat a; test -N a; echo $?",
		"1\n",
	},

	// no fifos on windows
	{
		"[ -p a ] && echo x; mkfifo a; [ -p a ] && echo y",
		"y\n",
	},
	// `read -t` on a FIFO opened read-write (#348). The runtime refuses a
	// deadline on that shape, and treating the refusal as "regular file"
	// left the read blocked until killed; it is answered with poll(2) now.
	{
		"mkfifo p; exec 9<> p; read -r -u 9 -t 0.1 x; echo \"st=$? x=[$x]\"",
		"st=142 x=[]\n",
	},
	{
		"mkfifo p; exec 9<> p; echo hi >&9; read -r -u 9 -t 1 x; echo \"st=$? x=[$x]\"",
		"st=0 x=[hi]\n",
	},
	{
		// Whatever arrived before the timeout is still assigned here too.
		"mkfifo p; exec 9<> p; printf par >&9; read -r -u 9 -t 0.1 x; echo \"st=$? x=[$x]\"",
		"st=142 x=[par]\n",
	},

	// Traps on real OS signals (#350). Each case uses a signal no other
	// case fires: these tests share one process, and a Notify fans a
	// delivery out to every runner armed for that signal. /bin/kill
	// rather than kill, which the interpreter leaves to the shell above.
	// The sleep gives Go's asynchronous signal forwarding a boundary to
	// land before; bash runs the trap before the sleep, so the visible
	// order is the same either way.
	{
		"trap 'echo t' USR1; /bin/kill -USR1 $$; sleep 0.1; echo after",
		"t\nafter\n",
	},
	{
		// The shell survives a trapped TERM instead of dying 143.
		"trap 'echo caught' TERM; /bin/kill -TERM $$; sleep 0.1; echo alive",
		"caught\nalive\n",
	},
	{
		// Control flow raised inside a signal trap propagates: this is
		// what lets `trap 'return' SIG` break a busy loop.
		"trap 'echo t; exit 3' ALRM; /bin/kill -ALRM $$; sleep 0.1; echo unreachable",
		"t\nexit status 3",
	},
	// `trap '' SIG` ignoring a signal and listing as `trap -- '' SIG...`
	// is covered in cmd/koi's builtin matrix, NOT here: signal.Ignore is
	// process-global and inherited by children, so a case ignoring a
	// signal in this shared test process makes every bash oracle spawned
	// after it list the inherited ignore — a cross-test flake that hit CI
	// on the first day (#352's PR). Subprocess tests cannot contaminate
	// each other that way.
	{
		// Signal traps are listed between EXIT and the pseudo-signals,
		// under their SIG names, and `trap -p` accepts any spec spelling.
		"trap 'echo x' hup; trap 'echo bye' 0; trap -p SIGHUP; trap",
		"trap -- 'echo x' SIGHUP\ntrap -- 'echo bye' EXIT\ntrap -- 'echo x' SIGHUP\nbye\n",
	},
	{
		// Numeric specs resolve to the signal, and `trap - N` restores
		// the default.
		"trap 'echo z' 2; trap -p INT; trap - 2; trap -p INT; echo done",
		"trap -- 'echo z' SIGINT\ndone\n",
	},
	{
		"[[ -p a ]] && echo x; mkfifo a; [[ -p a ]] && echo y",
		"y\n",
	},

	{"sh() { :; }; sh -c 'echo foo'", ""},
	{"sh() { :; }; command sh -c 'echo foo'", "foo\n"},

	// files without a shebang line are run as shell scripts; see issue #1065
	{
		"echo 'echo foo' >a; chmod +x a; ./a",
		"foo\n",
	},
	{
		"echo 'echo $#: $1' >a; chmod +x a; ./a one two",
		"2: one\n",
	},
	{
		"echo 'echo \"[$foo][$bar]\"' >a; chmod +x a; foo=1; export bar=2; ./a",
		"[][2]\n",
	},
	{
		"echo 'exit 5' >a; chmod +x a; ./a",
		"exit status 5",
	},
	{
		"printf '\\0\\n' >a; chmod +x a; ./a",
		"./a: cannot execute binary file\nexit status 126 #JUSTERR",
	},
	{
		"echo 'if' >a; chmod +x a; ./a",
		"./a:1:1: `if` must be followed by a statement list\nexit status 2 #JUSTERR",
	},

	// chmod is practically useless on Windows
	{
		"[ -x a ] && echo x; >a; chmod 0755 a; [ -x a ] && echo y",
		"y\n",
	},
	{
		"[[ -x a ]] && echo x; >a; chmod 0755 a; [[ -x a ]] && echo y",
		"y\n",
	},
	{
		">a; [ -k a ] && echo x; chmod +t a; [ -k a ] && echo y",
		"y\n",
	},
	{
		">a; [ -u a ] && echo x; chmod u+s a; [ -u a ] && echo y",
		"y\n",
	},
	{
		">a; [ -g a ] && echo x; chmod g+s a; [ -g a ] && echo y",
		"y\n",
	},
	{
		">a; [[ -k a ]] && echo x; chmod +t a; [[ -k a ]] && echo y",
		"y\n",
	},
	{
		">a; [[ -u a ]] && echo x; chmod u+s a; [[ -u a ]] && echo y",
		"y\n",
	},
	{
		">a; [[ -g a ]] && echo x; chmod g+s a; [[ -g a ]] && echo y",
		"y\n",
	},
	{
		`mkdir a; chmod 0100 a; cd a`,
		"",
	},
	// Note that these will succeed if we're root.
	{
		`mkdir a; chmod 0000 a; cd a`,
		"cd: permission denied: \"a\"\nexit status 1 #JUSTERR",
	},
	{
		`mkdir a; chmod 0222 a; cd a`,
		"cd: permission denied: \"a\"\nexit status 1 #JUSTERR",
	},
	{
		`mkdir a; chmod 0444 a; cd a`,
		"cd: permission denied: \"a\"\nexit status 1 #JUSTERR",
	},
	{
		`mkdir a; chmod 0010 a; cd a`,
		"cd: permission denied: \"a\"\nexit status 1 #JUSTERR",
	},
	{
		`mkdir a; chmod 0001 a; cd a`,
		"cd: permission denied: \"a\"\nexit status 1 #JUSTERR",
	},
	{
		`unset UID`,
		"UID: readonly variable\n #IGNORE",
	},
	{
		`test -n "$EUID" && echo OK`,
		"OK\n",
	},
	{
		`set EUID=newvalue; test EUID != newvalue && echo OK || echo EUID=$EUID`,
		"OK\n",
	},
	{
		`unset EUID`,
		"EUID: readonly variable\n #IGNORE",
	},
	// GID is not set in bash
	{
		`unset GID`,
		"GID: readonly variable\n #IGNORE",
	},
	{
		`[[ -z $GID ]] && echo "GID not set"`,
		"exit status 1 #JUSTERR #IGNORE",
	},

	// Unix-y PATH
	{
		"PATH=; bash -c 'echo foo'",
		"\"bash\": executable file not found in $PATH\nexit status 127 #JUSTERR",
	},
	{
		"cd /; sure/is/missing",
		"stat /sure/is/missing: no such file or directory\nexit status 127 #JUSTERR",
	},
	{
		"echo '#!/bin/sh\necho b' >a; chmod 0755 a; PATH=; a",
		"b\n",
	},
	{
		"mkdir c; cd c; echo '#!/bin/sh\necho b' >a; chmod 0755 a; PATH=; a",
		"b\n",
	},
	{
		"mkdir c; echo '#!/bin/sh\necho b' >c/a; chmod 0755 c/a; c/a",
		"b\n",
	},
	{
		"GOSH_CMD=lookpath $GOSH_PROG",
		"sh found\n",
	},

	// error strings which are too different on Windows
	{
		"echo foo >/shouldnotexist/file",
		"open /shouldnotexist/file: no such file or directory\nexit status 1 #JUSTERR",
	},
	{
		"set -e; echo foo >/shouldnotexist/file; echo foo",
		"open /shouldnotexist/file: no such file or directory\nexit status 1 #JUSTERR",
	},

	// process substitution; named pipes (fifos) are a TODO for windows
	{
		"sed 's/o/e/g' <(echo foo bar)",
		"fee bar\n",
	},
	{
		"cat <(echo foo) <(echo bar) <(echo baz)",
		"foo\nbar\nbaz\n",
	},
	{
		"cat <(cat <(echo nested))",
		"nested\n",
	},
	{
		// The tests here use "wait" because otherwise the parent may finish before
		// the subprocess has had time to process the input and print the result.
		"echo foo bar > >(sed 's/o/e/g'); wait",
		"fee bar\n",
	},
	{
		"echo foo bar | tee >(sed 's/o/e/g') >/dev/null; wait",
		"fee bar\n",
	},
	{
		"echo nested > >(cat > >(cat); wait); wait",
		"nested\n",
	},
	{
		"cat <(exit 0); wait $!; echo $?",
		"0\n",
	},
	{
		"cat <(exit 5); wait $!; echo $?",
		"5\n",
	},
	{
		// The reader here does not consume the named pipe.
		"test -e <(echo foo)",
		"",
	},
	// echo trace
	{
		`set -x; animals=("dog", "cat", "otter"); echo "hello ${animals[*]}"`,
		`+ animals=("dog", "cat", "otter")
+ echo 'hello dog, cat, otter'
hello dog, cat, otter
`,
	},
	{
		`set -x; s="always print a decimal point for %e, %E, %f, %F, %g and %G; do not remove trailing zeros for %g and %G"; echo "$s"`,
		`+ s='always print a decimal point for %e, %E, %f, %F, %g and %G; do not remove trailing zeros for %g and %G'
+ echo 'always print a decimal point for %e, %E, %f, %F, %g and %G; do not remove trailing zeros for %g and %G'
always print a decimal point for %e, %E, %f, %F, %g and %G; do not remove trailing zeros for %g and %G
`,
	},
	{
		`set -x
x=without; echo "$x"
x="double quote"; echo "$x"
x='single quote'; echo "$x"`,
		`+ x=without
+ echo without
without
+ x='double quote'
+ echo 'double quote'
double quote
+ x='single quote'
+ echo 'single quote'
single quote
`,
	},
	// for trace
	{
		`set -x
exec >/dev/null
echo "trace should go to stderr"`,
		`+ exec
+ echo 'trace should go to stderr'
`,
	},
	{
		`set -x
animals=(dog, cat, otter)
for i in ${animals[@]}
do
   echo "hello ${i}"
done
`,
		`+ animals=(dog, cat, otter)
+ for i in ${animals[@]}
+ echo 'hello dog,'
hello dog,
+ for i in ${animals[@]}
+ echo 'hello cat,'
hello cat,
+ for i in ${animals[@]}
+ echo 'hello otter'
hello otter
`,
	},
	{
		`set -x
loop() {
    for i do
        echo "something with $i"
    done
}
loop 1 2 3`,
		`+ loop 1 2 3
+ for i in "$@"
+ echo 'something with 1'
something with 1
+ for i in "$@"
+ echo 'something with 2'
something with 2
+ for i in "$@"
+ echo 'something with 3'
something with 3
`,
	},
	{
		`set -x; animals=(dog, cat, otter); for i in ${animals[@]}; do echo "hello ${i}"; done`,
		`+ animals=(dog, cat, otter)
+ for i in ${animals[@]}
+ echo 'hello dog,'
hello dog,
+ for i in ${animals[@]}
+ echo 'hello cat,'
hello cat,
+ for i in ${animals[@]}
+ echo 'hello otter'
hello otter
`,
	},
	{
		`set -x; a=x"y"$z b=(foo bar $none '')`,
		"+ a=xy\n+ b=(foo bar $none '')\n",
	},
	{
		`set -x; for i in a b; do echo $i; done`,
		`+ for i in a b
+ echo a
a
+ for i in a b
+ echo b
b
`,
	},
	{
		`set -x; for i in $none_a $none_b; do echo $i; done`,
		``,
	},
	// case trace
	{
		`set -x; pet=dog; case $pet in 'dog') echo "barks";; *) echo "unknown";; esac`,
		`+ pet=dog
+ case $pet in
+ echo barks
barks
`,
	},
	{
		`set -x
pet="dog"
case $pet in
  dog)
    echo "barks"
    ;;
  *)
    echo "unknown"
    ;;
esac`,
		`+ pet=dog
+ case $pet in
+ echo barks
barks
`,
	},
	// arithmetic
	{
		`set -x
a=$(( 4 + 5 )); echo $a
a=$((3+5)); echo $a`,
		`+ a=9
+ echo 9
9
+ a=8
+ echo 8
8
`,
	},
	{
		`set -x;
let a=5+4; echo $a
let "a = 5 + 4"; echo $a
let a++; echo $a`,
		`+ let a=5+4
+ echo 9
9
+ let 'a = 5 + 4'
+ echo 9
9
+ let a++
+ echo 10
10
`,
	},
	// functions
	{
		`set -x; function with_function () { echo 'hello, world'; }; with_function`,
		`+ with_function
+ echo 'hello, world'
hello, world
`,
	},
	{
		`set -x; without_function () { echo 'hello, world'; }; without_function`,
		`+ without_function
+ echo 'hello, world'
hello, world
`,
	},
	{
		// globbing wildcard as function name
		`@() { echo "$@"; }; @ lala; function +() { echo "$@"; }; + foo`,
		"lala\nfoo\n",
	},
	{
		`      @() { echo "$@"; }; @ lala;`,
		"lala\n",
	},
	{
		// globbing wildcard as function name but with space after the name
		`+ () { echo "$@"; }; + foo; @ () { echo "$@"; }; @ lala; ? () { echo "$@"; }; ? bar`,
		"foo\nlala\nbar\n",
	},
	// mapfile, no process substitution yet on Windows
	{
		`mapfile -t -d "" < <(printf "a\0b\n"); for x in "${MAPFILE[@]}"; do echo "$x"; done`,
		"a\nb\n\n",
	},
	// Windows does not support having a `\n` in a filename
	{
		`> $'bar\nbaz'; echo bar*baz`,
		"bar\nbaz\n",
	},
}

var runTestsWindows = []runTest{
	{"[[ -n $PPID || $PPID -gt 0 ]]", ""}, // os.Getppid can be 0 on windows
	{"cmd() { :; }; cmd /c 'echo foo'", ""},
	{"cmd() { :; }; command cmd /c 'echo foo'", "foo\r\n"},
	{
		"GOSH_CMD=lookpath $GOSH_PROG",
		"cmd found\n",
	},
	{
		"localCase=camel; LocalCase=pascal; echo $localcase",
		"pascal\n",
	},
	{
		// Matching the env var name set as a global
		// in a case sensitive way.
		"$ENV_PROG | grep -i '^mixedCase_interp'",
		"mixedCase_INTERP_GLOBAL=value\n",
	},
	{
		// Overwriting the env var set as a global
		// in a case insensitive way.
		"MIXEDCASE_interp_global=replaced; echo $MIXEDCASE_interp_GLOBAL",
		"replaced\n",
	},
	{
		"MIXEDCASE_interp_global=replaced; $ENV_PROG | grep -i '^mixedcase_interp'",
		"MIXEDCASE_interp_global=replaced\n",
	},
}

// These tests are specific to 64-bit architectures, and that's fine. We don't
// need to add explicit versions for 32-bit.
var runTests64bit = []runTest{
	{"printf %i,%u -3 -3", "-3,18446744073709551613"},
	{"printf %o -3", "1777777777777777777775"},
	{"printf %x -3", "fffffffffffffffd"},
}

func init() {
	if runtime.GOOS == "windows" {
		runTests = append(runTests, runTestsWindows...)
	} else { // Unix-y
		runTests = append(runTests, runTestsUnix...)
	}
	if bits.UintSize == 64 {
		runTests = append(runTests, runTests64bit...)
	}
}

// ln -s: wine doesn't implement symlinks; see https://bugs.winehq.org/show_bug.cgi?id=44948
// process substitutions are not supported on Windows
var skipOnWindows = regexp.MustCompile(`ln -s|<\(`)

// process substitutions seemflaky on mac; see https://github.com/mvdan/sh/issues/576
var skipOnMac = regexp.MustCompile(`>\(|<\(`)

func skipIfUnsupported(tb testing.TB, src string) {
	switch {
	case runtime.GOOS == "windows" && skipOnWindows.MatchString(src):
		tb.Skipf("skipping non-portable test on windows")
	case runtime.GOOS == "darwin" && skipOnMac.MatchString(src):
		tb.Skipf("skipping non-portable test on mac")
	}
}

func TestRunnerRun(t *testing.T) {
	t.Parallel()

	p := syntax.NewParser()
	for _, c := range runTests {
		t.Run("", func(t *testing.T) {
			skipIfUnsupported(t, c.in)
			t.Logf("input: %q", c.in)

			// Parse first, as we reuse a single parser.
			file := parse(t, p, c.in)

			t.Parallel()

			tdir := t.TempDir()
			var cb concBuffer
			r, err := interp.New(interp.Dir(tdir), interp.StdIO(nil, &cb, &cb),
				// TODO: why does this make some tests hang?
				// interp.Env(expand.ListEnviron(append(os.Environ(),
				// 	"foo_NULL_BAR=foo\x00bar")...)),
				interp.ExecHandlers(testExecHandler),
			)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), runnerRunTimeout)
			defer cancel()
			if err := r.Run(ctx, file); err != nil {
				cb.WriteString(err.Error())
			}

			// Some builtins like "pushd" can show absolute paths as part of error messages.
			// Allow a very simple search-and-replace for the equivalent to "$PWD/a".
			want := strings.ReplaceAll(c.want, "ABS_PATH_A", fmt.Sprintf("%q", filepath.Join(tdir, "a")))

			if i := strings.Index(want, " #"); i >= 0 {
				want = want[:i]
			}
			if got := cb.String(); got != want {
				if len(got) > 200 {
					got = "…" + got[len(got)-200:]
				}
				t.Fatalf("wrong output in %q:\nwant: %q\ngot:  %q",
					c.in, want, got)
			}
		})
	}
}

func TestRunnerUnsupported(t *testing.T) {
	t.Parallel()

	// Features from language variants that the interpreter does not
	// support, such as zsh, should error rather than panic.
	tests := []struct {
		lang syntax.LangVariant
		in   string
		want string
	}{
		{syntax.LangZsh, "echo x${}y", "unsupported\n"},
		{syntax.LangZsh, `echo "${}"`, "unsupported\n"},
		{syntax.LangZsh, "echo ${:-foo}", "unsupported\n"},
		{syntax.LangZsh, "echo ${+a}", "unsupported\n"},
		{syntax.LangZsh, "a=abc; echo ${a[(r)b]}", "unsupported\n"},
		{syntax.LangZsh, "() { echo anon; }", "unsupported\nexit status 1"},
		{syntax.LangZsh, "function { echo anon; }", "unsupported\nexit status 1"},
		{syntax.LangZsh, "function f g { echo multi; }", "unsupported\nexit status 1"},
		{syntax.LangZsh, "cat =(echo hi)", "unsupported\n"},
		{syntax.LangMirBSDKorn, "echo ${%a}", "unsupported\n"},
	}
	for _, tc := range tests {
		t.Run("", func(t *testing.T) {
			t.Parallel()
			t.Logf("input: %q", tc.in)
			p := syntax.NewParser(syntax.Variant(tc.lang))
			file := parse(t, p, tc.in)
			var cb concBuffer
			r, err := interp.New(interp.StdIO(nil, &cb, &cb))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), runnerRunTimeout)
			defer cancel()
			if err := r.Run(ctx, file); err != nil {
				cb.WriteString(err.Error())
			}
			if got := cb.String(); got != tc.want {
				t.Fatalf("wrong output in %q:\nwant: %q\ngot:  %q",
					tc.in, tc.want, got)
			}
		})
	}
}

func readLines(hc interp.HandlerContext) ([][]byte, error) {
	bs, err := io.ReadAll(hc.Stdin)
	if err != nil {
		return nil, err
	}
	if runtime.GOOS == "windows" {
		bs = bytes.ReplaceAll(bs, []byte("\r\n"), []byte("\n"))
	}
	bs = bytes.TrimSuffix(bs, []byte("\n"))
	return bytes.Split(bs, []byte("\n")), nil
}

func absPath(dir, path string) string {
	if path == "" {
		return ""
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	return filepath.Clean(path) // TODO: this clean is likely unnecessary
}

var testBuiltinsMap = map[string]func(interp.HandlerContext, []string) error{
	"cat": func(hc interp.HandlerContext, args []string) error {
		if len(args) == 0 {
			if hc.Stdin == nil || hc.Stdout == nil {
				return nil
			}
			_, err := io.Copy(hc.Stdout, hc.Stdin)
			return err
		}
		for _, arg := range args {
			path := absPath(hc.Dir, arg)
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			_, err = io.Copy(hc.Stdout, f)
			f.Close()
			if err != nil {
				return err
			}
		}
		return nil
	},
	"wc": func(hc interp.HandlerContext, args []string) error {
		bs, err := io.ReadAll(hc.Stdin)
		if err != nil {
			return err
		}
		if len(args) == 0 {
			fmt.Fprintf(hc.Stdout, "%7d", bytes.Count(bs, []byte("\n")))
			fmt.Fprintf(hc.Stdout, "%8d", len(bytes.Fields(bs)))
			fmt.Fprintf(hc.Stdout, "%8d\n", len(bs))
		} else if args[0] == "-c" {
			fmt.Fprintln(hc.Stdout, len(bs))
		} else if args[0] == "-l" {
			fmt.Fprintln(hc.Stdout, bytes.Count(bs, []byte("\n")))
		}
		return nil
	},
	"tr": func(hc interp.HandlerContext, args []string) error {
		if len(args) != 2 || len(args[1]) != 1 {
			return fmt.Errorf("usage: tr [-s -d] [character]")
		}
		squeeze := args[0] == "-s"
		char := args[1][0]
		bs, err := io.ReadAll(hc.Stdin)
		if err != nil {
			return err
		}
		for {
			i := bytes.IndexByte(bs, char)
			if i < 0 {
				hc.Stdout.Write(bs) // remaining
				break
			}
			hc.Stdout.Write(bs[:i]) // up to char
			bs = bs[i+1:]

			bs = bytes.TrimLeft(bs, string(char)) // remove repeats
			if squeeze {
				hc.Stdout.Write([]byte{char})
			}
		}
		return nil
	},
	"sort": func(hc interp.HandlerContext, args []string) error {
		lines, err := readLines(hc)
		if err != nil {
			return err
		}
		slices.SortFunc(lines, bytes.Compare)
		for _, line := range lines {
			fmt.Fprintf(hc.Stdout, "%s\n", line)
		}
		return nil
	},
	"grep": func(hc interp.HandlerContext, args []string) error {
		var rx *regexp.Regexp
		quiet := false
		caseInsensitive := false
		for _, arg := range args {
			if arg == "-q" {
				quiet = true
			} else if arg == "-i" {
				caseInsensitive = true
			} else if arg == "-E" {
			} else if rx == nil {
				if caseInsensitive {
					arg = "(?i)" + arg
				}
				rx = regexp.MustCompile(arg)
			} else {
				return fmt.Errorf("unexpected arg: %q", arg)
			}
		}
		lines, err := readLines(hc)
		if err != nil {
			return err
		}
		anyMatch := false
		for _, line := range lines {
			if rx.Match(line) {
				if quiet {
					return nil
				}
				anyMatch = true
				fmt.Fprintf(hc.Stdout, "%s\n", line)
			}
		}
		if !anyMatch {
			return interp.ExitStatus(1)
		}
		return nil
	},
	"sed": func(hc interp.HandlerContext, args []string) error {
		f := hc.Stdin
		switch len(args) {
		case 1:
		case 2:
			var err error
			f, err = os.Open(absPath(hc.Dir, args[1]))
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("usage: sed pattern [file]")
		}
		expr := args[0]
		if expr == "" || expr[0] != 's' {
			return fmt.Errorf("unimplemented")
		}
		sep := expr[1]
		expr = expr[2:]
		from := expr[:strings.IndexByte(expr, sep)]
		expr = expr[len(from)+1:]
		to := expr[:strings.IndexByte(expr, sep)]
		bs, err := io.ReadAll(f)
		if err != nil {
			return err
		}
		rx := regexp.MustCompile(from)
		bs = rx.ReplaceAllLiteral(bs, []byte(to))
		_, err = hc.Stdout.Write(bs)
		return err
	},
	"mkdir": func(hc interp.HandlerContext, args []string) error {
		for _, arg := range args {
			if arg == "-p" {
				continue
			}
			path := absPath(hc.Dir, arg)
			if err := os.MkdirAll(path, 0o777); err != nil {
				return err
			}
		}
		return nil
	},
	"rm": func(hc interp.HandlerContext, args []string) error {
		for _, arg := range args {
			if arg == "-r" {
				continue
			}
			path := absPath(hc.Dir, arg)
			if err := os.RemoveAll(path); err != nil {
				return err
			}
		}
		return nil
	},
	"ln": func(hc interp.HandlerContext, args []string) error {
		symbolic := args[0] == "-s"
		if symbolic {
			args = args[1:]
		}
		oldname := absPath(hc.Dir, args[0])
		newname := absPath(hc.Dir, args[1])
		if symbolic {
			return os.Symlink(oldname, newname)
		}
		return os.Link(oldname, newname)
	},
	"touch": func(hc interp.HandlerContext, args []string) error {
		filenames := args // create all arguments as filenames

		newTime := time.Now()
		if args[0] == "-t" {
			if len(args) < 3 {
				return fmt.Errorf("usage: touch [-t [[CC]YY]MMDDhhmm[.SS]] file")
			}
			filenames = args[2:] // treat the rest of the args as filenames

			arg := args[1]
			if len(arg) > 15 {
				return fmt.Errorf("usage: touch [-t [[CC]YY]MMDDhhmm[.SS]] file")
			}
			s, err := time.Parse("200601021504.05", arg)
			if err != nil {
				return err
			}
			newTime = s
		}

		for _, arg := range filenames {
			if strings.HasPrefix(arg, "-") {
				return fmt.Errorf("usage: touch [-t [[CC]YY]MMDDhhmm[.SS]] file")
			}
			path := absPath(hc.Dir, arg)
			// create the file if it does not exist
			f, err := os.OpenFile(path, os.O_CREATE, 0o666)
			if err != nil {
				return err
			}
			f.Close()
			// change the modification and access time
			if err := os.Chtimes(path, newTime, newTime); err != nil {
				return err
			}
		}
		return nil
	},
	"sleep": func(hc interp.HandlerContext, args []string) error {
		for _, arg := range args {
			// assume and default unit to be in seconds
			d, err := time.ParseDuration(fmt.Sprintf("%ss", arg))
			if err != nil {
				return err
			}
			time.Sleep(d)
		}
		return nil
	},
}

func testExecHandler(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
	return func(ctx context.Context, args []string) error {
		if fn := testBuiltinsMap[args[0]]; fn != nil {
			return fn(interp.HandlerCtx(ctx), args[1:])
		}
		return next(ctx, args)
	}
}

// Same as the syntax package.
var requireShells = os.Getenv("REQUIRE_SHELLS") == "1"

// koi-local: checkOracleTilde asks the oracle what it does rather than
// assuming from the platform, because assuming was wrong. Homebrew's bash
// resolves ~ from the password database and ignores a reassigned HOME; a
// vanilla bash built from source on the same Mac does not. The behavior
// belongs to the build, not the operating system, so the only reliable
// question is the one put to the bash that is actually about to run.
func checkOracleTilde() bool {
	out, err := exec.Command("bash", "-c", "HOME=/koi-oracle-probe; echo ~").Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) != "/koi-oracle-probe"
}

// skipIfOracleGap skips the cases where bash itself, not the interpreter, is
// what varies between machines. Both are matched on the exact script: other
// cases combine HOME with ~ and do pass, and a loose predicate would skip
// those too and quietly cost coverage. Tracked in #271.
func skipIfOracleGap(t *testing.T, src string) {
	switch src {
	case `HOME='/*'; echo ~; echo "$HOME"`,
		`HOME=/foo; rel=/bar; echo ~/bar ~/'bar' ~/"bar" ~/$rel ~/"$rel"`,
		`HOME=/foo; echo ~ ~/ ~/'' ~'' ~""`:
		if oracleTildeIgnoresHome {
			t.Skip("this bash resolves ~ from the password database rather than from HOME")
		}
	case `echo foo >&- 2>&-; :`:
		// Unlike the tilde cases this one survives a source build, so it
		// really does look like the platform rather than the packaging.
		if runtime.GOOS == "darwin" {
			t.Skip("bash on darwin reports a write error for a closed fd that Linux bash does not")
		}
	}
}

// oracleRetries is how many extra times a racing case may ask bash. The
// one case that races was measured at roughly one wrong answer in three
// hundred runs on a machine under three times its core count in load, so
// five retries put a spurious failure far below the rate of every other
// thing that can go wrong in CI.
const oracleRetries = 5

// oracleRacesItself reports whether bash answers this script differently
// from run to run.
//
// Matched on the exact script, the way [skipIfOracleGap] is and for the
// same reason: the neighbouring `wait -n` cases were measured too and are
// stable, so a predicate like "mentions wait" would hand a retry to cases
// that have earned a single-shot check.
//
//	(exit 3) & wait; wait -n; echo $?
//
// A bare `wait` reaps every job, so the `wait -n` after it has nothing
// left and answers 127. bash usually agrees and sometimes answers 3 --
// the job's own status -- because whether the job has left the table by
// then is not something bash sequences against `wait` returning. It is
// bash racing itself rather than disagreeing with the recorded answer:
// 300 runs under load gave 299 of the former and one of the latter, and
// `(exit 3) & p=$!; wait $p; wait -n` never varied at all.
func oracleRacesItself(src string) bool {
	return src == `(exit 3) & wait; wait -n; echo $?`
}

func TestRunnerRunConfirm(t *testing.T) {
	if testing.Short() {
		t.Skip("calling bash is slow")
	}
	if !hasBash53 {
		if requireShells {
			t.Fatal("bash 5.3 required to run")
		} else {
			t.Skip("bash 5.3 required to run")
		}
	}
	t.Parallel()

	if runtime.GOOS == "windows" {
		// For example, it seems to treat environment variables as
		// case-sensitive, which isn't how Windows works.
		t.Skip("bash on Windows emulates Unix-y behavior")
	}
	for _, c := range runTests {
		t.Run("", func(t *testing.T) {
			if strings.Contains(c.want, " #IGNORE") {
				return
			}
			skipIfUnsupported(t, c.in)
			skipIfOracleGap(t, c.in)
			t.Parallel()
			askBash := func() (string, error) {
				tdir := t.TempDir()
				ctx, cancel := context.WithTimeout(t.Context(), runnerRunTimeout)
				defer cancel()
				cmd := exec.CommandContext(ctx, "bash")
				cmd.Dir = tdir
				cmd.Stdin = strings.NewReader(c.in)
				out, err := cmd.CombinedOutput()
				return string(out), err
			}
			out, err := askBash()
			if strings.Contains(c.want, " #JUSTERR") {
				// bash sometimes exits with status code 0 and
				// stderr "bash: ..." for an error
				fauxErr := strings.HasPrefix(out, "bash:")
				if err == nil && !fauxErr {
					t.Fatalf("wanted bash to error in %q", c.in)
				}
				return
			}
			got := out
			if err != nil {
				got += err.Error()
			}
			// A case whose subject is bash's own job reaping does not
			// answer the same way every time, so asking once turns a
			// property of bash into a coin flip (#317). Asking again is
			// the honest assertion for it -- bash must still produce
			// this answer, just not on demand -- and it stays scoped to
			// the exact scripts measured to race, so no other case has
			// its check weakened.
			for try := 0; got != c.want && try < oracleRetries && oracleRacesItself(c.in); try++ {
				out, err = askBash()
				got = out
				if err != nil {
					got += err.Error()
				}
			}
			if got != c.want {
				t.Fatalf("wrong bash output in %q:\nwant: %q\ngot:  %q",
					c.in, c.want, got)
			}
		})
	}
}

func TestRunnerOpts(t *testing.T) {
	t.Parallel()

	withPath := func(strs ...string) func(*interp.Runner) error {
		prefix := []string{
			"PATH=" + os.Getenv("PATH"),
			"ENV_PROG=" + os.Getenv("ENV_PROG"),
		}
		return interp.Env(expand.ListEnviron(append(prefix, strs...)...))
	}
	opts := func(list ...interp.RunnerOption) []interp.RunnerOption {
		return list
	}
	cases := []struct {
		opts     []interp.RunnerOption
		in, want string
	}{
		{
			nil,
			"$ENV_PROG | grep -i '^interp_global='",
			"INTERP_GLOBAL=value\n",
		},
		{
			opts(withPath()),
			"$ENV_PROG | grep -i '^interp_global='",
			"exit status 1",
		},
		{
			opts(withPath("INTERP_GLOBAL=bar")),
			"$ENV_PROG | grep -i '^interp_global='",
			"INTERP_GLOBAL=bar\n",
		},
		{
			opts(withPath("a=b")),
			"echo $a",
			"b\n",
		},
		{
			opts(withPath("A=b")),
			"$ENV_PROG | grep '^A='; echo $A",
			"A=b\nb\n",
		},
		{
			opts(withPath("A=b", "A=c")),
			"$ENV_PROG | grep '^A='; echo $A",
			"A=c\nc\n",
		},
		{
			opts(withPath("HOME=")),
			"echo $HOME",
			"\n",
		},
		{
			opts(withPath("PWD=foo")),
			"[[ $PWD == foo ]]",
			"exit status 1",
		},
		{
			opts(interp.Params("foo")),
			"echo $@",
			"foo\n",
		},
		{
			opts(interp.Params("-u", "--", "foo")),
			"echo $@; echo $unset",
			"foo\nunset: unbound variable\nexit status 1",
		},
		{
			opts(interp.Params("-u", "--", "foo")),
			"echo $@; echo ${unset:-default}",
			"foo\ndefault\n",
		},
		{
			opts(interp.Params("foo")),
			"set >/dev/null; echo $@",
			"foo\n",
		},
		{
			opts(interp.Params("foo")),
			"set -e; echo $@",
			"foo\n",
		},
		{
			opts(interp.Params("foo")),
			"set --; echo $@",
			"\n",
		},
		{
			opts(interp.Params("foo")),
			"set bar; echo $@",
			"bar\n",
		},
		{
			opts(interp.Env(expand.FuncEnviron(func(name string) string {
				if name == "foo" {
					return "bar"
				}
				return ""
			}))),
			"(echo $foo); echo x | echo $foo",
			"bar\nbar\n",
		},
	}
	p := syntax.NewParser()
	for _, c := range cases {
		t.Run("", func(t *testing.T) {
			skipIfUnsupported(t, c.in)
			file := parse(t, p, c.in)
			var cb concBuffer
			r, err := interp.New(append(c.opts,
				interp.StdIO(nil, &cb, &cb),
				interp.ExecHandlers(testExecHandler),
			)...)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), runnerRunTimeout)
			defer cancel()
			if err := r.Run(ctx, file); err != nil {
				cb.WriteString(err.Error())
			}
			if got := cb.String(); got != c.want {
				t.Fatalf("wrong output in %q:\nwant: %q\ngot:  %q",
					c.in, c.want, got)
			}
		})
	}
}

func TestRunnerContext(t *testing.T) {
	t.Parallel()

	cases := []string{
		"",
		"while true; do true; done",
		"until false; do true; done",
		"sleep 1000",
		"while true; do true; done & wait",
		"sleep 1000 & wait",
		"(while true; do true; done)",
		"$(while true; do true; done)",
		"while true; do true; done | while true; do true; done",
	}
	p := syntax.NewParser()
	for _, in := range cases {
		t.Run("", func(t *testing.T) {
			file := parse(t, p, in)
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			r, _ := interp.New()
			errChan := make(chan error)
			go func() {
				errChan <- r.Run(ctx, file)
			}()

			timeout := 500 * time.Millisecond
			select {
			case err := <-errChan:
				if err != nil && err != ctx.Err() {
					t.Fatal("Runner did not use ctx.Err()")
				}
			case <-time.After(timeout):
				t.Fatalf("program was not killed in %s", timeout)
			}
		})
	}
}

func TestCancelBlockedStdinRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		// TODO: Why is this? The [os.File.SetReadDeadline] docs seem to imply that it should work
		// across all major platforms, and the file polling  implementation seems to be
		// for all posix platforms including Windows.
		// Our previous logic and tests with muesli/cancelreader did not test an os.Pipe
		// on Windows either, so skipping here is not any worse.
		t.Skip("os.Pipe on windows appears to not support cancellable reads")
	}
	t.Parallel()

	p := syntax.NewParser()
	file := parse(t, p, "read x")
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	// Make the linter happy, even though we deliberately wait for the timeout.
	defer cancel()

	stdinRead, stdinWrite, err := os.Pipe()
	if err != nil {
		t.Fatalf("Error calling os.Pipe: %v", err)
	}
	defer func() {
		stdinWrite.Close()
		stdinRead.Close()
	}()
	r, _ := interp.New(interp.StdIO(stdinRead, nil, nil))
	now := time.Now()
	errChan := make(chan error)
	go func() {
		errChan <- r.Run(ctx, file)
	}()

	timeout := 500 * time.Millisecond
	select {
	case err := <-errChan:
		if err == nil || err.Error() != "exit status 1" || ctx.Err() != context.DeadlineExceeded {
			t.Fatalf("'read x' did not timeout correctly; err: %v, ctx.Err(): %v; dur: %v",
				err, ctx.Err(), time.Since(now))
		}
	case <-time.After(timeout):
		t.Fatalf("program was not killed in %s", timeout)
	}
}

func TestRunnerAltNodes(t *testing.T) {
	t.Parallel()

	in := "echo foo"
	file := parse(t, nil, in)
	want := "foo\n"
	nodes := []syntax.Node{
		file,
		file.Stmts[0],
		file.Stmts[0].Cmd,
	}
	for _, node := range nodes {
		var cb concBuffer
		r, _ := interp.New(interp.StdIO(nil, &cb, &cb))
		ctx, cancel := context.WithTimeout(t.Context(), runnerRunTimeout)
		defer cancel()
		if err := r.Run(ctx, node); err != nil {
			cb.WriteString(err.Error())
		}
		if got := cb.String(); got != want {
			t.Fatalf("wrong output in %q:\nwant: %q\ngot:  %q",
				in, want, got)
		}
	}
}

func TestRunnerDir(t *testing.T) {
	t.Parallel()

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Run("Missing", func(t *testing.T) {
		_, err := interp.New(interp.Dir("missing"))
		if err == nil {
			t.Fatal("expected New to error when Dir is missing")
		}
	})
	t.Run("NotDir", func(t *testing.T) {
		_, err := interp.New(interp.Dir("interp_test.go"))
		if err == nil {
			t.Fatal("expected New to error when Dir is not a dir")
		}
	})
	t.Run("NotDirAbs", func(t *testing.T) {
		_, err := interp.New(interp.Dir(filepath.Join(wd, "interp_test.go")))
		if err == nil {
			t.Fatal("expected New to error when Dir is not a dir")
		}
	})
	t.Run("Relative", func(t *testing.T) {
		// On Windows, it's impossible to make a relative path from one
		// drive to another. Use the parent directory, as that's for
		// sure in the same drive as the current directory.
		rel := ".." + string(filepath.Separator)
		r, err := interp.New(interp.Dir(rel))
		if err != nil {
			t.Fatal(err)
		}
		if !filepath.IsAbs(r.Dir) {
			t.Errorf("Runner.Dir is not absolute")
		}
	})
	// Ensure that we treat symlinks and short paths properly, especially
	// with Dir and globbing.
	t.Run("SymlinkOrShortPath", func(t *testing.T) {
		tdir := t.TempDir()

		realDir := filepath.Join(tdir, "real-long-dir-name")
		realFile := filepath.Join(realDir, "realfile")

		if err := os.Mkdir(realDir, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(realFile, []byte(""), 0o666); err != nil {
			t.Fatal(err)
		}

		var altDir string
		if runtime.GOOS == "windows" {
			short, err := shortPathName(realDir)
			if err != nil {
				t.Fatal(err)
			}
			altDir = short
			// We replace tdir later, and it might have been shortened.
			tdir = filepath.Dir(altDir)
		} else {
			altDir = filepath.Join(tdir, "symlink")
			if err := os.Symlink(realDir, altDir); err != nil {
				t.Fatal(err)
			}
		}

		var b bytes.Buffer
		r, err := interp.New(interp.Dir(altDir), interp.StdIO(nil, &b, &b))
		if err != nil {
			t.Fatal(err)
		}
		file := parse(t, nil, "echo $PWD $PWD/*")
		ctx, cancel := context.WithTimeout(t.Context(), runnerRunTimeout)
		defer cancel()
		if err := r.Run(ctx, file); err != nil {
			t.Fatal(err)
		}
		got := b.String()
		got = strings.ReplaceAll(got, tdir, "")
		got = strings.TrimSpace(got)
		want := `/symlink /symlink/realfile`
		if runtime.GOOS == "windows" {
			want = `\\REAL.{4} \\REAL.{4}\\realfile`
		}
		if !regexp.MustCompile(want).MatchString(got) {
			t.Fatalf("\nwant regexp: %q\ngot: %q", want, got)
		}
	})
}

func TestRunnerIncremental(t *testing.T) {
	t.Parallel()

	file := parse(t, nil, "echo foo; false; echo bar; exit 0; echo baz")
	want := "foo\nbar\n"
	var b bytes.Buffer
	r, _ := interp.New(interp.StdIO(nil, &b, &b))
	ctx, cancel := context.WithTimeout(t.Context(), runnerRunTimeout)
	defer cancel()
	for _, stmt := range file.Stmts {
		err := r.Run(ctx, stmt)
		if !errors.As(err, new(interp.ExitStatus)) && err != nil {
			// Keep track of unexpected errors.
			b.WriteString(err.Error())
		}
		if r.Exited() {
			break
		}
	}
	if got := b.String(); got != want {
		t.Fatalf("\nwant: %q\ngot:  %q", want, got)
	}
}

func TestRunnerIncrementalExitTrap(t *testing.T) {
	t.Parallel()

	file := parse(t, nil, "trap 'echo bye' EXIT\necho a\necho b\nexit 3\necho never")
	want := "a\nb\nbye\n"
	var b bytes.Buffer
	r, _ := interp.New(interp.StdIO(nil, &b, &b))
	ctx, cancel := context.WithTimeout(t.Context(), runnerRunTimeout)
	defer cancel()
	var exit interp.ExitStatus
	for _, stmt := range file.Stmts {
		err := r.Run(ctx, stmt)
		if err != nil && !errors.As(err, &exit) {
			b.WriteString(err.Error())
		}
		if r.Exited() {
			break
		}
	}
	if exit != 3 {
		t.Fatalf("want exit status 3, got %d", exit)
	}
	if got := b.String(); got != want {
		t.Fatalf("\nwant: %q\ngot:  %q", want, got)
	}
}

func TestRunnerResetFields(t *testing.T) {
	t.Parallel()

	tdir := t.TempDir()
	logPath := filepath.Join(tdir, "log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	r, _ := interp.New(
		interp.Params("-f", "--", "first", tdir, logPath),
		interp.Dir(tdir),
		interp.ExecHandlers(testExecHandler),
	)
	// Check that using option funcs and Runner fields directly is still
	// kept by Reset.
	interp.StdIO(nil, logFile, os.Stderr)(r)
	r.Env = expand.ListEnviron(append(os.Environ(), "GLOBAL=foo")...)

	file := parse(t, nil, `
# Params set 3 arguments
[[ $# -eq 3 ]] || exit 10
[[ $1 == "first" ]] || exit 11

# Params set the -f option (noglob)
[[ -o noglob ]] || exit 12

# $PWD was set via Dir, and should be equal to $2
[[ "$PWD" == "$2" ]] || exit 13

# stdout should go into the log file, which is at $3
echo line1
echo line2
[[ "$(wc -l <$3)" == "2" ]] || exit 14

# $GLOBAL was set directly via the Env field
[[ "$GLOBAL" == "foo" ]] || exit 15

# Change all of the above within the script. Reset should undo this.
set +f -- newargs
cd
exec >/dev/null 2>/dev/null
GLOBAL=
export GLOBAL=
`)
	ctx, cancel := context.WithTimeout(t.Context(), runnerRunTimeout)
	defer cancel()
	for i := range 3 {
		if err := r.Run(ctx, file); err != nil {
			t.Fatalf("run number %d: %v", i, err)
		}
		r.Reset()
		// empty the log file too
		logFile.Truncate(0)
		logFile.Seek(0, io.SeekStart)
	}
}

func TestRunnerManyResets(t *testing.T) {
	t.Parallel()
	r, _ := interp.New()
	for range 5 {
		r.Reset()
	}
}

func TestRunnerFilename(t *testing.T) {
	t.Parallel()

	want := "f.sh\n"
	file, _ := syntax.NewParser().Parse(strings.NewReader("echo $0"), "f.sh")
	var b bytes.Buffer
	r, _ := interp.New(interp.StdIO(nil, &b, &b))
	ctx, cancel := context.WithTimeout(t.Context(), runnerRunTimeout)
	defer cancel()
	if err := r.Run(ctx, file); err != nil {
		t.Fatal(err)
	}
	if got := b.String(); got != want {
		t.Fatalf("\nwant: %q\ngot:  %q", want, got)
	}
}

func TestRunnerEnvNoModify(t *testing.T) {
	t.Parallel()

	env := expand.ListEnviron("one=1", "two=2")
	file := parse(t, nil, `echo -n "$one $two; "; one=x; unset two`)

	var b bytes.Buffer
	r, _ := interp.New(interp.Env(env), interp.StdIO(nil, &b, &b))
	ctx, cancel := context.WithTimeout(t.Context(), runnerRunTimeout)
	defer cancel()
	for range 3 {
		r.Reset()
		err := r.Run(ctx, file)
		if err != nil {
			t.Fatal(err)
		}
	}

	want := "1 2; 1 2; 1 2; "
	if got := b.String(); got != want {
		t.Fatalf("\nwant: %q\ngot:  %q", want, got)
	}
}

func TestRunnerASTNoModify(t *testing.T) {
	t.Parallel()

	file := parse(t, nil, "shopt -s expand_aliases; alias foo=echo\nfoo bar")
	printer := syntax.NewPrinter()
	var sb strings.Builder
	printer.Print(&sb, file)
	before := sb.String()

	var b bytes.Buffer
	r, _ := interp.New(interp.StdIO(nil, &b, &b))
	ctx, cancel := context.WithTimeout(t.Context(), runnerRunTimeout)
	defer cancel()
	if err := r.Run(ctx, file); err != nil {
		t.Fatal(err)
	}
	if want := "bar\n"; b.String() != want {
		t.Fatalf("want output %q, got %q", want, b.String())
	}

	sb.Reset()
	printer.Print(&sb, file)
	after := sb.String()
	if after != before {
		t.Fatalf("Run modified the AST:\nbefore: %q\nafter:  %q", before, after)
	}
}

func TestMalformedPathOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Skipping windows test on non-windows GOOS")
	}
	tdir := t.TempDir()
	t.Parallel()

	path := filepath.Join(tdir, "test.cmd")
	script := []byte("@echo foo")
	if err := os.WriteFile(path, script, 0o777); err != nil {
		t.Fatal(err)
	}

	// set PATH to c:\tmp\dir instead of C:\tmp\dir
	volume := filepath.VolumeName(tdir)
	pathList := strings.ToLower(volume) + tdir[len(volume):]

	file := parse(t, nil, "test.cmd")
	var cb concBuffer
	r, _ := interp.New(interp.Env(expand.ListEnviron("PATH="+pathList)), interp.StdIO(nil, &cb, &cb))
	ctx, cancel := context.WithTimeout(t.Context(), runnerRunTimeout)
	defer cancel()
	if err := r.Run(ctx, file); err != nil {
		t.Fatal(err)
	}
	want := "foo\r\n"
	if got := cb.String(); got != want {
		t.Fatalf("wrong output:\nwant: %q\ngot:  %q", want, got)
	}
}

func TestReadShouldNotPanicWithNilStdin(t *testing.T) {
	t.Parallel()

	r, err := interp.New()
	if err != nil {
		t.Fatal(err)
	}

	f := parse(t, nil, "read foobar")
	ctx, cancel := context.WithTimeout(t.Context(), runnerRunTimeout)
	defer cancel()
	if err := r.Run(ctx, f); err == nil {
		t.Fatal("it should have returned an error")
	}
}

func TestRunnerVars(t *testing.T) {
	t.Parallel()

	r, err := interp.New()
	if err != nil {
		t.Fatal(err)
	}

	f := parse(t, nil, "foo=updated; BAR=new")
	ctx, cancel := context.WithTimeout(t.Context(), runnerRunTimeout)
	defer cancel()
	if err := r.Run(ctx, f); err != nil {
		t.Fatal(err)
	}

	if want, got := "updated", r.Vars["foo"].String(); got != want {
		t.Fatalf("wrong output:\nwant: %q\ngot:  %q", want, got)
	}
}

func TestRunnerSubshell(t *testing.T) {
	t.Parallel()

	r1, err := interp.New()
	if err != nil {
		t.Fatal(err)
	}

	r2 := r1.Subshell()
	f1 := parse(t, nil, "PARENT=foo")
	f2 := parse(t, nil, "CHILD=bar")

	ctx, cancel := context.WithTimeout(t.Context(), runnerRunTimeout)
	defer cancel()
	if err := r1.Run(ctx, f1); err != nil {
		t.Fatal(err)
	}
	if err := r2.Run(ctx, f2); err != nil {
		t.Fatal(err)
	}

	if want, got := "foo", r1.Vars["PARENT"].String(); got != want {
		t.Fatalf("wrong output:\nwant: %q\ngot:  %q", want, got)
	}
	if want, got := "bar", r2.Vars["CHILD"].String(); got != want {
		t.Fatalf("wrong output:\nwant: %q\ngot:  %q", want, got)
	}

	r3 := r2.Subshell()
	f3 := parse(t, nil, "CHILD=modified")
	if err := r3.Run(ctx, f3); err != nil {
		t.Fatal(err)
	}
	if want, got := "bar", r2.Vars["CHILD"].String(); got != want {
		t.Fatalf("wrong output:\nwant: %q\ngot:  %q", want, got)
	}
	if want, got := "modified", r3.Vars["CHILD"].String(); got != want {
		t.Fatalf("wrong output:\nwant: %q\ngot:  %q", want, got)
	}
}

func TestRunnerNonFileStdin(t *testing.T) {
	t.Parallel()

	var cb concBuffer
	r, err := interp.New(interp.StdIO(strings.NewReader("a\nb\nc\n"), &cb, &cb))
	if err != nil {
		t.Fatal(err)
	}
	file := parse(t, nil, "while read a; do echo $a; GOSH_CMD=print_ok $GOSH_PROG; done")
	ctx, cancel := context.WithTimeout(t.Context(), runnerRunTimeout)
	defer cancel()
	if err := r.Run(ctx, file); err != nil {
		cb.WriteString(err.Error())
	}
	// TODO: just like with heredocs, the first print_ok call consumes all stdin.
	qt.Assert(t, qt.Equals(cb.String(), "a\nexec ok\nb\nexec ok\nc\nexec ok\n"))
}
