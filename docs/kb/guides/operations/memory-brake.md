---
title: Memory emergency brake (macOS)
summary: How the brake kills local models on file-backed memory growth, when it arms, and why reloads wait for the file cache to drain.
category: guides
tags: [memory, macos, kernel-panic, brake, file-cache, reload]
config_keys: [memoryBrake, memoryBrake.growthGB, memoryBrake.windowMinutes, memoryBrake.armAfterMinutes, memoryBrake.drainBelowGB, memoryBrake.holdMinutes]
updated: 2026-09-19
---

# Memory emergency brake (macOS)

On macOS, llama-swap samples file-backed memory once a second. If it grows by
`growthGB` above its minimum within any rolling `windowMinutes` window, on
`confirmSamples` consecutive samples, llama-swap SIGKILLs every local model.
That run-up came before the 2026-09-18 kernel panics. The block is on by
default, and llama-swap reads it only at startup.

```yaml
memoryBrake:
  growthGB: 3.5          # the only trigger
  windowMinutes: 5
  armAfterMinutes: 0     # armed as soon as the model is ready
  confirmSamples: 2
  drainBelowGB: 10       # reloads wait until file-backed is below this
```

## When it arms

The brake is off while a model is loading, because the GGUF read fills the
file cache. With `armAfterMinutes: 0` it arms as soon as the model is ready.
After a load, file-backed memory falls, and falling memory cannot trip a
growth-above-minimum rule. The old 10-minute warm-up only created a blind spot:
on 2026-09-19 a reload collapsed five minutes after ready, and the brake never
saw it.

## After a kill: evict, then wait for the drain

A killed server's weights stay in the file cache: 32-37 GB with nothing
loaded, draining on their own at only ~0.2 GB/min. After a kill, the brake:

1. waits for the killed processes to exit;
2. evicts every model file it has seen loaded (`-m`, `--mmproj`,
   `--model-draft`, plus all shards of a split GGUF) from the file cache. It
   logs `MEMORY BRAKE EVICT` with file-backed before and after;
3. holds every local-model load until file-backed has stayed below
   `drainBelowGB` for 60 s, re-evicting every 15 s until then.

A request that arrives during the hold is parked, not failed, and shows as
loading. The macOS menu shows `Memory brake: loads held until file-backed <
10 GB (now 36.1 GB)`, and `/api/events` carries the same state (`memoryBrake`:
`holding`, `fileBackedGB`, `drainBelowGB`). `drainBelowGB: 0` turns the hold
off. The eviction still runs.

## What goes wrong

- **The hold never opens.** Something other than the models is filling the
  file cache above `drainBelowGB`. Check the `MEMORY BRAKE HOLD` log lines,
  which repeat every 5 minutes. Then raise `drainBelowGB`, or free the cache
  (`sudo purge`).
- **The eviction freed nothing.** It skips models started with `-hf`, because
  their path is not on the command line. The log line names every file it
  could not evict.
- **`holdMinutes`** is obsolete. It still loads, is ignored, and triggers a
  startup warning. Remove it.
