package p10k

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// The segments every preset leans on. Each one is a pure function of the
// config and the context: read, decide, return. No segment here starts a
// process or waits on anything.

func init() {
	register("dir", renderDir)
	register("vcs", renderVCS)
	register("prompt_char", renderPromptChar)
	register("status", renderStatus)
	register("command_execution_time", renderExecutionTime)
	register("background_jobs", renderJobs)
	register("context", renderContext)
	register("time", renderTime)
}

// defaultForeground keeps a bare configuration looking deliberate. A
// preset overrides all of these; a user who sets only PROMPT_ELEMENTS
// still gets a prompt that reads as designed rather than as monochrome.
func defaultForeground(segment string) string {
	switch segment {
	case "dir":
		return "blue"
	case "vcs":
		return "green"
	case "status":
		return "green"
	case "command_execution_time":
		return "yellow"
	case "background_jobs":
		return "cyan"
	case "context":
		return "yellow"
	case "time":
		return "242"
	default:
		return "" // unset: the segment decides, or the terminal default stands
	}
}

// ---------------------------------------------------------------- dir

func renderDir(cfg *Config, ctx *Context) (Rendered, bool) {
	if ctx.Cwd == "" {
		return Rendered{}, false
	}
	path := tildify(ctx.Cwd, ctx.Home)

	// Writability costs a syscall, so it is only paid for when the
	// configuration actually shows it — upstream's default is off too.
	state := ""
	if cfg.Bool("DIR_SHOW_WRITABLE", false) && !writable(ctx.Cwd) {
		state = "NOT_WRITABLE"
	}
	// A directory class is a parameter *state* (#133), which is why it
	// needs no new mechanism: POWERLEVEL9K_DIR_WORK_FOREGROUND resolves
	// through the same three-step chain everything else uses. People use
	// this to make production directories visibly different, which makes
	// it a safety feature rather than a decoration.
	classIcon := ""
	if class, icon := classifyDir(cfg, ctx.Cwd, ctx.Home); class != "" {
		if state == "" {
			state = class
		}
		classIcon = icon
	}

	budget := cfg.ParamInt("dir", state, "MAX_LENGTH", 0)
	if budget <= 0 && ctx.Width > 0 {
		// No explicit cap: keep the directory from eating the line, the
		// way upstream's default does, but only once it actually would.
		budget = ctx.Width / 2
	}
	// The command line gets its columns first: a path is context, and
	// the thing being typed is the work.
	if reserved := commandColumns(cfg, ctx.Width); reserved > 0 {
		if room := ctx.Width - reserved; room < budget {
			budget = max(room, 1)
		}
	}
	parts := shortenPath(cfg, ctx, path, budget)

	// A path is colored in three registers: shortened components
	// recede, the final component (upstream's "anchor") stands out, and
	// everything else takes the segment's own color. Returning that as
	// spans is what lets a preset put a background under all of it.
	shortenedFg := colorOf(cfg, "dir", "SHORTENED", "")
	anchorFg, hasAnchor := ParseColor(cfg.Param("dir", state, "ANCHOR_FOREGROUND", ""))
	anchorBold := cfg.ParamBool("dir", state, "ANCHOR_BOLD", false)

	spans := make([]Span, 0, len(parts)*2)
	for i, part := range parts {
		if i > 0 {
			spans = append(spans, Span{Text: string(os.PathSeparator)})
		}
		switch {
		case i == len(parts)-1 && part.text != "":
			s := Span{Text: part.text, Bold: anchorBold}
			if hasAnchor {
				s.Fg = anchorFg
			}
			spans = append(spans, s)
		case part.shortened:
			spans = append(spans, Span{Text: part.text, Fg: shortenedFg})
		default:
			spans = append(spans, Span{Text: part.text})
		}
	}

	// OSC 8 costs nothing in layout: the renderer discounts escape
	// sequences from every width calculation, so a hyperlinked path
	// occupies exactly the columns its text does.
	if cfg.Bool("DIR_HYPERLINK", false) && len(spans) > 0 {
		spans[0].Text = hyperlinkOpen(ctx.Cwd) + spans[0].Text
		spans[len(spans)-1].Text += hyperlinkClose
	}

	out := Rendered{Spans: spans, State: state}
	switch {
	case classIcon != "":
		out.Icon = decodeEscapes(classIcon)
	case state != "":
		out.Icon = decodeEscapes(cfg.Icon("dir", state, ""))
	}
	return out, true
}

