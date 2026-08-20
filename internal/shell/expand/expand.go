// Copyright (c) 2017, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

package expand

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"maps"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/blairham/koi-shell/internal/shell/shinternal"
	"mvdan.cc/sh/v3/pattern"
	"mvdan.cc/sh/v3/syntax"
)

// A Config specifies details about how shell expansion should be performed. The
// zero value is a valid configuration.
type Config struct {
	// Env is used to get and set environment variables when performing
	// shell expansions. Some special parameters are also expanded via this
	// interface, such as:
	//
	//   * "#", "@", "*", "0"-"9" for the shell's parameters
	//   * "?", "$", "PPID" for the shell's status and process
	//   * "HOME foo" to retrieve user foo's home directory (if unset,
	//     os/user.Lookup will be used)
	//
	// If nil, there are no environment variables set. Use
	// ListEnviron(os.Environ()...) to use the system's environment
	// variables.
	Env Environ

	// CmdSubst expands a command substitution node, writing its standard
	// output to the provided [io.Writer].
	//
	// If nil, encountering a command substitution will result in an
	// UnexpectedCommandError.
	CmdSubst func(io.Writer, *syntax.CmdSubst) error

	// ProcSubst expands a process substitution node.
	ProcSubst func(*syntax.ProcSubst) (string, error)

	// TODO(v4): replace ReadDir with ReadDir2.

	// ReadDir is the older form of [ReadDir2], before io/fs.
	//
	// Deprecated: use ReadDir2 instead.
	ReadDir func(string) ([]fs.FileInfo, error)

	// ReadDir2 is used for file path globbing.
	// If nil, and [ReadDir] is nil as well, globbing is disabled.
	// Use [os.ReadDir] to use the filesystem directly.
	ReadDir2 func(string) ([]fs.DirEntry, error)

	// GlobStar corresponds to the shell option which allows globbing with "**".
	GlobStar bool

	// DotGlob corresponds to the shell option which allows filenames beginning
	// with a dot to be matched by a pattern which does not begin with a dot.
	DotGlob bool

	// NoCaseGlob corresponds to the shell option which causes case-insensitive
	// pattern matching in pathname expansion.
	NoCaseGlob bool

	// NullGlob corresponds to the shell option which allows globbing
	// patterns which match nothing to result in zero fields.
	NullGlob bool

	// NoBraces disables brace expansion, which is what `set +B` asks for.
	// The word is then left exactly as written -- `a{1,2}` is the string
	// `a{1,2}` -- rather than being expanded and the results discarded.
	NoBraces bool

	// NoUnset corresponds to the shell option which treats unset variables
	// as errors.
	NoUnset bool

	// ExtGlob corresponds to the shell option which allows using extended
	// pattern matching features when performing pathname expansion (globbing).
	ExtGlob bool

	// LineOffset shifts what $LINENO reports. A trap action is parsed as
	// its own little file whose positions start at line 1, while bash
	// reports lines continuing from a base — the triggering command's
	// line for DEBUG and ERR, the line the trap was set on for EXIT and
	// signal traps (#352). The offset is that base minus one; zero means
	// positions report as written, which is every non-trap expansion.
	LineOffset uint64

	bufferAlloc strings.Builder
	fieldAlloc  [4]fieldPart
	fieldsAlloc [4][]fieldPart

	ifs string
	// A pointer to a parameter expansion node, if we're inside one.
	// Necessary for ${LINENO}.
	curParam *syntax.ParamExp

	// wordResult records that a paramExp's answer was the operator's own
	// word — ${x:+word}, ${x-word}, ${x:=word} — so a caller that is
	// expanding a word can re-expand it in its own quoting context: the
	// flat string paramExp returns loses quoted nulls and inner "$@"
	// (#358). Keyed by the expansion node, so a *nested* expansion's
	// marker can never be mistaken for the outer one; readers clear the
	// pair, call paramExp, and only trust a marker naming the node they
	// passed.
	wordResult   *syntax.Word
	wordResultPe *syntax.ParamExp

	// paramOuterQuote is the quoting context surrounding the parameter
	// expansion being evaluated — set around each paramExp call — and
	// paramQuoteCtx is that context made visible to the operator word's
	// own expansion, for the ${x+word} family only. Inside a
	// double-quoted ${...}, bash keeps a single quote *literal* (#359):
	// "${IFS+'bar'}" prints 'bar' with its quotes, and "${IFS+'$a'}"
	// still expands $a between them. Inside a heredoc's ${...} the whole
	// quoted span stays as written, $'..' included. Pattern and
	// replacement operators are excluded — there quotes really do quote,
	// which is how "${a#'f'}" strips an f.
	paramOuterQuote quoteLevel
	paramQuoteCtx   quoteLevel

	// assignValue marks that a variable assignment's value is being
	// expanded, where bash also tilde-expands after each unquoted colon
	// (#364): p=/bin:~/bin reads the home directory. Set by
	// [LiteralAssign] only.
	assignValue bool

	// arithStrDepth bounds arithmWordStr's re-evaluation (#366): x=y
	// with y=x would otherwise chase names forever.
	arithStrDepth int
}

// UnexpectedCommandError is returned if a command substitution is encountered
// when [Config.CmdSubst] is nil.
type UnexpectedCommandError struct {
	Node *syntax.CmdSubst
}

func (u UnexpectedCommandError) Error() string {
	return fmt.Sprintf("unexpected command substitution at %s", u.Node.Pos())
}

var zeroConfig = &Config{}

// TODO: note that prepareConfig is modifying the user's config in place,
// which doesn't feel right - we should make a copy.

func prepareConfig(cfg *Config) *Config {
	cfg = cmp.Or(cfg, zeroConfig)
	cfg.Env = cmp.Or(cfg.Env, FuncEnviron(func(string) string { return "" }))

	cfg.ifs = " \t\n"
	if vr := cfg.Env.Get("IFS"); vr.IsSet() {
		cfg.ifs = vr.String()
	}

	if cfg.ReadDir != nil && cfg.ReadDir2 == nil {
		cfg.ReadDir2 = func(path string) ([]fs.DirEntry, error) {
			infos, err := cfg.ReadDir(path)
			if err != nil {
				return nil, err
			}
			entries := make([]fs.DirEntry, len(infos))
			for i, info := range infos {
				entries[i] = fs.FileInfoToDirEntry(info)
			}
			return entries, nil
		}
	}
	return cfg
}

func (cfg *Config) ifsRune(r rune) bool {
	return strings.ContainsRune(cfg.ifs, r)
}

// ifsWhitespace reports whether r is a space, tab, or newline present in IFS.
func (cfg *Config) ifsWhitespace(r rune) bool {
	return (r == ' ' || r == '\t' || r == '\n') && cfg.ifsRune(r)
}

func (cfg *Config) ifsJoin(strs []string) string {
	sep := ""
	if cfg.ifs != "" {
		// The separator is the first character of IFS, not the first byte.
		_, size := utf8.DecodeRuneInString(cfg.ifs)
		sep = cfg.ifs[:size]
	}
	return strings.Join(strs, sep)
}

func (cfg *Config) strBuilder() *strings.Builder {
	b := &cfg.bufferAlloc
	b.Reset()
	return b
}

func (cfg *Config) envGet(name string) string {
	return cfg.Env.Get(name).String()
}

