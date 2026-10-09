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
# proxy hands a large module (modernc.org/sqlite) to storage.googleapis.com, which stays blocked:
# it would reach any public bucket. Fetch such a module from the host into the same volume:
#   docker run --rm --user 1000:1000 --entrypoint sh -v coop-cache:/home/node/.cache \
#     -v "$PWD":/src:ro -w /src -e HOME=/home/node <this project's box image> -lc 'go mod download'
ENV GOMODCACHE=/home/node/.cache/go-mod

# The gate's own tools (make check), at the Makefile's STATICCHECK_VERSION and GOVULNCHECK_VERSION;
# its version checks fail loudly if these drift. Built as node, installed for every shell.
COPY --chown=node:node internal/box/staticcheck.mod /tmp/staticcheck/go.mod
COPY --chown=node:node internal/box/staticcheck.sum /tmp/staticcheck/go.sum
RUN GOBIN=/tmp/gate-tools go -C /tmp/staticcheck install -mod=readonly honnef.co/go/tools/cmd/staticcheck \
 && GOBIN=/tmp/gate-tools go install golang.org/x/vuln/cmd/govulncheck@v1.8.0

# Login shells reset PATH, so restore the same order after /etc/profile runs (this drop-in sorts
# after Coop's own, which leaves the shims out in a filtered box).
USER root
RUN printf 'export PATH="/opt/coop/bin:/home/node/.asdf/shims:$PATH"\n' > /etc/profile.d/zz-asdf.sh \
 && install -m 0755 /tmp/gate-tools/* /usr/local/bin/ && rm -rf /tmp/gate-tools \
 && apt-get update && apt-get install -y --no-install-recommends shellcheck git-lfs make \
 && rm -rf /var/lib/apt/lists/*
USER node

WORKDIR /workspace