// component is one element of a displayed path, and whether it had to be
// abbreviated to fit the width budget.
type component struct {
	text      string
	shortened bool
}

// tildify abbreviates the home directory to ~, matching every shell
// prompt anyone has used since 1979.
func tildify(dir, home string) string {
	if home != "" && dir == home {
		return "~"
	}
	if home == "" {
		return dir
	}
	if rest, ok := strings.CutPrefix(dir, home+string(os.PathSeparator)); ok {
		return "~" + string(os.PathSeparator) + rest
	}
	return dir
}

// shortenPath splits a path into components and brings it under a width
// budget, reporting which components it had to abbreviate.
//
// The strategies are upstream's, minus the ones that need to read the
// filesystem: nothing here stats a directory. Upstream's default
// ("shorten to unique prefix") lists every parent's siblings on every
// prompt, and that is a cost that shows up on network filesystems — the
// first-character rule below is the same shape without the I/O.
// A nil context means "no filesystem": the marker rules are the only
// part that needs one, and every other caller — including the tests —
// passes nil and gets the pure behavior.
func shortenPath(cfg *Config, ctx *Context, path string, budget int) []component {
	sep := string(os.PathSeparator)
	raw := strings.Split(path, sep)
	parts := make([]component, len(raw))
	for i, r := range raw {
		parts[i] = component{text: r}
	}

	// A marked directory (one holding .git, go.mod, …) is the one people
	// navigate by, so it survives shortening — and TRUNCATE_BEFORE_MARKER
	// says to drop everything above the last one entirely.
	var marked map[int]bool
	if ctx != nil {
		if idx := markerIndices(cfg, ctx, ctx.Cwd); len(idx) > 0 {
			marked = map[int]bool{}
			offset := len(parts) - len(strings.Split(ctx.Cwd, sep))
			for _, i := range idx {
				marked[i+offset] = true
			}
			if cfg.Bool("DIR_TRUNCATE_BEFORE_MARKER", false) {
				last := idx[len(idx)-1] + offset
				if last > 0 && last < len(parts) {
					parts = parts[last:]
					marked = map[int]bool{0: true}
				}
			}
		}
	}

	if len(parts) <= 1 || budget <= 0 || width(parts) <= budget {
		return parts
	}

	switch strings.ToLower(cfg.Str("SHORTEN_STRATEGY", "")) {
	case "truncate_to_last":
		return parts[len(parts)-1:]
	case "truncate_middle":
		if len(parts) > 2 {
			return []component{parts[0], {text: "…", shortened: true}, parts[len(parts)-1]}
		}
		return parts
	case "truncate_to_first_and_last":
		if len(parts) > 2 {
			out := []component{parts[0]}
			for range parts[1 : len(parts)-1] {
				out = append(out, component{text: "…", shortened: true})
			}
			return append(out, parts[len(parts)-1])
		}
		return parts
	}

	// Default: keep the tail readable and shorten leading components to
	// their first character, one at a time, until it fits.
	keep := max(cfg.Int("SHORTEN_DIR_LENGTH", 1), 1)
	for i := range parts {
		if width(parts) <= budget || i >= len(parts)-keep {
			break
		}
		if parts[i].text == "" || parts[i].text == "~" || marked[i] {
			continue
		}
		parts[i] = component{text: firstRune(parts[i].text), shortened: true}
	}
	return parts
}

// width is the rendered width of a component list, separators included.
func width(parts []component) int {
	total := len(parts) - 1
	for _, p := range parts {
		total += displayWidth(p.text)
	}
	return total
}

func firstRune(s string) string {
	for _, r := range s {
		return string(r)
	}
	return s
}

// ---------------------------------------------------------------- vcs

