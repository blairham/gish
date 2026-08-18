package main

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"sync"
)

// export mirrors carapace's `export` output (observed against
// carapace-bin 1.7.3 / export v1.13): the fields we consume, parsed
// tolerantly so unknown additions don't break the bridge.
type export struct {
	Values []exportValue `json:"values"`
}

type exportValue struct {
	Value       string `json:"value"`
	Display     string `json:"display"`
	Description string `json:"description"`
}

// bridge shells out to the carapace binary. It is resolved once; a
// missing binary makes every completion empty rather than an error.
type bridge struct {
	path string

	once      sync.Once
	supported map[string]bool
}

func newBridge() *bridge {
	path, err := exec.LookPath("carapace")
	if err != nil {
		return &bridge{}
	}
	return &bridge{path: path}
}

// commands lazily loads `carapace --list` — the set of CLIs carapace
// can complete. Cached for the plugin's resident lifetime.
func (b *bridge) commands(ctx context.Context) map[string]bool {
	b.once.Do(func() {
		b.supported = map[string]bool{}
		if b.path == "" {
			return
		}
		out, err := exec.CommandContext(ctx, b.path, "--list").Output()
		if err != nil {
			return
		}
		b.supported = parseList(out)
	})
	return b.supported
}

// parseList reads `carapace --list` output: modern versions emit a JSON
// object keyed by command name; older ones printed one name per line.
func parseList(out []byte) map[string]bool {
	supported := map[string]bool{}
	var byName map[string]json.RawMessage
	if err := json.Unmarshal(out, &byName); err == nil {
		for name := range byName {
			supported[name] = true
		}
		return supported
	}
	for line := range strings.Lines(string(out)) {
		if fields := strings.Fields(line); len(fields) > 0 {
			supported[fields[0]] = true
		}
	}
	return supported
}

// complete asks carapace for candidates for the given argv. words is
// the full command line up to the cursor, current word last (possibly
// empty).
func (b *bridge) complete(ctx context.Context, words []string) []exportValue {
	if b.path == "" || len(words) < 2 {
		return nil // command-name completion belongs to the shell core
	}
	cmd := words[0]
	if !b.commands(ctx)[cmd] {
		return nil
	}
	args := append([]string{cmd, "export"}, words...)
	out, err := exec.CommandContext(ctx, b.path, args...).Output()
	if err != nil {
		return nil
	}
	var ex export
	if err := json.Unmarshal(out, &ex); err != nil {
		return nil
	}
	return ex.Values
}

// splitLine turns the buffer up to the cursor into words for carapace,
// preserving a trailing empty word when the cursor follows a space —
// that's how "git checkout <cursor>" asks for all refs.
func splitLine(line string, cursor int) []string {
	runes := []rune(line)
	if cursor > len(runes) {
		cursor = len(runes)
	}
	head := string(runes[:cursor])
	words := strings.Fields(head)
	if head == "" {
		return nil
	}
	if strings.HasSuffix(head, " ") || strings.HasSuffix(head, "\t") {
		words = append(words, "")
	}
	return words
}
