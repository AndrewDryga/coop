#!/usr/bin/env bash
#
# Unit tests for the pure logic in bin/agent — no container runtime needed.
# We source the script (its source-guard skips `main`) and call functions
# directly. AGENT_RUNTIME is pinned so runtime detection doesn't require Docker.
#
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export AGENT_RUNTIME=docker
# shellcheck source=/dev/null
source "$here/bin/agent"
set +e   # a failing assertion shouldn't abort the run

pass=0; fail=0
ok() { pass=$((pass+1)); printf '  \033[32mok\033[0m  %s\n' "$1"; }
no() { fail=$((fail+1)); printf '  \033[31mNO\033[0m  %s\n' "$1"; }
eq() { if [ "$2" = "$3" ]; then ok "$1"; else no "$1 — want [$3] got [$2]"; fi; }
has() { case "$2" in *"$3"*) ok "$1";; *) no "$1 — [$3] missing";; esac; }
hasnt() { case "$2" in *"$3"*) no "$1 — [$3] unexpectedly present";; *) ok "$1";; esac; }

echo "services_project (compose project name is deterministic + sanitized):"
eq "lowercases and strips junk" "$(services_project /tmp/My_Repo.Name)" "agentbox-my_reponame"

echo "image_for_repo (which image a repo runs in):"
d="$(mktemp -d)"
eq "no Dockerfile.agent -> shared base" "$(image_for_repo "$d")" "$BASE_IMAGE"
AGENT_IMAGE=custom eq "explicit AGENT_IMAGE wins" "$(AGENT_IMAGE=custom image_for_repo "$d")" "custom"
touch "$d/Dockerfile.agent"
eq "Dockerfile.agent -> own tag" "$(image_for_repo "$d")" \
   "agentbox-$(basename "$d" | tr '[:upper:]' '[:lower:]' | tr -cd 'a-z0-9_-')"
rm -rf "$d"

echo "compute_mounts (secret enumeration — the security-critical part):"
f="$(mktemp -d)"; mkdir -p "$f/secrets" "$f/src"
: > "$f/.env"; : > "$f/.env.example"; : > "$f/src/app.js"; : > "$f/secrets/token"; : > "$f/prod.tfvars"
compute_mounts "$f"
m="$(printf '%s\n' "${MOUNTS[@]}")"
eq  "shadows exactly 3 paths (.env, tfvars, secrets/)" "$SHADOW_COUNT" "3"
has "workspace is bind-mounted"      "$m" "$f:$WORKDIR"
has ".env gets a read-only decoy"    "$m" "$WORKDIR/.env:ro"
has "*.tfvars gets a decoy"          "$m" "$WORKDIR/prod.tfvars:ro"
has "secrets/ gets a tmpfs"          "$m" "--tmpfs"
has "secrets/ tmpfs path"            "$m" "$WORKDIR/secrets"
hasnt ".env.example stays visible"   "$m" ".env.example"
hasnt "source stays visible"         "$m" "app.js"
rm -rf "$f"

echo
if [ "$fail" -eq 0 ]; then echo "all $pass unit checks passed"; exit 0
else echo "$pass passed, $fail failed"; exit 1; fi