func (cfg *Config) envSet(name, value string) error {
	wenv, ok := cfg.Env.(WriteEnviron)
	if !ok {
		return fmt.Errorf("environment is read-only")
	}
	// An arithmetic assignment writes the value, not a fresh variable:
	// building one from scratch stripped declare -i (and -x) from the
	// target, so `declare -i j=8; let j=j+1` left j a plain string and
	// every later assignment stored literals (#368).
	prev := cfg.Env.Get(name)
	return wenv.Set(name, Variable{
		Set:      true,
		Kind:     String,
		Str:      value,
		Local:    prev.Local,
		Exported: prev.Exported,
		ReadOnly: prev.ReadOnly,
		Integer:  prev.Integer,
	})
}

// Literal expands a single shell word. It is similar to [Fields], but the result
// is a single string. This is the behavior when a word is used as the value in
// a shell variable assignment, for example.
//
// The config specifies shell expansion options; nil behaves the same as an
// empty config.
func Literal(cfg *Config, word *syntax.Word) (string, error) {
	if word == nil {
		return "", nil
	}
	cfg = prepareConfig(cfg)
	field, err := cfg.wordField(word.Parts, quoteNone)
	if err != nil {
		return "", err
	}
	return cfg.fieldJoin(field), nil
}

// LiteralAssign is [Literal] for a variable assignment's value, where
// bash also tilde-expands after each unquoted colon (#364).
func LiteralAssign(cfg *Config, word *syntax.Word) (string, error) {
	if word == nil {
		return "", nil
	}
	cfg = prepareConfig(cfg)
	old := cfg.assignValue
	cfg.assignValue = true
	s, err := Literal(cfg, word)
	cfg.assignValue = old
	return s, err
}

// Document expands a single shell word as if it were a here-document body.
// It is similar to [Literal], but without brace expansion, tilde expansion, and
// globbing.
//
// The config specifies shell expansion options; nil behaves the same as an
// empty config.
func Document(cfg *Config, word *syntax.Word) (string, error) {
	if word == nil {
		return "", nil
	}
	cfg = prepareConfig(cfg)
	field, err := cfg.wordField(word.Parts, quoteHeredoc)
	if err != nil {
		return "", err
	}
	return cfg.fieldJoin(field), nil
}

// Pattern expands a single shell word as a pattern, using [pattern.QuoteMeta]
// on any non-quoted parts of the input word. The result can be used on
// [pattern.Regexp] directly.
//
// The config specifies shell expansion options; nil behaves the same as an
// empty config.
func Pattern(cfg *Config, word *syntax.Word) (string, error) {
	if word == nil {
		return "", nil
	}
	cfg = prepareConfig(cfg)
	field, err := cfg.wordFieldMode(word.Parts, quoteNone, true)
	if err != nil {
		return "", err
	}
	sb := cfg.strBuilder()
	for _, part := range field {
		if part.quote > quoteNone {
			sb.WriteString(pattern.QuoteMeta(part.val, 0))
		} else {
			sb.WriteString(part.val)
		}
	}
	return sb.String(), nil
}

// Format expands a format string with a number of arguments, following the
// shell's format specifications. These include printf(1), among others.
//
// The resulting string is returned, along with the number of arguments used.
// Note that the resulting string may contain null bytes, for example
// if the format string used `\x00`. The caller should terminate the string
// at the first null byte if needed, such as when expanding for `$'foo\x00bar'`.
//
// The config specifies shell expansion options; nil behaves the same as an
// empty config.
func Format(cfg *Config, format string, args []string) (string, int, error) {
	cfg = prepareConfig(cfg)
	sb := cfg.strBuilder()

	consumed, err := formatInto(sb, format, args, false)
	if err != nil {
		return "", 0, err
	}

	return sb.String(), consumed, err
}

// formatDollar expands a $'...' body. It differs from a printf format
// in two escapes bash gives only to dollar quotes (#365): \x{...}, the
// brace form of hex, and \cX control-character notation — printf's own
// \c means "stop output" in %b and stays literal in a format, so the
// contexts cannot share one rule.
func formatDollar(cfg *Config, s string) string {
	sb := cfg.strBuilder()
	formatInto(sb, s, nil, true) //nolint:errcheck // like Format's callers here, errors keep the text
	return sb.String()
}

func formatInto(sb *strings.Builder, format string, args []string, dollarQuote bool) (int, error) {
	var fmts []byte
	initialArgs := len(args)

	for i := 0; i < len(format); i++ {
		// readDigits reads from 0 to max digits, either octal or
		// hexadecimal.
		readDigits := func(max int, hex bool) string {
			j := 0
			for ; j < max && i+j < len(format); j++ {
				c := format[i+j]
				if (c >= '0' && c <= '9') ||
					(hex && c >= 'a' && c <= 'f') ||
					(hex && c >= 'A' && c <= 'F') {
					// valid octal or hex char
				} else {
					break
				}
			}
			digits := format[i : i+j]
			i += j - 1 // -1 since the outer loop does i++
			return digits
		}
		c := format[i]
		switch {
		case c == '\\': // escaped
			i++
			if i >= len(format) {
				sb.WriteByte('\\')
				break
			}
			switch c = format[i]; c {
			case 'a': // bell
				sb.WriteByte('\a')
			case 'b': // backspace
				sb.WriteByte('\b')
			case 'e', 'E': // escape
				sb.WriteByte('\x1b')
			case 'f': // form feed
				sb.WriteByte('\f')
			case 'n': // new line
				sb.WriteByte('\n')
			case 'r': // carriage return
				sb.WriteByte('\r')
			case 't': // horizontal tab
				sb.WriteByte('\t')
			case 'v': // vertical tab
				sb.WriteByte('\v')
			case '\\', '\'', '"', '?': // just the character
				sb.WriteByte(c)
			case '0', '1', '2', '3', '4', '5', '6', '7':
				digits := readDigits(3, false)
				// if digits don't fit in 8 bits, 0xff via strconv
				n, _ := strconv.ParseUint(digits, 8, 8)
				sb.WriteByte(byte(n))
			case 'c':
				// $'\cX': the byte is toupper(X) ^ 0x40 — \ca is 0x01,
				// \c? is DEL (#365). A trailing \c stays literal, and
				// \c\\ consumes the doubled backslash as its X.
				if !dollarQuote || i+1 >= len(format) {
					sb.WriteString(`\c`)
					break
				}
				i++
				x := format[i]
				if x == '\\' && i+1 < len(format) && format[i+1] == '\\' {
					i++
				}
				if x >= 'a' && x <= 'z' {
					x -= 'a' - 'A'
				}
				sb.WriteByte(x ^ 0x40)
			case 'x', 'u', 'U':
				if c == 'x' && dollarQuote && i+1 < len(format) && format[i+1] == '{' {
					// $'\x{...}': bash 5.3's brace form (#365). Hex
					// digits until the brace or the first non-hex byte
					// — the closing brace is optional, measured — the
					// value masked to one byte, and empty braces yield
					// NUL, which truncates the string the way bash's
					// does.
					i += 2
					j := i
					for j < len(format) && isHexDigit(format[j]) {
						j++
					}
					digits := format[i:j]
					i = j
					if i >= len(format) || format[i] != '}' {
						i-- // no closing brace; the outer loop advances
					}
					if len(digits) == 0 {
						sb.WriteByte(0)
						break
					}
					if len(digits) > 2 {
						digits = digits[len(digits)-2:]
					}
					n, _ := strconv.ParseUint(digits, 16, 8)
					sb.WriteByte(byte(n))
					break
				}
				i++
				max := 2
				switch c {
				case 'u':
					max = 4
				case 'U':
					max = 8
				}
				digits := readDigits(max, true)
				if len(digits) > 0 {
					// can't error
					n, _ := strconv.ParseUint(digits, 16, 32)
					if c == 'x' {
						// always as a single byte
						sb.WriteByte(byte(n))
					} else {
						sb.WriteRune(rune(n))
					}
					break
				}
				fallthrough
			default: // no escape sequence
				sb.WriteByte('\\')
				sb.WriteByte(c)
			}
		case len(fmts) > 0:
			switch c {
			case '%':
				sb.WriteByte('%')
				fmts = nil
			case 'c':
				var b byte
				if len(args) > 0 {
					arg := ""
					arg, args = args[0], args[1:]
					if len(arg) > 0 {
						b = arg[0]
					}
				}
				sb.WriteByte(b)
				fmts = nil
			case '+', '-', ' ':
				if len(fmts) > 1 {
					return 0, fmt.Errorf("invalid format char: %c", c)
				}
				fmts = append(fmts, c)
			case '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
				fmts = append(fmts, c)
			case 's', 'b', 'd', 'i', 'u', 'o', 'x':
				arg := ""
				if len(args) > 0 {
					arg, args = args[0], args[1:]
				}
				var farg any
				if c == 'b' {
					// Passing in nil for args ensures that % format
					// strings aren't processed; only escape sequences
					// will be handled.
					_, err := formatInto(sb, arg, nil, false)
					if err != nil {
						return 0, err
					}
				} else if c != 's' {
					n, _ := strconv.ParseInt(arg, 0, 0)
					if c == 'i' || c == 'd' {
						farg = int(n)
					} else {
						farg = uint(n)
					}
					if c == 'i' || c == 'u' {
						c = 'd'
					}
				} else {
					farg = arg
				}
				if farg != nil {
					fmts = append(fmts, c)
					fmt.Fprintf(sb, string(fmts), farg)
				}
				fmts = nil
			default:
				return 0, fmt.Errorf("invalid format char: %c", c)
			}
		case args != nil && c == '%':
			// if args == nil, we are not doing format
			// arguments
			fmts = []byte{c}
		default:
			sb.WriteByte(c)
		}
	}
	if len(fmts) > 0 {
		return 0, fmt.Errorf("missing format char")
	}
	return initialArgs - len(args), nil
}

