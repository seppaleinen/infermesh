#!/usr/bin/env bash
# validate-contracts.sh — cross-refer checks docs/contracts/*.md against source.
#
# Purpose: catch stale contract claims at PR time. The #123/#128/#129
# post-mortems showed that contract docs drifted from code across four merged
# PRs because nothing compared prose to source. This script checks the
# highest-impact, mechanically-verifiable claims:
#
#   1. CLI flag existence: every --flag-name mentioned in a contract doc must
#      be defined in cmd/router/main.go or cmd/worker/main.go (flag.* or
#      pflag.* definitions). Catches flags that don't exist.
#
#   2. CLI flag absence: "never passes --foo" / "does NOT pass --foo" claims
#      must be true — --foo must be ABSENT from app/args.go. Catches claims
#      that a flag is not passed when it actually is.
#
#   3. File:line references: "path/to/file.go:NNN" in a contract doc must
#      reference an existing file with at least NNN lines. Catches stale line
#      numbers.
#
#   4. "no auth is required" claims: warned (ambiguous — the phrase is often
#      true in dev mode but false in prod mode where mTLS wraps the mux).
#      Reviewer must confirm the claim is scoped to dev mode or qualified.
#
# Exit codes: 0 = all checks pass; 1 = one or more checks FAILED.
# Warnings (exit 0) are printed to stderr for reviewer attention.
#
# Usage: make validate-contracts   (or: hack/validate-contracts.sh)
#
# This script is intentionally conservative: it only checks claims it can
# verify mechanically. It does NOT attempt semantic analysis of prose.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

FAIL=0
WARN=0

pass() { echo "  PASS: $*"; }
fail() { echo "  FAIL: $*" >&2; FAIL=1; }
warn() { echo "  WARN: $*" >&2; WARN=1; }

echo "=== validate-contracts: docs/contracts/*.md vs source ==="

# ---------------------------------------------------------------------------
# Collect contract docs
# ---------------------------------------------------------------------------
DOCS=()
while IFS= read -r -d '' f; do
  DOCS+=("$f")
done < <(find docs/contracts -name '*.md' -print0 2>/dev/null)

