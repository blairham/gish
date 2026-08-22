package interp

import (
	"errors"
	"slices"
	"strconv"
	"strings"
)

// errNoRlimits is what the platforms without resource limits answer
// with; the builtin refuses before it can be returned, so it exists to
// keep those platforms compiling rather than to be printed.
var errNoRlimits = errors.New("resource limits are not available")

// ulimit reports and sets resource limits (#250).
//
// The shape is bash's and had to be measured rather than read off the
// usage line, because the option letters are not flags in the ordinary
// sense: a resource letter takes an *optional* argument, so `ulimit -n`
// asks and `ulimit -n 512` sets, and `ulimit -nH` is not "-n and -H" but
// "-n with the limit H", which is why bash answers that with "H: invalid
// number". -H and -S are the only letters that bundle.
//
// Asking about one resource prints a bare value, which is what makes
// `ulimit -n` usable in an assignment; asking about more than one prints
// the labelled form, since otherwise the answers could not be told
// apart. -a is the labelled form for everything, and wins over any
// resource asked for alongside it.

// noResource marks a row that is reported but is not an rlimit: bash
// publishes the pipe buffer under -p, and there is nothing to ask the
// kernel about.
const noResource = -1

// unlimited is how bash spells RLIM_INFINITY, in both directions.
const unlimited = "unlimited"

// ulimitSpec is one row of `ulimit -a`: the letter a script asks with,
// the description and unit bash prints, and the multiple of bytes the
// value is reported in.
//
// factor is why `ulimit -s` answers 8176 where the kernel holds
// 8372224 — the stack is reported in kbytes — and getting it wrong
// would produce a plausible number that is wrong by a factor of 1024.
type ulimitSpec struct {
	letter   byte
	label    string
	unit     string // bash's parenthesised unit; empty for a bare count
	factor   uint64 // the value is reported in units of this many bytes
	resource int
	fixed    uint64 // the answer when resource is noResource
}

// ulimitRequest is one resource the command line asked about, in the
// order it was asked, with the limit to set if one followed it.
type ulimitRequest struct {
	spec  ulimitSpec
	value string
	set   bool
}

func (r *Runner) ulimitBuiltin(args []string) exitStatus {
	var exit exitStatus
	failf := func(code uint8, format string, a ...any) exitStatus {
		r.errf(format, a...)
		exit.code = code
		return exit
	}
	if len(ulimitSpecs) == 0 {
		return failf(2, "ulimit: unsupported builtin\n")
	}

	// Which of the soft and hard limits the command is about. The two are
	// independent flags rather than a choice, because bash reads them
	// differently in each direction: asking answers with the soft limit
	// unless -H was the only one given, while setting with *neither*
	// moves both. That last rule is the one worth knowing — plain
	// `ulimit -c 0` lowers the hard limit too, and a hard limit can never
	// be raised back, so a shell that moved only the soft half would let
	// a script undo something bash makes permanent.
	wantSoft, wantHard := false, false
	all := false
	var requests []ulimitRequest
	operand, haveOperand := "", false

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			// `--` ends the options and the word after it is the limit,
			// which is how bash's own builtins11.sub writes
			// `ulimit -c -S -- 1999`. koi read the `--` as a resource
			// letter and answered "invalid option", so that line and
			// the one after it set nothing.
			if i+1 < len(args) {
				operand, haveOperand = args[i+1], true
			}
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			// A word no resource letter claimed is the limit itself:
			// `ulimit unlimited` is `ulimit -f unlimited`, the line
			// bash's own builtins11.sub opens with, and koi ignored it
			// and answered with a *read* of -f — so the line looked
			// like it had worked.
			//
			// It also ends the option scanning, the way getopt stops at
			// the first operand: `ulimit 1999 -c` sets -f and leaves -c
			// alone, measured. Anything after it is ignored, which is
			// what keeps `ulimit -n 100 200` setting 100.
			operand, haveOperand = arg, true
			break
		}
		for j := 1; j < len(arg); j++ {
			switch letter := arg[j]; letter {
			case 'H':
				wantHard = true
			case 'S':
				wantSoft = true
			case 'a':
				all = true
			default:
				spec, ok := lookupUlimitSpec(letter)
				if !ok {
					r.errf("ulimit: -%c: invalid option\n", letter)
					r.rawErrf("%s", ulimitUsage)
					exit.code = 2
					return exit
				}
				req := ulimitRequest{spec: spec}
				// The limit may be the rest of this word or the next one,
				// and a word starting with - is the next option instead.
				switch rest := arg[j+1:]; {
				case rest != "":
					req.value, req.set = rest, true
					j = len(arg)
				case i+1 < len(args) && !strings.HasPrefix(args[i+1], "-"):
					i++
					req.value, req.set = args[i], true
				}
				requests = append(requests, req)
			}
		}
	}

	// Asking answers with the soft limit unless -H stood alone.
	readHard := wantHard && !wantSoft

	if all {
		for _, spec := range ulimitSpecs {
			value, err := readUlimit(spec, readHard)
			if err != nil {
				return failf(1, "ulimit: %v\n", err)
			}
			r.outf("%s %s\n", ulimitLabel(spec), value)
		}
		return exit
	}

	// Bare `ulimit` is `ulimit -f`, which is why a shell that has never
	// heard of the command still answers "unlimited" rather than erroring.
	if len(requests) == 0 {
		spec, ok := lookupUlimitSpec('f')
		if !ok {
			return failf(2, "ulimit: unsupported builtin\n")
		}
		requests = append(requests, ulimitRequest{spec: spec})
	}
	if last := &requests[len(requests)-1]; haveOperand && !last.set {
		// An unclaimed operand is the limit for the resource named
		// *last*: `ulimit -c -d -- 1999` reads c and sets d, measured.
		// A resource that already took a value keeps it, which is the
		// `ulimit -n 100 200` rule.
		last.value, last.set = operand, true
	}

	labelled := len(requests) > 1
	for _, req := range requests {
		if req.set {
			raw, err := parseUlimitValue(req.value, req.spec)
			if err != nil {
				return failf(1, "ulimit: %s: invalid number\n", req.value)
			}
			if err := writeUlimit(req.spec, wantSoft, wantHard, raw); err != nil {
				// strerror's capitalisation, which is what bash prints
				// and what koi's other diagnostics already use: Go
				// spells the same errno "operation not permitted".
				return failf(1, "ulimit: %s: cannot modify limit: %s\n", req.spec.label, strerror(err))
			}
			continue
		}
		value, err := readUlimit(req.spec, readHard)
		if err != nil {
			return failf(1, "ulimit: %v\n", err)
		}
		if labelled {
			r.outf("%s %s\n", ulimitLabel(req.spec), value)
		} else {
			r.outf("%s\n", value)
		}
	}
	return exit
}

