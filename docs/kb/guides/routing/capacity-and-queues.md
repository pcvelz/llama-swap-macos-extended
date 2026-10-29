---
title: Routing capacity and request queues
summary: Configure concurrencyLimit and globalConcurrencyLimit, and understand queued work while a model is loading or busy.
category: guides
tags: [routing, queue, capacity, concurrency, concurrency-limit, max-concurrent-requests, global-concurrency-limit, rate-limit]
config_keys: [routing, models.*.concurrencyLimit, globalConcurrencyLimit]
updated: 2026-10-21
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

## What a booted stream looks like

With tiered entry points (`tiers:`), a higher-rank request that cannot be served
because of in-flight work boots a lower-rank request: the running request is
cancelled and the newcomer takes its place. On the Anthropic streaming path
(`/v1/messages` with `stream: true`) a booted request is replayed, and its client
never sees the cut, as long as it has read nothing that belongs to the message
itself. That means nothing but SSE comments, `ping` events and the
`message_start` event. An engine that speaks the Anthropic stream natively sends
the pings and the `message_start` when it admits a request, long before it
processes the prompt, and those frames do not make a stream live.

On a replay the new attempt's own `message_start` is dropped, so the client reads
one `message_start` per stream, and the terminal `error` event the proxy writes
into a cancelled stream is not sent. The message id the client keeps is the first
attempt's. Nothing later in the stream refers to it, so the replayed content
continues the message the client already holds; a second `message_start` would
name a message it never started, and stream parsers reject two of them. Only the
input token count differs, because the client keeps the first attempt's.

Once the first content frame has reached the client the stream is live: a
higher-rank request may still cut it, it is not replayed, and the stream ends
with an `error` event the client can retry on.

## When the engine still holds the request that was cut

Cancelling a request frees its place in the proxy at once, but not always in the
engine. llama-server frees the slot immediately, and where `/slots` is available
the proxy keeps the cut request's place until the slot reads idle. An engine with
a bounded queue and no slot endpoint may keep a cancelled request until it next
looks at the connection, seconds later, and answer every request in the meantime
with `503` and a `Retry-After`.

That refusal is the cut request's shadow, not an answer to the new request. The
proxy holds it back and sends the request again under the same grant, so a victim
queued for replay cannot be granted in between and race the arrival for the place
the engine is about to free. It applies only when a request on the same model was
cut within the last minute, the status is `503` or `529`, nothing of the attempt
has reached the client, and the request body can be sent again. It lasts at most a
minute per grant, follows the engine's `Retry-After` (between a quarter of a
second and two seconds) and logs `refusal-hold:` lines. Past the budget the next
answer goes to the client as the engine gives it; a `503` that follows no cut is
delivered at once.

A replayed stream's status line was committed long before, so a failure of the
replayed attempt is written as an SSE `error` event, never as the failure's body.

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
