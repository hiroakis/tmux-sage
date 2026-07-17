# tmux-sage

English | [日本語](README.ja.md)

A daemon that summarizes what you are working on in each tmux window (tab) using an LLM, and sets the summary as the window name automatically.

Even for windows with multiple panes, tmux-sage collects the on-screen contents, running commands, and working directories of **all** panes and summarizes them together — so you can tell what a tab is for just by looking at its name.

## Demo

https://github.com/user-attachments/assets/69f30ea5-9d9d-428a-a641-fceee40979c1

## How it works

1. On a fixed interval, tmux-sage walks every window and captures the bottom N lines of each pane (`capture-pane`) plus pane metadata.
2. If the content hash hasn't changed since the last summary, the window is skipped (no API call).
3. If it changed, the Anthropic API (Claude Haiku 4.5 by default) generates a short label and a longer description.
4. The label is applied with `tmux rename-window` (skipped when unchanged); the description is stored in the window's user option `@sage_desc`.
5. State (content hash `@sage_hash`, last call time `@sage_last_call`) is also persisted as tmux window options — restarting tmux-sage does not re-summarize unchanged or recently summarized windows. Restarting the tmux server resets the state.

## Requirements

- tmux
- An API key for your provider: `ANTHROPIC_API_KEY` (default), `OPENAI_API_KEY` for `-provider openai`, or `GEMINI_API_KEY` for `-provider gemini`. Local LLMs via an OpenAI-compatible server need no key.

## Installation

### Homebrew (macOS)

```sh
brew install --cask hiroakis/tap/tmux-sage
```

This installs the `tmux-sage` binary into your `PATH`, so the TPM plugin below picks it up automatically.

### With TPM (recommended)

Add to `~/.tmux.conf`:

```tmux
set -g @plugin 'hiroakis/tmux-sage'

# optional configuration (defaults shown)
set -g @sage_mode 'daemon'    # daemon | hook | off
set -g @sage_args ''          # extra flags, e.g. '-lang Japanese -min-api-interval 300s'
```

Then press `prefix + I` to install. The plugin builds the binary with `go build` if Go is installed (otherwise set `@sage_bin` to a pre-built binary path, or put `tmux-sage` in your `PATH`).

Make sure `ANTHROPIC_API_KEY` is visible to the tmux server — either start tmux from a shell that exports it, or set it explicitly:

```sh
tmux set-environment -g ANTHROPIC_API_KEY sk-ant-...
```

| Plugin option | Default | Description |
|---|---|---|
| `@sage_mode` | `daemon` | `daemon` runs tmux-sage in the background; `hook` summarizes on window switch only; `off` disables |
| `@sage_args` | (empty) | Extra command-line flags passed to tmux-sage |
| `@sage_bin` | auto-detect | Path to the tmux-sage binary (plugin dir → `PATH` → `go build`) |
| `@sage_log` | `$TMPDIR/tmux-sage.log` | Daemon / hook log file |

### Manual

```sh
go build -o tmux-sage .
export ANTHROPIC_API_KEY=sk-ant-...

# Try it first with dry-run (prints labels without renaming)
./tmux-sage -once -dry-run

# Run as a daemon
./tmux-sage &
```

## Daemon vs hook mode

**Daemon mode (default, recommended).** A background process polls every window on an interval, so names stay fresh even for windows you are *not* looking at — a build that finished or a long-running agent that completed in a background tab is reflected in its name. Idle windows cost nothing: unchanged content is skipped by hash comparison before any API call.

**Hook mode.** No resident process; a one-shot pass runs each time the current window changes. Because per-window state (content hash, last-call time) is persisted as tmux window options, debouncing works correctly across invocations. The trade-off: names of background windows only update when you interact with tmux. Enable it via TPM (`set -g @sage_mode 'hook'`) or manually:

```tmux
set-hook -g session-window-changed "run-shell -b 'tmux-sage -once >>/tmp/tmux-sage.log 2>&1'"
```

> **Note:** TPM's installer (`prefix + I`) only downloads plugins. The hook is registered when the plugin script runs — reload your config afterwards with `tmux source-file ~/.tmux.conf`.

You can also run `tmux-sage -once` from cron or any other trigger — every invocation is safe and cheap thanks to the persisted state.

## Options

