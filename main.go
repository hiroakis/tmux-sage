package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/anthropics/anthropic-sdk-go"
)

// version is injected at release time via -ldflags "-X main.version=...".
var version = "dev"

type config struct {
	interval        time.Duration
	minAPIInterval  time.Duration
	captureLines    int
	maxLabelLen     int
	maxDescLen      int
	model           string
	lang            string
	redact          bool
	changeThreshold float64
	maxCostPerDay   float64
	minContent      int
	dryRun          bool
	once            bool
	verbose         bool
}

// stats accumulates LLM usage across the process lifetime.
type stats struct {
	calls        int
	inputTokens  int64
	outputTokens int64
	costUSD      float64

	day          string // current calendar day (local), for the daily budget
	dayCostUSD   float64
	budgetLogged bool
}

// rollDay resets the daily spend counter when the local calendar day changes.
func (st *stats) rollDay(now time.Time) {
	d := now.Format("2006-01-02")
	if st.day != d {
		st.day = d
		st.dayCostUSD = 0
		st.budgetLogged = false
	}
}

// pricePerMTok returns input/output prices in USD per 1M tokens.
// Returns ok=false for unknown models (cost is then logged as unknown).
func pricePerMTok(model string) (in, out float64, ok bool) {
	switch {
	case strings.Contains(model, "haiku"):
		return 1.0, 5.0, true
	case strings.Contains(model, "sonnet"):
		return 3.0, 15.0, true
	case strings.Contains(model, "opus"):
		return 5.0, 25.0, true
	}
	return 0, 0, false
}

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

func main() {
	cfg := config{}
	flag.DurationVar(&cfg.interval, "interval", 30*time.Second, "polling interval")
	flag.DurationVar(&cfg.minAPIInterval, "min-api-interval", 180*time.Second, "minimum interval between summarizations per window")
	flag.IntVar(&cfg.captureLines, "lines", 30, "number of lines to capture from the bottom of each pane")
	flag.IntVar(&cfg.maxLabelLen, "max-label-len", 20, "maximum label length in runes")
	flag.IntVar(&cfg.maxDescLen, "max-desc-len", 60, "maximum description length in runes (stored in @sage_desc)")
	flag.StringVar(&cfg.model, "model", "claude-haiku-4-5", "Anthropic model ID")
	flag.StringVar(&cfg.lang, "lang", "English", "language for generated labels and descriptions (e.g. English, Japanese, ja, fr)")
	flag.BoolVar(&cfg.redact, "redact", true, "mask likely secrets (API keys, tokens, Authorization headers) in pane contents before sending them to the LLM")
	flag.Float64Var(&cfg.changeThreshold, "change-threshold", 0.1, "fraction of changed lines required to re-summarize (0 = any change); filters spinner/clock-only updates")
	flag.Float64Var(&cfg.maxCostPerDay, "max-cost-per-day", 0, "stop calling the API after this USD spend in a calendar day (0 = unlimited)")
	flag.IntVar(&cfg.minContent, "min-content", 100, "skip windows whose total pane content is smaller than this many bytes (e.g. an empty shell prompt)")
	flag.BoolVar(&cfg.dryRun, "dry-run", false, "print labels without renaming windows")
	flag.BoolVar(&cfg.once, "once", false, "run a single pass and exit")
	flag.BoolVar(&cfg.verbose, "verbose", false, "log per-window skip decisions (no change / debounced)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("tmux-sage %s\n", version)
		return
	}

	if _, err := exec.LookPath("tmux"); err != nil {
		log.Fatal("tmux not found in PATH")
	}

	// single-instance lock: hook mode can fire concurrently on rapid window
	// switches, and each racing process would summarize (and pay for) the
	// same windows before the persisted state is written.
	lockPath := filepath.Join(os.TempDir(), fmt.Sprintf("tmux-sage-%d.lock", os.Getuid()))
	lock, err := acquireLock(lockPath)
	if err != nil {
		log.Printf("another tmux-sage instance is already running (lock: %s); exiting", lockPath)
		return
	}
	defer lock.Close()

	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		log.Println("warning: ANTHROPIC_API_KEY is not set; API calls will fail unless another credential source is configured")
	}
	client := anthropic.NewClient()

	states := map[string]*windowState{}
	st := &stats{}
	failures := 0
	for {
		if err := runPass(cfg, &client, states, st); err != nil {
			log.Printf("pass failed: %v", err)
			failures++
			// exit when tmux is unreachable for a while (e.g. the server exited)
			if failures >= 10 {
				log.Fatal("10 consecutive failed passes; exiting")
			}
		} else {
			failures = 0
		}
		if cfg.once {
			return
		}
		time.Sleep(cfg.interval)
	}
}

