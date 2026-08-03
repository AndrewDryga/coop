# Transport bounds must not become task-completion limits

Bound retained state and individual untrusted payloads, not the cumulative volume of valid data that
is processed incrementally and discarded. A transport guard must not terminate legitimate long work
when cancellation, deadlines, per-frame limits, durable-output limits, and runtime isolation already
bound the actual resources.

If a host cannot retain an intermediate stream, compact, spill, aggregate, or discard that stream
while preserving the live task. Do not turn a host implementation detail into an arbitrary retry or
turn count that users must discover after their work fails.