func renderVCS(cfg *Config, ctx *Context) (Rendered, bool) {
	g := ctx.Git
	if g == nil || g.Dir == "" {
		return Rendered{}, false
	}

	state := "CLEAN"
	switch {
	case g.Conflicted > 0:
		state = "CONFLICTED"
	case g.Staged > 0 || g.Modified > 0:
		state = "MODIFIED"
	case g.Untracked > 0:
		state = "UNTRACKED"
	}

	clean := colorOf(cfg, "vcs", "CLEAN", "green")
	modified := colorOf(cfg, "vcs", "MODIFIED", "yellow")
	untracked := colorOf(cfg, "vcs", "UNTRACKED", "blue")
	conflicted := colorOf(cfg, "vcs", "CONFLICTED", "red")

	spans := []Span{{Text: branchLabel(cfg, g)}}

	// Counters, each in the color that explains what it is. The order is
	// upstream's, because people read these positionally.
	add := func(c Color, format string, n int) {
		if n > 0 {
			spans = append(spans, Span{Text: " " + fmt.Sprintf(format, n), Fg: c})
		}
	}
	add(clean, "⇣%d", g.Behind)
	add(clean, "⇡%d", g.Ahead)
	add(clean, "⇠%d", g.PushBehind)
	add(clean, "⇢%d", g.PushAhead)
	add(clean, "*%d", g.Stashed)
	add(conflicted, "~%d", g.Conflicted)
	add(modified, "+%d", g.Staged)
	add(modified, "!%d", g.Modified)
	add(untracked, "?%d", g.Untracked)

	if g.Action != "" {
		spans = append(spans, Span{Text: " | " + g.Action, Fg: conflicted})
	}
	if g.Stale {
		// Say so rather than present a cached answer as a current one.
		spans = append(spans, Span{Text: " …", Fg: mustColor("242")})
	}

	return Rendered{Spans: spans, State: state}, true
}

// branchLabel is what identifies the checkout: branch, else tag, else
// the short commit for a detached head.
func branchLabel(cfg *Config, g *GitStatus) string {
	icon := decodeEscapes(cfg.Param("vcs", "", "BRANCH_ICON", ""))
	suffix := decodeEscapes(cfg.Param("vcs", "", "BRANCH_SUFFIX", ""))
	switch {
	case g.Branch != "":
		label := icon + g.Branch + suffix
		if g.RemoteRef != "" && g.RemoteRef != g.Branch {
			label += ":" + g.RemoteRef
		}
		return label
	case g.Tag != "":
		return "#" + g.Tag
	case g.Commit != "":
		return "@" + g.Commit
	default:
		return icon + "(empty)" + suffix
	}
}

func colorOf(cfg *Config, segment, state, def string) Color {
	c, _ := ParseColor(cfg.Param(segment, state, "FOREGROUND", def))
	return c
}

// -------------------------------------------------------- prompt_char

func renderPromptChar(cfg *Config, ctx *Context) (Rendered, bool) {
	state := "OK"
	if ctx.ExitCode != 0 {
		state = "ERROR"
	}
	def := "❯"
	if ctx.Root {
		def = "#"
	}
	ch := cfg.Param("prompt_char", state, "CONTENT_EXPANSION", "")
	if ch == "" {
		ch = def
	}
	return Rendered{Content: decodeEscapes(ch), State: state}, true
}

// ------------------------------------------------------------- status

func renderStatus(cfg *Config, ctx *Context) (Rendered, bool) {
	if ctx.ExitCode == 0 {
		if !cfg.ParamBool("status", "OK", "OK", false) {
			return Rendered{}, false
		}
		return Rendered{
			Content: cfg.Param("status", "OK", "CONTENT_EXPANSION", ""),
			Icon:    decodeEscapes(cfg.Icon("status", "OK", "✔")),
			State:   "OK",
		}, true
	}

	if !cfg.ParamBool("status", "ERROR", "ERROR", true) {
		return Rendered{}, false
	}
	content := strconv.Itoa(ctx.ExitCode)
	state := "ERROR"
	// A signal death is more legible by name than by number, and this is
	// exactly the moment someone wants to know which signal it was.
	if name, ok := signalName(ctx.ExitCode); ok && cfg.Bool("STATUS_VERBOSE_SIGNAME", true) {
		content, state = name, "ERROR_SIGNAL"
	}
	return Rendered{
		Content: content,
		Icon:    decodeEscapes(cfg.Icon("status", state, "✘")),
		State:   state,
	}, true
}

