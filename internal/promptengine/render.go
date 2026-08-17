package promptengine

import (
	"strings"
)

// The layout pass. This is the part that makes presets data: there is
// one algorithm here, and lean, classic, rainbow and pure differ only in
// the values it reads. Getting that generic shape right is what buys
// "all the themes" without writing four renderers.
//
// The algorithm, per side of a line:
//
//	for each segment that rendered:
//	  emit a separator, whose colors are decided by whether this
//	  segment's background differs from the previous one
//	  emit whitespace, icon and content in the segment's own colors
//	emit the side's end symbol in the last background
//
// With no backgrounds set and the separator configured to a space, that
// produces lean. With backgrounds per segment and the separator set to a
// powerline arrow, the same loop produces rainbow.

// Prompt is a rendered prompt: the main prompt (which may span several
// lines), the continuation prompt for multi-line edits, and the right
// prompt for the editing line.
type Prompt struct {
	Prompt  string
	Cont    string
	RPrompt string
}

// side distinguishes the two halves of a line; the separator vocabulary
// is mirrored between them.
type side int

const (
	sideLeft side = iota
	sideRight
)

func (s side) String() string {
	if s == sideLeft {
		return "LEFT"
	}
	return "RIGHT"
}

// placed is a segment that rendered, with its appearance resolved.
type placed struct {
	name  string
	out   Rendered
	style Style
}

// promptGap is the single space between the end of the left prompt and
// whatever the user types.
//
// Upstream emits it unconditionally, and it is deliberately not any of
// the knobs it looks like. Measured against a real zsh+powerlevel10k
// under a pty: the space survives with every *_WHITESPACE parameter set
// to empty, and overriding both LEFT_PROMPT_LAST_SEGMENT_END_SYMBOL and
// its PROMPT_CHAR-scoped form leaves it untouched. So it is modeled here
// as what it measurably is — a fixed suffix on the line being typed on —
// rather than routed through a parameter that does not control it.
//
// It belongs to the last line only. Banner lines above it end where
// their content ends, and a trailing space there would fight the gap
// fill that places the right prompt.
const promptGap = " "

// Render produces the full prompt for one read.
func Render(cfg *Config, ctx *Context) Prompt {
	left := splitLines(cfg.List("LEFT_PROMPT_ELEMENTS"))
	right := splitLines(cfg.List("RIGHT_PROMPT_ELEMENTS"))

	lines := max(len(left), len(right))
	if lines == 0 {
		return Prompt{Prompt: "%# ", Cont: "> "}
	}

	rendered := make([]string, lines)
	var rprompt string
	for i := range lines {
		leftText := renderSide(cfg, ctx, sideLeft, lineAt(left, i))
		rightText := renderSide(cfg, ctx, sideRight, lineAt(right, i))

		prefix := Expand(decodeEscapes(cfg.Str(framePrefixKey(i, lines), "")), nil)
		suffix := Expand(decodeEscapes(cfg.Str(frameSuffixKey(i, lines), "")), nil)

		if i == lines-1 {
			// The last line is the one being typed on. Its right side
			// becomes a true right prompt so the editor can hide it when
			// the typed line grows into it, rather than baking it into
			// text that would then wrap.
			rendered[i] = prefix + leftText + promptGap
			rprompt = rightText + suffix
			continue
		}
		rendered[i] = joinLine(cfg, ctx, prefix+leftText, rightText+suffix, i)
	}

	prompt := strings.Join(rendered, "\n")
	if cfg.Bool("PROMPT_ADD_NEWLINE", false) {
		prompt = "\n" + prompt
	}
	return Prompt{Prompt: prompt, Cont: contPrompt(cfg), RPrompt: rprompt}
}