func (cfg *Config) fieldJoin(parts []fieldPart) string {
	switch len(parts) {
	case 0:
		return ""
	case 1: // short-cut without a string copy
		return parts[0].val
	}
	sb := cfg.strBuilder()
	for _, part := range parts {
		sb.WriteString(part.val)
	}
	return sb.String()
}

func (cfg *Config) escapedGlobField(parts []fieldPart) (escaped string, glob bool) {
	candidate := false
	for _, part := range parts {
		if part.quote == quoteNone && strings.ContainsAny(part.val, "*?[") {
			candidate = true
			break
		}
	}
	if !candidate {
		return "", false
	}
	sb := cfg.strBuilder()
	for _, part := range parts {
		if part.quote > quoteNone {
			sb.WriteString(pattern.QuoteMeta(part.val, 0))
		} else {
			sb.WriteString(part.val)
		}
	}
	// Check the entire escaped word, as a bracket expression could span
	// multiple unquoted parts, such as `[a$x` where x holds "]".
	escaped = sb.String()
	if pattern.HasMeta(escaped, 0) {
		return escaped, true
	}
	return "", false
}

// Fields is a pre-iterators API which now wraps [FieldsSeq].
func Fields(cfg *Config, words ...*syntax.Word) ([]string, error) {
	var fields []string
	for s, err := range FieldsSeq(cfg, words...) {
		if err != nil {
			return nil, err
		}
		fields = append(fields, s)
	}
	return fields, nil
}

// FieldsSeq expands a number of words as if they were arguments in a shell
// command. This includes brace expansion, tilde expansion, parameter expansion,
// command substitution, arithmetic expansion, quote removal, and globbing.
func FieldsSeq(cfg *Config, words ...*syntax.Word) iter.Seq2[string, error] {
	cfg = prepareConfig(cfg)
	dir := cfg.envGet("PWD")
	return func(yield func(string, error) bool) {
		expandWord := func(w *syntax.Word) (stop bool) {
			wfields, err := cfg.wordFields(w.Parts)
			if err != nil {
				yield("", err)
				return true
			}
			for _, field := range wfields {
				path, doGlob := cfg.escapedGlobField(field)
				if doGlob && cfg.ReadDir2 != nil {
					// Note that globbing requires keeping a slice state, so it doesn't
					// really benefit from using an iterator.
					matches, err := cfg.glob(dir, path)
					if err != nil {
						// We avoid [errors.As] as it allocates,
						// and we know that [Config.glob] returns [pattern.Regexp] errors without wrapping.
						if _, ok := err.(*pattern.SyntaxError); !ok {
							yield("", err)
							return true
						}
					} else if len(matches) > 0 || cfg.NullGlob {
						for _, m := range matches {
							if !yield(m, nil) {
								return true
							}
						}
						continue
					}
				}
				if !yield(cfg.fieldJoin(field), nil) {
					return true
				}
			}
			return false
		}
		for _, word := range words {
			word := *word // make a copy, since SplitBraces replaces the Parts slice
			if cfg.NoBraces || !syntax.SplitBraces(&word) {
				if expandWord(&word) {
					return
				}
				continue
			}
			for w, err := range BracesSeq(cfg, &word) {
				if err != nil {
					yield("", err)
					return
				}
				mergeBraceParams(w)
				if expandWord(w) {
					return
				}
			}
		}
	}
}

// mergeBraceParams rejoins a short-form $var with a literal glued to it
// by brace expansion (#363): bash expands braces before parameters,
// textually, so $var{x,y} means $varx $vary — the brace suffix extends
// the variable's *name*. The parser necessarily bound $var first, so
// after the split a short-form parameter followed by name characters
// re-reads as the longer name. ${var}{x,y} keeps its boundary, and a
// special parameter like $1 cannot be extended, since $1x never was a
// name. Nodes are replaced, never edited: the parts may be shared with
// other brace alternatives.
func mergeBraceParams(w *syntax.Word) {
	cloned := false
	for i := 0; i+1 < len(w.Parts); i++ {
		pe, ok := w.Parts[i].(*syntax.ParamExp)
		if !ok || !pe.Short || pe.Param == nil || !syntax.ValidName(pe.Param.Value) {
			continue
		}
		lit, ok := w.Parts[i+1].(*syntax.Lit)
		if !ok {
			continue
		}
		j := 0
		for j < len(lit.Value) {
			b := lit.Value[j]
			if b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' {
				j++
				continue
			}
			break
		}
		if j == 0 {
			continue
		}
		if !cloned {
			// The parts slice may share its backing with other brace
			// alternatives; never write into it.
			w.Parts = slices.Clone(w.Parts)
			cloned = true
		}
		pe2 := *pe
		param := *pe.Param
		param.Value += lit.Value[:j]
		pe2.Param = &param
		w.Parts[i] = &pe2
		if j == len(lit.Value) {
			w.Parts = slices.Delete(w.Parts, i+1, i+2)
			continue
		}
		lit2 := *lit
		lit2.Value = lit.Value[j:]
		w.Parts[i+1] = &lit2
	}
}

type fieldPart struct {
	val   string
	quote quoteLevel
}

type quoteLevel uint

const (
	quoteNone quoteLevel = iota
	quoteDouble
	quoteHeredoc
	quoteSingle
)

func (cfg *Config) wordField(wps []syntax.WordPart, ql quoteLevel) ([]fieldPart, error) {
	return cfg.wordFieldMode(wps, ql, false)
}

