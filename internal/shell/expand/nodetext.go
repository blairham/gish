// Copyright (c) 2026, koi contributors
// See LICENSE for licensing information

package expand

import (
	"strings"

	"github.com/blairham/koi-shell/internal/shell/syntax"
)

// nodeText renders a node back to source, for a diagnostic that names
// what it is complaining about. bash quotes the whole word around the
// expansion; koi names the expansion itself, which is the part it can
// answer for and the part a reader needs.
//
// A printing failure yields the empty string rather than a second
// error: this only ever runs on a path that is already reporting one.
func nodeText(node syntax.Node) string {
	var sb strings.Builder
	if err := syntax.NewPrinter(syntax.SingleLine(true)).Print(&sb, node); err != nil {
		return ""
	}
	return sb.String()
}