if [ ${#DOCS[@]} -eq 0 ]; then
  echo "  (no contract docs found; skipping)"
  exit 0
fi

echo "  ${#DOCS[@]} contract doc(s) found"

# ---------------------------------------------------------------------------
# Rule 1: CLI flag existence
#
# Every --flag-name mentioned in a contract doc must be defined in
# cmd/router/main.go or cmd/worker/main.go. The flag can be defined via
# flag.String/Bool/Int or pflag.String/Bool/Int/Var.
# ---------------------------------------------------------------------------
echo ""
echo "--- Rule 1: CLI flag existence ---"

# Build the set of flags defined in router and worker CLIs.
defined_flags() {
  local file="$1"
  [ -f "$file" ] || return 0
  local out=""
  # Pattern A: fs.StringVar(&f.X, "flag-name", ...) — the Go flagset style
  # used by cmd/router/main.go and cmd/worker/main.go.
  # awk -F', "' splits on ', "' so $2 starts with the flag name in quotes.
  out="$(grep -E '\.(StringVar|BoolVar|IntVar|DurationVar|Float64Var|StringSliceVar)\(&' "$file" 2>/dev/null \
    | awk -F', "' '{if (NF>=2) {split($2,a,"\""); print "--" a[1]}}' || true)"
  printf '%s\n' "$out"
  # Pattern B: flag.String("flag-name", ...) / pflag.* direct calls.
  out="$(grep -oE '(flag|pflag)\.(String|Bool|Int|Duration|Float64|StringSlice)\("[a-z0-9-]+"' "$file" 2>/dev/null \
    | sed -E 's/.*\("([a-z0-9-]+)"/--\1/' || true)"
  printf '%s\n' "$out"
}

ROUTER_FLAGS="$(defined_flags cmd/router/main.go)"
WORKER_FLAGS="$(defined_flags cmd/worker/main.go)"
ALL_FLAGS="$(printf '%s\n%s\n' "$ROUTER_FLAGS" "$WORKER_FLAGS" | sort -u)"

# Find all --flag-name references in contract docs.
while IFS= read -r match; do
  # match format: doc:lineno:--flag
  doc="${match%%:*}"
  rest="${match#*:}"
  lineno="${rest%%:*}"
  flag="${rest#*:}"

  if ! grep -qxF -- "$flag" <<< "$ALL_FLAGS"; then
    # Flag not found in either CLI. Check if it's a well-known flag that
    # might be defined via a different pattern (e.g., subcommand flag).
    # If truly missing, report FAIL.
    fail "${doc}:${lineno}: flag ${flag} not found in cmd/router/main.go or cmd/worker/main.go"
  fi
done < <(
  # Extract --flag-name from backtick-quoted strings in markdown.
  # Pattern: `--flag-name`
  grep -nHoE '`--[a-z0-9-]+`' "${DOCS[@]}" 2>/dev/null \
    | sed -E 's/`//g' \
    | while IFS=: read -r doc lineno flag; do
        printf '%s:%s:%s\n' "$doc" "$lineno" "$flag"
      done \
    | sort -u
)

# ---------------------------------------------------------------------------
# Rule 2: CLI flag absence
#
# "never passes --foo" / "does NOT pass --foo" claims must be true:
# --foo must be ABSENT from app/args.go.
# ---------------------------------------------------------------------------
echo ""
echo "--- Rule 2: CLI flag absence claims ---"

if [ ! -f app/args.go ]; then
  warn "app/args.go not found; skipping flag-absence check"
else
  while IFS= read -r match; do
    doc="${match%%:*}"
    rest="${match#*:}"
    lineno="${rest%%:*}"
    claim="${rest#*:}"
    # claim is the full matched line; extract the flag.
    flag="$(printf '%s\n' "$claim" | grep -oE '`--[a-z0-9-]+`' | head -1 | tr -d '`' || true)"
    [ -z "$flag" ] && continue

    if grep -qF -- "$flag" app/args.go; then
      fail "${doc}:${lineno}: claims \"${flag}\" is never passed, but app/args.go DOES reference it"
    else
      pass "${doc}:${lineno}: \"${flag}\" correctly absent from app/args.go"
    fi
  done < <(
    # Find lines that say "never passes" or "does NOT pass" or "never append"
    # and contain a --flag-name.
    grep -nHE '(never|NOT|never|does not|does not) pass|never append|not passed' "${DOCS[@]}" 2>/dev/null \
      | grep -E '`--[a-z0-9-]+`' || true
  )
fi

# ---------------------------------------------------------------------------
# Rule 3: File:line references
#
# "path/to/file.go:NNN" in a contract doc must reference an existing file
# with at least NNN lines.
# ---------------------------------------------------------------------------
echo ""
echo "--- Rule 3: File:line references ---"

while IFS= read -r match; do
  doc="${match%%:*}"
  rest="${match#*:}"
  lineno="${rest%%:*}"
  ref="${rest#*:}"

  # ref is like "pkg/router/server.go:562" or "app/args.go:23"
  ref_file="${ref%:*}"
  ref_line="${ref##*:}"

  # Only check .go references (skip .md, .ts, etc.)
  [[ "$ref_file" == *.go ]] || continue

  if [ ! -f "$ref_file" ]; then
    fail "${doc}:${lineno}: references ${ref_file} which does not exist"
    continue
  fi

  actual_lines=$(wc -l < "$ref_file" | tr -d ' ')
  if [ "$ref_line" -gt "$actual_lines" ]; then
    fail "${doc}:${lineno}: ${ref_file} has only ${actual_lines} lines, but doc references line ${ref_line}"
  else
    pass "${doc}:${lineno}: ${ref} (file has ${actual_lines} lines)"
  fi
done < <(
  # Extract file:line references from contract docs.
  # Pattern: "pkg/router/server.go:562" or "app/args.go:23"
  # The leading segment [a-z0-9_.]+ matches the first path component (e.g.
  # "pkg"), then [a-z0-9_/.]* matches the rest.
  grep -nHoE '[a-z0-9_.]+([a-z0-9_/.]*[a-z0-9_])?\.(go|ts|vue|py|sh|yml|yaml):[0-9]+' "${DOCS[@]}" 2>/dev/null \
    | sort -u
)

# ---------------------------------------------------------------------------
# Rule 4: "no auth is required" claims
#
# These are WARN, not FAIL — the phrase is often true in dev mode but false
# in prod mode (mTLS wraps the entire mux). The reviewer must confirm the
# claim is scoped to dev mode or qualified.
# ---------------------------------------------------------------------------
echo ""
echo "--- Rule 4: 'no auth is required' claims (warnings) ---"

while IFS= read -r match; do
  doc="${match%%:*}"
  rest="${match#*:}"
  lineno="${rest%%:*}"
  text="${rest#*:}"

  # Check if the claim is qualified with "dev mode" nearby.
  # If the line already says "in dev mode" or "dev mode", it's scoped.
  if printf '%s' "$text" | grep -qiE 'dev mode|dev-mode'; then
    pass "${doc}:${lineno}: 'no auth' claim is scoped to dev mode"
  else
    warn "${doc}:${lineno}: 'no auth is required' claim — confirm it is scoped to dev mode (prod mode requires mTLS via NewMTLSMiddleware at pkg/router/server.go)"
  fi
done < <(
  grep -nHE 'no auth is required|no authentication is required' "${DOCS[@]}" 2>/dev/null || true
)

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
echo ""
echo "============================================="
if [ $FAIL -eq 1 ]; then
  echo "RESULT: FAIL (one or more checks failed)"
  exit 1
elif [ $WARN -eq 1 ]; then
  echo "RESULT: PASS with warnings (review warnings above)"
  exit 0
else
  echo "RESULT: PASS (all checks passed)"
  exit 0
fi
