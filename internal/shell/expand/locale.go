// Copyright (c) 2026, koi contributors
// See LICENSE for licensing information

package expand

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// CLocale reports whether the effective LC_CTYPE is the C/POSIX locale,
// in which a character is a byte (#470).
//
// koi is UTF-8 everywhere else, which is right for every locale that has
// a multibyte encoding — but under LC_ALL=C bash counts, matches and
// reads *bytes*, so `${#x}` of a two-byte character is 2 and `?` matches
// one of its halves. A script that sets LC_ALL=C is asking for exactly
// that, usually to make its own output stable.
//
// The precedence is POSIX's: LC_ALL wins over LC_CTYPE, which wins over
// LANG. An unset or empty value means the next one decides, and nothing
// set at all means C — which is what a process started with an empty
// environment gets.
func (cfg *Config) CLocale() bool {
	for _, name := range [...]string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if v := cfg.envGet(name); v != "" {
			return isCLocale(v)
		}
	}
	return true
}

// isCLocale reads one locale name. The encoding suffix is what decides:
// "C", "POSIX" and "C.UTF-8" are all C locales by name, but the last
// one is UTF-8 by encoding, and it is the encoding this is about.
func isCLocale(v string) bool {
	if i := strings.IndexByte(v, '.'); i >= 0 {
		enc := strings.ToUpper(strings.ReplaceAll(v[i+1:], "-", ""))
		return enc != "UTF8"
	}
	switch v {
	case "C", "POSIX":
		return true
	}
	return false
}

// LatinBytes re-reads s as one rune per byte, which is what makes a
// rune-wise matcher behave byte-wise without touching the matcher: in
// the C locale each byte is its own character, and Latin-1 is the
// mapping that says so.
func LatinBytes(s string) string {
	ascii := true
	for i := range len(s) {
		if s[i] >= utf8Self {
			ascii = false
			break
		}
	}
	if ascii {
		return s
	}
	var sb strings.Builder
	sb.Grow(len(s) * 2)
	for i := range len(s) {
		sb.WriteRune(rune(s[i]))
	}
	return sb.String()
}

// utf8Self is the first byte value that is not its own rune.
const utf8Self = 0x80

// sliceUnits is `${x:offset:length}` over whatever a character is here:
// runes normally, bytes in the C locale (#470). Both halves clamp the
// way bash does — a negative offset counts from the end, and one past
// the end is the end rather than an error.
func sliceUnits[T any](units []T, offset, length int, hasOffset, hasLength bool) []T {
	pos := func(n int) int {
		if n < 0 {
			if n = len(units) + n; n < 0 {
				n = len(units)
			}
		} else if n > len(units) {
			n = len(units)
		}
		return n
	}
	if hasOffset {
		units = units[pos(offset):]
	}
	if hasLength {
		units = units[:pos(length)]
	}
	return units
}

// BytesOfLatin is [LatinBytes] read back: every rune below 256 becomes
// the byte it stood for, so a string built by matching or replacing
// byte-wise returns to being the bytes it describes.
func BytesOfLatin(s string) string {
	if len(s) == utf8.RuneCountInString(s) {
		return s // all one-byte runes already
	}
	var sb strings.Builder
	sb.Grow(len(s))
	for _, r := range s {
		if r < 0x100 {
			sb.WriteByte(byte(r))
			continue
		}
		// Not something LatinBytes produced; keep it as it is rather
		// than truncating a character to its low byte.
		sb.WriteRune(r)
	}
	return sb.String()
}

// asciiOnly restricts a case mapping to ASCII, which is all the C
// locale has: bash uppercases `a` there and leaves `ÿ` alone.
func asciiOnly(f func(rune) rune) func(rune) rune {
	return func(r rune) rune {
		if r >= utf8Self {
			return r
		}
		return f(r)
	}
}

// toggleCase swaps a character's case, which is what bash's `${x~}` and
// `${x~~}` do. A character with no case is left alone.
func toggleCase(r rune) rune {
	switch {
	case unicode.IsLower(r):
		return unicode.ToUpper(r)
	case unicode.IsUpper(r):
		return unicode.ToLower(r)
	}
	return r
}
