"use strict";
// Private per-request bridge. Never spawn the local CLI or inspect its login.
const http = require("http");
const fs = require("fs");
const { runCanvasCommand } = require("./canvas-command.js");
const invocation = JSON.parse(fs.readFileSync(0, "utf8"));
const endpoint = new URL(invocation.url);
if (endpoint.protocol !== "http:" || endpoint.hostname !== "127.0.0.1" || endpoint.pathname !== "/rpc") throw new Error("invalid bridge");
const rpc = (op, data = {}) => new Promise((resolve, reject) => {
  const payload = JSON.stringify({ op, data });
  const req = http.request(endpoint, { method: "POST", headers: { "Authorization": `Bearer ${invocation.token}`, "Content-Type": "application/json", "Content-Length": Buffer.byteLength(payload) } }, res => {
    let text = "";
    res.setEncoding("utf8");
    res.on("data", chunk => { text += chunk; if (text.length > 2 * 1024 * 1024) req.destroy(new Error("oversized response")); });
    res.on("end", () => { try { if (res.statusCode !== 200) throw new Error("tenant bridge rejected operation"); resolve(JSON.parse(text)); } catch (e) { reject(e); } });
    res.on("error", reject);
  });
  req.setTimeout(120000, () => req.destroy(new Error("bridge timeout")));
  req.on("error", reject); req.end(payload);
});
// The SDK must use the Go API bridge; generated image/video bytes stay upstream.
globalThis.fetch = async () => { throw new Error("Public Canvas does not fetch external media; use upstream asset metadata"); };
let tail = Promise.resolve();
const load = () => rpc("state.read");
const read = selector => { const work = tail.then(async () => selector(await load())); tail = work.catch(() => {}); return work; };
const update = mutate => { const work = tail.then(async () => { const state = await load(); state.clientId ??= null; state.snapshot ??= null; state.outbound ??= []; state.checkpoints ??= []; mutate(state); await rpc("state.write", { state }); }); tail = work.catch(() => {}); return work; };
const persistence = {
  appendOutbound: envelope => update(s => s.outbound.push(envelope)),
  clear: () => update(s => { s.clientId = null; s.snapshot = null; s.outbound = []; }),
  loadClientId: () => read(s => s.clientId ?? null), loadOutbound: () => read(s => s.outbound ?? []), loadSnapshot: () => read(s => s.snapshot ?? null),
  removeOutbound: txId => update(s => { s.outbound = s.outbound.filter(e => e.txId !== txId); }),
  replaceOutbound: envelopes => update(s => { s.outbound = envelopes; }),
  replaceOutboundPartition: (active, captured) => update(s => { const replaced = new Set([...captured, ...active.map(e => e.txId)]); s.outbound = [...active, ...s.outbound.filter(e => !replaced.has(e.txId))]; }),
  saveClientId: id => update(s => { s.clientId = id; }), saveSnapshot: snapshot => update(s => { s.snapshot = snapshot; }),
  quarantine: () => update(s => { s.quarantined = s.outbound; s.outbound = []; }).then(() => "PostgreSQL")
};
const checkpoints = {
  create: checkpoint => update(s => { s.checkpoints = s.checkpoints.filter(c => c.checkpointId !== checkpoint.checkpointId); s.checkpoints.push(checkpoint); }),
  get: id => read(s => (s.checkpoints ?? []).find(c => c.checkpointId === id)),
  list: canvasId => read(s => (s.checkpoints ?? []).filter(c => c.canvasId === canvasId))
};
const nativeClient = {
  async ensureCanvasAccess(id) { await this.getAssets([id]); return { logged_in: true, credential_scope: "request-tenant" }; },
  allocateAssetIds: count => rpc("allocate", { count }).then(r => r.asset_ids),
  apply: request => rpc("apply", request),
  getAssets: ids => rpc("get", { asset_ids: ids }).then(r => r.assets),
  getAssetsAllowMissing: ids => rpc("get", { asset_ids: ids }).then(r => r.assets)
};
runCanvasCommand(invocation.args, { nativeClient, persistence, checkpoints }).then(code => { process.exitCode = code; }, () => { process.exitCode = 1; });
