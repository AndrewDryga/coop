# Install

co:op runs on macOS and Linux, on amd64 or arm64, with Git and Docker:

```bash copy-install
curl -fsSL https://coop.dryga.com/install.sh | sh
```

The script puts `coop` in `~/.local/bin`. It needs `curl`, `tar`, and `sha256sum` or `shasum`. If
Docker is available, it also builds the box image and runs `coop doctor`. If not, install Docker and
run those yourself:

```bash copy
coop build --egress open && coop doctor
```

To change what the script does, set these before `sh`, as in
`curl -fsSL https://coop.dryga.com/install.sh | COOP_NO_BUILD=1 sh`:

| Variable | What it does |
| --- | --- |
| `COOP_VERSION` | Install this release tag instead of the latest one. |
| `COOP_BIN_DIR` | Install into this folder instead of `~/.local/bin`. |
| `COOP_NO_BUILD=1` | Skip building the box image. |

## Container runtimes

Online providers need Docker, which supports the helper that keeps provider credentials outside the
box. Apple [container](https://github.com/apple/container) on macOS 26+ can run commands with open
networking and no credentials. If both are installed, co:op chooses Docker.

## Build from source

```bash copy
git clone https://github.com/AndrewDryga/coop && cd coop && make install
```

## Update

`coop update` updates the binary, then prepares the image for this project's network mode.
[Upgrading](../MIGRATING.md) lists what to change when a new version needs it.

## Verifying a download

`install.sh` comes from the `main` branch, served by GitHub Pages at coop.dryga.com. You can also
fetch it from `https://raw.githubusercontent.com/AndrewDryga/coop/main/install.sh`.

The script checks the archive against the release's `checksums.txt` with `sha256sum` or `shasum`, and
stops if the file or the tool is missing. If [cosign](https://github.com/sigstore/cosign) is
installed, it also verifies the release signature, which proves `checksums.txt` came from the release
workflow for that tag.

To verify by hand before you run anything, download the release files. Set `VER` to a tag from the
[releases page](https://github.com/AndrewDryga/coop/releases), and change `darwin_arm64` for
another platform:

```bash
VER=vX.Y.Z
ASSET="coop_${VER#v}_darwin_arm64.tar.gz"
base="https://github.com/AndrewDryga/coop/releases/download/$VER"
curl -fsSLO "$base/$ASSET"
curl -fsSLO "$base/checksums.txt"
curl -fsSLO "$base/checksums.txt.bundle"

# 1. checksums.txt is signed by the release workflow (keyless Sigstore):
cosign verify-blob checksums.txt \
  --bundle checksums.txt.bundle \
  --certificate-identity "https://github.com/AndrewDryga/coop/.github/workflows/release.yml@refs/tags/$VER" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

# 2. the archive matches the now-trusted checksum (Linux: sha256sum -c -):
awk -v f="$ASSET" '$2==f{print $1"  "f}' checksums.txt | shasum -a 256 -c -
```

## Uninstall

Quit editor sessions, loops and other `coop` commands first. Then, in each project:

- Merge or save any fork you still need. `coop fork ls` lists them, and their folders sit next to the
  project in `<project>-forks/`.
- If the project has services, run `coop down`. Add `--delete-volumes` to also delete their data,
  such as a database.

Remove co:op's containers, images and volumes from Docker. The volumes hold caches and any service
data `coop down` kept, in volumes named `coop-<project>-<id>_<name>`. Each `ls` shows what the next
command removes, so check it first:

```bash
docker ps -a --filter label=coop
docker rm -f $(docker ps -aq --filter label=coop)

docker image ls --filter reference='coop-*'
docker image rm $(docker image ls --filter reference='coop-*' --format '{{.Repository}}:{{.Tag}}')

docker volume ls --filter name=^coop-
docker volume rm $(docker volume ls -q --filter name=^coop-)
```

Removing an image by name keeps it if another tag still uses it.

Remove the binary and this machine's records: run history, network approvals and editor sessions.
Use your `COOP_BIN_DIR` if you installed elsewhere, or `command -v coop` to find the binary.

```bash
rm -f ~/.local/bin/coop
rm -rf ~/.local/state/coop "${XDG_STATE_HOME:-$HOME/.local/state}/coop"
```

In each project you approved, delete the hidden approval file. Without the records above, a later
install refuses to run there until it is gone:

```bash
rm -f .coop-network-approval
```

The last step deletes your settings and every account you signed in to with `coop login`, along with
the API keys in `agents/env`, your presets and MCP servers. If the folder syncs through Dropbox or
another service, it disappears from your other machines too. Copy anything you want to keep, then:

```bash
rm -rf "${COOP_HOME:-${XDG_CONFIG_HOME:-$HOME/.config}/coop}"
```

Your projects keep their `.agent/` folder and the instruction files `coop init` added, which are
yours to keep or delete. Also remove any `coop completion` line from your shell startup file and any
editor setting that runs `coop acp`.
