package main

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
)

// windowState is the per-window scan state used to decide whether a window
// needs re-summarizing.
type windowState struct {
	contentHash string
	lastAPICall time.Time
	lastLabel   string
	lastDesc    string
	// lastContent is the pane content at the last summarization, used for the
	// change-ratio check. In-memory only: after a restart the first differing
	// pass falls back to plain hash comparison.
	lastContent string
}

// loadState restores per-window state persisted as tmux window options,
// so restarting tmux-sage doesn't re-summarize unchanged windows.
func loadState(windowID string) *windowState {
	ws := &windowState{
		contentHash: windowOption(windowID, "@sage_hash"),
		lastDesc:    windowOption(windowID, "@sage_desc"),
	}
	if s := windowOption(windowID, "@sage_last_call"); s != "" {
		if sec, err := strconv.ParseInt(s, 10, 64); err == nil {
			ws.lastAPICall = time.Unix(sec, 0)
		}
	}
	return ws
}

// runPass scans every window once and summarizes the ones that changed.
func runPass(cfg config, llm llmClient, states map[string]*windowState, st *stats) error {
	windows, err := listWindows(cfg)
	if err != nil {
		return err
	}

	alive := map[string]bool{}
	for _, w := range windows {
		alive[w.id] = true
		processWindow(cfg, llm, states, st, w)
	}
	// drop state for closed windows
	for id := range states {
		if !alive[id] {
			delete(states, id)
		}
	}
	return nil
}

func processWindow(cfg config, llm llmClient, states map[string]*windowState, st *stats, w window) {
	if optOut(w.id) {
		return
	}

	// a window with almost no content (e.g. a fresh shell prompt) has nothing
	// meaningful to summarize; the LLM would produce a garbage label
	if size := contentSize(w); size < cfg.minContent {
		if cfg.verbose {
			log.Printf("window %s (%s): content too small (%d bytes < %d), skipped", w.id, w.name, size, cfg.minContent)
		}
		return
	}

	ws, ok := states[w.id]
	if !ok {
		ws = loadState(w.id)
		states[w.id] = ws
	}

	hash := contentHash(w)
	if hash == ws.contentHash {
		if cfg.verbose {
			log.Printf("window %s (%s): no change", w.id, w.name)
		}
		return
	}

	content := windowContent(w)
	if cfg.changeThreshold > 0 && ws.lastContent != "" {
		if r := changeRatio(ws.lastContent, content); r < cfg.changeThreshold {
			if cfg.verbose {
				log.Printf("window %s (%s): minor change (%.1f%% of lines, threshold %.1f%%), skipped", w.id, w.name, r*100, cfg.changeThreshold*100)
			}
			return
		}
	}

	if time.Since(ws.lastAPICall) < cfg.minAPIInterval {
		if cfg.verbose {
			log.Printf("window %s (%s): changed, but debounced (last call less than %s ago)", w.id, w.name, cfg.minAPIInterval)
		}
		return
	}

	if cfg.maxCostPerDay > 0 {
		st.rollDay(time.Now())
		if st.dayCostUSD >= cfg.maxCostPerDay {
			if !st.budgetLogged {
				log.Printf("daily budget of $%.2f reached ($%.4f spent today); suspending API calls until tomorrow", cfg.maxCostPerDay, st.dayCostUSD)
				st.budgetLogged = true
			}
			return
		}
	}

	label, desc, err := summarize(cfg, llm, st, w.id, content)
	if err != nil {
		log.Printf("window %s: summarize failed: %v", w.id, err)
		return
	}
	ws.contentHash = hash
	ws.lastContent = content
	ws.lastAPICall = time.Now()

	if cfg.dryRun {
		fmt.Printf("[dry-run] %s (%s) -> %s | %s\n", w.id, w.name, label, desc)
		return
	}

	// persist state as tmux window options so a tmux-sage restart doesn't re-summarize
	setWindowOption(w.id, "@sage_hash", hash)
	setWindowOption(w.id, "@sage_last_call", strconv.FormatInt(ws.lastAPICall.Unix(), 10))

	if desc != "" && desc != ws.lastDesc {
		ws.lastDesc = desc
		setWindowOption(w.id, "@sage_desc", desc)
	}

	if label == "" || label == ws.lastLabel || label == w.name {
		return
	}
	ws.lastLabel = label

	if err := renameWindow(w.id, label); err != nil {
		log.Printf("window %s: rename failed: %v", w.id, err)
		return
	}
	log.Printf("window %s: %q -> %q", w.id, w.name, label)
}

// changeRatio returns the fraction of lines that differ between two snapshots
// (0 = identical, 1 = completely different). Line-order insensitive, so a
// spinner or clock update in one line of a large window yields a small ratio.
func changeRatio(old, cur string) float64 {
	oldLines := strings.Split(old, "\n")
	curLines := strings.Split(cur, "\n")
	counts := make(map[string]int, len(oldLines))
	for _, l := range oldLines {
		counts[l]++
	}
	matched := 0
	for _, l := range curLines {
		if counts[l] > 0 {
			counts[l]--
			matched++
		}
	}
	total := len(oldLines)
	if len(curLines) > total {
		total = len(curLines)
	}
	if total == 0 {
		return 0
	}
	return 1 - float64(matched)/float64(total)
}
