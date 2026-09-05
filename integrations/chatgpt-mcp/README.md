# ChatGPT 私有 App / MCP Bridge

这个目录提供一个面向 ChatGPT 的私有 MCP 服务，把当前仓库的小云雀 / Pippit 能力暴露为 ChatGPT 可调用工具。

## 目标

第一版只解决最关键的闭环：

1. 在 ChatGPT 对话中先用原生能力讨论创意、分镜、脚本并生成参考图。
2. 用户明确要求生成视频后，ChatGPT 调用 `create_video`。
3. ChatGPT 将当前对话里的图片/视频/音频文件作为工具文件参数传给 MCP。
4. MCP 下载这些临时文件，上传到小云雀，得到 `asset_id`。
5. MCP 调用 `/api/biz/v1/skill/submit_run` 提交生成任务。
6. ChatGPT 用 `get_video_status` 查询进度和产物；修改时使用 `continue_video` 复用原 thread。

仓库现有 `xyq-skill` 已经具备上传素材、提交 run 和查询 thread 的 OpenAPI 能力，本集成只是把它们改造成远程 MCP 形态，方便直接在 ChatGPT 聊天框里使用。

## 工具

### `create_video`

新建小云雀视频生成任务。

输入：

- `prompt`: 视频创作需求。
- `files`: 可选。ChatGPT 当前对话中的参考图片、视频或音频。

注意：这是写操作，会实际消耗小云雀 credits。

### `continue_video`

在已有小云雀 thread 中继续提出修改需求，可附加新的聊天文件。

### `get_video_status`

只读查询任务状态、消息和产物信息。

## ChatGPT 文件交接

工具 schema 使用 OpenAI 插件/MCP 的 `_meta["openai/fileParams"]` 声明 `files` 是文件参数。ChatGPT 调用工具时会提供临时下载 URL、file id、MIME type 等信息；服务端只在当前调用中下载后立即上传小云雀，不应把临时下载 URL 当作长期资源保存。

这是本分支最重要的 PoC：**ChatGPT 原生生成/上传的图片 → MCP → 小云雀 asset → 视频生成**。

## 本地启动

```bash
cd integrations/chatgpt-mcp
npm install

export XYQ_ACCESS_KEY="你的 Access Key"
# 可选
export XYQ_OPENAPI_BASE="https://xyq.jianying.com"

npm start
```

默认监听：

```text
http://127.0.0.1:8787/mcp
```

## 暴露 HTTPS

ChatGPT 连接的 MCP endpoint 需要公网可访问的 HTTPS 地址。开发阶段可以使用你自己的 Tunnel / 反向代理，例如将：

```text
https://your-domain.example/mcp
```

转发到：

```text
http://127.0.0.1:8787/mcp
```

Access Key 必须只存在服务端环境变量中，不要放到 ChatGPT tool 参数、前端代码、仓库或日志里。

## ChatGPT 中测试

开发/私有阶段，把 HTTPS MCP endpoint 连接到 ChatGPT 的开发者插件/私有插件配置中，然后测试：

```text
先帮我生成一张高级感香水广告参考图。
```

图片确认后：

```text
就用刚才这张图，让小云雀做成 15 秒 9:16 剧情广告。镜头先慢慢推进，再围绕瓶身旋转，最后品牌定格。
```

预期 ChatGPT 会把刚才的图片作为 `files` 参数交给 `create_video`。

随后可询问：

```text
现在生成到哪一步了？
```

ChatGPT 应调用 `get_video_status`。

继续修改：

```text
沿用这个版本，把前三秒节奏加快，结尾多停留两秒。
```

ChatGPT 应调用 `continue_video` 并复用原来的 `thread_id`。

## 当前边界

- 第一版专注通用视频创作，不把短剧专用工作流混进同一个工具。
- 不把小云雀 Key 暴露给 ChatGPT；服务端统一持有 `XYQ_ACCESS_KEY`。
- 文件上传沿用小云雀当前的 200 MB 单文件限制及支持的 image/video/mp3/wav 类型。
- 任务是异步的；提交和查询分成不同工具，避免长时间阻塞一次 ChatGPT 工具调用。
- 第一版没有自定义 UI，先验证纯聊天体验。需要任务卡片、进度条、视频预览时再增加 MCP App UI。
- 若未来给多个小云雀账户/用户使用，需要增加 OAuth/用户到 Access Key 的安全映射，不能继续共享单一服务端 Key。

## 下一阶段建议

PoC 验证后再增加：

- tool output 中抽取标准化视频 URL / 封面 / artifact metadata；
- ChatGPT 内的生成任务卡片和进度 UI；
- 用户级鉴权、多账号和额度隔离；
- 短剧专用 `create_short_drama` 工具；
- webhook/后台任务，减少聊天中手动查询进度；
- 部署配置和 CI。
