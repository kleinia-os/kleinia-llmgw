#!/bin/sh
# test_e2e.sh — semi-integration test: the sys.llm carrier's fwd/skill
# layer (via ./test_fwd --probe) against a REAL llmgwd.
#
# Boots a throwaway llmgwd on 127.0.0.1:18487 with a temp SQLite db, a
# temp internal token and one fake source (upstream http://127.0.0.1:1 —
# deliberately unreachable; we only exercise the forwarding layer, never
# a real model), then asserts:
#
#   GET /healthz        -> 200 {"ok":true}
#   GET /v1/models      -> 200 JSON list
#   GET /internal/usage -> 200 JSON with the internal token header
#   GET /internal/usage without token -> wrapped {"gateway_status":401}
#   POST /v1/chat/completions (internal auth) -> wrapped gateway error
#                                        (fake upstream refuses to connect)
#
# Everything the script starts, it stops (trap cleanup). Ports stay in
# the 18480-18499 range.
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/.." && pwd)
PORT=18487
MODEL=e2e-model

TMP=$(mktemp -d /tmp/sysagent_llm_e2e.XXXXXX)
LLMGWD_PID=""
cleanup() {
	if [ -n "$LLMGWD_PID" ]; then
		kill "$LLMGWD_PID" 2>/dev/null || true
		wait "$LLMGWD_PID" 2>/dev/null || true
	fi
	rm -rf "$TMP"
}
trap cleanup EXIT INT TERM

# --- build the test binary (and llmgwd itself when missing) ----------
make -C "$HERE" test_fwd >/dev/null
if [ ! -x "$ROOT/bin/llmgwd" ] || [ ! -x "$ROOT/bin/llmgwctl" ]; then
	echo "test_e2e: building llmgwd (PATH extended with ~/go/bin)..."
	PATH="$HOME/.local/go/bin:$PATH" make -C "$ROOT" build
fi

# --- throwaway gateway state -----------------------------------------
# NOTE: printf '%s' — no trailing newline: llmgwd uses the raw file
# bytes as the token, so the carrier trims while llmgwd does not.
printf '%s' 'e2e-internal-token-1f4c9a' > "$TMP/internal.token"
chmod 600 "$TMP/internal.token"
printf '%s\n' 'upstream-key-not-used' > "$TMP/upstream.key"

"$ROOT/bin/llmgwctl" --db "$TMP/state.db" source add fake \
	--type openai --base http://127.0.0.1:1 \
	--key-file "$TMP/upstream.key" --model "$MODEL"

"$ROOT/bin/llmgwd" \
	--db "$TMP/state.db" \
	--addr "127.0.0.1:$PORT" \
	--internal-token-file "$TMP/internal.token" \
	>"$TMP/llmgwd.log" 2>&1 &
LLMGWD_PID=$!

# --- probe (retries healthz internally while llmgwd comes up) --------
"$HERE/test_fwd" --probe "127.0.0.1:$PORT" 'e2e-internal-token-1f4c9a' "$MODEL"

echo "test_e2e: PASS (llmgwd log tail below)"
tail -3 "$TMP/llmgwd.log" || true
