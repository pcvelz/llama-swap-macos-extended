---
title: Reclaiming slots from stuck requests
summary: How peerStall, slotStall and prefillStall free a serving slot held by a frozen client, a wedged stream or a prefill that stopped moving.
category: guides
tags: [stall, reclaim, slots, prefill, llama-server]
config_keys: [peerStall, slotStall, prefillStall]
updated: 2026-09-25
---
# Reclaiming slots from stuck requests

llama-swap holds a request open as long as it takes. Slow is not stuck, and none
of these guards uses a tokens-per-second threshold. Each one watches a different
counter and acts only when that counter stops moving completely. Every verdict
takes the same path: the client gets an Anthropic SSE `event: error` it can retry
on, the request is cancelled the way a client disconnect is, the access log line
carries `cut=<verdict>`, and one line goes to the log.

| Guard | Watches | Verdict |
| --- | --- | --- |
| `peerStall` | a write to the client that cannot complete | `peer-stalled` |
| `slotStall` | upstream body bytes after the first byte | `slot-stalled` |
| `prefillStall` | the upstream prefill counter before the first token | `prefill-stalled` |

## Why prefillStall exists

llama-server sends an SSE comment (`:`) every 30 seconds (`--sse-ping-interval`)
from the moment it launches a task. A prompt that stops being processed therefore
still looks like a live byte stream, and no byte counter can see it.
`prefillStall` reads the upstream's own counter instead: `/slots`
`n_prompt_tokens_processed` for the same slot and task.

- **Flat, never slow.** The same task has to show the same processed count in
  successful polls `timeoutSeconds` apart (default 600). The counter moves once
  per batch, so a slow prefill of any size keeps moving and is never cut.
- **A failed poll is no evidence.** A busy child may not answer `/slots` in time.
  That tells the guard nothing, so it waits.
- **Who is cut.** The model's requests that have produced no token and were
  granted before the counter went flat. While another slot of the same model is
  still advancing a prefill, the cut waits, because a candidate could be that
  slot's healthy owner.
- **Freeing the slot.** Cancelling closes the upstream connection. llama-server
  checks the client connection every second while it waits for results, and
  when it finds the connection closed it cancels the task. If the same task is
  still flat `restartAfterSeconds` later (default 120), the model is unloaded and
  reloads on the next request. `/slots/<id>?action=erase` cannot clear a slot
  that is still processing.

```yaml
prefillStall:
  enabled: true
  timeoutSeconds: 600
  restartAfterSeconds: 120
```

The poller only runs while a request holds a slot on that model, so an idle
model is never polled.
