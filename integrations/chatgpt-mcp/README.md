# ChatGPT 私有 App / MCP Bridge（Go）

本集成使用仓库原生 Go 代码，把现有小云雀 / Pippit 能力暴露为 ChatGPT 可调用的 Streamable HTTP MCP 工具。

## 设计目标

第一版只验证最关键的闭环：

1. 在 ChatGPT 对话中讨论创意、脚本和分镜，并使用 ChatGPT 原生能力生成或修改参考图。
2. 用户明确要求制作视频后，ChatGPT 调用 `create_video`。
3. ChatGPT 把当前对话里的图片、视频或音频作为文件参数传给 MCP。
4. MCP 临时下载文件，调用仓库现有上传客户端写入小云雀资产库并取得 `asset_id`。
5. MCP 复用现有 `submit_run` / `get_thread` 实现提交、继续修改和查询结果。

实现位置：

```text
cmd/chatgpt_app/             Cobra 命令
internal/chatgpt_app/        MCP Server、ChatGPT 文件交接、小云雀编排
integrations/chatgpt-mcp/    本说明文档
```

运行时不依赖 Node.js。

## 为什么使用 Go

- 与主仓库保持单一技术栈和单一二进制。
- 直接复用 `pippit-tool-cli login` 写入系统 Keyring 的登录态；也兼容 `XYQ_ACCESS_KEY`。
- 直接复用仓库现有 HTTP 客户端、鉴权边界、multipart 上传、`SubmitRun` 和 `GetThread`。
- 继续沿用 GoReleaser、`go test ./...` 和 `go vet ./...`，不增加 Node 运行时与第二套锁文件。

## MCP 工具

### `create_video`

新建小云雀视频生成任务。

输入：

- `prompt`：视频创作要求。
- `files`：可选，ChatGPT 当前对话中的参考图片、视频或 mp3/wav 音频。

这是写操作，会上传素材并可能消耗小云雀 credits。

### `continue_video`

在已有小云雀 thread 中提交修改，可附加新的聊天文件。

### `get_video_status`

只读查询已有任务的可读进度和结果信息。

## ChatGPT 文件交接

`create_video` 和 `continue_video` 的工具元数据声明：

```text
_meta["openai/fileParams"] = ["files"]
```

`files` 中每个对象包含：

```json
{
  "download_url": "ChatGPT 提供的临时 HTTPS 地址",
  "file_id": "文件 ID",
  "mime_type": "image/png",
  "file_name": "reference.png"
}
```

服务只在当前调用中下载该临时文件，完成小云雀上传后立即删除本地临时副本，不保存临时 URL。下载器限制为 HTTPS、公开 IP、最多 5 次重定向和单文件 200 MB，以降低 SSRF 与超大文件风险。

## 本地启动

先使用现有 CLI 登录：

```bash
pippit-tool-cli login
pippit-tool-cli status
```

也可以在无 Keyring 的服务端环境中使用：

```bash
export XYQ_ACCESS_KEY="你的 Access Key"
```

启动原生 Go MCP Server：

```bash
pippit-tool-cli chatgpt-app serve
```

默认监听：

```text
http://127.0.0.1:8787/mcp
```

自定义监听地址与路径：

```bash
pippit-tool-cli chatgpt-app serve \
  --listen 127.0.0.1:8787 \
  --path /mcp
```

可选地为 MCP endpoint 设置独立 Bearer Token；该 token 不是小云雀 Access Key：

```bash
export PIPPIT_CHATGPT_MCP_TOKEN="生成一个足够长的随机值"
pippit-tool-cli chatgpt-app serve
```

健康检查：

```text
GET /healthz
```

## 暴露 HTTPS

ChatGPT 连接地址必须是公网可访问的 HTTPS endpoint。开发阶段可通过 Cloudflare Tunnel 或反向代理，把：

```text
https://your-domain.example/mcp
```

转发到：

```text
http://127.0.0.1:8787/mcp
```

不要把 `XYQ_ACCESS_KEY` 放入 ChatGPT 工具参数、前端代码、仓库或日志。公网部署还需要在 MCP 前增加合适的用户鉴权；当前静态 Bearer Token 仅用于私有 PoC。

## 端到端测试

先在 ChatGPT 中生成一张广告参考图，确认后说：

```text
就用刚才这张图，让小云雀制作一条 15 秒、9:16 的剧情广告。镜头先缓慢推进，再绕产品旋转，最后品牌定格。
```

预期流程：

```text
ChatGPT 文件参数
  -> create_video
  -> 小云雀 upload_file
  -> asset_id
  -> submit_run
  -> thread_id / run_id
```

随后询问：

```text
现在生成到哪一步了？
```

ChatGPT 应调用 `get_video_status`。继续修改时调用 `continue_video` 并复用原 `thread_id`。

## 当前边界

- 第一版专注通用视频创作，不混入短剧专用工作流。
- 提交和查询分开，避免一次 MCP 调用长期阻塞。
- 第一版没有自定义 ChatGPT UI，先验证纯聊天和文件交接。
- 多用户部署仍需 OAuth、用户与小云雀凭据映射、额度隔离和审计。
