#!/usr/bin/env bash
# TPM plugin entry point for tmux-sage.
#
# Configuration (set in ~/.tmux.conf before the plugin line):
#   set -g @sage_mode  'daemon'   # daemon (default) | hook | off
#   set -g @sage_args  '-lang Japanese -min-api-interval 300s'
#   set -g @sage_bin   '/path/to/tmux-sage'   # optional; auto-detected otherwise
#   set -g @sage_log   '/tmp/tmux-sage.log'
set -eu

CURRENT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

get_opt() { # <option> <default>
	local v
	v="$(tmux show-option -gqv "$1")"
	printf '%s' "${v:-$2}"
}

MODE="$(get_opt @sage_mode daemon)"
ARGS="$(get_opt @sage_args '')"
LOG="$(get_opt @sage_log "${TMPDIR:-/tmp}/tmux-sage.log")"
BIN="$(get_opt @sage_bin '')"

# Locate the binary: explicit option > plugin dir > PATH > build from source.
if [ -z "$BIN" ]; then
	if [ -x "$CURRENT_DIR/tmux-sage" ]; then
		BIN="$CURRENT_DIR/tmux-sage"
	elif command -v tmux-sage >/dev/null 2>&1; then
		BIN="$(command -v tmux-sage)"
	elif command -v go >/dev/null 2>&1; then
		(cd "$CURRENT_DIR" && go build -o tmux-sage .) && BIN="$CURRENT_DIR/tmux-sage"
	fi
fi

if [ -z "$BIN" ] || [ ! -x "$BIN" ]; then
	tmux display-message "tmux-sage: binary not found — set @sage_bin or install tmux-sage in PATH"
	exit 0
fi

case "$MODE" in
daemon)
	pid="$(tmux show-option -gqv @sage_pid)"
	if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
		exit 0 # already running
	fi
	# shellcheck disable=SC2086
	nohup "$BIN" $ARGS >>"$LOG" 2>&1 &
	tmux set-option -g @sage_pid "$!"
	;;
hook)
	tmux set-hook -g after-select-window "run-shell -b '$BIN -once $ARGS >>$LOG 2>&1'"
	;;
off) ;;
*)
	tmux display-message "tmux-sage: unknown @sage_mode '$MODE' (daemon | hook | off)"
	;;
esac
