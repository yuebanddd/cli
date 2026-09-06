# ChatGPT / MCP 接入

`pippit-tool-cli` 可以作为原生 Go MCP Server 运行，把小云雀 / Pippit 的图片、视频、短剧、视频处理和 Canvas 能力注册给 ChatGPT、Codex 与其他 MCP Host。

> 当前状态：实验性。服务端与 ChatGPT 文件参数协议已经实现；合并前仍需在真实 ChatGPT 会话中完成一次“聊天生图 → 小云雀图生视频”的端到端验证。

多用户 Public App 使用 `mcp serve --mode public`，采用独立 OIDC 服务账号和手动 Access Key 绑定，面向已有可用密钥的用户。部署、OAuth、PostgreSQL、临时缓存和验收门禁见 [Public MCP 部署文档](public-mcp-deployment.md)。下文 `mcp serve`、CLI 登录、静态 Bearer 与本地下载工具说明适用于 local 模式。

## 设计目标

ChatGPT 已经可以在聊天中直接生成和修改图片。本服务不重复调用 OpenAI 生图 API，而是负责后半段：

```text
ChatGPT 原生生图 / 用户上传图片
              ↓
用户确认使用该图片制作视频
              ↓
pippit_generate_video(images=[聊天中的图片])
              ↓
MCP Server 下载 ChatGPT 临时文件
              ↓
上传至小云雀并提交生成任务
              ↓
查询小云雀视频 URL 与 metadata（服务端不下载生成产物）
```

工具文件字段使用 ChatGPT 官方文件参数结构：

```json
{
  "download_url": "https://...",
  "file_id": "file_...",
  "mime_type": "image/png",
  "file_name": "reference.png"
}
```

`download_url` 和 `file_id` 必填，`mime_type` 与 `file_name` 可选。带文件的工具在 MCP Tool `_meta` 中声明 `openai/fileParams`。

## 本地启动

先完成小云雀网页登录。MCP Server 会复用 CLI 的 keyring 登录状态，不要求再次复制 Access Key：

```bash
pippit-tool-cli login
pippit-tool-cli status
```

从源码运行：

```bash
git switch feature/go-mcp-server
go mod tidy
go test ./...
go vet ./...
go run . mcp serve
```

从已安装的二进制运行：

```bash
pippit-tool-cli mcp serve
```

默认端点：

```text
MCP:    http://127.0.0.1:8787/mcp
Health: http://127.0.0.1:8787/healthz
```

自定义端口和路径：

```bash
pippit-tool-cli mcp serve \
  --listen 127.0.0.1:9000 \
  --path /mcp \
  --health-path /healthz
```

## 使用 MCP Inspector 验证

```bash
npx @modelcontextprotocol/inspector@latest
```

在 Inspector 中连接：

```text
http://127.0.0.1:8787/mcp
```

先验证：

1. `tools/list` 能发现全部工具。
2. `pippit_auth_status` 返回 `logged_in: true`，且不会返回 Access Key。
3. `pippit_get_thread` 等只读工具正常。
4. 使用一张测试图片调用 `pippit_generate_video`，然后调用 `pippit_query_result`。

## 私有 ChatGPT App：Secure MCP Tunnel

私有开发阶段建议使用 OpenAI Secure MCP Tunnel，不需要把本机 MCP Server 暴露到公网。

1. 在 OpenAI Platform 的 Tunnel 设置中创建 `tunnel_id`，并取得供 `tunnel-client` 使用的运行时 API Key。
2. 从 OpenAI Tunnel 设置页或 `openai/tunnel-client` 的最新 release 安装 `tunnel-client`。
3. 先运行：

```bash
tunnel-client help quickstart
```

4. 创建一个 HTTP MCP profile，把服务器地址设置为：

```text
http://127.0.0.1:8787/mcp
```

官方初始化流程要求提供 `tunnel_id` 与 `CONTROL_PLANE_API_KEY`。完成 profile 后执行：

```bash
tunnel-client doctor --profile pippit-local --explain
tunnel-client run --profile pippit-local
```

5. 在 ChatGPT 中打开 **Settings → Security and login → Developer mode**。
6. 打开 ChatGPT Plugins，新增开发者 App；Connection 选择 **Tunnel**，然后选择对应 tunnel 或填写 `tunnel_id`。
7. 检查 ChatGPT 扫描到的工具名称、Schema、Annotations 与文件参数。

Secure MCP Tunnel 仅适合私有连接和开发测试，不等同于公开插件发布。公开插件需要稳定的公网 HTTPS Streamable HTTP 端点与 OAuth 2.1 用户认证；mTLS 可额外验证客户端身份，不能替代用户 OAuth。

## ChatGPT 生图 → 小云雀视频测试

在已连接 MCP App 的新会话中测试：

```text
先生成一张竖屏香水广告主视觉：黑色背景、透明玻璃瓶、冷色轮廓光、商业摄影质感。
```

确认图片后继续：

```text
就用刚才这张图调用小云雀，制作 15 秒 9:16 广告视频。
镜头从远景缓慢推进，瓶身轻微旋转，液体高光流动，最后 3 秒停留品牌画面。
```

预期调用：

```text
pippit_generate_video
  images: [ChatGPT 生成的图片]
  prompt: 完整的视频要求
  duration_sec: 15
  ratio: 9:16
```

