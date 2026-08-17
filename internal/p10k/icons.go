package p10k

import "strings"

// The icon table (#131): what POWERLEVEL9K_MODE actually selects.
//
// MODE was parsed, stored, imported and written back out — and never
// read. Icons came from per-segment defaults hardcoded in the segment
// implementations, so `MODE=ascii` did not produce an ASCII prompt, a
// config imported from a nerdfont-complete `.p10k.zsh` rendered v3
// glyphs, and a global VISUAL_IDENTIFIER_EXPANSION had nothing to
// default *to*. The wizard worked around it by writing explicit ASCII
// overrides when the user said the glyphs did not render — the right
// answer reached the wrong way.
//
// Upstream keeps this as a table (internal/icons.zsh, ~1,174 lines
// across eight modes). So does this, with the same shape as the
// presets: data over one lookup, not code per segment.
//
// Three modes are carried rather than eight. nerdfont-v3, ascii and one
// awesome variant cover essentially everyone, and a mode that is not
// here is served by nerdfont-v3 *and says so* through `p10k show` —
// silently substituting a different glyph set is how a prompt ends up
// full of boxes with no explanation.

// IconMode is the name of a resolved icon set, plus whether it is the
// one that was asked for.
type IconMode struct {
	Requested string
	Serving   string
}

// Fallback reports whether the requested mode is being served by
// another one.
func (m IconMode) Fallback() bool {
	return m.Requested != "" && !strings.EqualFold(m.Requested, m.Serving)
}

const defaultIconMode = "nerdfont-v3"

// iconModes maps a mode name to its table. Keys are segment names in
// upstream's spelling.
var iconModes = map[string]map[string]string{
	// Nerd Font v3 code points — the current default, and what the
	// wizard writes when the user confirms the glyphs render.
	"nerdfont-v3": {
		"os_icon":                "",
		"dir":                    "",
		"vcs":                    "",
		"status_ok":              "✔",
		"status_error":           "✘",
		"command_execution_time": "",
		"background_jobs":        "",
		"time":                   "",
		"context":                "",
		"user":                   "",
		"root_indicator":         "",
		"host":                   "",
		"virtualenv":             "",
		"anaconda":               "",
		"pyenv":                  "",
		"nodenv":                 "",
		"nodeenv":                "",
		"nvm":                    "",
		"node_version":           "",
		"go_version":             "",
		"goenv":                  "",
		"rust_version":           "",
		"rbenv":                  "",
		"rvm":                    "",
		"ruby_version":           "",
		"php_version":            "",
		"phpenv":                 "",
		"java_version":           "",
		"jenv":                   "",
		"luaenv":                 "",
		"perlbrew":               "",
		"plenv":                  "",
		"scalaenv":               "",
		"haskell_stack":          "",
		"terraform":              "",
		"kubecontext":            "⎈",
		"aws":                    "",
		"aws_eb_env":             "",
		"azure":                  "ﴃ",
		"gcloud":                 "",
		"google_app_cred":        "",
		"docker":                 "",
		"toolbox":                "",
		"nix_shell":              "",
		"direnv":                 "",
		"asdf":                   "",
		"proxy":                  "",
		"vpn_ip":                 "",
		"public_ip":              "",
		"ip":                     "",
		"wifi":                   "",
		"battery":                "",
		"ram":                    "",
		"swap":                   "",
		"load":                   "",
		"disk_usage":             "",
		"todo":                   "",
		"cpu_arch":               "",
		"detect_virt":            "",
		"package":                "",
	},
	// ASCII: no font requirement at all. The whole point of the mode is
	// that a terminal without a patched font shows something legible
	// rather than a row of boxes, so these are short words and
	// punctuation, never a glyph that happens to be in most fonts.
	"ascii": {
		"os_icon":                "OS",
		"dir":                    "",
		"vcs":                    "",
		"status_ok":              "ok",
		"status_error":           "x",
		"command_execution_time": "",
		"background_jobs":        "%",
		"time":                   "",
		"context":                "",
		"user":                   "",
		"root_indicator":         "#",
		"host":                   "@",
		"virtualenv":             "py",
		"anaconda":               "conda",
		"pyenv":                  "py",
		"nodenv":                 "node",
		"nodeenv":                "node",
		"nvm":                    "node",
		"node_version":           "node",
		"go_version":             "go",
		"goenv":                  "go",
		"rust_version":           "rust",
		"rbenv":                  "rb",
		"rvm":                    "rb",
		"ruby_version":           "rb",
		"php_version":            "php",
		"java_version":           "java",
		"terraform":              "tf",
		"kubecontext":            "k8s",
		"aws":                    "aws",
		"azure":                  "az",
		"gcloud":                 "gcp",
		"docker":                 "docker",
		"nix_shell":              "nix",
		"direnv":                 "env",
		"asdf":                   "asdf",
		"proxy":                  "proxy",
		"vpn_ip":                 "vpn",
		"battery":                "bat",
		"ram":                    "ram",
		"disk_usage":             "disk",
		"todo":                   "todo",
	},
	// Font Awesome, as patched into the "awesome-*" fonts. Kept as one
	// variant rather than three: the differences between patched,
	// fontconfig and mapped-fontconfig are code-point ranges for the
	// same glyphs, and carrying all three is a lot of table for very
	// few users.
	"awesome-fontconfig": {
		"os_icon":                "",
		"dir":                    "",
		"vcs":                    "",
		"command_execution_time": "",
		"background_jobs":        "",
		"time":                   "",
		"context":                "",
		"root_indicator":         "",
		"host":                   "",
		"virtualenv":             "",
		"anaconda":               "",
		"pyenv":                  "",
		"kubecontext":            "⎈",
		"aws":                    "",
		"gcloud":                 "",
		"docker":                 "",
		"battery":                "",
		"ram":                    "",
		"disk_usage":             "",
		"todo":                   "",
	},
}

