import { describe, it, expect } from "vitest";
import { formatCountdown, getTTLLabel, groupModels, matchesCapabilities, modelServerPath, ZERO_TIME } from "./modelUtils";
import type { Model } from "./types";

function makeModel(overrides: Partial<Model> = {}): Model {
  return {
    id: "test-model",
    state: "ready",
    name: "Test Model",
    description: "",
    unlisted: false,
    peerID: "",
    ttl: 0,
    lastUse: "",
    pinned: false,
    ...overrides,
  };
}

describe("modelServerPath", () => {
  it("uses the ComfyUI endpoint for the reserved model", () => {
    expect(modelServerPath("comfyui_auto")).toBe("/comfyui/");
  });

  it("uses the encoded upstream endpoint for other models", () => {
    expect(modelServerPath("org/model name")).toBe("/upstream/org%2Fmodel%20name/");
  });
});

describe("matchesCapabilities", () => {
  it("returns true when required is empty", () => {
    const model = makeModel();
    expect(matchesCapabilities(model, [])).toBe(true);
  });

  it("returns false when model has no capabilities", () => {
    const model = makeModel();
    expect(matchesCapabilities(model, ["vision"])).toBe(false);
  });

  it("returns false when model has empty capabilities object", () => {
    const model = makeModel({ capabilities: {} });
    expect(matchesCapabilities(model, ["vision"])).toBe(false);
  });

  it("returns true when model has the single required capability", () => {
    const model = makeModel({ capabilities: { vision: true } });
    expect(matchesCapabilities(model, ["vision"])).toBe(true);
  });

  it("returns false when model lacks the required capability", () => {
    const model = makeModel({ capabilities: { vision: true } });
    expect(matchesCapabilities(model, ["audio_transcriptions"])).toBe(false);
  });

  it("AND semantics: returns true only when all required are present", () => {
    const model = makeModel({ capabilities: { vision: true, audio_transcriptions: true } });
    expect(matchesCapabilities(model, ["vision", "audio_transcriptions"])).toBe(true);
    expect(matchesCapabilities(model, ["vision", "reranker"])).toBe(false);
  });

  it("matchAny=true: returns true when at least one required is present", () => {
    const model = makeModel({ capabilities: { vision: true } });
    expect(matchesCapabilities(model, ["vision", "reranker"], true)).toBe(true);
    expect(matchesCapabilities(model, ["audio_transcriptions", "reranker"], true)).toBe(false);
  });

  it("matchAny=true with empty required returns true", () => {
    const model = makeModel();
    expect(matchesCapabilities(model, [], true)).toBe(true);
  });
});

describe("formatCountdown", () => {
  it("formats sub-minute durations", () => {
    expect(formatCountdown(7)).toBe("0:07");
  });

  it("formats minutes and seconds with zero-padding", () => {
    expect(formatCountdown(468)).toBe("7:48");
  });

  it("clamps negative durations to 0:00", () => {
    expect(formatCountdown(-5)).toBe("0:00");
  });
});

describe("getTTLLabel", () => {
  it("shows pinned regardless of ttl/lastUse", () => {
    const model = makeModel({ pinned: true, ttl: 600, lastUse: ZERO_TIME });
    expect(getTTLLabel(model)).toBe("pinned");
  });

  it("shows resident when ttl is 0", () => {
    const model = makeModel({ ttl: 0 });
    expect(getTTLLabel(model)).toBe("resident");
  });

  it("shows the full TTL when the model has never been used", () => {
    const model = makeModel({ ttl: 600, lastUse: ZERO_TIME });
    expect(getTTLLabel(model)).toBe("TTL 10:00");
  });

  it("shows a live countdown to eviction based on lastUse", () => {
    const now = Date.parse("2026-08-14T12:05:00Z");
    const model = makeModel({ ttl: 600, lastUse: "2026-08-14T12:00:00Z" });
    expect(getTTLLabel(model, now)).toBe("evicts in 5:00");
  });

  it("shows evicting once the countdown reaches zero", () => {
    const now = Date.parse("2026-08-14T12:10:00Z");
    const model = makeModel({ ttl: 600, lastUse: "2026-08-14T12:00:00Z" });
    expect(getTTLLabel(model, now)).toBe("evicting");
  });
});

describe("groupModels", () => {
  const models: Model[] = [
    makeModel({ id: "chat-model", capabilities: { vision: true } }),
    makeModel({ id: "audio-model", capabilities: { audio_transcriptions: true } }),
    makeModel({ id: "no-caps-model" }),
    makeModel({ id: "peer1/peer-model", peerID: "peer1" }),
    makeModel({ id: "unlisted-model", unlisted: true, capabilities: { vision: true } }),
  ];

  it("filters out unlisted models", () => {
    const result = groupModels(models);
    expect(result.localMatching.length + result.local.length).toBe(3);
    expect([...result.localMatching, ...result.local].every((m) => !m.unlisted)).toBe(true);
  });

  it("separates peer models into peers", () => {
    const result = groupModels(models);
    expect(result.peers).toHaveLength(1);
    expect(result.peers[0].id).toBe("peer1/peer-model");
  });

  it("without capabilities, all local models go to local (non-matching)", () => {
    const result = groupModels(models);
    expect(result.localMatching).toHaveLength(0);
    expect(result.local).toHaveLength(3);
  });

  it("with capabilities, matching models go to localMatching", () => {
    const result = groupModels(models, ["vision"]);
    expect(result.localMatching).toHaveLength(1);
    expect(result.localMatching[0].id).toBe("chat-model");
    expect(result.local).toHaveLength(2);
  });

  it("with capabilities, models without capabilities go to local", () => {
    const result = groupModels(models, ["vision"]);
    expect(result.local.find((m) => m.id === "no-caps-model")).toBeDefined();
  });

  it("with matchAny, matches models with any listed capability", () => {
    const result = groupModels(models, ["vision", "audio_transcriptions"], true);
    expect(result.localMatching).toHaveLength(2);
    expect(result.localMatching.map((m) => m.id)).toContain("chat-model");
    expect(result.localMatching.map((m) => m.id)).toContain("audio-model");
    expect(result.local).toHaveLength(1);
  });

  it("with empty capabilities array, all local go to local (non-matching)", () => {
    const result = groupModels(models, []);
    expect(result.localMatching).toHaveLength(0);
    expect(result.local).toHaveLength(3);
  });
});
