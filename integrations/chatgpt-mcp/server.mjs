import { createServer } from "node:http";
import { registerAppTool } from "@modelcontextprotocol/ext-apps/server";
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { StreamableHTTPServerTransport } from "@modelcontextprotocol/sdk/server/streamableHttp.js";
import { z } from "zod";

const PORT = Number(process.env.PORT ?? 8787);
const HOST = process.env.HOST ?? "127.0.0.1";
const MCP_PATH = "/mcp";
const XYQ_BASE = (process.env.XYQ_OPENAPI_BASE ?? process.env.XYQ_BASE_URL ?? "https://xyq.jianying.com").replace(/\/$/, "");
const XYQ_ACCESS_KEY = process.env.XYQ_ACCESS_KEY ?? "";
const MAX_FILE_BYTES = 200 * 1024 * 1024;

const OPENAI_FILE_SCHEMA = z.object({
  download_url: z.string().url(),
  file_id: z.string().min(1),
  mime_type: z.string().optional(),
  file_name: z.string().optional(),
});

const SUBMIT_OUTPUT_SCHEMA = {
  thread_id: z.string(),
  run_id: z.string(),
  web_thread_link: z.string(),
  uploaded_asset_ids: z.array(z.string()),
};

const STATUS_OUTPUT_SCHEMA = {
  thread_id: z.string(),
  run_id: z.string(),
  status: z.enum(["queued", "in_progress", "completed", "failed", "cancelled", "unknown"]),
  state: z.number(),
  fail_reason: z.string(),
  entries: z.array(z.any()),
};

function requireAccessKey() {
  if (!XYQ_ACCESS_KEY) {
    throw new Error("XYQ_ACCESS_KEY is not configured on the MCP server.");
  }
}

async function xyqPost(path, body) {
  requireAccessKey();
  const response = await fetch(`${XYQ_BASE}${path}`, {
    method: "POST",
    headers: {
      Authorization: `Bearer ${XYQ_ACCESS_KEY}`,
      "Content-Type": "application/json",
    },
    body: JSON.stringify(body),
    signal: AbortSignal.timeout(30 * 60 * 1000),
  });

  const text = await response.text();
  if (!response.ok) {
    throw new Error(`小云雀 API HTTP ${response.status}: ${text.slice(0, 1000)}`);
  }

  let payload;
  try {
    payload = JSON.parse(text);
  } catch {
    throw new Error("小云雀 API returned a non-JSON response.");
  }

  if (payload?.ret !== "0") {
    throw new Error(`小云雀 API error ${payload?.ret ?? "unknown"}: ${payload?.errmsg ?? "unknown error"}`);
  }
  return payload?.data ?? {};
}

async function downloadChatGPTFile(file) {
  const url = new URL(file.download_url);
  if (url.protocol !== "https:") {
    throw new Error("Only HTTPS ChatGPT file download URLs are accepted.");
  }

  const response = await fetch(url, {
    redirect: "follow",
    signal: AbortSignal.timeout(5 * 60 * 1000),
  });
  if (!response.ok) {
    throw new Error(`Failed to download ChatGPT file ${file.file_id}: HTTP ${response.status}`);
  }

  const contentLength = Number(response.headers.get("content-length") ?? 0);
  if (contentLength > MAX_FILE_BYTES) {
    throw new Error(`File ${file.file_id} exceeds the 200 MB Xiaoyunque upload limit.`);
  }

  const bytes = new Uint8Array(await response.arrayBuffer());
  if (bytes.byteLength > MAX_FILE_BYTES) {
    throw new Error(`File ${file.file_id} exceeds the 200 MB Xiaoyunque upload limit.`);
  }

  const mimeType = file.mime_type || response.headers.get("content-type") || "application/octet-stream";
  const supported = mimeType.startsWith("image/") || mimeType.startsWith("video/") || mimeType === "audio/mpeg" || mimeType === "audio/wav" || mimeType === "audio/x-wav";
  if (!supported) {
    throw new Error(`Unsupported media type for ${file.file_id}: ${mimeType}`);
  }

  return {
    blob: new Blob([bytes], { type: mimeType }),
    fileName: file.file_name || `${file.file_id}.${mimeType.startsWith("image/") ? "png" : mimeType.startsWith("video/") ? "mp4" : "bin"}`,
  };
}