// iconModeAliases map the mode names upstream accepts onto the tables
// carried here.
var iconModeAliases = map[string]string{
	"nerdfont-complete":         defaultIconMode,
	"nerdfont-fontconfig":       defaultIconMode,
	"nerdfont-v3":               "nerdfont-v3",
	"awesome-patched":           "awesome-fontconfig",
	"awesome-fontconfig":        "awesome-fontconfig",
	"awesome-mapped-fontconfig": "awesome-fontconfig",
	"ascii":                     "ascii",
	"flat":                      "ascii",
	"compatible":                "ascii",
}

// ResolveIconMode reports which table serves the configured MODE.
func (c *Config) ResolveIconMode() IconMode {
	requested := strings.ToLower(strings.TrimSpace(c.Str("MODE", "")))
	if requested == "" {
		return IconMode{Requested: "", Serving: defaultIconMode}
	}
	if serving, ok := iconModeAliases[requested]; ok {
		return IconMode{Requested: requested, Serving: serving}
	}
	return IconMode{Requested: requested, Serving: defaultIconMode}
}

// Icon resolves a segment's visual identifier through the same
// three-step chain every other parameter uses, with the mode table
// slotted in below the explicit overrides:
//
//	<SEGMENT>_<STATE>_VISUAL_IDENTIFIER_EXPANSION
//	<SEGMENT>_VISUAL_IDENTIFIER_EXPANSION
//	VISUAL_IDENTIFIER_EXPANSION
//	icons[MODE][segment]
//	def
//
// An explicit override still wins, which is what keeps an imported
// config's per-segment icons exact.
func (c *Config) Icon(segment, state, def string) string {
	if c.ParamSet(segment, state, "VISUAL_IDENTIFIER_EXPANSION") {
		return c.Param(segment, state, "VISUAL_IDENTIFIER_EXPANSION", def)
	}
	mode := c.ResolveIconMode()
	table, ok := iconModes[mode.Serving]
	if !ok {
		return def
	}
	// State first: `status` is one segment with two icons, and a
	// table keyed only by segment name silently replaced the error
	// mark with the ok one — visible immediately as a prompt that
	// stops flagging failures.
	if state != "" {
		if icon, ok := table[strings.ToLower(segment)+"_"+strings.ToLower(state)]; ok {
			return icon
		}
	}
	if icon, ok := table[strings.ToLower(segment)]; ok {
		return icon
	}
	return def
}
