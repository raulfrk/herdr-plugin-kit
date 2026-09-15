import { createHash } from "node:crypto";
import path from "node:path";

export function deriveOMPStatusSocket(herdrSocket: string, paneID: string): string | undefined {
  if (
    !path.isAbsolute(herdrSocket) ||
    path.normalize(herdrSocket) !== herdrSocket ||
    paneID === "" ||
    Buffer.byteLength(paneID) > 256 ||
    paneID.includes("\0")
  ) return;
  const basenameLength = Buffer.byteLength(path.basename(herdrSocket));
  if (basenameLength < 9) return;
  const digest = createHash("sha256").update(herdrSocket).update("\0").update(paneID).digest("base64url");
  return path.join(path.dirname(herdrSocket), "o" + digest.slice(0, Math.min(digest.length, basenameLength - 1)));
}

const derived = deriveOMPStatusSocket(process.env.HERDR_SOCKET_PATH ?? "", process.env.HERDR_PANE_ID ?? "");
if (derived) process.env.HERDR_PLUGIN_KIT_OMP_STATUS_SOCKET = derived;

// OMP requires an extension factory even though this companion's work must
// happen during module loading, before the status extension factory runs.
export default function () {}
