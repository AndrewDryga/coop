# The POC changes only its own container network namespace, never the host firewall.
FROM envoyproxy/envoy:v1.39.1@sha256:57e14a549d7bd43c8d3f6d03e8cfa653e037d4b38e133acd9b54f38c524401b4
USER root
RUN apt-get update && apt-get install -y --no-install-recommends \
    nftables dnsmasq-base python3 curl nodejs openssl iproute2 util-linux dnsutils \
    && rm -rf /var/lib/apt/lists/*
COPY runtime.py /poc/runtime.py
ENTRYPOINT ["python3", "/poc/runtime.py"]
