package editor

import "testing"

func TestKillRingCoalesce(t *testing.T) {
	t.Parallel()

	var k killRing
	k.push("file")
	k.coalesce("to/", true) // backward kill prepends
	k.coalesce("path/", true)
	if got := k.top(); got != "path/to/file" {
		t.Errorf("top() = %q", got)
	}
	k.push("word")
	k.coalesce(" next", false) // forward kill appends
	if got := k.top(); got != "word next" {
		t.Errorf("top() = %q", got)
	}
}

func TestKillRingRotate(t *testing.T) {
	t.Parallel()

	var k killRing
	k.push("one")
	k.push("two")
	k.push("three")
	if got := k.top(); got != "three" {
		t.Fatalf("top() = %q", got)
	}
	k.rotate()
	if got := k.top(); got != "two" {
		t.Errorf("top() after rotate = %q", got)
	}
	k.rotate()
	if got := k.top(); got != "one" {
		t.Errorf("top() after 2 rotates = %q", got)
	}
	k.rotate()
	if got := k.top(); got != "three" {
		t.Errorf("top() after 3 rotates = %q", got)
	}
}

func TestUndoStack(t *testing.T) {
	t.Parallel()

	var u undoStack
	var b Buffer
	b.Insert("one")
	u.push(&b)
	b.Insert(" two")
	u.push(&b)
	b.Insert(" three")

	s, ok := u.pop()
	if !ok || s.text != "one two" {
		t.Fatalf("pop() = %q,%v", s.text, ok)
	}
	s, ok = u.pop()
	if !ok || s.text != "one" {
		t.Fatalf("pop() = %q,%v", s.text, ok)
	}
	if _, ok := u.pop(); ok {
		t.Error("pop() on empty stack = true")
	}
}
