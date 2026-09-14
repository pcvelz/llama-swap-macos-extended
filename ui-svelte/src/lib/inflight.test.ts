import { describe, expect, it } from "vitest";
import { callerPurpose, formatBytes, liveElapsedMs, requestHeader, sessionID } from "./inflight";

describe("inflight helpers", () => {
  it("reads the proxy-sanitized caller purpose, not the raw header", () => {
    const headers = { "X-Caller-Purpose": "raw value" };
    expect(callerPurpose({ metadata: { purpose: "commit-subject" }, req_headers: headers })).toBe("commit-subject");
    expect(callerPurpose({ req_headers: headers })).toBe("");
  });

  it("looks up headers case-insensitively", () => {
    expect(requestHeader({ "User-Agent": "agent" }, "user-agent")).toBe("agent");
  });

  it("uses configured session header precedence", () => {
    const headers = { "X-Litellm-Session-Id": "second", "X-Session-Id": "first" };
    expect(sessionID(headers, ["x-session-id", "x-litellm-session-id"])).toBe("first");
  });

  it("formats byte counts", () => {
    expect(formatBytes(42)).toBe("42 B");
    expect(formatBytes(2048)).toBe("2.00 KB");
  });

  it("advances server elapsed time from a client-local receipt time", () => {
    expect(liveElapsedMs(250, 1_000, 2_500)).toBe(1_750);
  });
});