// signalName turns a shell's 128+N exit status back into a signal name.
func signalName(code int) (string, bool) {
	if code <= 128 || code > 128+64 {
		return "", false
	}
	names := map[int]string{
		1: "HUP", 2: "INT", 3: "QUIT", 4: "ILL", 6: "ABRT", 8: "FPE",
		9: "KILL", 11: "SEGV", 13: "PIPE", 14: "ALRM", 15: "TERM",
	}
	if name, ok := names[code-128]; ok {
		return "SIG" + name, true
	}
	return "", false
}

// --------------------------------------------- command_execution_time

func renderExecutionTime(cfg *Config, ctx *Context) (Rendered, bool) {
	threshold := cfg.ParamInt("command_execution_time", "", "THRESHOLD", 3)
	if ctx.Duration < time.Duration(threshold)*time.Second {
		return Rendered{}, false
	}
	precision := cfg.ParamInt("command_execution_time", "", "PRECISION", 0)
	return Rendered{
		Content: formatDuration(ctx.Duration, precision),
		Icon:    decodeEscapes(cfg.Icon("command_execution_time", "", "")),
	}, true
}

// formatDuration renders like upstream: seconds under a minute, then
// m:ss, then h:mm:ss — the units people actually compare.
func formatDuration(d time.Duration, precision int) string {
	secs := d.Seconds()
	switch {
	case d < time.Minute:
		return strconv.FormatFloat(secs, 'f', precision, 64) + "s"
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(secs)%60)
	default:
		return fmt.Sprintf("%dh%02dm%02ds", int(d.Hours()), int(d.Minutes())%60, int(secs)%60)
	}
}

// ---------------------------------------------------- background_jobs

func renderJobs(cfg *Config, ctx *Context) (Rendered, bool) {
	if ctx.Jobs <= 0 {
		return Rendered{}, false
	}
	content := ""
	if cfg.ParamBool("background_jobs", "", "VERBOSE", true) {
		content = strconv.Itoa(ctx.Jobs)
	}
	return Rendered{
		Content: content,
		Icon:    decodeEscapes(cfg.Icon("background_jobs", "", "⚙")),
	}, true
}

// ------------------------------------------------------------ context

func renderContext(cfg *Config, ctx *Context) (Rendered, bool) {
	state := "DEFAULT"
	switch {
	case ctx.Root:
		state = "ROOT"
	case ctx.SSH:
		state = "REMOTE"
	}
	// Upstream's default is to stay quiet locally: user@host is noise
	// until you are somewhere it could surprise you.
	if state == "DEFAULT" && !cfg.ParamBool("context", state, "ALWAYS_SHOW", false) {
		return Rendered{}, false
	}
	tmpl := cfg.Param("context", state, "TEMPLATE", "%n@%m")
	content := strings.NewReplacer("%n", ctx.Username, "%m", ctx.Hostname).Replace(tmpl)
	return Rendered{Content: content, State: state}, true
}

// --------------------------------------------------------------- time

func renderTime(cfg *Config, ctx *Context) (Rendered, bool) {
	format := cfg.Param("time", "", "FORMAT", "15:04:05")
	return Rendered{
		Content: strftime(ctx.clock(), format),
		Icon:    decodeEscapes(cfg.Icon("time", "", "")),
	}, true
}

// strftime accepts both Go layouts and the strftime spellings a ported
// configuration will contain, so a transcribed TIME_FORMAT works
// unedited — including zsh's %D{...} date wrapper.
func strftime(t time.Time, format string) string {
	if inner, ok := strings.CutPrefix(format, "%D{"); ok {
		if trimmed, closed := strings.CutSuffix(inner, "}"); closed {
			format = trimmed
		}
	}
	if !strings.Contains(format, "%") {
		return t.Format(format)
	}
	repl := strings.NewReplacer(
		"%H", "15", "%M", "04", "%S", "05",
		"%I", "03", "%p", "PM", "%y", "06", "%Y", "2006",
		"%m", "01", "%d", "02", "%e", "_2",
		"%a", "Mon", "%A", "Monday", "%b", "Jan", "%B", "January",
		"%D", "01/02/06", "%F", "2006-01-02", "%T", "15:04:05",
		"%%", "%",
	)
	return t.Format(repl.Replace(format))
}