| Flag | Default | Description |
|---|---|---|
| `-interval` | `30s` | Polling interval |
| `-min-api-interval` | `180s` | Minimum interval between summarizations per window |
| `-lines` | `30` | Number of lines captured from the bottom of each pane |
| `-max-label-len` | `20` | Maximum label length in characters |
| `-max-desc-len` | `60` | Maximum description length (stored in `@sage_desc`) |
| `-provider` | `anthropic` | LLM provider: `anthropic`, `openai` (works with any OpenAI-compatible API), `gemini`, or `vertex` |
| `-base-url` | | API base URL override for `-provider openai` / `gemini` / `vertex` |
| `-vertex-project` | `$GOOGLE_CLOUD_PROJECT` | GCP project ID for `-provider vertex` |
| `-vertex-location` | `global` | GCP location for `-provider vertex` (e.g. `us-central1`, `asia-northeast1`) |
| `-model` | `claude-haiku-4-5` | Model ID (required for providers other than `anthropic`) |
| `-price-in` / `-price-out` | `0` | USD per 1M input/output tokens for cost logging; overrides built-in prices (useful for OpenAI/local models) |
| `-lang` | `English` | Language for generated labels and descriptions (e.g. `English`, `Japanese`, `ja`, `fr`) |
| `-redact` | `true` | Mask likely secrets (API keys, tokens, `Authorization:` headers) in pane contents before sending them to the LLM |
| `-change-threshold` | `0.1` | Fraction of changed lines required to re-summarize (0 = any change). Filters spinner/clock-only updates from TUI apps |
| `-max-cost-per-day` | `0` | Stop calling the API after this USD spend in a calendar day (0 = unlimited). Resets at local midnight; tracked in-memory per process |
| `-min-content` | `100` | Skip windows whose total pane content is smaller than this many bytes (e.g. an empty shell prompt) |
| `-dry-run` | `false` | Print labels without renaming windows |
| `-once` | `false` | Run a single pass and exit |
| `-verbose` | `false` | Also log per-window skip decisions (no change / debounced) |
| `-version` | | Print version and exit |

## Using OpenAI, Gemini, Vertex AI, or local LLMs

`-provider openai` speaks the OpenAI chat completions API, which is also served by Ollama, llama.cpp, LM Studio, vLLM, and most other local LLM runtimes. `-provider gemini` speaks the Gemini API (Google AI Studio, API-key auth). `-provider vertex` runs Gemini models on Vertex AI with GCP Application Default Credentials:

```sh
# OpenAI
export OPENAI_API_KEY=sk-...
tmux-sage -provider openai -model gpt-4o-mini

# Gemini (Google AI Studio)
export GEMINI_API_KEY=...
tmux-sage -provider gemini -model gemini-2.5-flash-lite

# Vertex AI (uses ADC: gcloud auth application-default login)
tmux-sage -provider vertex -vertex-project my-project -model gemini-2.5-flash-lite

# Ollama (no API key; pane contents never leave your machine)
tmux-sage -provider openai -base-url http://localhost:11434/v1 -model qwen2.5:7b
```

Cost logging has built-in prices for Claude tiers (haiku/sonnet/opus), `gpt-4o` / `gpt-4o-mini`, and the `gemini-2.5` family (**prices as of July 2026** — see `builtinPrices` in `llm.go`). For other models, or when prices change, pass `-price-in` / `-price-out` (USD per 1M tokens); otherwise the log shows `cost=unknown`.

## Showing the longer description in choose-window

tmux-sage stores a longer description of each window's activity in the `@sage_desc` window option. You can display it in the window list by customizing the choose-window (`choose-tree -w`) format. In `~/.tmux.conf`:

```tmux
bind Space choose-tree -w -F '#{window_name}#{window_flags} #{?#{!=:#{@sage_desc},},— #{@sage_desc},}'
```

If you also want to keep the active pane's title (shown by the default format), add `"#{pane_title}"` to the format string.

## Excluding specific windows

In the window you want to exclude:

```sh
tmux set-option -w @sage_off 1
```

To re-enable: `tmux set-option -wu @sage_off`.

## Notes

- Pane contents are sent to the Anthropic API for summarization. Likely secrets are masked by default (`-redact`), but this is best-effort pattern matching — set `@sage_off` on windows whose panes may display sensitive material.
- Only one tmux-sage instance runs per user (enforced with a lock file) — concurrent hook invocations or a daemon + hook combination won't double-summarize.
- Automatically renamed windows have tmux's `automatic-rename` turned off (same behavior as a manual rename).
- Each successful LLM call is logged with its token usage and cost, plus running totals. Built-in model prices (`builtinPrices` in `llm.go`) are a snapshot as of July 2026 — override with `-price-in` / `-price-out` when they drift.
