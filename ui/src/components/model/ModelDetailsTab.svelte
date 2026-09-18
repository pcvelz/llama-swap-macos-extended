<script lang="ts">
  import type { Model } from "../../lib/types";
  import { capabilityLabels } from "../../lib/capabilities";
  import { getTTLLabel } from "../../lib/modelUtils";
  import * as Card from "$lib/components/ui/card/index.js";
  import Tag from "../Tag.svelte";

  interface Props {
    model: Model;
  }

  let { model }: Props = $props();

  let capabilities = $derived.by(() => {
    const caps = model?.capabilities ?? {};
    return Object.entries(caps).filter(([, v]) => v);
  });

  // Reactive clock -- ticks every second for the idle-TTL countdown.
  let now = $state(Date.now());
  $effect(() => {
    const id = setInterval(() => { now = Date.now(); }, 1000);
    return () => clearInterval(id);
  });
</script>

<div class="flex flex-col gap-4">
  {#if !model.peerID}
    <Card.Root class="shrink-0 gap-0 overflow-hidden py-0">
      <Card.Header class="border-b px-4 py-2">
        <Card.Title class="text-sm font-semibold">Lifecycle</Card.Title>
      </Card.Header>
      <Card.Content class="flex flex-wrap items-center gap-x-6 gap-y-1 p-3 text-sm">
        <span>
          <span class="text-muted-foreground">TTL:</span>
          {model.ttl > 0 ? `${model.ttl}s` : "none (resident)"}
        </span>
        {#if model.state !== "stopped"}
          <span>
            <span class="text-muted-foreground">Status:</span>
            {getTTLLabel(model, now)}
          </span>
        {/if}
        {#if model.lastUse}
          <span>
            <span class="text-muted-foreground">Last use:</span>
            {model.lastUse}
          </span>
        {/if}
      </Card.Content>
    </Card.Root>
  {/if}

  <Card.Root class="shrink-0 gap-0 overflow-hidden py-0">
    <Card.Header class="border-b px-4 py-2">
      <Card.Title class="text-sm font-semibold">Capabilities</Card.Title>
    </Card.Header>
    <Card.Content class="p-3">
      {#if capabilities.length === 0}
        <span class="text-muted-foreground text-sm">No capabilities reported.</span>
      {:else}
        <div class="flex flex-wrap gap-1.5">
          {#each capabilities as [key] (key)}
            <Tag>{capabilityLabels[key] ?? key}</Tag>
          {/each}
        </div>
      {/if}
    </Card.Content>
  </Card.Root>
</div>
