#!/usr/bin/env sh
# Install the prebuilt `coop` binary — no Go, no clone:
#   curl -fsSL https://raw.githubusercontent.com/AndrewDryga/coop/main/install.sh | sh
# Env: COOP_VERSION (pin a release tag), COOP_BIN_DIR (default ~/.local/bin),
#      COOP_NO_BUILD=1 (skip building the box image).
set -eu

repo="AndrewDryga/coop"
bindir="${COOP_BIN_DIR:-$HOME/.local/bin}"

# verify_checksum ASSET CHECKSUMS_FILE ARCHIVE — verify ARCHIVE against the sha256 entry for
# ASSET in CHECKSUMS_FILE. Returns non-zero (and aborts the install) on a release-integrity
# failure: a *missing* entry for ASSET is treated as a failure, not as "unverified" — a
# fetched-but-incomplete checksums.txt must never let an unchecked binary through. Returns 0
# with a warning only when an entry exists but no sha256 tool is available to check it. Pure
# (no network), so it's unit-testable by sourcing this script with COOP_INSTALL_LIB=1.
verify_checksum() {
  vc_asset=$1
  vc_sums=$2
  vc_archive=$3
  vc_want=$(awk -v f="$vc_asset" '$2 == f {print $1}' "$vc_sums")
  if [ -z "$vc_want" ]; then
    echo "coop: checksums.txt has no entry for $vc_asset — aborting (release integrity check failed)" >&2
    return 1
  fi
  if command -v sha256sum >/dev/null 2>&1; then
    vc_got=$(sha256sum "$vc_archive" | cut -d ' ' -f 1)
  elif command -v shasum >/dev/null 2>&1; then
    vc_got=$(shasum -a 256 "$vc_archive" | cut -d ' ' -f 1)
  else
    echo "coop: no sha256 tool found; skipping checksum verification" >&2
    return 0
  fi
  if [ "$vc_want" != "$vc_got" ]; then
    echo "coop: checksum mismatch for $vc_asset — aborting (expected $vc_want, got $vc_got)" >&2
    return 1
  fi
  return 0
}

# Tests source this file (COOP_INSTALL_LIB=1) to reach the functions above without running
# the installer — stop here before any uname probing or network access.
if [ "${COOP_INSTALL_LIB:-}" = 1 ]; then
  return 0 2>/dev/null
fi

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) echo "coop: unsupported architecture: $arch" >&2; exit 1 ;;
esac
case "$os" in
  linux | darwin) ;;
  *) echo "coop: unsupported OS: $os (Linux and macOS only)" >&2; exit 1 ;;
esac

ver="${COOP_VERSION:-}"
if [ -z "$ver" ]; then
  ver=$(curl -fsSL "https://api.github.com/repos/$repo/releases/latest" |
    grep '"tag_name"' | head -1 | cut -d '"' -f 4)
fi
[ -n "$ver" ] || { echo "coop: could not resolve the latest release; set COOP_VERSION" >&2; exit 1; }

asset="coop_${ver#v}_${os}_${arch}.tar.gz"
url="https://github.com/$repo/releases/download/$ver/$asset"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
echo "coop: downloading $asset ($ver)…"
curl -fsSL "$url" -o "$tmp/coop.tar.gz" || { echo "coop: download failed: $url" >&2; exit 1; }

# Verify the download against the release's published checksums — defends against a
# tampered or MITM'd asset. Fails closed on a mismatch or a missing entry; best-effort
# (warn, continue) only when an entry exists but no sha256 tool is available to check it.
# When cosign is present we first verify checksums.txt's Sigstore signature, so the
# checksum file itself is trusted (not just internally consistent) — an attacker who
# swapped both the archive and checksums.txt would be caught here. Without cosign we
# fall back to the plain checksum and say the signature was not verified.
if curl -fsSL "https://github.com/$repo/releases/download/$ver/checksums.txt" -o "$tmp/checksums.txt"; then
  if command -v cosign >/dev/null 2>&1; then
    if curl -fsSL "https://github.com/$repo/releases/download/$ver/checksums.txt.bundle" -o "$tmp/checksums.txt.bundle"; then
      if cosign verify-blob "$tmp/checksums.txt" \
          --bundle "$tmp/checksums.txt.bundle" \
          --certificate-identity-regexp '^https://github.com/AndrewDryga/coop/' \
          --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
          >/dev/null 2>&1; then
        echo "coop: verified checksums.txt signature with cosign"
      else
        echo "coop: checksums.txt failed cosign signature verification — aborting" >&2
        exit 1
      fi
    else
      echo "coop: no checksums.txt.bundle for $ver; skipping signature verification" >&2
    fi
  else
    echo "coop: cosign not found; skipping signature check (see README → Verifying a download)" >&2
  fi
  verify_checksum "$asset" "$tmp/checksums.txt" "$tmp/coop.tar.gz" || exit 1
else
  echo "coop: could not fetch checksums.txt; skipping verification" >&2
fi

tar -xzf "$tmp/coop.tar.gz" -C "$tmp"
mkdir -p "$bindir"
install -m 0755 "$tmp/coop" "$bindir/coop"
echo "coop: installed $bindir/coop ($("$bindir/coop" version))"

case ":$PATH:" in
  *":$bindir:"*) ;;
  *) printf "\n  %s is not on your PATH — add to your shell rc:\n    export PATH=\"%s:\$PATH\"\n\n" "$bindir" "$bindir" ;;
esac

# Carry over an existing agent-box config (the pre-rename location), once.
conf="${XDG_CONFIG_HOME:-$HOME/.config}/coop"
old="${XDG_CONFIG_HOME:-$HOME/.config}/agent-box/agents"
if [ ! -d "$conf/agents" ] && [ -d "$old" ]; then
  mkdir -p "$conf"
  cp -R "$old" "$conf/agents"
  echo "coop: migrated config from $old"
fi

# Build the sandbox image + verify, when a container runtime is available.
if [ "${COOP_NO_BUILD:-0}" = 1 ]; then
  echo "coop: skipped image build (COOP_NO_BUILD=1) — next: coop build && coop doctor"
elif command -v container >/dev/null 2>&1 || command -v docker >/dev/null 2>&1 || command -v podman >/dev/null 2>&1; then
  "$bindir/coop" build && "$bindir/coop" doctor
else
  echo "coop: no container runtime found — install Docker, Podman, or Apple 'container',"
  echo "      then run: coop build && coop doctor"
fi

echo
echo "Done. From any repo:  coop claude   # a sandboxed agent"
