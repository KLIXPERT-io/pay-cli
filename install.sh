#!/bin/sh
# install.sh — install the PayCLI `pay` binary on macOS or Linux.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/KLIXPERT-io/pay-cli/main/install.sh | sh
#   curl -fsSL https://raw.githubusercontent.com/KLIXPERT-io/pay-cli/main/install.sh | sh -s -- --with-skills
#
# Flags:
#   --version TAG   install a specific release (e.g. v0.1.0); default: latest
#   --dir PATH      install directory; default: $HOME/.local/bin
#   --with-skills   run `pay skills install` afterwards
#   --dry-run       print what would happen and exit 0
#   --help
#
# Environment:
#   PAY_VERSION       same as --version
#   INSTALL_DIR       same as --dir
#   GH_TOKEN          used for the api.github.com call (the anonymous GitHub API
#                     is 60 req/h per IP, which shared CI addresses exhaust
#                     constantly)
#   PAY_INSTALL_JSON  when 1, print {"version":…,"path":…} as the final line
#
# POSIX sh only: no bash, no jq. The one hard dependency beyond curl and tar is
# a sha256 tool, because an install that skips verification is not an install.

set -eu

REPO="KLIXPERT-io/pay-cli"
BIN="pay"

VERSION="${PAY_VERSION:-}"
TARGET_DIR="${INSTALL_DIR:-}"
WITH_SKILLS=0
DRY_RUN=0

usage() {
  cat <<EOF
$BIN installer

Usage: sh install.sh [--version TAG] [--dir PATH] [--with-skills] [--dry-run]

Flags:
  --version TAG   release tag to install (default: latest)
  --dir PATH      install directory (default: \$HOME/.local/bin)
  --with-skills   run 'pay skills install' after installing
  --dry-run       print the plan and exit without touching the filesystem
  -h, --help      this text

Environment:
  PAY_VERSION, INSTALL_DIR, GH_TOKEN, PAY_INSTALL_JSON
EOF
}

err()  { printf 'error: %s\n' "$*" >&2; exit 1; }
info() {
  if [ "$DRY_RUN" -eq 1 ]; then
    printf '[dry-run] %s\n' "$*"
  else
    printf '%s\n' "$*"
  fi
}

while [ $# -gt 0 ]; do
  case "$1" in
    --version) [ $# -ge 2 ] || err "--version needs a value"; VERSION="$2"; shift 2 ;;
    --version=*) VERSION="${1#--version=}"; shift ;;
    --dir)     [ $# -ge 2 ] || err "--dir needs a value"; TARGET_DIR="$2"; shift 2 ;;
    --dir=*)   TARGET_DIR="${1#--dir=}"; shift ;;
    --with-skills) WITH_SKILLS=1; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) err "unknown argument: $1 (try --help)" ;;
  esac
done

# --- dependencies ----------------------------------------------------------
have() { command -v "$1" >/dev/null 2>&1; }
have curl || err "curl is required"
have tar  || err "tar is required"

if have sha256sum; then sha_cmd="sha256sum"
elif have shasum;  then sha_cmd="shasum -a 256"
else err "sha256sum or shasum is required (the archive is always verified)"
fi

# --- platform --------------------------------------------------------------
os_raw="$(uname -s)"
arch_raw="$(uname -m)"

case "$os_raw" in
  Linux)  os="linux" ;;
  Darwin) os="darwin" ;;
  *) err "unsupported OS: $os_raw (use install.ps1 on Windows)" ;;
esac

case "$arch_raw" in
  x86_64|amd64)  arch="amd64" ;;
  arm64|aarch64) arch="arm64" ;;
  *) err "unsupported architecture: $arch_raw" ;;
esac

# --- version ---------------------------------------------------------------
auth_header=""
if [ -n "${GH_TOKEN:-}" ]; then
  auth_header="Authorization: Bearer $GH_TOKEN"
fi

if [ -z "$VERSION" ]; then
  # Parse "tag_name": "v1.2.3" without jq.
  if [ -n "$auth_header" ]; then
    body="$(curl -fsSL -H "$auth_header" "https://api.github.com/repos/${REPO}/releases/latest")" \
      || err "could not reach the GitHub release API"
  else
    body="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest")" \
      || err "could not reach the GitHub release API (set GH_TOKEN if you are rate-limited)"
  fi
  VERSION="$(printf '%s' "$body" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n1)"
  [ -n "$VERSION" ] || err "could not resolve the latest release tag"
fi
case "$VERSION" in v*) ;; *) VERSION="v$VERSION" ;; esac