// wordFieldMode is [Config.wordField] with the backslash question made
// explicit. In an unquoted word, `\X` is a quoting: quote removal drops
// the backslash and keeps X literal, so an assignment value or a case
// subject written `a\;b` reads back `a;b` (#357). A *pattern* is the one
// consumer that must keep the backslash — there `\*` means a literal
// star at match time, and stripping it here would turn it into a glob —
// so [Pattern] passes keepEscapes and everything else does not.
func (cfg *Config) wordFieldMode(wps []syntax.WordPart, ql quoteLevel, keepEscapes bool) ([]fieldPart, error) {
	var field []fieldPart
	for i, wp := range wps {
		switch wp := wp.(type) {
		case *syntax.Lit:
			s := wp.Value
			// No tilde expansion inside a quoted ${x+word}: "${u:-~}"
			// prints a literal tilde in bash (#360), while the unquoted
			// form expands it.
			if i == 0 && ql == quoteNone && cfg.paramQuoteCtx == quoteNone {
				if prefix, rest := cfg.expandUser(s, len(wps) > 1); prefix != "" {
					// TODO: return two separate fieldParts,
					// like in wordFields?
					s = prefix + rest
				}
			}
			if cfg.assignValue && ql == quoteNone {
				// Before backslash processing, so an escaped colon does
				// not read as a tilde position.
				s = cfg.expandTildesAfterColons(s, false)
			}
			if (ql == quoteDouble || ql == quoteHeredoc) && strings.Contains(s, "\\") {
				sb := cfg.strBuilder()
				for i := 0; i < len(s); i++ {
					b := s[i]
					if b == '\\' && i+1 < len(s) {
						switch s[i+1] {
						case '"':
							if ql != quoteDouble {
								break
							}
							fallthrough
						case '\\', '$', '`': // special chars
							i++
							b = s[i] // write the special char, skipping the backslash
						}
					}
					sb.WriteByte(b)
				}
				s = sb.String()
			} else if ql == quoteNone && !keepEscapes && strings.Contains(s, "\\") {
				// Unquoted: the backslash quotes the next byte, and quote
				// removal drops it — the same pass wordFields applies to
				// command words (#357).
				sb := cfg.strBuilder()
				for i := 0; i < len(s); i++ {
					b := s[i]
					if b == '\\' {
						if i++; i >= len(s) {
							sb.WriteByte(b)
							break
						}
						b = s[i]
					}
					sb.WriteByte(b)
				}
				s = sb.String()
			}
			s, _, _ = strings.Cut(s, "\x00") // TODO: why is this needed?
			field = append(field, fieldPart{val: s})
		case *syntax.SglQuoted:
			if ql == quoteNone && cfg.paramQuoteCtx == quoteHeredoc {
				// A heredoc's ${x+word}: the quoted span stays exactly
				// as written, $'..' included (#359).
				val := "'" + wp.Value + "'"
				if wp.Dollar {
					val = "$" + val
				}
				field = append(field, fieldPart{quote: quoteSingle, val: val})
				continue
			}
			if ql == quoteNone && cfg.paramQuoteCtx == quoteDouble && !wp.Dollar {
				// A double-quoted ${x+word}: the single quotes are
				// literal text and what sits between them still expands
				// (#359) — "${IFS+'$a'}" prints the value in quotes. The
				// span re-reads under heredoc rules, which are exactly
				// that: quotes literal, expansions live.
				w, err := syntax.NewParser().Document(strings.NewReader("'" + wp.Value + "'"))
				if err == nil && w != nil {
					sub, err := cfg.wordFieldMode(w.Parts, quoteHeredoc, false)
					if err != nil {
						return nil, err
					}
					field = append(field, sub...)
					continue
				}
			}
			fp := fieldPart{quote: quoteSingle, val: wp.Value}
			if wp.Dollar {
				fp.val = formatDollar(cfg, fp.val)
				fp.val, _, _ = strings.Cut(fp.val, "\x00") // cut the string if format included \x00
			}
			field = append(field, fp)
		case *syntax.DblQuoted:
			wfield, err := cfg.wordField(wp.Parts, quoteDouble)
			if err != nil {
				return nil, err
			}
			for _, part := range wfield {
				part.quote = quoteDouble
				field = append(field, part)
			}
		case *syntax.ParamExp:
			oldOuter := cfg.paramOuterQuote
			cfg.paramOuterQuote = ql
			val, err := cfg.paramExp(wp)
			cfg.paramOuterQuote = oldOuter
			if err != nil {
				return nil, err
			}
			field = append(field, fieldPart{val: val})
		case *syntax.CmdSubst:
			val, err := cfg.cmdSubst(wp)
			if err != nil {
				return nil, err
			}
			field = append(field, fieldPart{val: val})
		case *syntax.ArithmExp:
			n, err := Arithm(cfg, wp.X)
			if err != nil {
				return nil, err
			}
			field = append(field, fieldPart{val: strconv.Itoa(n)})
		case *syntax.ProcSubst:
			path, err := cfg.ProcSubst(wp)
			if err != nil {
				return nil, err
			}
			field = append(field, fieldPart{val: path})
		case *syntax.ExtGlob:
			// Like how [Config.wordFields] deals with [syntax.ExtGlob],
			// except that we allow these through even when [Config.ExtGlob]
			// is false, as it only applies to pathname expansion.
			field = append(field, fieldPart{val: wp.Op.String() + wp.Pattern.Value + ")"})
		default:
			panic(fmt.Sprintf("unhandled word part: %T", wp))
		}
	}
	return field, nil
}

func (cfg *Config) cmdSubst(cs *syntax.CmdSubst) (string, error) {
	if cfg.CmdSubst == nil {
		return "", UnexpectedCommandError{Node: cs}
	}
	sb := cfg.strBuilder()
	if err := cfg.CmdSubst(sb, cs); err != nil {
		return "", err
	}
	out := sb.String()
	out = strings.ReplaceAll(out, "\x00", "")
	return strings.TrimRight(out, "\n"), nil
}

func (cfg *Config) wordFields(wps []syntax.WordPart) ([][]fieldPart, error) {
	return cfg.wordFieldsBuf(wps, true, false)
}