async function uploadToXiaoyunque(file) {
  requireAccessKey();
  const { blob, fileName } = await downloadChatGPTFile(file);
  const form = new FormData();
  form.append("accessKey", XYQ_ACCESS_KEY);
  form.append("file", blob, fileName);

  const response = await fetch(`${XYQ_BASE}/api/biz/v1/skill/upload_file`, {
    method: "POST",
    headers: {
      Authorization: `Bearer ${XYQ_ACCESS_KEY}`,
    },
    body: form,
    signal: AbortSignal.timeout(30 * 60 * 1000),
  });

  const text = await response.text();
  if (!response.ok) {
    throw new Error(`小云雀 upload HTTP ${response.status}: ${text.slice(0, 1000)}`);
  }

  let payload;
  try {
    payload = JSON.parse(text);
  } catch {
    throw new Error("小云雀 upload returned a non-JSON response.");
  }
  if (payload?.ret !== "0") {
    throw new Error(`小云雀 upload error ${payload?.ret ?? "unknown"}: ${payload?.errmsg ?? "unknown error"}`);
  }

  const assetId = payload?.data?.pippit_asset_id || payload?.data?.asset_id;
  if (!assetId) {
    throw new Error("小云雀 upload succeeded but did not return an asset id.");
  }
  return assetId;
}

async function uploadFiles(files = []) {
  const assetIds = [];
  for (const file of files) {
    assetIds.push(await uploadToXiaoyunque(file));
  }
  return assetIds;
}

async function submitVideo({ prompt, files = [], threadId = "" }) {
  const assetIds = await uploadFiles(files);
  const body = { message: prompt };
  if (threadId) body.thread_id = threadId;
  if (assetIds.length) body.asset_ids = assetIds;

  const data = await xyqPost("/api/biz/v1/skill/submit_run", body);
  const run = data?.run ?? {};
  const thread_id = run?.thread_id ?? "";
  const run_id = run?.run_id ?? "";
  if (!thread_id || !run_id) {
    throw new Error("小云雀 submit_run did not return thread_id/run_id.");
  }

  return {
    thread_id,
    run_id,
    web_thread_link: data?.web_thread_link ?? "",
    uploaded_asset_ids: assetIds,
  };
}

function mapRunState(state) {
  if (state === 3) return "completed";
  if (state === 4) return "failed";
  if (state === 5) return "cancelled";
  if (state === 0) return "queued";
  if (state === 1 || state === 2) return "in_progress";
  return "unknown";
}

function extractEntries(run) {
  return (run?.entry_list ?? []).map((entry) => {
    if (entry?.message) {
      return {
        id: entry.message.message_id ?? "",
        type: "message",
        role: entry.message.role ?? "",
        content: [...(entry.message.content ?? []), ...(entry.message.client_tool_calls ?? [])],
      };
    }
    if (entry?.artifact) {
      return {
        id: entry.artifact.artifact_id ?? "",
        type: "artifact",
        role: entry.artifact.role ?? "",
        content: entry.artifact.content ?? [],
      };
    }
    return { type: "unknown", raw: entry };
  });
}

async function getVideoStatus({ threadId, runId = "", afterSeq = 0 }) {
  const body = { thread_id: threadId, after_seq: afterSeq };
  if (runId) body.run_id = runId;
  const data = await xyqPost("/api/biz/v1/skill/get_thread", body);
  const runs = data?.thread?.run_list ?? [];
  const run = (runId && runs.find((item) => item?.run_id === runId)) || runs[0];
  if (!run) {
    throw new Error("小云雀 get_thread returned no run.");
  }

  const state = Number(run?.state ?? -1);
  return {
    thread_id: run?.thread_id ?? threadId,
    run_id: run?.run_id ?? runId,
    status: mapRunState(state),
    state,
    fail_reason: run?.fail_reason ?? "",
    entries: extractEntries(run),
  };
}

