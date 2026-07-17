package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os/exec"
	"strings"
)

type pane struct {
	id      string
	command string
	path    string
	content string
}

type window struct {
	id    string
	name  string
	panes []pane
}

func listWindows(cfg config) ([]window, error) {
	out, err := exec.Command("tmux", "list-windows", "-F", "#{window_id}\t#{window_name}").Output()
	if err != nil {
		return nil, fmt.Errorf("list-windows: %w", err)
	}
	var windows []window
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		w := window{id: parts[0], name: parts[1]}
		panes, err := listPanes(cfg, w.id)
		if err != nil {
			log.Printf("window %s: list panes failed: %v", w.id, err)
			continue
		}
		w.panes = panes
		windows = append(windows, w)
	}
	return windows, nil
}

func listPanes(cfg config, windowID string) ([]pane, error) {
	out, err := exec.Command("tmux", "list-panes", "-t", windowID,
		"-F", "#{pane_id}\t#{pane_current_command}\t#{pane_current_path}").Output()
	if err != nil {
		return nil, fmt.Errorf("list-panes: %w", err)
	}
	var panes []pane
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		p := pane{id: parts[0], command: parts[1], path: parts[2]}
		content, err := exec.Command("tmux", "capture-pane", "-p", "-t", p.id,
			"-S", fmt.Sprintf("-%d", cfg.captureLines)).Output()
		if err != nil {
			log.Printf("pane %s: capture failed: %v", p.id, err)
		}
		p.content = strings.TrimSpace(string(content))
		if cfg.redact {
			p.content = redactSecrets(p.content)
		}
		panes = append(panes, p)
	}
	return panes, nil
}

// optOut reports whether the window has the @sage_off option set,
// e.g. tmux set-option -w @sage_off 1
func optOut(windowID string) bool {
	return windowOption(windowID, "@sage_off") != ""
}

func windowOption(windowID, name string) string {
	out, err := exec.Command("tmux", "show-options", "-t", windowID, "-wqv", name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func setWindowOption(windowID, name, value string) {
	if err := exec.Command("tmux", "set-option", "-w", "-t", windowID, name, value).Run(); err != nil {
		log.Printf("window %s: set %s failed: %v", windowID, name, err)
	}
}

func renameWindow(windowID, name string) error {
	return exec.Command("tmux", "rename-window", "-t", windowID, name).Run()
}

func contentHash(w window) string {
	h := sha256.New()
	for _, p := range w.panes {
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00", p.id, p.command, p.path, p.content)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// windowContent renders all panes of a window into the text sent to the LLM.
func windowContent(w window) string {
	var b strings.Builder
	for i, p := range w.panes {
		fmt.Fprintf(&b, "## Pane %d (command: %s, directory: %s)\n%s\n\n", i+1, p.command, p.path, p.content)
	}
	return b.String()
}

// contentSize returns the total size of all panes' trimmed content.
func contentSize(w window) int {
	total := 0
	for _, p := range w.panes {
		total += len(strings.TrimSpace(p.content))
	}
	return total
}