// wordFieldsBuf is [Config.wordFields] with two knobs for expanding a
// ${x:+word}'s word (#358). The buffer reuse is optional because the
// shared per-Config field buffers cannot back two live calls at once,
// and that expansion recurses while the outer call's slices still alias
// them. splitLits makes literal text split like an expansion's result:
// inside ${...} a word may carry literal whitespace, and bash splits the
// operator's answer wherever it was not quoted — including at a
// backslash-escaped space, which quote removal has already unescaped by
// the time splitting looks (measured: ${a:=a\ b} yields two fields).
func (cfg *Config) wordFieldsBuf(wps []syntax.WordPart, useAlloc, splitLits bool) ([][]fieldPart, error) {
	var fields [][]fieldPart
	var curField []fieldPart
	if useAlloc {
		fields = cfg.fieldsAlloc[:0]
		curField = cfg.fieldAlloc[:0]
	}
	allowEmpty := false
	flush := func() {
		if len(curField) == 0 {
			return
		}
		fields = append(fields, curField)
		curField = nil
	}
	// POSIX field splitting distinguishes IFS whitespace from the other
	// IFS bytes (#356): whitespace runs collapse and never delimit empty
	// fields, while each non-whitespace delimiter — together with any
	// adjacent IFS whitespace — terminates exactly one field, which may
	// be empty. So ":a::b:" under IFS=: splits into ("", a, "", b): the
	// leading colon yields an empty first field, the doubled one an
	// empty middle, and the trailing one nothing, because the end of the
	// word never makes a field. delimPending tracks whether a
	// non-whitespace delimiter arriving here would own an empty field —
	// true at the start of the word and right after another such
	// delimiter, false once IFS whitespace has closed a field with real
	// content, since that whitespace and the delimiter merge into one
	// separator.
	delimPending := true
	splitAdd := func(val string) {
		fieldStart := -1
		haveContent := func() bool { return fieldStart >= 0 || len(curField) > 0 }
		closeField := func(end int) {
			if fieldStart >= 0 {
				curField = append(curField, fieldPart{val: val[fieldStart:end]})
				fieldStart = -1
			}
			flush()
		}
		for i, r := range val {
			switch {
			case !cfg.ifsRune(r):
				if fieldStart < 0 { // starting a new field
					fieldStart = i
				}
			case cfg.ifsWhitespace(r):
				if haveContent() {
					closeField(i)
					delimPending = false
				}
			default: // a non-whitespace IFS delimiter
				if haveContent() {
					closeField(i)
				} else if delimPending {
					fields = append(fields, []fieldPart{{}})
				}
				delimPending = true
			}
		}
		if fieldStart >= 0 { // ending a field without IFS
			curField = append(curField, fieldPart{val: val[fieldStart:]})
		}
	}
	for i, wp := range wps {
		switch wp := wp.(type) {
		case *syntax.Lit:
			s := wp.Value
			if i == 0 {
				prefix, rest := cfg.expandUser(s, len(wps) > 1)
				if prefix != "" || !splitLits {
					curField = append(curField, fieldPart{
						quote: quoteSingle,
						val:   prefix,
					})
				}
				s = rest
				// An argument shaped like an assignment tilde-expands
				// after its = and after each colon in the value (#364):
				// `make FOO=~/mumble` hands make a path.
				if eq := strings.IndexByte(s, '='); eq > 0 && syntax.ValidName(s[:eq]) {
					s = s[:eq+1] + cfg.expandTildesAfterColons(s[eq+1:], true)
				}
			}
			if strings.Contains(s, "\\") {
				sb := cfg.strBuilder()
				for i := 0; i < len(s); i++ {
					b := s[i]
					if b == '\\' {
						if i++; i >= len(s) {
							sb.WriteByte(b)
							break
						}
						b = s[i]
					}
					sb.WriteByte(b)
				}
				s = sb.String()
			}
			if splitLits {
				splitAdd(s)
				continue
			}
			curField = append(curField, fieldPart{val: s})
		case *syntax.SglQuoted:
			allowEmpty = true
			fp := fieldPart{quote: quoteSingle, val: wp.Value}
			if wp.Dollar {
				fp.val = formatDollar(cfg, fp.val)
				fp.val, _, _ = strings.Cut(fp.val, "\x00") // cut the string if format included \x00
			}
			curField = append(curField, fp)
		case *syntax.DblQuoted:
			// Each part is processed on its own, because a "$@"-style
			// list splits fields at its element boundaries even with
			// text attached — "x $@ y" is "x 1" "2 y", and "$@$@" is
			// three fields with the middle one joined (#361) — where
			// flattening the whole quoted string joined everything into
			// one word. hadNonList decides whether an all-empty result
			// may still be one empty field: "" is, "$@" with no
			// parameters is zero fields.
			hadNonList := len(wp.Parts) == 0
			var processQuoted func(parts []syntax.WordPart) error
			processQuoted = func(parts []syntax.WordPart) error {
				for _, part := range parts {
					if inner, ok := part.(*syntax.DblQuoted); ok {
						// Nested quotes inside a re-expanded ${x+word}
						// keep the same context.
						if err := processQuoted(inner.Parts); err != nil {
							return err
						}
						continue
					}
					if pe, ok := part.(*syntax.ParamExp); ok {
						// The default/alternate/error family on a list
						// expansion decides by the list, measured against
						// 5.3: an empty array counts as unset even for
						// the plain forms, ("") is null for the colon
						// forms, a not-taken default keeps the elements
						// one per field, and a not-taken alternate is
						// zero fields.
						var wordOp syntax.ParExpOperator
						if pe.Exp != nil {
							switch pe.Exp.Op {
							case syntax.DefaultUnset, syntax.DefaultUnsetOrNull,
								syntax.AlternateUnset, syntax.AlternateUnsetOrNull,
								syntax.ErrorUnset, syntax.ErrorUnsetOrNull:
								wordOp = pe.Exp.Op
							}
						}
						if wordOp != 0 {
							elems, star, ok := cfg.listElems(pe)
							if !ok && nodeLit(pe.Index) == "@" && !cfg.Env.Get(pe.Param.Value).IsSet() {
								// An unset name[@] is the empty list.
								elems, star, ok = nil, false, true
							}
							if ok {
								set := len(elems) > 0
								null := strings.Join(elems, "") == ""
								colon := wordOp == syntax.DefaultUnsetOrNull ||
									wordOp == syntax.AlternateUnsetOrNull ||
									wordOp == syntax.ErrorUnsetOrNull
								alternate := wordOp == syntax.AlternateUnset ||
									wordOp == syntax.AlternateUnsetOrNull
								taken := !set || (colon && null)
								if alternate {
									taken = !taken
								}
								switch {
								case !taken && alternate:
									continue // zero fields
								case !taken && star:
									curField = append(curField, fieldPart{
										quote: quoteDouble,
										val:   cfg.ifsJoin(elems),
									})
									continue
								case !taken:
									for j, elem := range elems {
										if j > 0 {
											flush()
										}
										curField = append(curField, fieldPart{
											quote: quoteDouble,
											val:   elem,
										})
									}
									continue
								case wordOp == syntax.ErrorUnset || wordOp == syntax.ErrorUnsetOrNull:
									msg, err := Literal(cfg, pe.Exp.Word)
									if err != nil {
										return err
									}
									return UnsetParameterError{Node: pe, Message: msg}
								case pe.Exp.Word == nil:
									continue
								default:
									// The word, inside these quotes.
									if err := processQuoted(pe.Exp.Word.Parts); err != nil {
										return err
									}
									hadNonList = true
									continue
								}
							}
						}
						elems, err := cfg.quotedElemFields(pe)
						if err != nil {
							return err
						}
						if elems != nil {
							for j, elem := range elems {
								if j > 0 {
									flush()
								}
								curField = append(curField, fieldPart{
									quote: quoteDouble,
									val:   elem,
								})
							}
							continue
						}
						// A ${x+word} whose answer is the word re-expands
						// inside these quotes, keeping "$@" identity
						// (#360): "${1+$@}" is "$@".
						cfg.wordResult, cfg.wordResultPe = nil, nil
						oldOuter := cfg.paramOuterQuote
						cfg.paramOuterQuote = quoteDouble
						val, err := cfg.paramExp(pe)
						cfg.paramOuterQuote = oldOuter
						if err != nil {
							return err
						}
						if cfg.wordResultPe == pe {
							w := cfg.wordResult
							cfg.wordResult, cfg.wordResultPe = nil, nil
							if err := processQuoted(w.Parts); err != nil {
								return err
							}
							continue
						}
						hadNonList = true
						curField = append(curField, fieldPart{quote: quoteDouble, val: val})
						continue
					}
					hadNonList = true
					// A single-quoted part can only arrive here through a
					// re-expanded ${x+word}, where #359's rule applies:
					// the quotes are literal and their content still
					// expands, exactly as the flat path reads them.
					ql := quoteDouble
					if _, ok := part.(*syntax.SglQuoted); ok {
						oldCtx := cfg.paramQuoteCtx
						cfg.paramQuoteCtx = quoteDouble
						wfield, err := cfg.wordFieldMode([]syntax.WordPart{part}, quoteNone, false)
						cfg.paramQuoteCtx = oldCtx
						if err != nil {
							return err
						}
						for _, fp := range wfield {
							fp.quote = quoteDouble
							curField = append(curField, fp)
						}
						continue
					}
					wfield, err := cfg.wordField([]syntax.WordPart{part}, ql)
					if err != nil {
						return err
					}
					for _, fp := range wfield {
						fp.quote = quoteDouble
						curField = append(curField, fp)
					}
				}
				return nil
			}
			if err := processQuoted(wp.Parts); err != nil {
				return nil, err
			}
			if hadNonList {
				allowEmpty = true
			}
		case *syntax.ParamExp:
			if elems, ok := cfg.unquotedElemFields(wp); ok {
				// Unquoted "*" or "@" expansions produce one field per
				// element; joining and re-splitting them would lose
				// fields when IFS is empty.
				for j, elem := range elems {
					if j > 0 {
						flush()
					}
					splitAdd(elem)
				}
				continue
			}
			cfg.wordResult, cfg.wordResultPe = nil, nil
			val, err := cfg.paramExp(wp)
			if err != nil {
				return nil, err
			}
			if cfg.wordResultPe == wp {
				// The answer is the operator's word: re-expand it in this
				// unquoted context so quoted nulls survive as empty
				// fields and an inner "$@" splits into parameters (#358)
				// — the flat string loses both.
				w := cfg.wordResult
				cfg.wordResult, cfg.wordResultPe = nil, nil
				subFields, err := cfg.wordFieldsBuf(w.Parts, false, true)
				if err != nil {
					return nil, err
				}
				for j, sf := range subFields {
					if j > 0 {
						flush()
					}
					if len(sf) == 0 {
						// A quoted null came back as a field with no
						// parts; keep it a field, or flush drops it.
						sf = []fieldPart{{quote: quoteDouble}}
					}
					curField = append(curField, sf...)
				}
				continue
			}
			splitAdd(val)
		case *syntax.CmdSubst:
			val, err := cfg.cmdSubst(wp)
			if err != nil {
				return nil, err
			}
			splitAdd(val)
		case *syntax.ArithmExp:
			n, err := Arithm(cfg, wp.X)
			if err != nil {
				return nil, err
			}
			curField = append(curField, fieldPart{val: strconv.Itoa(n)})
		case *syntax.ProcSubst:
			path, err := cfg.ProcSubst(wp)
			if err != nil {
				return nil, err
			}
			splitAdd(path)
		case *syntax.ExtGlob:
			if !cfg.ExtGlob {
				return nil, fmt.Errorf("extended globbing operator used without the \"extglob\" option set")
			}
			// We don't translate or interpret the pattern here in any way;
			// that's done later when globbing takes place via [pattern.Regexp].
			// Here, all we do is keep the extended globbing expression in string form.
			//
			// TODO(v4): perhaps the syntax parser should keep extended globbing expressions
			// as plain literal strings, because a custom node is not particularly helpful.
			// It's not like other globbing operators like `*` or `**` get their own nodes.
			curField = append(curField, fieldPart{val: wp.Op.String() + wp.Pattern.Value + ")"})
		default:
			panic(fmt.Sprintf("unhandled word part: %T", wp))
		}
	}
	flush()
	if allowEmpty && len(fields) == 0 {
		fields = append(fields, curField)
	}
	return fields, nil
}

