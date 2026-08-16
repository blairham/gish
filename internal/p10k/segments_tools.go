package p10k

import (
	"path/filepath"
	"strings"
)

// Version-manager segments.
//
// There are a dozen of these and they are all the same idea: a language
// runtime is pinned somewhere, and the prompt should say so when the pin
// is local to what you are working on. Upstream implements them one by
// one; here they are one rule with a table, because the differences are
// data — which environment variable, which dotfile, which icon.
//
// The rule, and the reason it is this rule: show the version when
// something in this directory tree asked for it (a .python-version, an
// exported PYENV_VERSION), and stay quiet otherwise. A prompt that
// announces the global default everywhere is noise; a prompt that goes
// quiet the moment you leave a project is information.
//
// Nothing here runs `pyenv version`. Every one of these managers writes
// its pin to a file, and reading the file is microseconds against tens
// of milliseconds for a subprocess — which is the entire reason prompts
// built on `$(tool --version)` feel slow.

// versionManager describes one runtime pin.
type versionManager struct {
	segment string // element name, upstream's spelling
	envVar  string // an explicit pin exported into the environment
	local   string // per-project pin file, searched up the tree
	global  string // home-relative fallback, shown only when asked
	icon    string
}

var versionManagers = []versionManager{
	{"pyenv", "PYENV_VERSION", ".python-version", ".pyenv/version", ""},
	{"goenv", "GOENV_VERSION", ".go-version", ".goenv/version", ""},
	{"nodenv", "NODENV_VERSION", ".node-version", ".nodenv/version", ""},
	{"rbenv", "RBENV_VERSION", ".ruby-version", ".rbenv/version", ""},
	{"jenv", "JENV_VERSION", ".java-version", ".jenv/version", ""},
	{"plenv", "PLENV_VERSION", ".perl-version", ".plenv/version", ""},
	{"phpenv", "PHPENV_VERSION", ".php-version", ".phpenv/version", ""},
	{"luaenv", "LUAENV_VERSION", ".lua-version", ".luaenv/version", ""},
	{"scalaenv", "SCALAENV_VERSION", ".scala-version", ".scalaenv/version", ""},
	{"haskell_stack", "STACK_YAML", "stack.yaml", "", ""},
	{"fvm", "", ".fvmrc", "", ""},
	{"terraform", "", ".terraform/environment", "", ""},
}

func init() {
	for _, vm := range versionManagers {
		register(vm.segment, vm.render)
	}
	register("asdf", renderAsdf)
	register("virtualenv", renderVirtualenv)
	register("anaconda", renderAnaconda)
	register("nodeenv", renderNodeenv)
	register("nvm", renderNvm)
	register("rvm", renderRvm)
	register("perlbrew", renderPerlbrew)
}

// render is the shared implementation. The closure captures vm, so
// adding a manager is a table row.
func (vm versionManager) render(cfg *Config, ctx *Context) (Rendered, bool) {
	version := ""
	switch {
	case vm.envVar != "" && ctx.Env(vm.envVar) != "":
		version = ctx.Env(vm.envVar)
		// STACK_YAML and friends hold a path, not a version.
		if strings.ContainsRune(version, filepath.Separator) {
			version = filepath.Base(filepath.Dir(version))
		}
	case vm.local != "":
		path, found := ctx.FindUp(vm.local)
		if !found {
			break
		}
		version = firstLine(path)
		if version == "" {
			// A marker file with no version in it (stack.yaml, a
			// .terraform directory) still says "this is that kind of
			// project" — name the project rather than nothing.
			version = filepath.Base(filepath.Dir(path))
		}
	}

	if version == "" && cfg.ParamBool(vm.segment, "", "PROMPT_ALWAYS_SHOW", false) && vm.global != "" {
		version = firstLine(filepath.Join(ctx.Home, vm.global))
	}
	if version == "" || version == "system" && !cfg.ParamBool(vm.segment, "", "SHOW_SYSTEM", false) {
		return Rendered{}, false
	}
	return Rendered{
		Content: version,
		Icon:    decodeEscapes(cfg.Param(vm.segment, "", "VISUAL_IDENTIFIER_EXPANSION", vm.icon)),
	}, true
}