func runPass(cfg config, client *anthropic.Client, states map[string]*windowState, st *stats) error {
	windows, err := listWindows(cfg)
	if err != nil {
		return err
	}

	alive := map[string]bool{}
	for _, w := range windows {
		alive[w.id] = true
		processWindow(cfg, client, states, st, w)
	}
	// drop state for closed windows
	for id := range states {
		if !alive[id] {
			delete(states, id)
		}
	}
	return nil
}

// acquireLock takes an exclusive advisory lock so only one tmux-sage instance
// runs per user. The lock is released automatically when the process exits.
func acquireLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// contentSize returns the total size of all panes' trimmed content.
func contentSize(w window) int {
	total := 0
	for _, p := range w.panes {
		total += len(strings.TrimSpace(p.content))
	}
	return total
}

func processWindow(cfg config, client *anthropic.Client, states map[string]*windowState, st *stats, w window) {
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

	label, desc, err := summarize(cfg, client, st, w.id, content)
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

	if err := exec.Command("tmux", "rename-window", "-t", w.id, label).Run(); err != nil {
		log.Printf("window %s: rename failed: %v", w.id, err)
		return
	}
	log.Printf("window %s: %q -> %q", w.id, w.name, label)
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

// secretPatterns match strings that are likely credentials. Patterns with a
// capture group keep the matched prefix and mask only the value part.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bsk-(?:ant-)?[A-Za-z0-9_-]{16,}`),                                                 // Anthropic / OpenAI keys
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`),                                                   // GitHub tokens
	regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`),                                                   // Slack tokens
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),                                                             // AWS access key IDs
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]+`),                       // JWTs
	regexp.MustCompile(`(?i)(authorization:\s*(?:bearer|basic)\s+)\S+`),                                    // Authorization headers
	regexp.MustCompile(`(?i)([A-Z0-9_]*(?:api_?key|secret|token|password|passwd)[A-Z0-9_]*\s*[=:]\s*)\S+`), // KEY=value / key: value
}

// redactSecrets masks likely credentials in pane content before it is sent to
// the LLM. Best-effort: it catches common formats, not every possible secret.
func redactSecrets(s string) string {
	for _, re := range secretPatterns {
		s = re.ReplaceAllString(s, "${1}[REDACTED]")
	}
	return s
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

func summarize(cfg config, client *anthropic.Client, st *stats, windowID, content string) (label, desc string, err error) {
	system := fmt.Sprintf(`The user message contains the on-screen contents of every pane in a single tmux window.
Summarize the work being done in this window as a whole and output exactly two lines:

Line 1: a short label usable as a tmux window name, at most %d characters. Examples: "tmux-sage dev", "log analysis", "PR#123 review"
Line 2: a one-sentence, slightly more detailed description of the work, at most %d characters.

Rules:
- Output only the two lines. No preamble, quotes, or blank lines.
- Prefer concrete project names and command names when they appear.
- Write both lines in %s.`, cfg.maxLabelLen, cfg.maxDescLen, cfg.lang)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resp, err := client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(cfg.model),
		MaxTokens: 300,
		System:    []anthropic.TextBlockParam{{Text: system}},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(content)),
		},
	})
	if err != nil {
		return "", "", fmt.Errorf("anthropic API: %w", err)
	}

	in, out := resp.Usage.InputTokens, resp.Usage.OutputTokens
	st.calls++
	st.inputTokens += in
	st.outputTokens += out
	if pIn, pOut, ok := pricePerMTok(cfg.model); ok {
		cost := float64(in)/1e6*pIn + float64(out)/1e6*pOut
		st.costUSD += cost
		st.rollDay(time.Now())
		st.dayCostUSD += cost
		log.Printf("window %s: LLM call: model=%s input=%d output=%d cost=$%.5f (total: %d calls, input=%d output=%d $%.4f)",
			windowID, cfg.model, in, out, cost, st.calls, st.inputTokens, st.outputTokens, st.costUSD)
	} else {
		log.Printf("window %s: LLM call: model=%s input=%d output=%d cost=unknown (total: %d calls, input=%d output=%d)",
			windowID, cfg.model, in, out, st.calls, st.inputTokens, st.outputTokens)
	}

	var text strings.Builder
	for _, block := range resp.Content {
		if t, ok := block.AsAny().(anthropic.TextBlock); ok {
			text.WriteString(t.Text)
		}
	}
	lines := nonEmptyLines(text.String())
	if len(lines) > 0 {
		label = sanitizeLabel(lines[0], cfg.maxLabelLen)
	}
	if len(lines) > 1 {
		desc = sanitizeLabel(lines[1], cfg.maxDescLen)
	}
	return label, desc, nil
}

func nonEmptyLines(s string) []string {
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func sanitizeLabel(s string, maxLen int) string {
	// keep only the first non-empty line
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		line = strings.Trim(line, `"'`+"`")
		if line == "" {
			continue
		}
		line = strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return -1
			}
			return r
		}, line)
		runes := []rune(line)
		if len(runes) > maxLen {
			runes = runes[:maxLen]
		}
		return string(runes)
	}
	return ""
}
