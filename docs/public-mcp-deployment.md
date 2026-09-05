# Public ChatGPT MCP 部署与验收

Public 模式入口为 `pippit-tool-cli mcp serve --mode public`。默认 `mcp serve` 仍是单用户 local 模式，复用 CLI 登录；Public 模式不读取 `XYQ_ACCESS_KEY`、keyring 或本地 Canvas 状态。两种模式应使用独立进程和端口。

## 认证与数据边界

ChatGPT 使用预注册的公开 OAuth 客户端，执行 authorization code + PKCE S256；scope 为 `xiaoyunque:tools`，resource 为 `https://你的域名/mcp`。授权服务器与受保护资源 metadata 均公开，匿名 MCP 返回 401 和 `WWW-Authenticate` discovery URL。metadata 声明 `none` 客户端认证，不需要 client secret，不支持 password/client-credentials grant。按 [OpenAI 官方认证文档](https://developers.openai.com/plugins/build/auth) 在 App 管理页选择预定义客户端，并将页面显示的**完整回调 URL**加入 allowlist；不要自行假定回调路径。授权成功和用户取消均返回原 state 和 issuer。

设计流程是用户在本服务授权页跳转小云雀官网登录，完成后回到原页批准 ChatGPT。网页登录回调绑定一次性随机 secret、device、callback 和浏览器 flow，并独立向小云雀 `/api/biz/v1/user/info` 验证 Access Key 所属 UID。未通过验证时拒绝绑定，不能信任浏览器提交的 UID。**2026-09-05 实测该身份端点返回业务码 1015，且当前小云雀网页登录代码仅接受 loopback HTTP callback；Public HTTPS 绑定尚不能工作。** 必须由上游提供支持 Public callback 和可信身份校验的接口后再次验收，不能通过放松 UID 校验发布。证据见验收记录。

PostgreSQL 保存用户、已验证上游身份、账号、OAuth flow/code/token family、凭据密文、资源归属、job metadata、Canvas 状态、限流桶与审计。Access/refresh token、浏览器 session 与 binding secret 只保存 SHA-256 摘要。凭据使用每账号随机 AES-256-GCM data key 加密，data key 再用 master key 包装，账号 ID 作为 AAD，防止密文跨账号搬移。数据库备份与 master key 必须分别保管。

Access token 默认 1 小时；refresh family 默认 30 天且不滑动延长，每次 refresh 轮换，旧 refresh 重放会撤销整族 token。每个 MCP 请求解析 OAuth 用户，每次上游操作重新检查 family、当前账号和凭据。所有输入的 thread/run/asset/project 标识检查当前用户与账号归属，run 同时检查所属 thread；猜测 ID 返回 `resource_not_found`。HTTP MCP 使用 stateless transport，无跨用户 MCP session。

浏览器退出会销毁当前 session 和该浏览器未完成的授权 flow，保留 ChatGPT 连接。解除绑定或 OAuth revoke 会删除凭据、撤销该用户所有 OAuth family、session、binding 和授权 code；已提交到小云雀的任务无法撤回。`POST /account/delete` 使用同一授权 flow 的 CSRF 字段以及 `confirm=delete` 删除本服务用户和关联数据，不删除小云雀上的作品。

## 容器部署

需要 Docker Compose、可解析至此主机的稳定公网域名、开放 80/443 端口。镜像包含 Go 服务与已校验发布源中的 Canvas runtime，Node 不会获得数据库 URL、主密钥或用户凭据。Compose 不向主机开放数据库和应用端口，只通过 Caddy 提供 TLS。

在仓库根目录执行。以下变量由部署用 secret manager 注入；不要把真实值提交到仓库、PR、命令日志或聊天中：

```bash
export PUBLIC_HOST=mcp.example.com
export POSTGRES_PASSWORD="$(openssl rand -hex 32)"
export PIPPIT_CREDENTIAL_MASTER_KEY="$(openssl rand -base64 32)"
export PIPPIT_PUBLIC_CLIENTS_JSON='{"chatgpt-production":["https://chatgpt.com/connector_platform_oauth_redirect"]}'
docker compose -f deploy/public/compose.yaml build app
docker compose -f deploy/public/compose.yaml up -d postgres
docker compose -f deploy/public/compose.yaml run --rm app mcp migrate up
docker compose -f deploy/public/compose.yaml up -d app proxy
curl --fail "https://${PUBLIC_HOST}/readyz"
curl --fail "https://${PUBLIC_HOST}/.well-known/oauth-authorization-server"
```

客户端 JSON 示例中的回调必须替换为 App 管理页的实际值。生成的数据库密码只包含十六进制字符，可以安全放入示例 DSN；使用其他密码时须做 URL 编码。生产必须持久保管上述 secret，后续部署使用原值。示例数据库在隔离容器网络使用 `sslmode=disable`；托管或跨主机 PostgreSQL 必须使用 `sslmode=verify-full` 和受信 CA。示例网段冲突时，同时修改 Compose 的子网、proxy 地址和 trusted proxy CIDR。

## 配置

| 环境变量 | 默认值 / 约束 |
|---|---|
| `PIPPIT_PUBLIC_ISSUER` | 必填，HTTPS origin，无 path/query/fragment |
| `DATABASE_URL` | 必填，PostgreSQL DSN |
| `PIPPIT_CREDENTIAL_MASTER_KEY` | 必填，32 随机字节的标准 base64 |
| `PIPPIT_PUBLIC_CLIENTS_JSON` | 必填，client ID 到精确 HTTPS redirect URI 数组的 JSON |
| `PIPPIT_PUBLIC_CANVAS_SCRIPT` | 必填，`scripts/public-canvas-command.js` 绝对路径；同级需有 `canvas-command.js`，上级 `dist` 需有 runtime |
| `PIPPIT_MCP_LISTEN` | `0.0.0.0:8787`；CLI `--listen` 可覆盖 |
| `PIPPIT_OAUTH_ACCESS_TTL` | `1h`，范围 1 分钟至 1 小时 |
| `PIPPIT_OAUTH_REFRESH_TTL` | `720h`，范围 1 小时至 90 天 |
| `PIPPIT_MEDIA_CACHE_DIR` | 必填，独立的 0700 私有目录，每进程单独挂载 |
| `PIPPIT_MEDIA_CACHE_TTL` | `6h`，至少 20 分钟 |
| `PIPPIT_MEDIA_CACHE_MAX_BYTES` | `4294967296`，每副本 4 GiB |
| `PIPPIT_MEDIA_MIN_FREE_BYTES` | `1073741824`，保留 1 GiB 空间 |
| `PIPPIT_MCP_MAX_FILE_BYTES` | `209715200`，最大 200 MiB |
| `PIPPIT_MEDIA_MAX_FILES` | `12`，范围 1 至 20 |
| `PIPPIT_PUBLIC_GLOBAL_CONCURRENT` | `16`，所有副本共享的每类读/写并发上限 |
| `PIPPIT_PUBLIC_USER_ACTIVE_JOBS` | `3`，每用户尚未确认完成的生成任务数 |
| `PIPPIT_TRUSTED_PROXY_CIDRS` | 默认空，只信任直连 IP；必须只填实际代理地址 |
| `PIPPIT_CHATGPT_ALLOW_FAKE_IP` | `false`，见下节 |

Public 路由固定为 `/mcp`、`/healthz`、`/readyz`、`/oauth/*`、`/bind/*`、`/account/*` 和 `/.well-known/*`。local 的 bearer、output-dir、private-URL 和 CLI wrapper 等选项不配置 Public 安全边界。代理必须保留原 Host；只信任明确配置代理的 `X-Forwarded-For`，从右向左剥离可信跳数。不要记录 Authorization、Cookie、OAuth query/form、binding payload、工具参数或 signed URL。

## 输入缓存、Fake-IP 与产物

Public 输入仅接受 `https://oaiusercontent.com` 或其子域的 443 端口 URL。禁止 userinfo、其他 scheme/域、私网、loopback、link-local、metadata、保留地址及混合公网/私网 DNS 响应；每次 redirect 与实际 dial 都重新检查，连接已检查的 IP，TLS 仍验证原域名。下载不使用系统 HTTP 代理，也不携带 OAuth/小云雀凭据。还检查文件数、字节数、扩展名和内容 MIME。

优先让 Clash 对 `*.oaiusercontent.com` 使用真实 DNS（fake-ip-filter 或 redir-host）。确需 TUN Fake-IP 时可设置 `PIPPIT_CHATGPT_ALLOW_FAKE_IP=true`：唯一例外是这些精确域名在 443 上解析到 `198.18.0.0/15`，TLS 证书检查保持开启；IP literal、其他域名和其他内网段仍拒绝。不应为此开启 local 的 `--allow-private-file-urls`。

输入在请求内下载到随机私有目录，上传小云雀后随调用清理，失败和取消也会清理；进程崩溃残留在启动时及每分钟按 TTL 清理，活动目录不会误删。Compose 使用有限容量 tmpfs，进程重建时输入自然清空；大规模部署可换独立加密临时卷，保持目录权限与容量配额。

生成后的图片和视频由小云雀托管。Public 不提供 `pippit_download_result`；`pippit_query_result` 只查询 metadata，返回 URL 和完成状态，不访问或代理媒体字节。URL 可能有有效期，需要时再次查询上游。禁止把媒体抓取代理/CDN 缓存加入返回链路。

## 幂等、限流与运维

所有写工具必须传 8 至 128 字符 `idempotency_key`。同一用户、同一 key 和同一参数只提交一次；完成后返回已保存响应，参数变化返回冲突。并发重试或网络结果不确定时返回 pending/uncertain，不能换 key 自动重试，避免重复扣积分。请求被取消、响应解析或持久化失败也可能已在上游执行；这些执行后失败会立即尝试写入 uncertain 与失败审计，数据库不可用或进程崩溃则由 janitor 兜底。数据库永久保留幂等 tombstone，响应及结果 URL metadata 30 天后清除；重新创建同一 key 仍被拒绝。

首次响应丢失时，调用仅 Public 模式提供的 `pippit_get_job(idempotency_key=原键)`。它只读取当前用户、当前账号的 PostgreSQL job，不调用小云雀、不提交任务、不下载媒体，继续受 OAuth、限流、并发和审计控制。不存在、其他用户或其他账号的记录均返回 `found=false`；只识别原幂等键，不支持用 job ID 查询。找到后返回 `job_id`、`tool`、`submission_state`、可用的 thread/run ID、`generation_finished`、已保存的 `submission` 与 `result`。`submission_state=completed` 仅表示提交响应已保存，生成是否结束以 `generation_finished` 为准；查询最新上游进度仍使用 `pippit_query_result`。两个已保存响应合计超出 2 MiB 时优先省略 submission，再按需省略 result，并设置 `metadata_omitted=true`，归属内的恢复 ID 始终保留。

如果 job 仍是 pending，等待当前请求完成；如果是 uncertain 且数据库也没有 thread/run，需要运维核对小云雀记录，不能声称 exactly-once，也不能因为 `found=false` 就断言上游从未接受过请求。30 天后的 tombstone 仍可查询恢复 ID 和状态，但不再包含已清理的 URL metadata。

HTTP 每分钟全局 1200、每来源 IP 180；工具每用户每分钟合计 120、每读工具 60、每写工具 6。每用户最多 20 个读调用、1 个写调用；全局读写各自受配置上限控制。限流桶与工作租约在 PostgreSQL 中跨副本共享。工具请求 15 分钟超时，租约 16 分钟过期，崩溃 pending job 在 20 分钟后标为 uncertain。生成任务达到 active 上限时必须查询已有任务至终态才能释放名额。

`/healthz` 只检查进程；`/readyz` 检查 PostgreSQL 与 migration checksum，失败返回 503。JSON 应用日志只记安全路由、request ID、耗时与工具结果；审计保留 90 天。应监控 401/403、429、503、数据库连接/容量、缓存容量、uncertain job、上游失败和长期未完成任务。

迁移显式执行，事务与 advisory lock 防止多副本重复执行，checksum 不符拒绝启动。升级前备份 PostgreSQL 并验证恢复；普通应用回滚先回滚镜像，不能随意回退 schema。`mcp migrate down` 会删除全部 Public 表，仅可在确定销毁数据时设置 `PIPPIT_CONFIRM_DROP_PUBLIC_DATA=yes`，不是正常发布回滚步骤。

当前 master key 版本为 `local-v1`。不能直接替换环境变量来轮换已有密文；本版不含在线 rewrap 命令。应采用受控维护窗口：先备份、解除现有凭据与连接，再换 key，要求用户重新绑定。恢复数据库时必须同时恢复对应版本密钥。密钥泄露时撤销连接并重新绑定，不能只恢复旧密文。

## 验收门禁

本地与 CI 必须执行真实 PostgreSQL 测试，未设置 `TEST_DATABASE_URL` 的普通 `go test` 会跳过数据库集成用例，不足以验收 Public：

```bash
docker run -d --name pippit-public-test -e POSTGRES_USER=test -e POSTGRES_PASSWORD=test -e POSTGRES_DB=publicapp_test -p 127.0.0.1:55432:5432 postgres:17
export TEST_DATABASE_URL='postgres://test:test@127.0.0.1:55432/publicapp_test?sslmode=disable'
npm run prepare:canvas-runtime
npm test
go test -race ./...
go build ./...
```

已有本地网页登录状态时，可以单独执行只读上游身份门禁。该测试只在内存中读取本机凭据并请求小云雀，不写入 Public 用户、不输出凭据或 UID、不消耗生成积分：

```bash
PIPPIT_TEST_LIVE_XYQ_IDENTITY=1 go test -v ./internal/publicapp -run '^TestLiveXiaoyunqueIdentity$' -count=1
```

`.github/workflows/ci.yml` 配置 PostgreSQL 17 + Go race/build/vet，并包含 Linux/Windows Node runtime 测试。最新验收记录见 [public-mcp-acceptance.md](public-mcp-acceptance.md)。PR #1 在以下真实外部验证全部完成前保持 Draft：生产 HTTPS callback、真实 UID 校验、两个独立用户绑定/工具隔离、ChatGPT metadata/文件参数扫描、聊天生图到小云雀图生视频到 URL 返回、用户确认扣积分、撤销与重新连接，以及 macOS/Linux/Windows 基础启动验证。自动测试使用的上游 fixture 不等同于真实 ChatGPT 或小云雀验收。
