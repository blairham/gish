package editor

import "testing"

func TestBufferInsertAndMove(t *testing.T) {
	t.Parallel()

	var b Buffer
	b.Insert("echo hi")
	if got := b.String(); got != "echo hi" {
		t.Fatalf("String() = %q", got)
	}
	if b.Cursor() != 7 {
		t.Fatalf("Cursor() = %d, want 7", b.Cursor())
	}
	b.MoveLineStart()
	b.Insert("x")
	if got := b.String(); got != "xecho hi" {
		t.Errorf("String() = %q", got)
	}
}

func TestBufferGraphemeMovement(t *testing.T) {
	t.Parallel()

	var b Buffer
	// 👍🏽 is one grapheme cluster of two runes (emoji + skin tone).
	b.Insert("a👍🏽b")
	b.MoveEnd()
	b.MoveLeft() // over 'b'
	b.MoveLeft() // over the whole cluster
	if got := string(b.text[b.Cursor():]); got != "👍🏽b" {
		t.Errorf("cursor landed mid-cluster: rest = %q", got)
	}
	b.DeleteForward() // must delete the whole cluster
	if got := b.String(); got != "ab" {
		t.Errorf("String() = %q, want %q", got, "ab")
	}
}

func TestBufferDeleteBackGrapheme(t *testing.T) {
	t.Parallel()

	var b Buffer
	b.Insert("hi👍🏽")
	b.DeleteBack()
	if got := b.String(); got != "hi" {
		t.Errorf("String() = %q, want %q", got, "hi")
	}
}

func TestBufferWordOps(t *testing.T) {
	t.Parallel()

	var b Buffer
	b.Insert("git commit --amend")
	b.MoveEnd()
	b.MoveWordLeft()
	if got := string(b.text[b.Cursor():]); got != "amend" {
		t.Errorf("after MoveWordLeft rest = %q", got)
	}
	if killed := b.KillWordForward(); killed != "amend" {
		t.Errorf("KillWordForward() = %q", killed)
	}
	if got := b.String(); got != "git commit --" {
		t.Errorf("String() = %q", got)
	}
}

func TestBufferKillToWhitespace(t *testing.T) {
	t.Parallel()

	var b Buffer
	b.Insert("rm --force ./path/to/file")
	b.MoveEnd()
	if killed := b.KillToWhitespace(); killed != "./path/to/file" {
		t.Errorf("KillToWhitespace() = %q", killed)
	}
	if got := b.String(); got != "rm --force " {
		t.Errorf("String() = %q", got)
	}
}

func TestBufferLineOps(t *testing.T) {
	t.Parallel()

	var b Buffer
	b.Set("for x in a b; do\necho $x\ndone", 0)
	b.MoveEnd()
	line, before := b.CursorLine()
	if line != 2 || before != "done" {
		t.Errorf("CursorLine() = %d,%q", line, before)
	}
	if !b.MoveUp() {
		t.Fatal("MoveUp() = false")
	}
	line, _ = b.CursorLine()
	if line != 1 {
		t.Errorf("line after MoveUp = %d", line)
	}
	b.MoveLineStart()
	if killed := b.KillToLineEnd(); killed != "echo $x" {
		t.Errorf("KillToLineEnd() = %q", killed)
	}
	// Cursor at empty line end: Ctrl-K joins with the next line.
	if killed := b.KillToLineEnd(); killed != "\n" {
		t.Errorf("KillToLineEnd() at eol = %q", killed)
	}
	if got := b.String(); got != "for x in a b; do\ndone" {
		t.Errorf("String() = %q", got)
	}
}

func TestBufferKillToLineStart(t *testing.T) {
	t.Parallel()

	var b Buffer
	b.Insert("echo hello")
	for range 5 {
		b.MoveLeft()
	}
	if killed := b.KillToLineStart(); killed != "echo " {
		t.Errorf("KillToLineStart() = %q", killed)
	}
	if got := b.String(); got != "hello" {
		t.Errorf("String() = %q", got)
	}
}

func TestBufferMoveUpDownClampsColumn(t *testing.T) {
	t.Parallel()

	var b Buffer
	b.Set("short\nmuch longer line", 0)
	b.MoveEnd() // end of long line
	b.MoveUp()
	line, before := b.CursorLine()
	if line != 0 || before != "short" {
		t.Errorf("after MoveUp: line=%d before=%q", line, before)
	}
	b.MoveDown()
	line, _ = b.CursorLine()
	if line != 1 {
		t.Errorf("after MoveDown: line=%d", line)
	}
	if b.MoveDown() {
		t.Error("MoveDown() on last line = true")
	}
}
