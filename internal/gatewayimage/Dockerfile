FROM golang:1.27.1-bookworm@sha256:69a7b9788769bec032d238959b61854e9ae87f57be9029ec04e9885fabf99195 AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -mod=readonly -trimpath -buildvcs=false -o /out/coop-net ./cmd/coop-net

FROM envoyproxy/envoy:v1.39.1@sha256:57e14a549d7bd43c8d3f6d03e8cfa653e037d4b38e133acd9b54f38c524401b4
USER root
# Pin the complete added package closure; inherited libc/GMP/tinfo/CA versions
# come from the digest-pinned base. Missing versions fail the build, never float.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      nftables=1.0.2-1ubuntu3.1 libnftables1=1.0.2-1ubuntu3.1 \
      libnftnl11=1.2.1-1build1 libmnl0=1.0.4-3build2 \
      libxtables12=1.8.7-1ubuntu5.2 libedit2=3.1-20210910-1build1 \
      libjansson4=2.13.1-1.1build3 libbsd0=0.11.5-1 libmd0=1.0.4-1build1 \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir -p /ipc /private /observations \
 && chmod 0755 /ipc \
 && chown 65532:65532 /private /observations \
 && chmod 0700 /private /observations
COPY --from=build /out/coop-net /usr/local/bin/coop-net
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/coop-net"]
