# Tools are installed at the versions in .tool-versions.
# After changing that file, run coop build to rebuild this project's box.
# Coop's box image brings the agent CLIs and editor adapters, at the versions Coop qualifies.
ARG COOP_BASE_IMAGE=coop-box
FROM ${COOP_BASE_IMAGE}

# Add an apt-get layer here, as root, if a tool you add to .tool-versions needs system packages.

# Coop's filtered client image keeps asdf off PATH, so put the shims back — after the client
# launchers, which nothing may shadow.
ENV PATH="/opt/coop/bin:/home/node/.asdf/shims:${PATH}"

# Install the tool versions declared by the project.
# Installation failures stop the build.
COPY --chown=node:node .tool-versions /home/node/.tool-versions
RUN cd /home/node \
 && awk 'NF && $1 !~ /^#/ {print $1}' .tool-versions | while read -r tool; do asdf plugin add "$tool" || exit 1; done \
 && MAKEFLAGS="-j$(nproc)" asdf install \
 && asdf reshim

# Go's module downloads land in ~/.cache, the volume every box shares, so a box doesn't fetch them
# again (proxy.golang.org, sum.golang.org and vuln.go.dev are allowed in .agent/project.yaml). The
# proxy hands a large module (modernc.org/sqlite) to storage.googleapis.com, which .agent/project.yaml
# allows too, so a box downloads it itself. If a module still fails to download in a box, fetch it
# from the host into the same volume:
#   docker run --rm --user 1000:1000 --entrypoint sh -v coop-cache:/home/node/.cache \
#     -v "$PWD":/src:ro -w /src -e HOME=/home/node <this project's box image> -lc 'go mod download'
ENV GOMODCACHE=/home/node/.cache/go-mod

# The gate's own tools (make check), at the Makefile's STATICCHECK_VERSION and GOVULNCHECK_VERSION,
# since Coop's box images carry neither. The gate's version checks fail loudly if these drift, and
# agent_config_test.go holds the pins equal. Built as node, installed for every shell.
COPY --chown=node:node tools/staticcheck.mod /tmp/staticcheck/go.mod
COPY --chown=node:node tools/staticcheck.sum /tmp/staticcheck/go.sum
RUN GOBIN=/tmp/gate-tools go -C /tmp/staticcheck install -mod=readonly honnef.co/go/tools/cmd/staticcheck \
 && GOBIN=/tmp/gate-tools go install golang.org/x/vuln/cmd/govulncheck@v1.9.0

# Login shells reset PATH, so restore the same order after /etc/profile runs (this drop-in sorts
# after Coop's own, which leaves the shims out in a filtered box).
USER root
# This is Coop's development gate tool, not a change to the bundled client lock.
RUN npm install --global --ignore-scripts --no-audit --no-fund npm@11.21.2
RUN printf 'export PATH="/opt/coop/bin:/home/node/.asdf/shims:$PATH"\n' > /etc/profile.d/zz-asdf.sh \
 && install -m 0755 /tmp/gate-tools/* /usr/local/bin/ && rm -rf /tmp/gate-tools \
 && apt-get update && apt-get install -y --no-install-recommends git-lfs make \
 && rm -rf /var/lib/apt/lists/*
# ShellCheck at the Makefile's SHELLCHECK_VERSION, checked against the digests in
# tools/installshellcheck/checksums.txt; it lands in /usr/local/bin, ahead of the base image's
# Debian copy. agent_config_test.go holds the version and digests equal.
RUN arch="$(uname -m)" \
 && case "$arch" in \
      x86_64) sum=b7af85e41cc99489dcc21d66c6d5f3685138f06d34651e6d34b42ec6d54fe6f6 ;; \
      aarch64) sum=68a8133197a50beb8803f8d42f9908d1af1c5540d4bb05fdfca8c1fa47decefc ;; \
      *) echo "no reviewed ShellCheck for $arch" >&2; exit 1 ;; \
    esac \
 && curl --fail --silent --show-error --location --proto '=https' --output /tmp/shellcheck.tar.gz "https://github.com/koalaman/shellcheck/releases/download/v0.11.0/shellcheck-v0.11.0.linux.$arch.tar.gz" \
 && printf '%s  %s\n' "$sum" /tmp/shellcheck.tar.gz | sha256sum -c - \
 && tar -C /tmp -xzf /tmp/shellcheck.tar.gz shellcheck-v0.11.0/shellcheck \
 && install -m 0755 /tmp/shellcheck-v0.11.0/shellcheck /usr/local/bin/shellcheck \
 && rm -rf /tmp/shellcheck.tar.gz /tmp/shellcheck-v0.11.0
USER node

WORKDIR /workspace
