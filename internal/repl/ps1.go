package repl

import (
	"context"
	"os"
	"os/user"
	"strconv"
	"strings"
	"time"

	"mvdan.cc/sh/v3/interp"
)

// PS1 (#159): the prompt the ecosystem writes.
//
// starship's bash init sets PS1 from a PROMPT_COMMAND; so do oh-my-posh,
// liquidprompt, powerline-shell, and every hand-written prompt anyone
// has carried for twenty years. koi has its own prompt pipeline and
// was ignoring PS1 outright, which meant the tool ran, produced a
// prompt, and the prompt went nowhere — the most confusing possible
// outcome, because everything *looked* like it worked.
//
// Precedence is the same principle the rest of the theme engine uses:
// an explicit choice by the user beats an inherited one. A manual
// KOI_PROMPT wins; a KOI_THEME the user selected wins; otherwise, if
// something in the session set PS1, that is what renders. The default
// naked prompt is what you get when nobody asked for anything.

// ps1Theme renders the session's PS1 (and PS2 as the continuation).
func ps1Theme(runner *interp.Runner, info promptInfo) (string, string, string) {
	ps1 := sessionPS1(runner)
	if ps1 == "" {
		return nakedPromptTriple(info)
	}
	ps2 := shellVar(runner, "PS2", "> ")
	return expandBashPrompt(runner, ps1, info), expandBashPrompt(runner, ps2, info), ""
}

func nakedPromptTriple(info promptInfo) (string, string, string) {
	p, cp := nakedPrompt(info)
	return p, cp, ""
}

// sessionPS1 returns a PS1 the session actually set.
//
// The environment is deliberately not consulted: an inherited PS1 is
// the *previous* shell's prompt, and rendering bash's prompt because
// bash exported it is how a shell ends up looking like a shell it is
// not. A PS1 that arrives by assignment — from an rc, or from a tool's
// PROMPT_COMMAND — is a live request.
func sessionPS1(runner *interp.Runner) string {
	v, ok := runner.Vars["PS1"]
	if !ok || !v.IsSet() {
		return ""
	}
	return v.Str
}

// expandBashPrompt renders a bash PS1: first the shell expansions
// (command substitution above all — that is how starship gets in), then
// bash's own backslash escapes.
func expandBashPrompt(runner *interp.Runner, ps string, info promptInfo) string {
	if strings.ContainsAny(ps, "$`") {
		if expanded, err := expandPromptString(context.Background(), runner, ps); err == nil {
			ps = expanded
		}
	}
	return expandPromptEscapes(ps, info)
}

// expandPromptEscapes translates bash's prompt escapes.
//
// \[ and \] are dropped rather than kept: they mark non-printing runs
// so readline can count columns, and koi's renderer measures escape
// sequences as zero-width already (#157). Passing them through would
// put two stray bytes into every colored prompt in the world.
func expandPromptEscapes(ps string, info promptInfo) string {
	var b strings.Builder
	runes := []rune(ps)
	for i := 0; i < len(runes); i++ {
		if runes[i] != '\\' || i+1 >= len(runes) {
			b.WriteRune(runes[i])
			continue
		}
		i++
		switch runes[i] {
		case 'u':
			b.WriteString(promptUser())
		case 'h':
			b.WriteString(shortHost())
		case 'H':
			b.WriteString(fullHost())
		case 'w':
			b.WriteString(tildify(info.dir, info.home))
		case 'W':
			b.WriteString(baseName(info.dir))
		case 's':
			b.WriteString("koi")
		case 'v', 'V':
			b.WriteString(claimedBashVersion)
		case '$':
			if os.Geteuid() == 0 {
				b.WriteString("#")
			} else {
				b.WriteString("$")
			}
		case 'n':
			b.WriteString("\n")
		case 'r':
			b.WriteString("\r")
		case 'e':
			b.WriteString("\x1b")
		case 'a':
			b.WriteString("\a")
		case '\\':
			b.WriteString("\\")
		case 't':
			b.WriteString(time.Now().Format("15:04:05"))
		case 'T':
			b.WriteString(time.Now().Format("03:04:05"))
		case 'A':
			b.WriteString(time.Now().Format("15:04"))
		case '@':
			b.WriteString(time.Now().Format("03:04 PM"))
		case 'd':
			b.WriteString(time.Now().Format("Mon Jan 02"))
		case 'j':
			b.WriteString(strconv.Itoa(info.jobs))
		case '[', ']':
			// Non-printing markers: dropped, see above.
		case '0', '1', '2', '3', '4', '5', '6', '7':
			// \nnn octal — three digits, or the escape is not one.
			if i+2 < len(runes) && isOctal(runes[i+1]) && isOctal(runes[i+2]) {
				if n, err := strconv.ParseInt(string(runes[i:i+3]), 8, 32); err == nil {
					b.WriteRune(rune(n))
					i += 2
					continue
				}
			}
			b.WriteRune('\\')
			b.WriteRune(runes[i])
		default:
			// An escape we do not know passes through with its
			// backslash, which is what bash does and what keeps a
			// prompt from silently losing characters.
			b.WriteRune('\\')
			b.WriteRune(runes[i])
		}
	}
	return b.String()
}

func isOctal(r rune) bool { return r >= '0' && r <= '7' }

func promptUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return os.Getenv("USER")
}

func fullHost() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}

func shortHost() string {
	h := fullHost()
	if i := strings.IndexByte(h, '.'); i > 0 {
		return h[:i]
	}
	return h
}

func baseName(dir string) string {
	if dir == "" {
		return ""
	}
	if i := strings.LastIndexByte(dir, '/'); i >= 0 && i+1 < len(dir) {
		return dir[i+1:]
	}
	return dir
}
