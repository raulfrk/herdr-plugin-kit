import { describe, expect, test } from "bun:test";
import { deriveOMPStatusSocket } from "./omp-status-address";

describe("OMP status socket address", () => {
  test("matches the supported Herdr named-session vector", () => {
    const herdr = "/home/raul/.config/herdr/sessions/hpk-api5-prompt-live/herdr.sock";
    const derived = deriveOMPStatusSocket(herdr, "w1:p3");
    expect(derived).toBe("/home/raul/.config/herdr/sessions/hpk-api5-prompt-live/olFqKETBRT");
    expect(Buffer.byteLength(derived!)).toBe(Buffer.byteLength(herdr));
  });

  test("rejects identities that cannot yield a safe address", () => {
    expect(deriveOMPStatusSocket("relative/herdr.sock", "w1:p1")).toBeUndefined();
    expect(deriveOMPStatusSocket("/tmp/x", "w1:p1")).toBeUndefined();
    expect(deriveOMPStatusSocket("/tmp/herdr.sock", "")).toBeUndefined();
    expect(deriveOMPStatusSocket("/tmp/herdr.sock", "bad\0pane")).toBeUndefined();
    expect(deriveOMPStatusSocket("/tmp/herdr.sock", "p".repeat(257))).toBeUndefined();
  });
});