// listElems returns the elements of a "*" or "@" expansion of a list, like
// $@ or ${arr[*]}, with star set for the "*" forms which join into a single
// field when quoted. ok is false for any other parameter expansion.
func (cfg *Config) listElems(pe *syntax.ParamExp) (elems []string, star, ok bool) {
	if pe.Param == nil { // e.g. zsh's ${}; paramExp rejects it
		return nil, false, false
	}
	switch name := pe.Param.Value; name {
	case "*", "@":
		return cfg.sliceElems(pe, cfg.Env.Get(name).List, nil, true), name == "*", true
	}
	switch lit := nodeLit(pe.Index); lit {
	case "@", "*":
		switch vr := cfg.Env.Get(pe.Param.Value); vr.Kind {
		case Indexed:
			return cfg.sliceElems(pe, vr.List, vr.Indexes, false), lit == "*", true
		case Associative:
			return slices.Sorted(maps.Values(vr.Map)), lit == "*", true
		}
	}
	return nil, false, false
}

// unquotedElemFields returns the elements of an unquoted "*" or "@" list
// expansion like $* or ${foo[@]}; ok is false for any other expansion.
func (cfg *Config) unquotedElemFields(pe *syntax.ParamExp) ([]string, bool) {
	if pe.Excl || pe.Length || pe.Width || pe.IsSet {
		return nil, false
	}
	// Per-element operators keep the one-field-per-element shape: with
	// an empty IFS, ${*##} joining its elements first would collapse
	// them into one word (#361). The default/alternate family stays on
	// the flat path, whose word re-expansion has its own rules.
	if pe.Exp != nil {
		switch pe.Exp.Op {
		case syntax.RemSmallPrefix, syntax.RemLargePrefix,
			syntax.RemSmallSuffix, syntax.RemLargeSuffix,
			syntax.UpperFirst, syntax.UpperAll,
			syntax.LowerFirst, syntax.LowerAll:
		default:
			return nil, false
		}
	}
	elems, _, ok := cfg.listElems(pe)
	if !ok {
		return nil, false
	}
	if pe.Repl != nil || pe.Exp != nil {
		var err error
		if elems, err = cfg.perElemOps(pe, elems); err != nil {
			return nil, false
		}
	}
	return elems, true
}

// quotedElemFields returns the list of elements resulting from a quoted
// parameter expansion that should be treated especially, like "${foo[@]}".
// The result is nil for any other parameter expansion.
func (cfg *Config) quotedElemFields(pe *syntax.ParamExp) ([]string, error) {
	if pe == nil || pe.Param == nil || pe.Length || pe.Width || pe.IsSet {
		return nil, nil
	}
	name := pe.Param.Value
	if pe.Excl {
		switch pe.Names {
		case syntax.NamesPrefixWords: // "${!prefix@}"
			return cfg.namesByPrefix(pe.Param.Value), nil
		case syntax.NamesPrefix: // "${!prefix*}"
			return nil, nil
		}
		switch nodeLit(pe.Index) {
		case "@": // "${!name[@]}"
			switch vr := cfg.Env.Get(name); vr.Kind {
			case Indexed:
				return vr.indexedKeys(), nil
			case Associative:
				return slices.Collect(maps.Keys(vr.Map)), nil
			}
		}
		return nil, nil
	}
	if elems, star, ok := cfg.listElems(pe); ok {
		// Operators like "${foo[@]#prefix}" apply to each element.
		elems, err := cfg.perElemOps(pe, elems)
		if err != nil {
			return nil, err
		}
		if star {
			return []string{cfg.ifsJoin(elems)}, nil
		}
		return elems, nil
	}
	if nodeLit(pe.Index) == "@" && !cfg.Env.Get(name).IsSet() {
		// An unset "${name[@]}" produces zero fields, like an empty array.
		return []string{}, nil
	}
	return nil, nil
}