# goreleaser strips the leading v from .Version.
ver_noV="${VERSION#v}"
# FROZEN archive name — must match .goreleaser.yaml's name_template,
# internal/update.archiveAssetName and install.ps1. scripts/arch-lint.sh
# asserts all four agree.
archive="${BIN}_${ver_noV}_${os}_${arch}.tar.gz"
base_url="${PAY_DOWNLOAD_BASE:-https://github.com/${REPO}/releases/download/${VERSION}}"

# --- install directory -----------------------------------------------------
# $HOME/.local/bin by default, not /usr/local/bin: agents rarely have sudo, and
# a user-writable install is a precondition for `pay update-self` working at all.
if [ -z "$TARGET_DIR" ]; then
  TARGET_DIR="$HOME/.local/bin"
fi
install_path="$TARGET_DIR/$BIN"

info "pay $VERSION  ($os/$arch)"
info "  archive: $base_url/$archive"
info "  install: $install_path"

if [ "$DRY_RUN" -eq 1 ]; then
  if [ "$WITH_SKILLS" -eq 1 ]; then
    info "  then: $BIN skills install"
  fi
  exit 0
fi

mkdir -p "$TARGET_DIR" || err "cannot create $TARGET_DIR"

# --- download + verify + extract -------------------------------------------
tmp="$(mktemp -d 2>/dev/null || mktemp -d -t pay-install)"
trap 'rm -rf "$tmp"' EXIT INT HUP TERM

printf 'Downloading %s ...\n' "$archive"
curl -fsSL -o "$tmp/$archive"      "$base_url/$archive"      || err "download failed: $archive"
curl -fsSL -o "$tmp/checksums.txt" "$base_url/checksums.txt" || err "download failed: checksums.txt"

printf 'Verifying checksum ...\n'
expected="$(grep " $archive\$" "$tmp/checksums.txt" | awk '{print $1}')"
[ -n "$expected" ] || err "no checksum entry for $archive in checksums.txt"
actual="$(cd "$tmp" && $sha_cmd "$archive" | awk '{print $1}')"
[ "$expected" = "$actual" ] || err "CHECKSUM MISMATCH for $archive
  expected $expected
  computed $actual
Refusing to install. This is what a tampered or truncated download looks like."

# Optional signature verification. The release signs checksums.txt, so the
# chain is archive -> checksums.txt -> signature. cosign being absent is not a
# failure: it is "no evidence either way", and the install says so out loud
# rather than silently implying it checked.
if have cosign; then
  if curl -fsSL -o "$tmp/checksums.txt.sig" "$base_url/checksums.txt.sig" 2>/dev/null &&
     curl -fsSL -o "$tmp/checksums.txt.pem" "$base_url/checksums.txt.pem" 2>/dev/null; then
    printf 'Verifying signature with cosign ...\n'
    if cosign verify-blob \
        --certificate "$tmp/checksums.txt.pem" \
        --signature   "$tmp/checksums.txt.sig" \
        --certificate-identity-regexp "^https://github.com/${REPO}/" \
        --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
        "$tmp/checksums.txt" >/dev/null 2>&1; then
      printf '  signature OK\n'
    else
      err "SIGNATURE VERIFICATION FAILED for checksums.txt. Refusing to install."
    fi
  else
    printf 'note: no signature published for this release; checksum only.\n' >&2
  fi
else
  printf 'note: cosign is not installed, so only the checksum was verified.\n' >&2
fi

tar -xzf "$tmp/$archive" -C "$tmp" || err "could not extract $archive"
[ -f "$tmp/$BIN" ] || err "binary $BIN not found inside $archive"

mv "$tmp/$BIN" "$install_path" || err "cannot write $install_path"
chmod 0755 "$install_path"

printf '\nInstalled %s to %s\n' "$VERSION" "$install_path"
"$install_path" --version 2>/dev/null || true

if [ "$WITH_SKILLS" -eq 1 ]; then
  printf '\nInstalling the agent skill ...\n'
  "$install_path" skills install || printf 'note: pay skills install failed; run it manually.\n' >&2
fi

case ":$PATH:" in
  *":$TARGET_DIR:"*) ;;
  *) printf '\nNote: %s is not on your $PATH. Add it with:\n  export PATH="%s:$PATH"\n' \
       "$TARGET_DIR" "$TARGET_DIR" ;;
esac

if [ "${PAY_INSTALL_JSON:-}" = "1" ]; then
  printf '{"version":"%s","path":"%s","os":"%s","arch":"%s"}\n' \
    "$VERSION" "$install_path" "$os" "$arch"
fi
