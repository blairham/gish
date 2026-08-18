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

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			// A bare word with no resource before it is bash's leftover:
			// `ulimit -n 100 200` sets from 100 and ignores 200.
			continue
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
					r.errf("ulimit: invalid option %q\n", "-"+string(letter))
					r.errf("ulimit: usage: ulimit [-SHa%s] [limit]\n", ulimitLetters())
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

	labelled := len(requests) > 1
	for _, req := range requests {
		if req.set {
			raw, err := parseUlimitValue(req.value, req.spec)
			if err != nil {
				return failf(1, "ulimit: %s: invalid number\n", req.value)
			}
			if err := writeUlimit(req.spec, wantSoft, wantHard, raw); err != nil {
				return failf(1, "ulimit: %s: cannot modify limit: %v\n", req.spec.label, err)
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

// ulimitLetters renders the supported letters for the usage line, in the
// same order they are listed.
func ulimitLetters() string {
	var b strings.Builder
	for _, spec := range ulimitSpecs {
		b.WriteByte(spec.letter)
	}
	return b.String()
}

// ulimitLabel renders the description column of the labelled form.
//
// bash pads so that the closing parenthesis lands in a fixed column, not
// so that the descriptions line up — which is why "open files" is
// followed by far more spaces than "max locked memory" is.
const ulimitLabelWidth = 40

func ulimitLabel(spec ulimitSpec) string {
	opt := "(-" + string(spec.letter) + ")"
	if spec.unit != "" {
		opt = "(" + spec.unit + ", -" + string(spec.letter) + ")"
	}
	pad := max(ulimitLabelWidth-len(spec.label)-len(opt), 1)
	return spec.label + strings.Repeat(" ", pad) + opt
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
