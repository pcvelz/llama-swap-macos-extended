import type { Model } from "./types";

export interface GroupedModels {
  local: Model[];
  localMatching: Model[];
  peers: Model[];
}

export function modelServerPath(modelId: string): string {
  if (modelId === "comfyui_auto") return "/comfyui/";
  return `/upstream/${encodeURIComponent(modelId)}/`;
}

export function matchesCapabilities(model: Model, required: string[], matchAny = false): boolean {
  if (!required.length) return true;
  if (!model.capabilities) return false;
  const caps = model.capabilities as Record<string, boolean>;
  if (matchAny) {
    return required.some((cap) => caps[cap] === true);
  }
  return required.every((cap) => caps[cap] === true);
}

// Zero-time sentinel used by the backend when a model has never been used.
export const ZERO_TIME = "0001-01-01T00:00:00Z";

/** Format seconds as M:SS (e.g. 7:48). */
export function formatCountdown(seconds: number): string {
  const s = Math.max(0, Math.floor(seconds));
  const m = Math.floor(s / 60);
  const rem = s % 60;
  return `${m}:${rem.toString().padStart(2, "0")}`;
}

/**
 * Returns a human-readable TTL label for a running model.
 * - pinned -> "pinned"
 * - ttl == 0 -> "resident" (no idle eviction configured)
 * - never used (zero-time lastUse) -> "TTL N:SS"
 * - otherwise -> "evicts in M:SS" (or "evicting" when <= 0)
 *
 * `now` defaults to Date.now() but can be injected for deterministic tests.
 */
export function getTTLLabel(model: Model, now: number = Date.now()): string {
  if (model.pinned) return "pinned";
  if (!model.ttl) return "resident";
  if (!model.lastUse || model.lastUse === ZERO_TIME) {
    return `TTL ${formatCountdown(model.ttl)}`;
  }
  const lastUseMs = new Date(model.lastUse).getTime();
  const idleSec = (now - lastUseMs) / 1000;
  const remaining = model.ttl - idleSec;
  if (remaining <= 0) return "evicting";
  return `evicts in ${formatCountdown(remaining)}`;
}

export function groupModels(models: Model[], capabilities?: string[], matchAny = false): GroupedModels {
  const available = models.filter((m) => !m.unlisted);
  const local = available.filter((m) => !m.peerID);
  const peers = available.filter((m) => m.peerID);

  let localMatching: Model[] = [];
  let localRest: Model[] = [];

  if (capabilities && capabilities.length > 0) {
    for (const model of local) {
      if (matchesCapabilities(model, capabilities, matchAny)) {
        localMatching.push(model);
      } else {
        localRest.push(model);
      }
    }
  } else {
    localRest = local;
  }

  return { local: localRest, localMatching, peers };
}
