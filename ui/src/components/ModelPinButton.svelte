<script lang="ts">
  import type { Model } from "../lib/types";
  import { handlePinToggle } from "../stores/modelLoad";
  import { Pin, PinOff } from "@lucide/svelte";

  interface Props {
    model: Model;
    /** "md" for list rows (size-7), "sm" for the detail header (size-5). */
    size?: "md" | "sm";
  }

  let { model, size = "md" }: Props = $props();

  let btnSize = $derived(size === "sm" ? "size-5 rounded-sm" : "size-7 rounded-md");
  let iconSize = $derived(size === "sm" ? "size-3.5" : "size-4");
</script>

<button
  type="button"
  class="text-muted-foreground hover:bg-accent hover:text-accent-foreground flex {btnSize} shrink-0 items-center justify-center"
  title={model.pinned ? "Unpin (re-enable idle eviction)" : "Pin (prevent idle eviction)"}
  aria-label={model.pinned ? "Unpin model" : "Pin model"}
  onclick={() => handlePinToggle(model)}
>
  {#if model.pinned}
    <PinOff class={iconSize} />
  {:else}
    <Pin class={iconSize} />
  {/if}
</button>
