#!/usr/bin/env bash
#
# arch-lint.sh — the CI-enforced architectural rules of ARCHITECTURE.md §3.1.
#
# These are greps, not a type system, and that is the point: each one guards an
# invariant that is cheap to state, expensive to re-derive during review, and
# catastrophic to lose silently. A violation is a build failure, never a
# warning.
#
# Usage:  ./scripts/arch-lint.sh            (from anywhere; it finds the repo root)
#         make lint                          (what CI runs)
#
# Exit 0 = clean. Exit 1 = at least one rule was violated.

set -uo pipefail

cd "$(dirname "$0")/.." || exit 1
ROOT="$(pwd)"

FAIL=0
RULE=""

red()  { printf '\033[31m%s\033[0m\n' "$*"; }
bold() { printf '\033[1m%s\033[0m\n' "$*"; }

# rule starts a new named rule and prints its heading once a violation is found.
rule() { RULE="$1"; }

fail() {
  if [ "$FAIL" -eq 0 ] || [ -n "$RULE" ]; then
    red "FAIL: $RULE"
    RULE=""
  fi
  printf '  %s\n' "$*"
  FAIL=1
}

# go_files lists every tracked .go file, excluding vendor and generated trees.
go_files() {
  find . -type f -name '*.go' \
    -not -path './vendor/*' \
    -not -path './.git/*' \
    | LC_ALL=C sort
}

# scan greps a pattern over a file list and reports every hit that is not a
# comment. Comments are excluded so that documenting a banned symbol (which
# every one of these rules does, in prose) does not trip the rule that bans it.
#
#   scan <pattern> <file...>
scan() {
  local pattern="$1"; shift
  local f hits
  for f in "$@"; do
    [ -f "$f" ] || continue
    hits="$(grep -nE "$pattern" "$f" 2>/dev/null \
      | grep -vE '^[0-9]+:[[:space:]]*(//|/\*|\*)' \
      | grep -vE 'arch-lint:allow')"
    if [ -n "$hits" ]; then
      while IFS= read -r line; do
        fail "$f:$line"
      done <<< "$hits"
    fi
  done
}

bold "arch-lint: ARCHITECTURE.md §3.1"

# ---------------------------------------------------------------------------
# Rule 1 — only internal/cli/app.go and cmd/ may see the process itself.
#
# This is the single change that makes CLI-level testing possible: every
# command's output, environment and clock arrive through an injected App, so a
# test can drive the whole program over bytes.Buffer and a pinned clock.
# ---------------------------------------------------------------------------
# Two trees are excluded throughout:
#   tools/              developer commands, not part of the shipped binary; a
#                       standalone main() legitimately owns its own streams.
#   internal/payloadtest test-support code, imported only from _test.go. It
#                       never links into `pay`, and UPDATE_GOLDEN plus writing a
#                       golden file are exactly what it exists to do.
rule "os.Stdout / os.Stderr / os.Stdin / os.Args / os.Exit are confined to internal/cli/app.go and cmd/"
PROCESS_SYMBOLS='(^|[^[:alnum:]_.])os\.(Stdout|Stderr|Stdin|Args|Exit)\b'
mapfile -t process_scope < <(go_files \
  | grep -v '^\./cmd/' \
  | grep -v '^\./internal/cli/app\.go$' \
  | grep -v '^\./tools/' \
  | grep -v '_test\.go$')
scan "$PROCESS_SYMBOLS" "${process_scope[@]}"

# This one deliberately includes _test.go. A test that reads the real clock or
# the real environment is a test that passes on one machine and fails on
# another, and §3.1's injected App exists precisely so it never has to.
rule "os.Getenv and time.Now are confined to internal/cli/app.go and cmd/ (tests included)"
CLOCK_ENV_SYMBOLS='(^|[^[:alnum:]_.])(os\.(Getenv|LookupEnv|Environ)|time\.Now)\('
mapfile -t clock_scope < <(go_files \
  | grep -v '^\./cmd/' \
  | grep -v '^\./internal/cli/app\.go$' \
  | grep -v '^\./tools/' \
  | grep -v '^\./internal/payloadtest/')
scan "$CLOCK_ENV_SYMBOLS" "${clock_scope[@]}"

# os.Setenv is not in §3.1's list, but mutating the process environment from
# library code is the same mistake from the other side: it makes one package's
# behaviour depend on another's execution order. Test files are exempt — a test
# of the process boundary may touch the process boundary — but production code
# is not.
rule "os.Setenv is never called from non-test code"
mapfile -t setenv_scope < <(go_files \
  | grep -v '^\./cmd/' \
  | grep -v '^\./tools/' \
  | grep -v '_test\.go$')
scan '(^|[^[:alnum:]_.])os\.Setenv\(' "${setenv_scope[@]}"

