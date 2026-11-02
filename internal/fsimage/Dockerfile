FROM golang:1.27.3-bookworm@sha256:21ade014faf952365b9ecbd8dbdb87176fbacb82fac9fb132d9f2a7b8cc3f132 AS build
# Compiler and shared-library inputs are fixed by both version and archive date.
# Nothing is installed on the host, and no project build context enters this image.
RUN rm -f /etc/apt/sources.list.d/* \
 && printf '%s\n' \
    'deb [check-valid-until=no] https://snapshot.debian.org/archive/debian/20261101T000000Z bookworm main' \
    'deb [check-valid-until=no] https://snapshot.debian.org/archive/debian-security/20261101T000000Z bookworm-security main' > /etc/apt/sources.list \
 && apt-get update \
 && apt-get install -y --no-install-recommends \
    clang-16=1:16.0.6-15~deb12u1 libbpf-dev=1:1.1.2-0+deb12u1 \
    libelf-dev=0.188-2.1 zlib1g-dev=1:1.2.13.dfsg-1 \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY . .
RUN mkdir -p /out/usr/local/libexec/coop-fs /out/usr/local/bin /out/usr/share/coop-fs /out/tmp /out/host-cgroup /out/host-bpf /out/state \
 && chmod 1777 /out/tmp \
 && CGO_ENABLED=0 go build -mod=readonly -trimpath -buildvcs=false -o /out/usr/local/bin/coop-fs ./cmd/coop-fs \
 && /out/usr/local/bin/coop-fs unicode-table /out/usr/share/coop-fs/unicode.bin \
 && clang-16 -O2 -g -target bpfel -mcpu=v3 -I/usr/include/"$(gcc -print-multiarch)" -Wall -Werror \
    -c internal/fsfilter/assets/filter.c -o /out/usr/local/libexec/coop-fs/filter.o \
 && gcc -O2 -Wall -Werror internal/fsfilter/assets/loader.c -lbpf -lelf -lz -o /out/usr/local/libexec/coop-fs-kernel \
 && ldd /out/usr/local/libexec/coop-fs-kernel > /tmp/coop-fs-libraries \
 && ! grep -q 'not found' /tmp/coop-fs-libraries \
 && awk '/=> \// { print $3 } $1 ~ /^\// { print $1 }' /tmp/coop-fs-libraries \
    | while read -r library; do cp -L --parents "$library" /out/ || exit 1; done \
 && cp LICENSE /out/usr/share/coop-fs/LICENSE \
 && cp -L --parents /usr/share/doc/libbpf1/copyright /usr/share/doc/libelf1/copyright \
    /usr/share/doc/zlib1g/copyright /usr/share/doc/libc6/copyright /out/ \
 && cp internal/fsfilter/assets/loader.c internal/fsfilter/assets/filter.c /out/usr/share/coop-fs/

FROM scratch
COPY --from=build /out/ /
USER 0:0
ENTRYPOINT ["/usr/local/bin/coop-fs"]