// RenderTransient produces the prompt an accepted line is left with: the
// last line's left-hand segments only, with no frame, no right side and
// no banner above it.
//
// The point is scrollback. A two-line framed prompt with a clock and a
// git status is worth looking at while you are typing into it; twenty of
// them stacked above your output is just noise you have to read past.
// Upstream calls this the transient prompt, and it is the single
// setting most responsible for a themed prompt staying usable.
//
// ok is false when the configuration does not ask for one.
func RenderTransient(cfg *Config, ctx *Context) (string, bool) {
	mode := strings.ToLower(cfg.Str("TRANSIENT_PROMPT", "off"))
	switch mode {
	case "always":
	case "same-dir":
		// Upstream's reasoning: when a command changed directory, the
		// full prompt is the record of where you were, so it stays.
		if ctx.PrevCwd != "" && ctx.PrevCwd != ctx.Cwd {
			return "", false
		}
	default:
		return "", false
	}

	left := splitLines(cfg.List("LEFT_PROMPT_ELEMENTS"))
	if len(left) == 0 {
		return "", false
	}
	line := renderSide(cfg, ctx, sideLeft, left[len(left)-1])
	if strings.TrimSpace(plainText(line)) == "" {
		// A last line with nothing on it (an element list that ends in a
		// newline) would collapse the accepted command onto a bare line
		// with no marker at all. Keep the full prompt instead.
		return "", false
	}
	// After the emptiness check, not before: the check reads the segments
	// alone, and a gap added first would make every empty last line look
	// occupied. The accepted line keeps the gap so scrollback reads the
	// same as the live prompt did.
	return line + promptGap, true
}

// plainText drops escapes, for emptiness checks.
func plainText(s string) string {
	var b strings.Builder
	for len(s) > 0 {
		if rest, ok := skipEscape(s); ok {
			s = rest
			continue
		}
		b.WriteByte(s[0])
		s = s[1:]
	}
	return b.String()
}

// joinLine puts the two halves of a banner line at opposite edges,
// filling the middle with the gap character. When the width is unknown
// or the halves would collide, the right half is dropped rather than
// wrapped — a wrapped frame line corrupts every repaint after it.
func joinLine(cfg *Config, ctx *Context, left, right string, lineIdx int) string {
	if right == "" {
		return left
	}
	if ctx.Width <= 0 {
		// Width unknown: there is no way to place the right side, and
		// gluing it on wraps the line at whatever the real edge turns
		// out to be — which smears the frame on every repaint after it.
		// The editor's rule for a right prompt applies here too: hide
		// rather than wrap.
		return left
	}
	gap := ctx.Width - displayWidth(left) - displayWidth(right)
	if gap < 1 {
		return left
	}
	gapChar := cfg.Str("MULTILINE_FIRST_PROMPT_GAP_CHAR", " ")
	if lineIdx > 0 {
		gapChar = cfg.Str("MULTILINE_NEWLINE_PROMPT_GAP_CHAR", gapChar)
	}
	if gapChar == "" {
		gapChar = " "
	}
	gapChar = decodeEscapes(gapChar)
	filler := strings.Repeat(gapChar, gap/max(displayWidth(gapChar), 1))
	if fg, ok := ParseColor(cfg.Str("MULTILINE_FIRST_PROMPT_GAP_FOREGROUND", "")); ok {
		filler = Style{Fg: fg}.Apply(filler)
	}
	return left + filler + right
}

// framePrefixKey and frameSuffixKey name the frame decoration for a
// line, which differs for the first line, the last, and any between.
func framePrefixKey(i, lines int) string {
	switch {
	case lines == 1 || i == lines-1:
		return "MULTILINE_LAST_PROMPT_PREFIX"
	case i == 0:
		return "MULTILINE_FIRST_PROMPT_PREFIX"
	default:
		return "MULTILINE_NEWLINE_PROMPT_PREFIX"
	}
}

func frameSuffixKey(i, lines int) string {
	switch {
	case lines == 1 || i == lines-1:
		return "MULTILINE_LAST_PROMPT_SUFFIX"
	case i == 0:
		return "MULTILINE_FIRST_PROMPT_SUFFIX"
	default:
		return "MULTILINE_NEWLINE_PROMPT_SUFFIX"
	}
}

