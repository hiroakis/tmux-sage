package main

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"
	"unicode"
)

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

func summarize(cfg config, llm llmClient, st *stats, windowID, content string) (label, desc string, err error) {
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
	text, in, out, err := llm.complete(ctx, cfg.llm.model, system, content, 300)
	if err != nil {
		return "", "", err
	}

	st.calls++
	st.inputTokens += in
	st.outputTokens += out
	pIn, pOut, priced := pricePerMTok(cfg.llm.model)
	if cfg.llm.priceIn > 0 || cfg.llm.priceOut > 0 {
		pIn, pOut, priced = cfg.llm.priceIn, cfg.llm.priceOut, true
	}
	if priced {
		cost := float64(in)/1e6*pIn + float64(out)/1e6*pOut
		st.costUSD += cost
		st.rollDay(time.Now())
		st.dayCostUSD += cost
		log.Printf("window %s: LLM call: model=%s input=%d output=%d cost=$%.5f (total: %d calls, input=%d output=%d $%.4f)",
			windowID, cfg.llm.model, in, out, cost, st.calls, st.inputTokens, st.outputTokens, st.costUSD)
	} else {
		log.Printf("window %s: LLM call: model=%s input=%d output=%d cost=unknown (total: %d calls, input=%d output=%d)",
			windowID, cfg.llm.model, in, out, st.calls, st.inputTokens, st.outputTokens)
	}

	lines := nonEmptyLines(text)
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
