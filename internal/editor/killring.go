package editor

const killRingMax = 60

// killRing holds killed text, most recent last. Consecutive kill commands
// coalesce into one entry so a run of Ctrl-W Ctrl-W yanks back as a unit.
type killRing struct {
	entries []string
}

// push starts a new entry.
func (k *killRing) push(s string) {
	if s == "" {
		return
	}
	k.entries = append(k.entries, s)
	if len(k.entries) > killRingMax {
		k.entries = k.entries[1:]
	}
}

// coalesce extends the most recent entry: prepend for backward kills,
// append for forward kills.
func (k *killRing) coalesce(s string, prepend bool) {
	if s == "" {
		return
	}
	if len(k.entries) == 0 {
		k.push(s)
		return
	}
	last := len(k.entries) - 1
	if prepend {
		k.entries[last] = s + k.entries[last]
	} else {
		k.entries[last] += s
	}
}

// top returns the most recent kill, or "".
func (k *killRing) top() string {
	if len(k.entries) == 0 {
		return ""
	}
	return k.entries[len(k.entries)-1]
}

// rotate moves the most recent kill to the back, exposing the previous
// one — the yank-pop traversal.
func (k *killRing) rotate() {
	if len(k.entries) < 2 {
		return
	}
	last := len(k.entries) - 1
	top := k.entries[last]
	copy(k.entries[1:], k.entries[:last])
	k.entries[0] = top
}
