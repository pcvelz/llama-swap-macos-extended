---
title: Observability, storage and Activity
summary: Use logs, metrics, captures and the Activity view to diagnose requests and retain useful history.
category: guides
tags: [operations, logs, metrics, activity, captures, debug, history]
config_keys: [logLevel, logToStdout, metricsMaxInMemory, captureBuffer, debugHistory, debugHistory.enabled, debugHistory.intervalMs, debugHistory.retainMinutes]
updated: 2026-09-19
---

# Observability, storage and Activity

Use the Activity view for recent request timing and model events, logs for
process and proxy failures, and metrics for trends. `metricsMaxInMemory` and
`captureBuffer` bound retained in-memory data; increase them only when the
memory cost is acceptable.

```yaml
logLevel: debug
logToStdout: true
metricsMaxInMemory: 1000
captureBuffer: 100
```

Do not put secrets in captures or debug logs. Reduce retention after diagnosing
an issue.

## Debug history

To answer "what happened in the last few minutes" in one request, turn on
`debugHistory` and query `GET /api/debug/history`:

```yaml
debugHistory:
  enabled: true
  intervalMs: 1000
  retainMinutes: 30
```

It returns one sample per interval (memory, resident model, queue, memory
brake, every session's phase and counters) plus events: finished requests with
status, duration and `cut=` reason, session phase changes and memory-brake
trips. Narrow it with `?minutes=10` and `?session=<id or prefix>`.

What goes wrong: the endpoint returns 404 until `enabled: true` is set and
llama-swap is restarted. `intervalMs` below 1000 is raised to 1000. Successful
GET requests are status polls and are not recorded as events. The history
lives in memory only and is lost on restart.
