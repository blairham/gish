package p10k

import (
	"runtime"
	"strconv"
	"strings"
)

// Shell-state segments: the ones that answer "what kind of shell am I
// in?". Almost all of them are a single environment variable, because
// the tools that spawn a subshell announce themselves that way.
//
// They exist for one reason: a nested shell looks exactly like a normal
// one until a command behaves unexpectedly. Saying so costs a map
// lookup.

// envSegment is a segment that shows when a variable is set.
type envSegment struct {
	segment string
	envVar  string
	label   string // fixed text, or empty to show the variable's value
	icon    string
}

var envSegments = []envSegment{
	{"direnv", "DIRENV_DIR", "direnv", ""},
	{"nix_shell", "IN_NIX_SHELL", "nix", ""},
	{"chezmoi_shell", "CHEZMOI", "chezmoi", ""},
	{"vim_shell", "VIMRUNTIME", "vim", ""},
	{"midnight_commander", "MC_SID", "mc", ""},
	{"ranger", "RANGER_LEVEL", "ranger", ""},
	{"yazi", "YAZI_LEVEL", "yazi", ""},
	{"nnn", "NNNLVL", "nnn", ""},
	{"lf", "LF_LEVEL", "lf", ""},
	{"xplr", "XPLR_PID", "xplr", ""},
	{"per_directory_history", "_per_directory_history_is_global", "", ""},
	{"toolbox_shell", "TOOLBOX_PATH", "toolbox", "⬢"},
}

func init() {
	for _, es := range envSegments {
		register(es.segment, es.render)
	}
	register("os_icon", renderOSIcon)
	register("cpu_arch", renderCPUArch)
	register("todo", renderTodo)
	register("proxy", renderProxy)
	register("detect_virt", renderDetectVirt)
}

func (es envSegment) render(cfg *Config, ctx *Context) (Rendered, bool) {
	value := ctx.Env(es.envVar)
	if value == "" {
		return Rendered{}, false
	}
	content := es.label
	if content == "" {
		content = value
	}
	return Rendered{
		Content: content,
		Icon:    decodeEscapes(cfg.Icon(es.segment, "", es.icon)),
	}, true
}

// renderOSIcon marks which operating system this is — worth a glyph when
// you keep several terminals open across several machines.
func renderOSIcon(cfg *Config, ctx *Context) (Rendered, bool) {
	icons := map[string]string{
		"darwin": "", "linux": "", "windows": "", "freebsd": "", "openbsd": "",
	}
	icon := icons[runtime.GOOS]
	if icon == "" {
		icon = runtime.GOOS
	}
	return Rendered{
		Icon: decodeEscapes(cfg.Icon("os_icon", "", icon)),
	}, true
}

func renderCPUArch(cfg *Config, ctx *Context) (Rendered, bool) {
	return Rendered{
		Content: runtime.GOARCH,
		Icon:    decodeEscapes(cfg.Icon("cpu_arch", "", "")),
	}, true
}

// renderTodo counts outstanding todo.txt items.
func renderTodo(cfg *Config, ctx *Context) (Rendered, bool) {
	path, found := ctx.FindUp("todo.txt")
	if !found {
		return Rendered{}, false
	}
	count := 0
	for line := range strings.SplitSeq(readFile(path), "\n") {
		line = strings.TrimSpace(line)
		// A leading "x " is todo.txt's marker for a completed task.
		if line != "" && !strings.HasPrefix(line, "x ") {
			count++
		}
	}
	if count == 0 {
		return Rendered{}, false
	}
	return Rendered{
		Content: strconv.Itoa(count),
		Icon:    decodeEscapes(cfg.Icon("todo", "", "☑")),
	}, true
}

// renderProxy shows that traffic is going through a proxy, which is the
// other classic "why is this command behaving strangely" cause.
func renderProxy(cfg *Config, ctx *Context) (Rendered, bool) {
	for _, name := range []string{"all_proxy", "ALL_PROXY", "http_proxy", "HTTP_PROXY", "https_proxy", "HTTPS_PROXY"} {
		if ctx.Env(name) != "" {
			return Rendered{
				Content: "proxy",
				Icon:    decodeEscapes(cfg.Icon("proxy", "", "")),
			}, true
		}
	}
	return Rendered{}, false
}

// renderDetectVirt reports a container, when the environment says so.
// Upstream asks systemd-detect-virt; that is a subprocess, so this is
// limited to the markers containers leave behind in the environment.
func renderDetectVirt(cfg *Config, ctx *Context) (Rendered, bool) {
	switch {
	case ctx.Env("container") != "":
		return virtRendered(cfg, ctx.Env("container")), true
	case ctx.Env("DOCKER_CONTAINER") != "", ctx.Env("KUBERNETES_SERVICE_HOST") != "":
		return virtRendered(cfg, "container"), true
	}
	return Rendered{}, false
}

func virtRendered(cfg *Config, label string) Rendered {
	return Rendered{
		Content: label,
		Icon:    decodeEscapes(cfg.Icon("detect_virt", "", "")),
	}
}
