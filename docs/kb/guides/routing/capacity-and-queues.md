---
title: Routing capacity and request queues
summary: Configure concurrencyLimit and globalConcurrencyLimit, and understand queued work while a model is loading or busy.
category: guides
tags: [routing, queue, capacity, concurrency, concurrency-limit, max-concurrent-requests, global-concurrency-limit, rate-limit]
config_keys: [routing, models.*.concurrencyLimit, globalConcurrencyLimit]
updated: 2026-10-20
---

# Routing capacity and request queues

There is no `models.*.maxConcurrentRequests` setting. The real per-model key
is `models.*.concurrencyLimit`, which limits active parallel requests.

The router serializes model swaps and queues requests until the selected model
is ready. Avoid using many simultaneous client retries as a capacity control:
they create more queued work. Choose a routing policy that fits the models that
may coexist, then set client timeouts high enough for the queue and load time.

Inspect the Activity view and logs when latency grows. A queue that never drains
usually means the command, proxy, or health check is wrong; see
`guides/model-runtime/troubleshooting-model-wont-load`.

## Global concurrency limit

`globalConcurrencyLimit` is a top-level setting, separate from
`models.*.concurrencyLimit`. It caps the number of inference requests served
at once across every model combined, using a single shared semaphore:

```yaml
globalConcurrencyLimit: 8
```

The default is `0`, meaning no limit — the semaphore is not even added to the
request chain, so a default config pays no cost for the feature. Once the
limit is reached, further requests are rejected immediately with an HTTP 429
response rather than queued; there is no wait for a slot to free up. Clients
should treat a 429 as a signal to back off and retry, the same way they handle
a per-model concurrency limit rejection.

Use this to protect shared hardware (CPU, disk, network) from being
overwhelmed by traffic spread across many different models, which a per-model
`concurrencyLimit` cannot do since it only counts requests to one model at a
time.

## Per-session slot cooldown opt-out

A session that finished a request keeps its serving slot COOLING for the
model's `swapGraceSeconds`: another session's request is held while granting
it would displace that cooling slot (pool tokens or slot count). A caller can
opt its own session out per request with the header:

```
X-Slot-Cooldown: off
```

With the header, the session's slot does not cool after the request ends and a
waiting request may take it at once; KV pool admission is unchanged (the slot
stays displaceable KV). Without the header behaviour is exactly as before.
This is a generic per-session switch - the proxy does not know who sends it or
why; e.g. llama-cm sends it for dispatched subagent child sessions, which run
tools between turns and would otherwise hold their slot cooling while the GPU
sits idle. The same effect can be applied to an existing session via
`POST /api/sessions/{sessionId}/slot-cooldown` with body `{"enabled":false}`
(the full session id is required; `enabled:true` is a no-op).
