import net from "node:net";
import path from "node:path";
import { existsSync, lstatSync, chmodSync, unlinkSync } from "node:fs";

export type OMPStatus = "working" | "blocked" | "idle";
export type OMPState = {
  sessionPath?: string;
  blockedCount: number;
  agentActive: boolean;
  retryActive: boolean;
  compactionActive: boolean;
  continuationPending: boolean;
  status: OMPStatus;
  stateSeq: number;
};

export type OMPAction =
  | { type: "session"; sessionPath?: string; agentActive: boolean }
  | { type: "session_change"; sessionPath?: string }
  | { type: "agent_start" }
  | { type: "agent_end"; willContinue: boolean }
  | { type: "block_start" }
  | { type: "block_end" }
  | { type: "retry_start" }
  | { type: "retry_end" }
  | { type: "compaction_start" }
  | { type: "compaction_end"; willRetry: boolean };

export const initialOMPState = (): OMPState => ({
  blockedCount: 0,
  agentActive: false,
  retryActive: false,
  compactionActive: false,
  continuationPending: false,
  status: "idle",
  stateSeq: 0,
});

const semanticStatus = (state: OMPState): OMPStatus => {
  if (state.blockedCount > 0) return "blocked";
  if (state.agentActive || state.retryActive || state.compactionActive || state.continuationPending) return "working";
  return "idle";
};

export function reduceOMPState(previous: OMPState, action: OMPAction): OMPState {
  const next = { ...previous };
  let forceSequence = false;
  switch (action.type) {
    case "session":
      next.sessionPath = action.sessionPath;
      next.blockedCount = 0;
      next.agentActive = action.agentActive;
      next.retryActive = false;
      next.compactionActive = false;
      next.continuationPending = false;
      forceSequence = true;
      break;
    case "session_change":
      next.sessionPath = action.sessionPath;
      next.blockedCount = 0;
      next.agentActive = false;
      next.retryActive = false;
      next.compactionActive = false;
      next.continuationPending = false;
      forceSequence = true;
      break;
    case "agent_start":
      next.agentActive = true;
      next.continuationPending = false;
      break;
    case "agent_end":
      next.agentActive = false;
      next.continuationPending = action.willContinue;
      break;
    case "block_start":
      next.blockedCount++;
      break;
    case "block_end":
      next.blockedCount = Math.max(0, next.blockedCount - 1);
      break;
    case "retry_start":
      next.retryActive = true;
      break;
    case "retry_end":
      next.retryActive = false;
      next.continuationPending = false;
      break;
    case "compaction_start":
      next.compactionActive = true;
      break;
    case "compaction_end":
      next.compactionActive = false;
      next.continuationPending = action.willRetry;
      break;
  }
  next.status = semanticStatus(next);
  if (forceSequence || next.sessionPath !== previous.sessionPath || next.status !== previous.status) {
    next.stateSeq = previous.stateSeq + 1;
  }
  return next;
}


