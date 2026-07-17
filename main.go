package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// version is injected at release time via -ldflags "-X main.version=...".
var version = "dev"

type config struct {
	llm llmConfig

	interval        time.Duration
	minAPIInterval  time.Duration
	captureLines    int
	maxLabelLen     int
	maxDescLen      int
	lang            string
	redact          bool
	changeThreshold float64
	maxCostPerDay   float64
	minContent      int
	dryRun          bool
	once            bool
	verbose         bool
}

func main() {
	cfg := config{}
	flag.DurationVar(&cfg.interval, "interval", 30*time.Second, "polling interval")
	flag.DurationVar(&cfg.minAPIInterval, "min-api-interval", 180*time.Second, "minimum interval between summarizations per window")
	flag.IntVar(&cfg.captureLines, "lines", 30, "number of lines to capture from the bottom of each pane")
	flag.IntVar(&cfg.maxLabelLen, "max-label-len", 20, "maximum label length in runes")
	flag.IntVar(&cfg.maxDescLen, "max-desc-len", 60, "maximum description length in runes (stored in @sage_desc)")
	flag.StringVar(&cfg.llm.provider, "provider", "anthropic", "LLM provider: anthropic | openai | gemini | vertex (openai works with any OpenAI-compatible API, e.g. Ollama)")
	flag.StringVar(&cfg.llm.baseURL, "base-url", "", "API base URL override for -provider openai / gemini / vertex (e.g. http://localhost:11434/v1 for Ollama)")
	flag.StringVar(&cfg.llm.vertex.project, "vertex-project", "", "GCP project ID for -provider vertex (default: GOOGLE_CLOUD_PROJECT env var)")
	flag.StringVar(&cfg.llm.vertex.location, "vertex-location", "global", "GCP location for -provider vertex (e.g. global, us-central1, asia-northeast1)")
	flag.StringVar(&cfg.llm.model, "model", "", "model ID (default claude-haiku-4-5 for anthropic; required for openai / gemini / vertex)")
	flag.StringVar(&cfg.lang, "lang", "English", "language for generated labels and descriptions (e.g. English, Japanese, ja, fr)")
	flag.Float64Var(&cfg.llm.priceIn, "price-in", 0, "input price in USD per 1M tokens for cost logging (overrides built-in prices)")
	flag.Float64Var(&cfg.llm.priceOut, "price-out", 0, "output price in USD per 1M tokens for cost logging (overrides built-in prices)")
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

	llm, err := newLLMClient(&cfg.llm)
	if err != nil {
		log.Fatal(err)
	}

	states := map[string]*windowState{}
	st := &stats{}
	failures := 0
	for {
		if err := runPass(cfg, llm, states, st); err != nil {
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
