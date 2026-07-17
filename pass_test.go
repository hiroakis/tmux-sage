package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestContentHash(t *testing.T) {
	w := func(panes ...pane) window { return window{id: "@1", panes: panes} }
	p1 := pane{id: "%1", command: "zsh", path: "/tmp", content: "hello"}
	p2 := pane{id: "%2", command: "vim", path: "/src", content: "world"}

	if contentHash(w(p1, p2)) != contentHash(w(p1, p2)) {
		t.Error("same panes should produce the same hash")
	}
	changed := p1
	changed.content = "hello!"
	if contentHash(w(p1, p2)) == contentHash(w(changed, p2)) {
		t.Error("content change should change the hash")
	}
	if contentHash(w(p1, p2)) == contentHash(w(p2, p1)) {
		t.Error("pane order should affect the hash")
	}
	// field boundaries: (command="ab", path="c") vs (command="a", path="bc")
	a := pane{id: "%1", command: "ab", path: "c", content: "x"}
	b := pane{id: "%1", command: "a", path: "bc", content: "x"}
	if contentHash(w(a)) == contentHash(w(b)) {
		t.Error("field boundaries should be preserved in the hash")
	}
}

func TestChangeRatio(t *testing.T) {
	lines := func(n int, prefix string) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "%s line %d\n", prefix, i)
		}
		return b.String()
	}

	if r := changeRatio("a\nb\nc", "a\nb\nc"); r != 0 {
		t.Errorf("identical content: ratio = %v, want 0", r)
	}
	if r := changeRatio(lines(100, "x"), lines(100, "y")); r < 0.9 {
		t.Errorf("fully different content: ratio = %v, want ~1", r)
	}
	// spinner-like change: 1 line out of 100 differs
	old := lines(99, "x") + "spinner ⠋"
	cur := lines(99, "x") + "spinner ⠙"
	if r := changeRatio(old, cur); r > 0.05 {
		t.Errorf("one-line change in 100 lines: ratio = %v, want small", r)
	}
	// order-insensitive: reordered lines are not a change
	if r := changeRatio("a\nb\nc", "c\na\nb"); r != 0 {
		t.Errorf("reordered content: ratio = %v, want 0", r)
	}
	if r := changeRatio("", ""); r != 0 {
		t.Errorf("empty content: ratio = %v, want 0", r)
	}
}

func TestAcquireLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.lock")

	first, err := acquireLock(path)
	if err != nil {
		t.Fatalf("first acquireLock failed: %v", err)
	}
	if _, err := acquireLock(path); err == nil {
		t.Fatal("second acquireLock should fail while the first lock is held")
	}
	first.Close()
	third, err := acquireLock(path)
	if err != nil {
		t.Fatalf("acquireLock after release failed: %v", err)
	}
	third.Close()
}

func TestContentSize(t *testing.T) {
	w := window{panes: []pane{
		{content: "  hello  "},
		{content: ""},
		{content: "world"},
	}}
	if got := contentSize(w); got != 10 {
		t.Errorf("contentSize = %d, want 10 (trimmed)", got)
	}
	if got := contentSize(window{}); got != 0 {
		t.Errorf("contentSize of empty window = %d, want 0", got)
	}
}