// sliceElems applies ${var:offset:length} slicing to a list of elements.
// When positional is true, $0 is prepended to the list before slicing.
// In bash, positional parameter offsets ($@ and $*) are 1-based and
// offset 0 includes $0 (the shell or script name). Negative offsets
// count from $# + 1, so $0 is reachable via large enough negative values.
// A non-nil indexes records the index of each element in a sparse array;
// see [Variable.Indexes].
func (cfg *Config) sliceElems(pe *syntax.ParamExp, elems []string, indexes []int, positional bool) []string {
	if pe.Slice == nil {
		return elems
	}
	if positional {
		elems = append([]string{cfg.Env.Get("0").Str}, elems...)
	}
	slicePos := func(n int) int {
		if n < 0 {
			n = len(elems) + n
			if n < 0 {
				n = len(elems)
			}
		} else if n > len(elems) {
			n = len(elems)
		}
		return n
	}
	if pe.Slice.Offset != nil {
		offset, err := Arithm(cfg, pe.Slice.Offset)
		if err != nil {
			return elems
		}
		if len(indexes) > 0 {
			// Sparse arrays slice by index: a negative offset counts
			// from one past the maximum index, and the result begins
			// with the first element whose index is at least the offset.
			if offset < 0 {
				offset += indexes[len(indexes)-1] + 1
				if offset < 0 {
					offset = indexes[len(indexes)-1] + 1
				}
			}
			pos, _ := slices.BinarySearch(indexes, offset)
			elems = elems[pos:]
		} else {
			elems = elems[slicePos(offset):]
		}
	}
	if pe.Slice.Length != nil {
		length, err := Arithm(cfg, pe.Slice.Length)
		if err != nil {
			return elems
		}
		elems = elems[:slicePos(length)]
	}
	return elems
}

// expandTildesAfterColons tilde-expands each `:~...` segment of an
// assignment-style value (#364); expandStart also expands a tilde at
// the string's own beginning, for the segment right after an `=`.
func (cfg *Config) expandTildesAfterColons(s string, expandStart bool) string {
	if !strings.Contains(s, "~") {
		return s
	}
	pieces := strings.Split(s, ":")
	changed := false
	for i, p := range pieces {
		if i == 0 && !expandStart {
			continue
		}
		if prefix, rest := cfg.expandUser(p, false); prefix != "" {
			pieces[i] = prefix + rest
			changed = true
		}
	}
	if !changed {
		return s
	}
	return strings.Join(pieces, ":")
}

func (cfg *Config) expandUser(field string, moreFields bool) (prefix, rest string) {
	name, ok := strings.CutPrefix(field, "~")
	if !ok {
		// No tilde prefix to expand, e.g. "foo".
		return "", field
	}
	// A colon ends a tilde prefix like a slash does (#364): `echo ~:x`
	// expands in bash, and PATH-style assignment values depend on it.
	i := strings.IndexAny(name, "/:")
	if i < 0 && moreFields {
		// There is a tilde prefix, but followed by more fields, e.g. "~'foo'".
		// We only proceed if an unquoted slash was found in this field, e.g. "~/'foo'".
		return "", field
	}
	if i >= 0 {
		rest = name[i:]
		name = name[:i]
	}
	// ~+ and ~- are the directory pair cd maintains (#364); when the
	// variable is unset the tilde stays literal, as bash leaves it.
	switch name {
	case "+":
		if vr := cfg.Env.Get("PWD"); vr.IsSet() {
			return vr.String(), rest
		}
		return "", field
	case "-":
		if vr := cfg.Env.Get("OLDPWD"); vr.IsSet() {
			return vr.String(), rest
		}
		return "", field
	}
	if name == "" {
		// Current user; try via "HOME", otherwise fall back to the
		// system's appropriate home dir env var. Don't use os/user, as
		// that's overkill. We can't use [os.UserHomeDir], because we want
		// to use cfg.Env, and we always want to check "HOME" first.

		if vr := cfg.Env.Get("HOME"); vr.IsSet() {
			return vr.String(), rest
		}

		if runtime.GOOS == "windows" {
			if vr := cfg.Env.Get("USERPROFILE"); vr.IsSet() {
				return vr.String(), rest
			}
		}
		return "", field
	}

	// Not the current user; try via "HOME <name>", otherwise fall back to
	// os/user. There isn't a way to lookup user home dirs without cgo.

	if vr := cfg.Env.Get("HOME " + name); vr.IsSet() {
		return vr.String(), rest
	}

	u, err := user.Lookup(name)
	if err != nil {
		return "", field
	}
	return u.HomeDir, rest
}

func findAllIndex(pat, name string, n int) [][]int {
	expr, err := pattern.Regexp(pat, 0)
	if err != nil {
		return nil
	}
	rx := regexp.MustCompile(expr)
	return rx.FindAllStringIndex(name, n)
}

var (
	rxGlobStar        = regexp.MustCompile(`^[^/.][^/]*$`)
	rxGlobStarDotGlob = regexp.MustCompile(`^[^/]*$`)
)

// pathJoin2 is a simpler version of [filepath.Join] without cleaning the result,
// since that's needed for globbing.
func pathJoin2(elem1, elem2 string) string {
	if elem1 == "" {
		return elem2
	}
	if strings.HasSuffix(elem1, string(filepath.Separator)) {
		return elem1 + elem2
	}
	return elem1 + string(filepath.Separator) + elem2
}

// pathSplit splits a file path into its elements, retaining empty ones. Before
// splitting, slashes are replaced with [filepath.Separator], so that splitting
// Unix paths on Windows works as well.
func pathSplit(path string) []string {
	path = filepath.FromSlash(path)
	return strings.Split(path, string(filepath.Separator))
}