// renderAsdf shows the pins from the nearest .tool-versions.
//
// asdf pins several tools at once, so unlike the single-runtime managers
// this one can have a lot to say. It is capped, because a prompt that
// lists nine runtimes has stopped being a prompt.
func renderAsdf(cfg *Config, ctx *Context) (Rendered, bool) {
	path, found := ctx.FindUp(".tool-versions")
	if !found {
		return Rendered{}, false
	}
	data := firstLines(path, cfg.ParamInt("asdf", "", "MAX_TOOLS", 3))
	if len(data) == 0 {
		return Rendered{}, false
	}
	return Rendered{
		Content: strings.Join(data, " "),
		Icon:    decodeEscapes(cfg.Param("asdf", "", "VISUAL_IDENTIFIER_EXPANSION", "")),
	}, true
}

// firstLines reads up to max "tool version" pins from a .tool-versions.
func firstLines(path string, max int) []string {
	if max <= 0 {
		max = 3
	}
	var out []string
	for line := range strings.SplitSeq(readFile(path), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		out = append(out, fields[0]+" "+fields[1])
		if len(out) == max {
			break
		}
	}
	return out
}

// renderVirtualenv names the active Python virtual environment.
func renderVirtualenv(cfg *Config, ctx *Context) (Rendered, bool) {
	env := ctx.Env("VIRTUAL_ENV")
	if env == "" {
		return Rendered{}, false
	}
	name := filepath.Base(env)
	// A venv called "venv" or ".venv" says nothing; the directory holding
	// it is what identifies the project.
	if generic := map[string]bool{"venv": true, ".venv": true, "env": true, ".env": true}; generic[name] {
		name = filepath.Base(filepath.Dir(env))
	}
	return Rendered{
		Content: name,
		Icon:    decodeEscapes(cfg.Param("virtualenv", "", "VISUAL_IDENTIFIER_EXPANSION", "")),
	}, true
}

// renderAnaconda names the active conda environment.
func renderAnaconda(cfg *Config, ctx *Context) (Rendered, bool) {
	name := ctx.Env("CONDA_DEFAULT_ENV")
	if name == "" {
		if prefix := ctx.Env("CONDA_PREFIX"); prefix != "" {
			name = filepath.Base(prefix)
		}
	}
	if name == "" {
		return Rendered{}, false
	}
	return Rendered{
		Content: name,
		Icon:    decodeEscapes(cfg.Param("anaconda", "", "VISUAL_IDENTIFIER_EXPANSION", "")),
	}, true
}

func renderNodeenv(cfg *Config, ctx *Context) (Rendered, bool) {
	env := ctx.Env("NODE_VIRTUAL_ENV")
	if env == "" {
		return Rendered{}, false
	}
	return Rendered{
		Content: filepath.Base(env),
		Icon:    decodeEscapes(cfg.Param("nodeenv", "", "VISUAL_IDENTIFIER_EXPANSION", "")),
	}, true
}

// renderNvm reads the active node version out of NVM_BIN, which nvm sets
// to <nvm dir>/versions/node/v20.11.0/bin — the version is in the path,
// so no subprocess is needed to learn it.
func renderNvm(cfg *Config, ctx *Context) (Rendered, bool) {
	bin := ctx.Env("NVM_BIN")
	if bin == "" {
		return Rendered{}, false
	}
	version := filepath.Base(filepath.Dir(bin))
	if version == "" || version == "." {
		return Rendered{}, false
	}
	return Rendered{
		Content: version,
		Icon:    decodeEscapes(cfg.Param("nvm", "", "VISUAL_IDENTIFIER_EXPANSION", "")),
	}, true
}

func renderRvm(cfg *Config, ctx *Context) (Rendered, bool) {
	version := ctx.Env("rvm_ruby_string")
	if version == "" {
		if home := ctx.Env("GEM_HOME"); strings.Contains(home, "rvm") {
			version = filepath.Base(home)
		}
	}
	if version == "" {
		return Rendered{}, false
	}
	return Rendered{
		Content: version,
		Icon:    decodeEscapes(cfg.Param("rvm", "", "VISUAL_IDENTIFIER_EXPANSION", "")),
	}, true
}

func renderPerlbrew(cfg *Config, ctx *Context) (Rendered, bool) {
	version := ctx.Env("PERLBREW_PERL")
	if version == "" {
		return Rendered{}, false
	}
	return Rendered{
		Content: strings.TrimPrefix(version, "perl-"),
		Icon:    decodeEscapes(cfg.Param("perlbrew", "", "VISUAL_IDENTIFIER_EXPANSION", "")),
	}, true
}
