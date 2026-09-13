import { afterEach, describe, expect, test } from "bun:test";
import net from "node:net";
import path from "node:path";
import { existsSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import ompStatusExtension, { initialOMPState, reduceOMPState, type OMPAction, type OMPState } from "./omp-status";

const apply = (state: OMPState, ...actions: OMPAction[]) =>
  actions.reduce((current, action) => reduceOMPState(current, action), state);

const temporaryDirectories: string[] = [];

afterEach(() => {
  delete process.env.HERDR_PLUGIN_KIT_OMP_STATUS_SOCKET;
  for (const directory of temporaryDirectories.splice(0)) rmSync(directory, { recursive: true, force: true });
});

const connect = (socketPath: string) => new Promise<net.Socket>((resolve, reject) => {
  const connection = net.createConnection({ path: socketPath, allowHalfOpen: true });
  connection.once("connect", () => resolve(connection));
  connection.once("error", reject);
});

const readStatus = async (socketPath: string) => {
  // The extension exposes readiness only by binding the real Unix socket.
  let lastError: unknown;
  for (let attempt = 0; attempt < 50; attempt++) {
    try {
      const connection = await connect(socketPath);
      let payload = "";
      connection.on("data", (chunk) => { payload += chunk.toString(); });
      await new Promise<void>((resolve, reject) => {
        connection.once("end", resolve);
        connection.once("error", reject);
      });
      connection.destroy();
      return JSON.parse(payload);
    } catch (error) {
      lastError = error;
      await Bun.sleep(10);
    }
  }
  throw lastError;
};

describe("OMP native state reducer", () => {
  test("covers idle, working, nested approval/question blocking, and terminal settle", () => {
    let state = reduceOMPState(initialOMPState(), {
      type: "session",
      sessionPath: "/tmp/session.jsonl",
      agentActive: false,
    });
    expect({ status: state.status, seq: state.stateSeq }).toEqual({ status: "idle", seq: 1 });

    state = reduceOMPState(state, { type: "agent_start" });
    expect({ status: state.status, seq: state.stateSeq }).toEqual({ status: "working", seq: 2 });

    state = apply(state, { type: "block_start" }, { type: "block_start" });
    expect({ status: state.status, blocked: state.blockedCount, seq: state.stateSeq }).toEqual({
      status: "blocked",
      blocked: 2,
      seq: 3,
    });
    state = reduceOMPState(state, { type: "block_end" });
    expect({ status: state.status, blocked: state.blockedCount, seq: state.stateSeq }).toEqual({
      status: "blocked",
      blocked: 1,
      seq: 3,
    });
    state = reduceOMPState(state, { type: "block_end" });
    expect({ status: state.status, blocked: state.blockedCount, seq: state.stateSeq }).toEqual({
      status: "working",
      blocked: 0,
      seq: 4,
    });
    state = reduceOMPState(state, { type: "agent_end", willContinue: false });
    expect({ status: state.status, seq: state.stateSeq }).toEqual({ status: "idle", seq: 5 });
  });

  test("keeps nonterminal ends and retry sagas working until their single end", () => {
    let state = apply(
      initialOMPState(),
      { type: "session", sessionPath: "/tmp/session.jsonl", agentActive: true },
      { type: "agent_end", willContinue: true },
    );
    expect(state.status).toBe("working");
    expect(state.continuationPending).toBe(true);

    state = reduceOMPState(state, { type: "retry_start" });
    const firstRetrySeq = state.stateSeq;
    state = reduceOMPState(state, { type: "retry_start" });
    expect(state.stateSeq).toBe(firstRetrySeq);
    expect(state.retryActive).toBe(true);

    state = reduceOMPState(state, { type: "retry_end" });
    expect({ status: state.status, retry: state.retryActive, continuation: state.continuationPending }).toEqual({
      status: "idle",
      retry: false,
      continuation: false,
    });
  });

  test("handles compaction continuations and session branches without parsing text", () => {
    let state = apply(
      initialOMPState(),
      { type: "session", sessionPath: "/tmp/old.jsonl", agentActive: false },
      { type: "compaction_start" },
    );
    expect(state.status).toBe("working");

    state = reduceOMPState(state, { type: "compaction_end", willRetry: true });
    expect({ status: state.status, continuation: state.continuationPending }).toEqual({
      status: "working",
      continuation: true,
    });
    state = reduceOMPState(state, { type: "agent_start" });
    expect(state.continuationPending).toBe(false);
    state = reduceOMPState(state, { type: "agent_end", willContinue: false });
    expect(state.status).toBe("idle");

    const beforeBranch = state.stateSeq;
    state = reduceOMPState(state, { type: "session_change", sessionPath: "/tmp/new.jsonl" });
    expect({ path: state.sessionPath, status: state.status, seq: state.stateSeq }).toEqual({
      path: "/tmp/new.jsonl",
      status: "idle",
      seq: beforeBranch + 1,
    });

    state = apply(state, { type: "compaction_start" }, { type: "compaction_end", willRetry: false });
    expect({ status: state.status, continuation: state.continuationPending }).toEqual({
      status: "idle",
      continuation: false,
    });
  });
});

test("shipped handlers settle failed retries and bound socket connections", async () => {
  const directory = mkdtempSync(path.join(tmpdir(), "hpk-omp-extension-"));
  temporaryDirectories.push(directory);
  const socketPath = path.join(directory, "status.sock");
  process.env.HERDR_PLUGIN_KIT_OMP_STATUS_SOCKET = socketPath;
  const handlers = new Map<string, (event?: any, ctx?: any) => void>();
  ompStatusExtension({ on: (name: string, handler: (event?: any, ctx?: any) => void) => handlers.set(name, handler) });
  const sessionPath = path.join(directory, "session.jsonl");
  handlers.get("session_start")?.({}, {
    hasUI: true,
    isIdle: () => true,
    sessionManager: { getSessionFile: () => sessionPath },
  });
  expect((await readStatus(socketPath)).status).toBe("idle");

  handlers.get("auto_retry_start")?.();
  handlers.get("auto_retry_end")?.({ success: false });
  handlers.get("agent_end")?.({ willContinue: false });
  // Exercise the shipped 250 ms terminal debounce against the real socket;
  // fake timers do not drive Node's net-server event loop.
  await Bun.sleep(300);
  expect((await readStatus(socketPath)).status).toBe("idle");

  handlers.get("auto_retry_start")?.();
  handlers.get("auto_retry_end")?.({ success: false });
  expect((await readStatus(socketPath)).status).toBe("working");
  // The same real debounce must hold when retry is the only active lifecycle.
  await Bun.sleep(300);
  expect((await readStatus(socketPath)).status).toBe("idle");

  handlers.get("agent_start")?.();
  handlers.get("agent_end")?.({ willContinue: false });
  handlers.get("auto_compaction_start")?.();
  handlers.get("auto_compaction_end")?.({ willRetry: false });
  expect((await readStatus(socketPath)).status).toBe("idle");

  const heldOpen = await connect(socketPath);
  const remoteEnded = new Promise<void>((resolve) => heldOpen.once("end", resolve));
  heldOpen.resume();
  await remoteEnded;

  handlers.get("session_shutdown")?.();
  // The integration deliberately exercises real Unix-socket teardown; no
  // callback is exposed before the owned path is unlinked.
  for (let attempt = 0; attempt < 50 && existsSync(socketPath); attempt++) await Bun.sleep(10);
  expect(existsSync(socketPath)).toBe(false);
  heldOpen.destroy();
});
