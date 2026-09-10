package networkgateway

// EnvoyBootstrap is fixed trusted configuration for the pinned 1.39.1 binary.
// The capless guard is the sole filesystem-socket caller and has already checked
// the shared canonical name matcher, ECH and the concrete controller lease.
// No destination is selected from client metadata or HTTP application fields:
// the original_dst cluster dials the address AND port of the PROXY v2 header the
// guard wrote, which carries the kernel's own record of the redirect.
const EnvoyBootstrap = `admin:
  address:
    pipe: {path: /private/admin.sock, mode: 384}
  allow_paths:
    - exact: /ready
static_resources:
  listeners:
    - name: guarded_tls
      address:
        pipe: {path: /private/data.sock, mode: 432}
      listener_filters_timeout: 3s
      continue_on_listener_filters_timeout: false
      per_connection_buffer_limit_bytes: 65536
      listener_filters:
        - name: envoy.filters.listener.proxy_protocol
          typed_config:
            "@type": type.googleapis.com/envoy.extensions.filters.listener.proxy_protocol.v3.ProxyProtocol
            allow_requests_without_proxy_protocol: false
            disallowed_versions: [V1]
            tlv_location: DYNAMIC_METADATA
            rules:
              - tlv_type: 5
                on_tlv_present:
                  metadata_namespace: coop.network
                  key: flow_id
                  value_string_encoding: SANITIZED_UTF8
        - name: envoy.filters.listener.tls_inspector
          typed_config:
            "@type": type.googleapis.com/envoy.extensions.filters.listener.tls_inspector.v3.TlsInspector
            close_connection_on_client_hello_parsing_errors: true
            max_client_hello_size: 16384
      filter_chains:
        - filter_chain_match: {transport_protocol: tls}
          filters:
            - name: envoy.filters.network.tcp_proxy
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.network.tcp_proxy.v3.TcpProxy
                stat_prefix: guarded
                cluster: validated_destination
                max_connect_attempts: 1
                idle_timeout: 300s
                access_log_options:
                  flush_access_log_on_start: true
                  flush_access_log_on_connected: true
                  access_log_flush_interval: 1s
                access_log:
                  - name: envoy.access_loggers.file
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.access_loggers.file.v3.FileAccessLog
                      path: /dev/stdout
                      log_format:
                        text_format_source:
                          inline_string: |
                            {"flow_id":"%DYNAMIC_METADATA(coop.network:flow_id)%","connection_id":"%CONNECTION_ID%","phase":"%ACCESS_LOG_TYPE%","peer":"%UPSTREAM_HOST%","local":"%UPSTREAM_LOCAL_ADDRESS%","sent":"%UPSTREAM_WIRE_BYTES_SENT%","received":"%UPSTREAM_WIRE_BYTES_RECEIVED%","duration_ms":"%DURATION%","connect_ms":"%COMMON_DURATION(US_CX_BEG:US_CX_END:ms)%","flags":"%RESPONSE_FLAGS%","close_type":"%UPSTREAM_DETECTED_CLOSE_TYPE%"}
  clusters:
    - name: validated_destination
      connect_timeout: 3s
      lb_policy: CLUSTER_PROVIDED
      cluster_type:
        name: envoy.clusters.original_dst
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.clusters.original_dst.v3.OriginalDstCluster
`