提交后，用返回的 `thread_id` 和 `run_id` 调用：

```text
pippit_query_result
```

继续修改时，可以调用：

```text
pippit_nest_submit
  thread_id: 原 thread_id
  message: 修改要求
```

## MCP 工具

### 登录与通用工作流

- `pippit_auth_status`
- `pippit_upload_media`
- `pippit_nest_submit`
- `pippit_get_thread`
- `pippit_list_thread_files`
- `pippit_download_result`

### 图片与视频生成

- `pippit_generate_image`
- `pippit_generate_video`
- `pippit_query_result`

ChatGPT 已有原生生图能力，所以普通聊天优先由 ChatGPT 生图；`pippit_generate_image` 保留用于 CLI 功能对齐和小云雀特定模型/编辑工作流。

### 短剧

- `pippit_short_drama_submit`
- `pippit_short_drama_upload`

### 视频处理

- `pippit_video_super_resolution`
- `pippit_erase_video_subtitle`

### Canvas

- `pippit_canvas_create`
- `pippit_canvas_allocate`
- `pippit_canvas_get`
- `pippit_canvas_apply`
- `pippit_canvas_upload`
- `pippit_canvas_command_list`
- `pippit_canvas_command_describe`
- `pippit_canvas_command_run`

`canvas command` 依赖 npm 安装包中的 Canvas SDK Runtime。通过源码运行 Go 服务时，机器上仍需存在可执行的 `pippit-tool-cli` npm wrapper；也可以通过 `--cli-command` 指定路径。

## 为什么不把 login / logout / update 注册为工具

这些是宿主控制面能力，不适合由会话模型自动调用：

- `login` 需要浏览器交互。
- `logout` 会撤销宿主凭据。
- `update` 会改变正在运行的服务程序。

MCP 只提供不会泄露凭据的 `pippit_auth_status`。登录、退出和升级仍由操作者在终端明确执行。

## 安全默认值

- 默认只监听 `127.0.0.1`。
- 非 loopback 监听必须配置 `--allowed-host` 和 `--auth-token`。
- 校验 `Host` 与浏览器 `Origin`。
- 文件默认只接受 HTTPS 下载 URL。
- 默认阻止 loopback、私网、链路本地、云元数据和文档保留 IP，防止 SSRF。
- 重定向的每一跳都会重新校验。
- 不使用系统代理下载 ChatGPT 临时文件。
- 单文件上限为 200 MiB。
- 下载产物只能写入服务器配置的 `output-dir`，禁止路径穿越。
- 生成、编辑、上传和 Canvas 修改工具都标记为写操作；会消耗 credits 的工具会在描述中明确说明。

本地调试时确需下载内网 URL，可显式设置：

```bash
pippit-tool-cli mcp serve --allow-private-file-urls
```

不要在公网部署中开启该选项。

## 公网部署

监听公网地址时必须至少配置：

```bash
export PIPPIT_MCP_AUTH_TOKEN="使用密码管理器生成的长随机值"

pippit-tool-cli mcp serve \
  --listen 0.0.0.0:8787 \
  --allowed-host mcp.example.com \
  --allowed-origin https://chatgpt.com
```

静态 Bearer Token 适合反向代理、内部 MCP Client 或额外保护层；它不是公开 ChatGPT 插件所需 OAuth 2.1 的替代品。公网端点还应位于 TLS 反向代理之后，并配置限流、审计、密钥轮换和进程守护。

## 环境变量

| 变量 | 含义 |
|---|---|
| `PIPPIT_MCP_LISTEN` | 监听地址 |
| `PIPPIT_MCP_PATH` | MCP 路径 |
| `PIPPIT_MCP_HEALTH_PATH` | 健康检查路径 |
| `PIPPIT_MCP_AUTH_TOKEN` | MCP Client Bearer Token |
| `PIPPIT_MCP_ALLOWED_HOSTS` | 逗号分隔的 Host allowlist |
| `PIPPIT_MCP_ALLOWED_ORIGINS` | 逗号分隔的 Origin allowlist |
| `PIPPIT_MCP_OUTPUT_DIR` | 产物下载根目录 |
| `PIPPIT_MCP_CLI_COMMAND` | Canvas SDK wrapper 命令 |
| `PIPPIT_MCP_ALLOW_PRIVATE_FILE_URLS` | 是否允许 HTTP / 私网文件 URL，默认 false |
| `PIPPIT_MCP_MAX_FILE_BYTES` | 单文件下载上限，最大 200 MiB |
| `PIPPIT_MCP_MAX_REQUEST_BODY_BYTES` | MCP JSON 请求体上限 |
| `XYQ_ACCESS_KEY` | 可选；服务器部署时覆盖 CLI keyring 登录状态 |

## 合并前检查清单

```bash
go mod tidy
go test ./...
go vet ./...
```

另外必须完成：

- MCP Inspector 工具枚举与调用测试。
- ChatGPT Developer Mode 连接测试。
- ChatGPT 生成图片文件参数扫描测试。
- 一次真实“聊天生图 → 小云雀图生视频 → 查询结果”的 E2E。
- macOS、Linux、Windows 的基础启动测试。
- 确认上传和生成工具会显示用户确认，并正确消耗小云雀 credits。