func (cfg *Config) glob(base, pat string) ([]string, error) {
	parts := pathSplit(pat)
	// Adjacent ** components collapse into one (#371): bash treats
	// a/**/**/b as a/**/b, where expanding each independently
	// cross-multiplies the matches — globstar2.sub's entire diff. A
	// doubled ** also loses the literal-prefix trailing slash below:
	// a/**/** answers "a" bare where a/** answers "a/", measured.
	var gsDoubled []bool
	if cfg.GlobStar {
		newParts := make([]string, 0, len(parts))
		doubled := make([]bool, 0, len(parts))
		for _, p := range parts {
			if p == "**" && len(newParts) > 0 && newParts[len(newParts)-1] == "**" {
				doubled[len(doubled)-1] = true
				continue
			}
			newParts = append(newParts, p)
			doubled = append(doubled, false)
		}
		parts, gsDoubled = newParts, doubled
	}
	matches := []string{""}
	if filepath.IsAbs(pat) {
		if parts[0] == "" {
			// unix-like
			matches[0] = string(filepath.Separator)
		} else {
			// windows (for some reason it won't work without the
			// trailing separator)
			matches[0] = parts[0] + string(filepath.Separator)
		}
		parts = parts[1:]
	}
	// TODO: as an optimization, we could do chunks of the path all at once,
	// like doing a single stat for "/foo/bar" in "/foo/bar/*".

	// TODO: Another optimization would be to reduce the number of ReadDir2 calls.
	// For example, /foo/* can end up doing one duplicate call:
	//
	//    ReadDir2("/foo") to ensure that "/foo/" exists and only matches a directory
	//    ReadDir2("/foo") glob "*"

	sawMeta := false
	for i, part := range parts {
		// Keep around for debugging.
		// log.Printf("matches %q part %d %q", matches, i, part)

		wantDir := i < len(parts)-1
		switch {
		case part == "", part == ".", part == "..":
			for i, dir := range matches {
				matches[i] = pathJoin2(dir, part)
			}
			continue
		case !pattern.HasMeta(part, 0):
			var newMatches []string
			for _, dir := range matches {
				match := dir
				if !filepath.IsAbs(match) {
					match = filepath.Join(base, match)
				}
				match = pathJoin2(match, part)
				// We can't use [Config.ReadDir2] on the parent and match the directory
				// entry by name, because short paths on Windows break that.
				// Our only option is to [Config.ReadDir2] on the directory entry itself,
				// which can be wasteful if we only want to see if it exists,
				// but at least it's correct in all scenarios.
				if _, err := cfg.ReadDir2(match); err != nil {
					if isWindowsErrPathNotFound(err) {
						// Unfortunately, [os.File.Readdir] on a regular file on
						// Windows returns an error that satisfies [fs.ErrNotExist].
						// Luckily, it returns a special "path not found" rather
						// than the normal "file not found" for missing files,
						// so we can use that knowledge to work around the bug.
						// See https://github.com/golang/go/issues/46734.
						// TODO: remove when the Go issue above is resolved.
					} else if errors.Is(err, fs.ErrNotExist) {
						continue // simply doesn't exist
					}
					if wantDir {
						continue // exists but not a directory
					}
				}
				newMatches = append(newMatches, pathJoin2(dir, part))
			}
			matches = newMatches
			continue
		case part == "**" && cfg.GlobStar:
			// Find all recursive matches for "**".
			// Note that we need the results to be in depth-first order,
			// and to avoid recursion, we use a slice as a stack.
			// Since we pop from the back, we populate the stack backwards.
			stack := make([]string, 0, len(matches))
			for _, match := range slices.Backward(matches) {
				// "a/**" should match "a/ a/b a/b/cfg ..." — the
				// zero-match case keeps a trailing separator when the
				// prefix was written literally, and only then: measured
				// against 5.3, **/a/** answers "a" bare where a/**
				// answers "a/" (#371).
				if sawMeta || gsDoubled[i] {
					stack = append(stack, match)
					continue
				}
				stack = append(stack, pathJoin2(match, ""))
			}
			sawMeta = true
			matches = matches[:0]
			var newMatches []string // to reuse its capacity
			for len(stack) > 0 {
				dir := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				matches = append(matches, dir)

				// If dir is not a directory, we keep the stack as-is and continue.
				newMatches = newMatches[:0]
				rx := rxGlobStar.MatchString
				if cfg.DotGlob {
					rx = rxGlobStarDotGlob.MatchString
				}
				newMatches, _ = cfg.globDir(base, dir, rx, wantDir, newMatches)
				for _, match := range slices.Backward(newMatches) {
					stack = append(stack, match)
				}
			}
			continue
		}
		mode := pattern.Filenames | pattern.EntireString | pattern.NoGlobStar
		if cfg.NoCaseGlob {
			mode |= pattern.NoGlobCase
		}
		if cfg.DotGlob {
			mode |= pattern.GlobLeadingDot
		}
		if cfg.ExtGlob {
			mode |= pattern.ExtendedOperators
		}
		matcher, err := shinternal.ExtendedPatternMatcher(part, mode)
		if err != nil {
			return nil, err
		}
		var newMatches []string
		for _, dir := range matches {
			newMatches, err = cfg.globDir(base, dir, matcher, wantDir, newMatches)
			if err != nil {
				return nil, err
			}
		}
		matches = newMatches
		// Any pattern-derived prefix drops the later **'s zero-match
		// slash too: */** answers bare names where a/** answers "a/",
		// measured against 5.3.
		sawMeta = true
	}
	// Note that the results need to be sorted.
	// TODO: above we do a BFS; if we did a DFS, the matches would already be sorted.
	slices.Sort(matches)
	// Remove any empty matches left behind from "**".
	if len(matches) > 0 && matches[0] == "" {
		matches = matches[1:]
	}
	return matches, nil
}

func (cfg *Config) globDir(base, dir string, matcher func(string) bool, wantDir bool, matches []string) ([]string, error) {
	fullDir := dir
	if !filepath.IsAbs(dir) {
		fullDir = filepath.Join(base, dir)
	}
	infos, err := cfg.ReadDir2(fullDir)
	if err != nil {
		// We still want to return matches, for the sake of reusing slices.
		return matches, err
	}
	for _, info := range infos {
		name := info.Name()
		if !wantDir {
			// No filtering.
		} else if mode := info.Type(); mode&os.ModeSymlink != 0 {
			// We need to know if the symlink points to a directory.
			// This requires an extra syscall, as [Config.ReadDir] on the parent directory
			// does not follow symlinks for each of the directory entries.
			// ReadDir is somewhat wasteful here, as we only want its error result,
			// but we could try to reuse its result as per the TODO in [Config.glob].
			if _, err := cfg.ReadDir2(filepath.Join(fullDir, info.Name())); err != nil {
				continue
			}
		} else if !mode.IsDir() {
			// Not a symlink nor a directory.
			continue
		}
		if matcher(name) {
			matches = append(matches, pathJoin2(dir, name))
		}
	}
	return matches, nil
}

// ReadFields splits and returns n fields from s, like the "read" shell builtin.
// If raw is set, backslash escape sequences are not interpreted.
//
// The config specifies shell expansion options; nil behaves the same as an
// empty config.
func ReadFields(cfg *Config, s string, n int, raw bool) []string {
	cfg = prepareConfig(cfg)
	type pos struct {
		start, end int
	}
	var fpos []pos

	// The same POSIX rule wordFields applies (#356): IFS whitespace runs
	// collapse and never delimit empty fields, while each non-whitespace
	// delimiter — with its adjacent IFS whitespace — terminates exactly
	// one field, possibly empty, and the end of the line never makes one.
	runes := make([]rune, 0, len(s))
	infield := false
	esc := false
	delimPending := true
	for _, r := range s {
		isIFS := cfg.ifsRune(r) && (raw || !esc)
		switch {
		case !isIFS:
			if !infield {
				fpos = append(fpos, pos{start: len(runes), end: -1})
				infield = true
			}
		case cfg.ifsWhitespace(r):
			if infield {
				fpos[len(fpos)-1].end = len(runes)
				infield = false
				delimPending = false
			}
		default: // a non-whitespace IFS delimiter
			if infield {
				fpos[len(fpos)-1].end = len(runes)
				infield = false
			} else if delimPending {
				fpos = append(fpos, pos{start: len(runes), end: len(runes)})
			}
			delimPending = true
		}
		if r == '\\' {
			if raw || esc {
				runes = append(runes, r)
			}
			esc = !esc
			continue
		}
		runes = append(runes, r)
		esc = false
	}
	if len(fpos) == 0 {
		return nil
	}
	if infield {
		fpos[len(fpos)-1].end = len(runes)
	}

	// With more fields than names, the last name takes the rest of the
	// line as written — separators included — trimmed of leading and
	// trailing IFS *whitespace* only: `IFS=: read x <<< "a:b:"` leaves
	// x as `a:b:`, while `IFS=: read x <<< "a:"` leaves it as `a`,
	// because there the field count fits the names and the plain field
	// is assigned. Both measured against 5.3.
	if n != -1 && n < len(fpos) {
		hi := len(runes)
		for hi > fpos[len(fpos)-1].end && cfg.ifsWhitespace(runes[hi-1]) {
			hi--
		}
		if n == 1 {
			lo := 0
			for lo < fpos[0].start && cfg.ifsWhitespace(runes[lo]) {
				lo++
			}
			fpos[0].start = lo
		}
		fpos[n-1].end = hi
		fpos = fpos[:n]
	}

	fields := make([]string, len(fpos))
	for i, p := range fpos {
		fields[i] = string(runes[p.start:p.end])
	}
	return fields
}

func isHexDigit(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'f' || b >= 'A' && b <= 'F'
}