// renderSide runs the segments of one line and composes them.
func renderSide(cfg *Config, ctx *Context, s side, elements []string) string {
	segs := make([]placed, 0, len(elements))
	for _, name := range elements {
		fn, ok := registry[name]
		if !ok {
			continue // unknown element: silently absent, reported by doctor
		}
		out, show := fn(cfg, ctx)
		if !show {
			continue
		}
		segs = append(segs, placed{
			name:  name,
			out:   out,
			style: cfg.SegmentStyle(name, out.State, defaultForeground(name), cfg.Str("BACKGROUND", "")),
		})
	}
	if len(segs) == 0 {
		return ""
	}

	var b strings.Builder
	var prevBg Color
	for i, p := range segs {
		b.WriteString(separator(cfg, s, i == 0, p, prevBg))
		b.WriteString(segmentBody(cfg, s, p))
		prevBg = p.style.Bg
	}
	b.WriteString(endSymbol(cfg, s, segs[len(segs)-1], prevBg))
	return b.String()
}

// separator emits what goes before a segment: the side's start symbol
// for the first one, then either a subsegment separator (same background
// as the previous segment, so the boundary is a hairline) or a full
// segment separator (backgrounds differ, so the boundary is an arrow
// colored from one background to the other).
func separator(cfg *Config, s side, first bool, p placed, prevBg Color) string {
	if first {
		sym := symbolFor(cfg, p.name, s.String()+"_PROMPT_FIRST_SEGMENT_START_SYMBOL")
		if sym == "" {
			return ""
		}
		return arrowStyle(s, p.style.Bg, Color{}).Apply(sym)
	}
	same := prevBg.params == p.style.Bg.params && prevBg.set == p.style.Bg.set
	if same {
		sym := symbolFor(cfg, p.name, s.String()+"_SUBSEGMENT_SEPARATOR")
		if sym == "" {
			return ""
		}
		fg, _ := ParseColor(cfg.Param(p.name, "", "SUBSEGMENT_SEPARATOR_FOREGROUND", ""))
		return Style{Fg: fg, Bg: p.style.Bg}.Apply(sym)
	}
	sym := symbolFor(cfg, p.name, s.String()+"_SEGMENT_SEPARATOR")
	if sym == "" {
		return ""
	}
	return arrowStyle(s, p.style.Bg, prevBg).Apply(sym)
}

// arrowStyle colors a powerline separator. On the left the arrow is
// drawn in the *previous* background over the next one; on the right the
// relationship is mirrored, which is why the two sides need different
// glyphs as well as different colors.
func arrowStyle(s side, curBg, prevBg Color) Style {
	if s == sideLeft {
		return Style{Fg: prevBg, Bg: curBg}
	}
	return Style{Fg: curBg, Bg: prevBg}
}

// endSymbol closes a side: on the left the arrow trails off into the
// terminal background, on the right there is usually nothing.
func endSymbol(cfg *Config, s side, last placed, lastBg Color) string {
	key := "LEFT_PROMPT_LAST_SEGMENT_END_SYMBOL"
	if s == sideRight {
		key = "RIGHT_PROMPT_LAST_SEGMENT_END_SYMBOL"
	}
	sym := symbolFor(cfg, last.name, key)
	if sym == "" {
		return ""
	}
	return Style{Fg: lastBg}.Apply(sym)
}

// segmentBody is whitespace, icon and content in the segment's colors.
func segmentBody(cfg *Config, s side, p placed) string {
	leftWS := symbolOr(cfg, p.name, s.String()+"_LEFT_WHITESPACE", " ")
	rightWS := symbolOr(cfg, p.name, s.String()+"_RIGHT_WHITESPACE", " ")

	// Spans carry their own foregrounds; the segment's background goes
	// under every one of them, including the padding, so the ribbon has
	// no holes in it.
	if len(p.out.Spans) > 0 {
		var b strings.Builder
		b.WriteString(Style{Bg: p.style.Bg}.Apply(leftWS))
		if icon := p.out.Icon; icon != "" && cfg.ParamBool(p.name, p.out.State, "ICON_BEFORE_CONTENT", true) {
			b.WriteString(p.style.Apply(icon + iconPad(cfg)))
		}
		for _, s := range coalesce(p.out.Spans, p.style) {
			fg := s.Fg
			if !fg.set {
				fg = p.style.Fg
			}
			b.WriteString(Style{Fg: fg, Bg: p.style.Bg, Bold: s.Bold || p.style.Bold}.Apply(s.Text))
		}
		if icon := p.out.Icon; icon != "" && !cfg.ParamBool(p.name, p.out.State, "ICON_BEFORE_CONTENT", true) {
			b.WriteString(p.style.Apply(iconPad(cfg) + icon))
		}
		b.WriteString(Style{Bg: p.style.Bg}.Apply(rightWS))
		return b.String()
	}

	content := p.out.Content
	if icon := p.out.Icon; icon != "" {
		pad := iconPad(cfg)
		if cfg.ParamBool(p.name, p.out.State, "ICON_BEFORE_CONTENT", true) {
			content = icon + pad + content
		} else {
			content = content + pad + icon
		}
	}
	return p.style.Apply(leftWS + content + rightWS)
}

