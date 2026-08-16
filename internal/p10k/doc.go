// Package p10k is gish's native powerlevel10k engine: the prompt shape
// people came for, rewritten in Go.
//
// This is a port of *behavior*, not of code. powerlevel10k is a ~10k
// line zsh program; gish will not carry a zsh dialect on the prompt
// path (AGENTS.md), so nothing here interprets zsh, shells out, or
// reads a .p10k.zsh at render time. The upstream project (MIT,
// Roman Perepelitsa and contributors, https://github.com/romkatv/powerlevel10k)
// is the specification this package implements; the observable rules it
// reproduces are documented at each site.
//
// Three ideas carry over from upstream, because they are what makes the
// theme configurable rather than merely pretty:
//
//   - The configuration is a flat *parameter namespace*, not a struct.
//     Upstream exposes ~565 POWERLEVEL9K_* variables across its presets,
//     and every one of them is looked up through the same three-step
//     fallback chain (see [Config.Param]). Modeling that faithfully is
//     what lets a preset be data instead of code.
//   - A prompt is a list of *segments*, each of which may decline to
//     render. Layout is a separate pass over whatever survived.
//   - Anything that could be slow is not allowed on the render path.
//     Segments read files and environment variables; they never fork,
//     and never block. See segment.go.
//
// What deliberately did not carry over is the zsh surface: CONTENT_EXPANSION
// strings that call user-defined shell functions cannot be honored
// natively, and are reported at import time rather than silently faked.
package p10k