# ---------------------------------------------------------------------------
# Rule 2 — every http.Request is built in internal/payload/transport.go.
#
# Accept-Language (§6) and the Authorization redaction rule (§5.3) are applied
# there. A request built anywhere else silently bypasses both.
# ---------------------------------------------------------------------------
rule "http.Request construction is confined to internal/payload/transport.go"
mapfile -t request_scope < <(go_files \
  | grep -v '^\./internal/payload/transport\.go$' \
  | grep -v '_test\.go$' \
  | grep -v '^\./internal/update/' \
  | grep -v '^\./internal/fetch/')   # neither the self-updater nor the URL fetcher talks to Payload
scan '(^|[^[:alnum:]_.])http\.(NewRequest|NewRequestWithContext)\(|&http\.Request\{' "${request_scope[@]}"

# ---------------------------------------------------------------------------
# Rule 3 — every http.Transport honours the proxy environment.
# ---------------------------------------------------------------------------
rule "every http.Transport sets Proxy: http.ProxyFromEnvironment"
mapfile -t transport_files < <(grep -rlE '&http\.Transport\{' --include='*.go' . 2>/dev/null | grep -v '_test\.go$')
for f in "${transport_files[@]:-}"; do
  [ -n "$f" ] || continue
  if ! grep -q 'ProxyFromEnvironment' "$f"; then
    fail "$f constructs an http.Transport without Proxy: http.ProxyFromEnvironment"
  fi
done

# ---------------------------------------------------------------------------
# Rule 4 — no logging of a request's headers.
#
# An Authorization header reaching a log line is the exact §5.3 failure this
# whole program is built to make impossible.
# ---------------------------------------------------------------------------
rule "a request's Header map is never passed to fmt/log/slog"
mapfile -t header_scope < <(go_files | grep -v '^\./internal/redact/')
scan '(fmt\.(Print|Printf|Println|Sprint|Sprintf|Fprint|Fprintf|Fprintln)|log\.(Print|Printf|Println|Fatal|Fatalf)|slog\.(Any|Info|Debug|Warn|Error))\([^)]*\breq\.Header\b' "${header_scope[@]}"
scan '(fmt\.(Print|Printf|Println|Sprint|Sprintf|Fprint|Fprintf|Fprintln)|log\.(Print|Printf|Println|Fatal|Fatalf))\([^)]*\b(request|r|req)\.Header\b' "${header_scope[@]}"

# ---------------------------------------------------------------------------
# Rule 5 — internal/discovery and internal/payload never import the CLI layer.
#
# They are libraries. An import edge in this direction would make the discovery
# pipeline untestable without a cobra command and would create an import cycle
# the moment internal/cli needed a discovery type.
# ---------------------------------------------------------------------------
rule "internal/discovery and internal/payload never import internal/cli or spf13/cobra"
mapfile -t library_scope < <(go_files | grep -E '^\./internal/(discovery|payload)/')
scan '"github\.com/KLIXPERT-io/pay-cli/internal/cli"|"github\.com/spf13/(cobra|pflag)"' "${library_scope[@]}"

# ---------------------------------------------------------------------------
# Rule 6 — §8.4: reactive invalidation is never triggered from discovery.
#
# Discovery IS the thing invalidation re-runs. A call from inside it is an
# unbounded recursion waiting for a schema change.
# ---------------------------------------------------------------------------
rule "internal/discovery never calls payload.WithReactiveInvalidation (§8.4)"
mapfile -t reactive_scope < <(go_files | grep -E '^\./internal/discovery/' | grep -v '_test\.go$')
scan 'WithReactiveInvalidation' "${reactive_scope[@]}"

# ---------------------------------------------------------------------------
# Rule 7 — §4.2/§5: a plaintext api_key never appears as a TOML key.
#
# config.Parse rejects it at any nesting depth with config_secret_in_plaintext.
# An example or a fixture that teaches the opposite is worse than no example.
# ---------------------------------------------------------------------------
rule "no example, fixture or document writes api_key as a TOML key"
mapfile -t config_like < <(find . -type f -name '*.toml' -not -path './.git/*' 2>/dev/null)
for f in "${config_like[@]:-}"; do
  [ -n "$f" ] || continue
  hits="$(grep -nE '^[[:space:]]*api_key[[:space:]]*=' "$f" 2>/dev/null)"
  if [ -n "$hits" ]; then fail "$f: $hits"; fi
done
for f in README.md INSTALL.md CHANGELOG.md; do
  [ -f "$f" ] || continue
  hits="$(grep -nE '^[[:space:]]*api_key[[:space:]]*=' "$f" 2>/dev/null)"
  if [ -n "$hits" ]; then fail "$f: $hits — documents a secret in plaintext config"; fi
done