// coalesce merges neighboring spans that would render identically.
//
// A path arrives as one span per component and separator, which would
// otherwise emit an escape sequence for each — five for ~/dev/gish. The
// prompt is re-rendered on every keystroke, so those bytes are paid for
// continuously; merging them is free and makes the output legible when
// something goes wrong and you have to read the escapes by eye.
func coalesce(spans []Span, base Style) []Span {
	out := make([]Span, 0, len(spans))
	for _, s := range spans {
		if s.Text == "" {
			continue
		}
		fg := s.Fg
		if !fg.set {
			fg = base.Fg
		}
		s.Fg = fg
		if n := len(out); n > 0 && out[n-1].Fg == fg && out[n-1].Bold == s.Bold {
			out[n-1].Text += s.Text
			continue
		}
		out = append(out, s)
	}
	return out
}

// iconPad is the gap between an icon and its content.
func iconPad(cfg *Config) string {
	switch strings.ToLower(cfg.Str("ICON_PADDING", "none")) {
	case "generous":
		return "  "
	case "none":
		return ""
	default: // moderate
		return " "
	}
}

// symbolFor resolves a decorative symbol through the segment fallback
// chain, decoding the \uXXXX spellings presets use for glyphs.
func symbolFor(cfg *Config, segment, key string) string {
	return decodeEscapes(cfg.Param(segment, "", key, ""))
}

// symbolOr is symbolFor with a default for keys the user has not set at
// all — distinguishing unset from deliberately empty.
func symbolOr(cfg *Config, segment, key, def string) string {
	if !cfg.ParamSet(segment, "", key) {
		return def
	}
	return decodeEscapes(cfg.Param(segment, "", key, def))
}

// contPrompt is the continuation prompt. Upstream has no equivalent —
// zsh keeps PS2 separate — so this follows the frame: a multi-line
// prompt continues with its own newline decoration, and anything else
// gets a plain marker.
func contPrompt(cfg *Config) string {
	if v := cfg.Str("CONT_PROMPT", ""); v != "" {
		return Expand(decodeEscapes(v), nil)
	}
	if v := cfg.Str("MULTILINE_NEWLINE_PROMPT_PREFIX", ""); v != "" {
		return Expand(decodeEscapes(v), nil) + " "
	}
	return Style{Fg: mustColor("242")}.Apply("│") + " "
}

// mustColor is for the package's own literals, which are known good.
func mustColor(spec string) Color {
	c, _ := ParseColor(spec)
	return c
}

// splitLines cuts an element list at each newline pseudo-element.
func splitLines(elements []string) [][]string {
	if len(elements) == 0 {
		return nil
	}
	lines := [][]string{{}}
	for _, e := range elements {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if e == elementNewline {
			lines = append(lines, []string{})
			continue
		}
		lines[len(lines)-1] = append(lines[len(lines)-1], e)
	}
	// A trailing newline element means "the last line is empty", which is
	// how right-hand element lists park their content on the first line.
	return lines
}

// lineAt returns the elements for one line, or nothing when this side
// has fewer lines than the other.
func lineAt(lines [][]string, i int) []string {
	if i >= len(lines) {
		return nil
	}
	return lines[i]
}