function createMcpServer() {
  const server = new McpServer(
    { name: "pippit-chatgpt", version: "0.1.0" },
    {
      instructions:
        "Use create_video when the user explicitly asks to create a Xiaoyunque/Pippit video. ChatGPT files can be passed as references. Video creation consumes the user's Xiaoyunque credits. Use get_video_status for progress/results, and continue_video for revisions in an existing thread.",
    },
  );

  registerAppTool(
    server,
    "create_video",
    {
      title: "Create Xiaoyunque video",
      description:
        "Create a new Xiaoyunque/Pippit video job from a user-approved prompt, optionally using images, videos, or audio from the current ChatGPT conversation as references. This action consumes Xiaoyunque credits.",
      inputSchema: {
        prompt: z.string().min(1),
        files: z.array(OPENAI_FILE_SCHEMA).max(12).optional(),
      },
      outputSchema: SUBMIT_OUTPUT_SCHEMA,
      annotations: {
        readOnlyHint: false,
        destructiveHint: false,
        openWorldHint: true,
      },
      _meta: {
        "openai/fileParams": ["files"],
        "openai/toolInvocation/invoking": "正在提交小云雀视频任务…",
        "openai/toolInvocation/invoked": "小云雀视频任务已提交",
      },
    },
    async ({ prompt, files }) => {
      const result = await submitVideo({ prompt, files: files ?? [] });
      return {
        structuredContent: result,
        content: [
          {
            type: "text",
            text: `已提交小云雀视频任务。thread_id=${result.thread_id}, run_id=${result.run_id}${result.web_thread_link ? `, ${result.web_thread_link}` : ""}`,
          },
        ],
      };
    },
  );

  registerAppTool(
    server,
    "continue_video",
    {
      title: "Revise Xiaoyunque video",
      description:
        "Continue an existing Xiaoyunque/Pippit creative thread with revision instructions, optionally adding new ChatGPT files as reference material. This action can consume Xiaoyunque credits.",
      inputSchema: {
        thread_id: z.string().min(1),
        prompt: z.string().min(1),
        files: z.array(OPENAI_FILE_SCHEMA).max(12).optional(),
      },
      outputSchema: SUBMIT_OUTPUT_SCHEMA,
      annotations: {
        readOnlyHint: false,
        destructiveHint: false,
        openWorldHint: true,
      },
      _meta: {
        "openai/fileParams": ["files"],
        "openai/toolInvocation/invoking": "正在向小云雀提交修改…",
        "openai/toolInvocation/invoked": "小云雀修改任务已提交",
      },
    },
    async ({ thread_id, prompt, files }) => {
      const result = await submitVideo({ prompt, files: files ?? [], threadId: thread_id });
      return {
        structuredContent: result,
        content: [
          {
            type: "text",
            text: `已在 thread ${result.thread_id} 提交修改。run_id=${result.run_id}${result.web_thread_link ? `, ${result.web_thread_link}` : ""}`,
          },
        ],
      };
    },
  );

  registerAppTool(
    server,
    "get_video_status",
    {
      title: "Get Xiaoyunque video status",
      description:
        "Check the progress and returned messages/artifacts for a Xiaoyunque/Pippit video run. Use thread_id and run_id returned by create_video or continue_video.",
      inputSchema: {
        thread_id: z.string().min(1),
        run_id: z.string().optional(),
        after_seq: z.number().int().min(0).optional(),
      },
      outputSchema: STATUS_OUTPUT_SCHEMA,
      annotations: {
        readOnlyHint: true,
        destructiveHint: false,
        openWorldHint: false,
      },
      _meta: {
        "openai/toolInvocation/invoking": "正在查询小云雀生成进度…",
        "openai/toolInvocation/invoked": "已获取小云雀生成进度",
      },
    },
    async ({ thread_id, run_id, after_seq }) => {
      const result = await getVideoStatus({
        threadId: thread_id,
        runId: run_id ?? "",
        afterSeq: after_seq ?? 0,
      });
      return {
        structuredContent: result,
        content: [
          {
            type: "text",
            text: `小云雀任务状态：${result.status}${result.fail_reason ? `；${result.fail_reason}` : ""}。`,
          },
        ],
      };
    },
  );

  return server;
}

const httpServer = createServer(async (req, res) => {
  if (!req.url) {
    res.writeHead(400).end("Missing URL");
    return;
  }

  const url = new URL(req.url, `http://${req.headers.host ?? "localhost"}`);

  if (req.method === "OPTIONS" && url.pathname === MCP_PATH) {
    res.writeHead(204, {
      "Access-Control-Allow-Origin": "*",
      "Access-Control-Allow-Methods": "POST, GET, DELETE, OPTIONS",
      "Access-Control-Allow-Headers": "content-type, mcp-session-id, authorization",
      "Access-Control-Expose-Headers": "Mcp-Session-Id",
    });
    res.end();
    return;
  }

  if (req.method === "GET" && url.pathname === "/") {
    res.writeHead(200, { "content-type": "text/plain; charset=utf-8" });
    res.end("Pippit / Xiaoyunque ChatGPT MCP server");
    return;
  }

  const MCP_METHODS = new Set(["POST", "GET", "DELETE"]);
  if (url.pathname === MCP_PATH && req.method && MCP_METHODS.has(req.method)) {
    res.setHeader("Access-Control-Allow-Origin", "*");
    res.setHeader("Access-Control-Expose-Headers", "Mcp-Session-Id");

    const server = createMcpServer();
    const transport = new StreamableHTTPServerTransport({
      sessionIdGenerator: undefined,
      enableJsonResponse: true,
    });

    res.on("close", () => {
      transport.close();
      server.close();
    });

    try {
      await server.connect(transport);
      await transport.handleRequest(req, res);
    } catch (error) {
      console.error("Error handling MCP request:", error);
      if (!res.headersSent) {
        res.writeHead(500).end("Internal server error");
      }
    }
    return;
  }

  res.writeHead(404).end("Not Found");
});

httpServer.listen(PORT, HOST, () => {
  console.log(`Pippit ChatGPT MCP listening on http://${HOST}:${PORT}${MCP_PATH}`);
});