func lookupUlimitSpec(letter byte) (ulimitSpec, bool) {
	i := slices.IndexFunc(ulimitSpecs, func(s ulimitSpec) bool { return s.letter == letter })
	if i < 0 {
		return ulimitSpec{}, false
	}
	return ulimitSpecs[i], true
}

// ulimitUsage is bash's usage line verbatim. It is a fixed string there
// rather than a render of the platform's own resource table — bash on
// darwin advertises `-b` and then refuses it — and koi rendered the
// eleven letters it has, which made a usage line a caller diffs differ
// on every platform. koi's *accepted* set is byte-for-byte bash's on
// each platform, which is what makes borrowing the claim honest.
const ulimitUsage = "ulimit: usage: ulimit [-SHabcdefiklmnpqrstuvxPRT] [limit]\n"

// strerror renders an errno the way C's strerror does, which is the
// wording bash prints: Go's errno strings are the same text with a
// lowercase first letter.
func strerror(err error) string {
	msg := err.Error()
	if msg == "" || msg[0] < 'a' || msg[0] > 'z' {
		return msg
	}
	return string(msg[0]-('a'-'A')) + msg[1:]
}

// ulimitLabel renders the description column of the labelled form.
//
// Two fields of twenty, not one of forty: the description is padded on
// the right to twenty and the "(unit, -x)" part is padded on the *left*
// to twenty. Those agree for every row that fits — the closing
// parenthesis lands in column forty either way, which is what made a
// single fixed column look right — and disagree for the one row that
// does not. Linux's "real-time non-blocking time" is twenty-seven
// characters wide, so bash pushes its suffix past forty and still gives
// it a full twenty-wide field; a single fixed column would have left it
// one space, and that one row is the whole difference.
const ulimitFieldWidth = 20

func ulimitLabel(spec ulimitSpec) string {
	opt := "(-" + string(spec.letter) + ")"
	if spec.unit != "" {
		opt = "(" + spec.unit + ", -" + string(spec.letter) + ")"
	}
	label := spec.label + strings.Repeat(" ", max(ulimitFieldWidth-len(spec.label), 0))
	return label + strings.Repeat(" ", max(ulimitFieldWidth-len(opt), 0)) + opt
}

// readUlimit answers one row, in the units bash reports it in.
func readUlimit(spec ulimitSpec, hard bool) (string, error) {
	if spec.resource == noResource {
		return strconv.FormatUint(spec.fixed, 10), nil
	}
	raw, err := getRlimit(spec.resource, hard)
	if err != nil {
		return "", err
	}
	if isUnlimited(raw) {
		return unlimited, nil
	}
	return strconv.FormatUint(raw/spec.factor, 10), nil
}

// writeUlimit sets one limit. Naming neither half moves both, which is
// bash's rule and not the obvious one; naming a half moves only that
// one, which is what makes `ulimit -S -n $(ulimit -Hn)` raise the soft
// limit to the ceiling without touching the ceiling.
func writeUlimit(spec ulimitSpec, soft, hard bool, raw uint64) error {
	if spec.resource == noResource {
		return nil // bash accepts `ulimit -p N` and does nothing with it
	}
	if !soft && !hard {
		soft, hard = true, true
	}
	return setRlimit(spec.resource, soft, hard, raw)
}

// parseUlimitValue reads a limit as written. bash takes a number in the
// row's own units, or one of three words.
func parseUlimitValue(s string, spec ulimitSpec) (uint64, error) {
	switch s {
	case unlimited:
		return rlimInfinity, nil
	case "hard", "soft":
		if spec.resource == noResource {
			return 0, nil
		}
		return getRlimit(spec.resource, s == "hard")
	}
	n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, err
	}
	return n * spec.factor, nil
}