# ---------------------------------------------------------------------------
# Rule 8 — every durable write goes through internal/fsatomic.
#
# One implementation of temp-file -> fsync -> chmod -> rename -> fsync dir. The
# audit log is the single documented exception: an append-only JSONL log cannot
# be rewritten in full per record without O(N^2) bytes, so it uses one O_APPEND
# write plus fsync, and rotation is os.Rename.
# ---------------------------------------------------------------------------
rule "durable writes go through internal/fsatomic (audit.log is the documented exception)"
mapfile -t write_scope < <(go_files \
  | grep -v '_test\.go$' \
  | grep -v '^\./internal/fsatomic/' \
  | grep -v '^\./internal/audit/' \
  | grep -v '^\./internal/update/' \
  | grep -v '^\./internal/payloadtest/' \
  | grep -v '^\./tools/')
scan '(^|[^[:alnum:]_.])os\.WriteFile\(' "${write_scope[@]}"

# ---------------------------------------------------------------------------
# Rule 9 — PRODUCT INVARIANT: PayCLI is strictly an API client.
#
# It never writes to a Payload project's source files. Every change it makes to
# a project goes through the REST API. The one thing it may write inside a
# project directory is the agent skill (internal/skills), which is documentation
# about PayCLI, not project source.
#
# Violating this turns a CLI that an agent can be trusted to run into one that
# can silently rewrite payload.config.ts.
# ---------------------------------------------------------------------------
rule "PayCLI never writes to a Payload project's source files"
mapfile -t project_write_scope < <(go_files | grep -v '_test\.go$')
scan '(os\.WriteFile|os\.Create|os\.OpenFile|fsatomic\.Write|fsatomic\.WriteFrom)\([^)]*(PayloadConfig|payload\.config|PackageJSON|package\.json|SrcDir)' \
  "${project_write_scope[@]}"

# internal/config's project detection and scan are READ-ONLY by construction
# (§4.3, §7.10): they must contain no write primitive at all.
rule "internal/config project detection and scanning never write"
scan '(os\.WriteFile|os\.Create|os\.OpenFile|os\.Remove|os\.Rename|os\.Mkdir|fsatomic\.Write)' \
  ./internal/config/project.go ./internal/config/projectscan.go

# internal/discovery must not write anything at all: it returns typed values and
# internal/cache owns persistence (§7.8).
rule "internal/discovery writes nothing to disk"
mapfile -t disc_scope < <(go_files | grep -E '^\./internal/discovery/' | grep -v '_test\.go$')
scan '(os\.WriteFile|os\.Create|os\.OpenFile|os\.MkdirAll|fsatomic\.Write)' "${disc_scope[@]}"

# ---------------------------------------------------------------------------
# Rule 10 — §5.3: no test fixture or golden file carries the live API key.
# ---------------------------------------------------------------------------
rule "no committed file contains the fixture API key"
if [ -d testdata ] || [ -d docs ]; then
  hits="$(grep -rInE 'paycli-dev-key-[0-9a-f]{8,}' \
    --include='*.json' --include='*.out' --include='*.err' --include='*.toml' \
    --include='*.yaml' --include='*.yml' \
    testdata docs 2>/dev/null)"
  if [ -n "$hits" ]; then
    while IFS= read -r line; do fail "$line"; done <<< "$hits"
  fi
fi

# ---------------------------------------------------------------------------
# Rule 11 — §15: the archive name template is frozen in four places.
#
# Changing it after v0.1.0 permanently breaks self-update for every already
# installed binary, because old binaries construct the old name.
# ---------------------------------------------------------------------------
rule "the frozen archive-name template agrees across goreleaser, install.sh, install.ps1 and internal/update"
check_frozen() {
  local file="$1" needle="$2"
  [ -f "$file" ] || return 0
  grep -qF -- "$needle" "$file" || fail "$file no longer contains the frozen template: $needle"
}
check_frozen .goreleaser.yaml 'pay_{{ .Version }}_{{ .Os }}_{{ .Arch }}'
check_frozen install.sh       '${BIN}_${ver_noV}_${os}_${arch}'
check_frozen install.ps1      '${Bin}_${verNoV}_${os}_${arch}'
check_frozen internal/update/update.go 'pay_%s_%s_%s'

# ---------------------------------------------------------------------------
# Rule 12 — VERSION drives the tag, and nothing else may.
# ---------------------------------------------------------------------------
rule "VERSION holds a bare semver and is the single source of the release tag"
if [ -f VERSION ]; then
  if ! grep -qE '^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$' VERSION; then
    fail "VERSION is not a bare semver (no leading v): $(cat VERSION)"
  fi
else
  fail "VERSION is missing"
fi

# ---------------------------------------------------------------------------

if [ "$FAIL" -ne 0 ]; then
  red ""
  red "arch-lint: FAILED. These rules are ARCHITECTURE.md §3.1 and are not advisory."
  red "If a hit is a deliberate, reviewed exception, append '// arch-lint:allow <reason>'"
  red "to that line — it is greppable, so every exception stays visible."
  exit 1
fi

bold "arch-lint: OK ($ROOT)"
exit 0
