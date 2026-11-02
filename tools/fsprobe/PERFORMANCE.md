# Filesystem admission performance

Measured on 2026-11-01: Apple M3 Pro, 36 GiB RAM, macOS 15.6.1/APFS,
OrbStack Docker 29.8.2, Linux arm64 7.2.12, cgroupfs/v2 and BPF LSM.
The baseline is `09be2a284ba3f898a5242440f0d90557f0508548`; the candidate is the
change containing this report. Images were already built. No host configuration
or packages changed. These are local measurements, not a cross-runtime guarantee.

## Reproduce

Use a locally built, trusted Coop clients image. The opt-in harness builds its own
inert APFS fixtures and removes its admissions and containers:

```sh
COOP_FILESYSTEM_BENCHMARK=1 \
COOP_FILESYSTEM_CLIENT_IMAGE=<cached-coop-clients-image> \
go test -tags filesystemruntimee2e ./internal/box \
  -run '^TestFilesystemPerformanceRuntime$' -count=5 -timeout 30m -v > filesystem-benchmark.log 2>&1
```

Each repeat runs 256 public files at concurrency 1, 4 and 8, then 10,000 public
files at concurrency 1. Each independent root also has 128 inert secret files
eight directories deep. Every admission remains attached through the workload:
ten stat/read passes over each public file; 256 write/rename/hardlink/read/unlink
sequences; and exact-case plus uppercase secret reads that must return EPERM.

Admission wall time includes qualification and preparation of the full concurrent
group. Workload timing includes guarded Docker creation and final revalidation;
the Python read/write timers below isolate operations inside the running box.
Revalidation is also timed once separately. Memory is sampled after all owners
are ready, before workloads. Observer containers and Docker stats are outside the
read/write timers. Teardown is measured per admission, concurrently for each group.

## Five-repeat medians

Admission wall time uses five group observations. Other timings pool per-admission
observations: 5, 20, 40 and 5 for the four rows, respectively.

| Public files | Concurrent admissions | Admission wall, before → after | Revalidate, before → after | Read, before → after | Write, before → after | Cleanup, before → after |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 256 | 1 | 1919 → 1383 ms | 3 → 2 ms | 70 → 68 ms | 164 → 169 ms | 6383 → 6223 ms |
| 256 | 4 | 2781 → 1845 ms | 5 → 3 ms | 119 → 100 ms | 278 → 277 ms | 2995 → 4741 ms |
| 256 | 8 | 6835 → 4975 ms | 12 → 8 ms | 190 → 158 ms | 555 → 442 ms | 2641 → 1486 ms |
| 10,000 | 1 | 2127 → 1416 ms | 71 → 38 ms | 3054 → 2890 ms | 174 → 172 ms | 6391 → 6091 ms |

Admission improved 27–34% across these fixtures. Read/write results are workload
measurements, not proof of a general I/O speedup. Cleanup remains variable:
four-way cleanup was slower in this run. No broad cleanup-speed claim follows.

The baseline had one failed cleanup among 70 admissions; all workload controls
passed. The candidate passed all 70 admissions, workloads and cleanups, including
five eight-way runs. The concurrent teardown investigation reproduced an unrelated
container disappearing between inventory and inspect. Retirement now retries only
after a fresh inventory proves a failed-batch ID vanished, and rescans the entire
inventory under one deadline. Uncertain or occupied parents still retain protection.

The baseline's failed owner remained attached during its final 10,000-file batch,
then was cooperatively retired after proving it had no Docker consumers. That last
batch was the fastest baseline 10,000-file admission, but its residual attachment
is a limitation of the five-sample comparison. The final run began with no retained
owners; every candidate batch cleaned up before the next began.

## Complete filtered command

Three alternating baseline/candidate pairs ran `coop run -- true` in the real
Coop checkout, with cached images and existing network approval. Every run exited 0.
The command includes project preparation, filtered networking, filesystem admission,
execution and teardown; it is a different fixture from the microbenchmarks above.

| Seconds | Baseline | Candidate |
| --- | ---: | ---: |
| Run 1 | 31.081 | 19.202 |
| Run 2 | 26.120 | 20.589 |
| Run 3 | 24.133 | 19.308 |
| Median | 26.120 | 19.308 |

The median decreased 26% on this checkout. Cold builds and provider startup
are excluded. Launch latency still includes substantial runtime setup and teardown.

## Memory per admission

| Measurement | Before | After |
| --- | ---: | ---: |
| Kernel map `fdinfo` memlock total | 11,941,488 bytes | 1,910,112 bytes |
| Object map capacity | 100,000 | 140 |
| Unicode map capacity | 20,000 | 15,446 |
| Retained object descriptors | 140 | 140 |
| Owner RSS, median across cases | 2,660–2,664 KiB | 2,660–2,666 KiB |
| Owner cgroup peak, case medians | 27.2–49.3 MiB | 12.4–12.7 MiB |

Kernel-reported map allocation fell 84%. `memlock` is the kernel-reported map
accounting figure, not process RSS or an independently measured physical-memory
total. RSS is essentially unchanged. Cgroup current/peak accounting and Docker
stats are retained separately; they must not be added to map accounting as though
they were disjoint. Current cgroup usage was about 0.9–1.4 MiB in both versions.

The object capacity comes from bounded root, unique path/ancestor and grant rows.
Unicode data is generated once during image build. Deduplicating ancestor opens
does not drop the 140 retained identities. All 27 enforcement hooks and final
policy/identity revalidation remain enabled. There is no authorization cache.

Raw phase, memory and workload JSON, image IDs, failure logs and review evidence
are retained in the task artifacts for
`2026-11-01-polish-and-accelerate-filesystem-protection`. The historical experimental
[probe results](RESULTS.md) describe a different, narrower prototype.
