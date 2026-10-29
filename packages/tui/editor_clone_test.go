package tui

import "testing"

func TestEditorCloneIsIndependent(t *testing.T) {
	e := NewEditor("> ")
	e.Insert("hello")
	e.Render(40)
	c := e.Clone()

	e.Insert(" world")
	e.Prompt = ""

	if got := c.Value(); got != "hello" {
		t.Fatalf("clone value = %q, want %q", got, "hello")
	}
	if c.Prompt != "> " {
		t.Fatalf("clone prompt = %q, want %q", c.Prompt, "> ")
	}
	if c.CursorC != 5 || c.lastRenderWidth != 40 {
		t.Fatalf("clone cursor/width = %d/%d, want 5/40", c.CursorC, c.lastRenderWidth)
	}
	c.Insert("!")
	if got := e.Value(); got != "hello world" {
		t.Fatalf("original value = %q after clone edit", got)
	}
}