export default function (pi: any) {
  const configuredSocketPath = process.env.HERDR_PLUGIN_KIT_OMP_STATUS_SOCKET;
  if (typeof configuredSocketPath !== "string" || !path.isAbsolute(configuredSocketPath)) return;
  const socketPath = configuredSocketPath;

  let rootSession = false;
  let state = initialOMPState();
  let publishedStatus = state.status;
  let publishedSeq = state.stateSeq;
  let idleTimer: NodeJS.Timeout | undefined;
  let server: net.Server | undefined;
  let socketIdentity: { dev: bigint; ino: bigint } | undefined;
  const connections = new Set<net.Socket>();
  const approvals = new Set<string>();
  const questions = new Set<string>();

  const clearIdleTimer = () => {
    if (idleTimer) clearTimeout(idleTimer);
    idleTimer = undefined;
  };
  const sessionPath = (ctx: any): string | undefined => {
    const value = ctx?.sessionManager?.getSessionFile?.();
    return typeof value === "string" && path.isAbsolute(value) ? value : undefined;
  };
  const publish = () => {
    publishedStatus = state.status;
    publishedSeq = state.stateSeq;
  };
  const apply = (action: OMPAction, debounceIdle = false) => {
    state = reduceOMPState(state, action);
    if (debounceIdle && state.status === "idle") {
      idleTimer = setTimeout(() => {
        idleTimer = undefined;
        publish();
      }, 250);
      idleTimer.unref?.();
      return;
    }
    publish();
  };
  const removeOwnedSocket = () => {
    if (!socketIdentity || !existsSync(socketPath)) return;
    try {
      const current = lstatSync(socketPath, { bigint: true });
      if (current.dev === BigInt(socketIdentity.dev) && current.ino === BigInt(socketIdentity.ino)) unlinkSync(socketPath);
    } catch {}
  };
  const closeServer = () => {
    const owned = server;
    server = undefined;
    for (const connection of connections) connection.destroy();
    connections.clear();
    if (owned) owned.close(removeOwnedSocket);
    else removeOwnedSocket();
  };
  const bind = () => {
    if (server || existsSync(socketPath) || !existsSync(path.dirname(socketPath))) return;
    const candidate = net.createServer((connection) => {
      if (connections.size >= 16) {
        connection.destroy();
        return;
      }
      connections.add(connection);
      connection.on("close", () => connections.delete(connection));
      connection.on("error", () => {});
      connection.setTimeout(1000, () => connection.destroy());
      if (!state.sessionPath || !path.isAbsolute(state.sessionPath)) {
        connection.end(() => connection.destroy());
        return;
      }
      connection.end(JSON.stringify({
        version: 1,
        harness: "omp",
        session_kind: "path",
        session_value: state.sessionPath,
        status: publishedStatus,
        state_seq: publishedSeq,
      }) + "\n", () => connection.destroy());
    });
    candidate.on("error", () => {
      if (server === candidate) server = undefined;
      try { candidate.close(); } catch {}
    });
    candidate.listen(socketPath, () => {
      if (server !== candidate) return;
      try {
        chmodSync(socketPath, 0o600);
        const info = lstatSync(socketPath, { bigint: true });
        socketIdentity = { dev: info.dev, ino: info.ino };
      } catch {
        closeServer();
      }
    });
    server = candidate;
  };
  const resetSession = (ctx: any, initial: boolean) => {
    clearIdleTimer();
    approvals.clear();
    questions.clear();
    apply(initial
      ? { type: "session", sessionPath: sessionPath(ctx), agentActive: ctx?.isIdle?.() === false }
      : { type: "session_change", sessionPath: sessionPath(ctx) });
  };
  const terminalEnd = (willContinue: boolean) => {
    clearIdleTimer();
    apply({ type: "agent_end", willContinue }, !willContinue);
  };
  const blockerID = (event: any): string | undefined =>
    typeof event?.toolCallId === "string" && event.toolCallId !== "" ? event.toolCallId : undefined;

  pi.on("session_start", (_event: any, ctx: any) => {
    if (ctx?.hasUI !== true) return;
    rootSession = true;
    resetSession(ctx, true);
    bind();
  });
  pi.on("session_switch", (_event: any, ctx: any) => { if (rootSession) resetSession(ctx, false); });
  pi.on("session_branch", (_event: any, ctx: any) => { if (rootSession) resetSession(ctx, false); });
  pi.on("agent_start", () => { if (rootSession) { clearIdleTimer(); apply({ type: "agent_start" }); } });
  pi.on("agent_end", (event: any) => { if (rootSession) terminalEnd(event?.willContinue === true); });
  pi.on("tool_approval_requested", (event: any) => {
    if (!rootSession) return;
    const id = blockerID(event);
    if (id && !approvals.has(id)) { approvals.add(id); clearIdleTimer(); apply({ type: "block_start" }); }
  });
  pi.on("tool_approval_resolved", (event: any) => {
    if (!rootSession) return;
    const id = blockerID(event);
    if (id && approvals.delete(id)) apply({ type: "block_end" });
  });
  pi.on("tool_execution_start", (event: any) => {
    if (!rootSession || event?.toolName !== "ask") return;
    const id = blockerID(event);
    if (id && !questions.has(id)) { questions.add(id); clearIdleTimer(); apply({ type: "block_start" }); }
  });
  pi.on("tool_execution_end", (event: any) => {
    if (!rootSession || event?.toolName !== "ask") return;
    const id = blockerID(event);
    if (id && questions.delete(id)) apply({ type: "block_end" });
  });
  pi.on("auto_retry_start", () => { if (rootSession) { clearIdleTimer(); apply({ type: "retry_start" }); } });
  pi.on("auto_retry_end", (event: any) => {
    if (!rootSession) return;
    clearIdleTimer();
    apply({ type: "retry_end" }, event?.success === false);
  });
  pi.on("auto_compaction_start", () => { if (rootSession) { clearIdleTimer(); apply({ type: "compaction_start" }); } });
  pi.on("auto_compaction_end", (event: any) => { if (rootSession) { clearIdleTimer(); apply({ type: "compaction_end", willRetry: event?.willRetry === true }); } });
  pi.on("session_shutdown", () => { clearIdleTimer(); closeServer(); });
}
