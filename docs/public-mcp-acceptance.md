# Public MCP 验收记录

更新日期：2026-09-06。分支：`feature/go-mcp-server`。PR：[yuebanddd/cli#1](https://github.com/yuebanddd/cli/pull/1)，必须保持 Draft。当前结论：**自动化实现验证已取得进展，完整 Public App 尚未通过验收，禁止转 Ready。**

产品方向已调整为 **独立 OIDC 服务账号 + 用户手动绑定已有 Access Key**。不再把小云雀网页登录当作服务身份，也不再依赖其公网回调或 UID introspection。目标用户必须能自行持有可用 Access Key；本版不解决普通网页用户获取密钥的问题。

## 要求与当前证据

| 要求 | 当前证据与结论 |
|---|---|
| 多租户 Public MCP，保留 local CLI/MCP | `mcp serve --mode public` 独立入口；默认 local 不变；全仓库 Go/Node 测试通过；Docker Linux Public 启动与 401 discovery challenge 通过 |
| OAuth 2.1 + PKCE + refresh | PostgreSQL 集成验证浏览器 flow、错误 PKCE、code/callback 重放、refresh 轮换与重放整族撤销；metadata 与精确 resource/client/redirect/scope 检查已实现；真实 ChatGPT 连接待验证 |
| 独立服务登录与手动密钥绑定 | TLS OIDC fixture 经真实 RSA/JWKS 验证，错误签名/issuer/audience/azp/nonce/expiry 与 callback 重放被拒绝；同一身份找回账号，不同身份即使使用同一 Access Key 仍隔离；CSRF、session 与明确绑定确认受检验；生产 OIDC 与可用 AK 待真实验收 |
| 换绑、迁移与凭据来源 | 每次绑定新建归属、删除旧密文、撤销旧授权，不继承旧资源/job/Canvas；旧任务不占新绑定额度；code 发行与换绑串行，防止撤销时遗漏新 family；002 增量迁移不修改 001，不把旧上游身份提升为 OIDC 身份 |
| 用户、OAuth、加密凭据、job metadata 使用 PostgreSQL | PostgreSQL 17 实测 migrations up/down/reapply/checksum readiness、密文无明文、AES-GCM 账号 AAD 与篡改拒绝；生成提交与终态 URL metadata 落库通过 |
| 不使用全局 credential | 双租户 MCP 测试设置恶意 `XYQ_ACCESS_KEY`，上游只收到当前 OAuth 用户凭据；进行中的 runner 在 family 被撤销后拒绝解析凭据；Public Node 环境不传凭据 |
| thread/run tenant isolation | 双用户读取/修改/查询隔离、run 与 thread 配对、嵌套 Canvas asset 输入检查、拒绝通过只读响应认领未知 thread；已归属 thread 中发现的嵌套 run 记录正确父级 |
| ChatGPT 输入临时缓存与 TTL | 正常/失败重复清理、残留 TTL、活动目录保留、数量/字节容量限制、MIME spoof、私网/重定向/混合 DNS 拒绝测试通过 |
| oaiusercontent + Fake-IP + SSRF | 默认拒绝 Fake-IP；仅特定域名 + HTTPS 443 + 198.18.0.0/15 的显式例外；TLS 开启、禁代理、非法域/IP/端口拒绝用例通过；真实 ChatGPT 文件与 Clash TUN 组合待验收 |
| 生成图片/视频不下载、不落地 | 完整 `pippit_query_result` MCP + PostgreSQL + mock 上游查询返回图/视频 URL；媒体监听器收到 0 次请求，缓存为空，响应无 output_path；Public 不注册下载工具；真实生成流程待验收 |
| 幂等 | 同 key 同参数两次请求只调用上游一次；参数冲突、并发重复、结果不确定不自动重复提交测试通过；保留 tombstone，30 天清理响应及结果 URL metadata |
| 幂等恢复与终态审计 | `pippit_get_job` 按 OAuth 用户、账号、原幂等键查询，不调用上游；响应丢失恢复、pending 查询、跨租户及账号替换后的拒绝、到期 tombstone 与大 metadata 有界返回通过；无效 JSON、超大输出、归属冲突、数据库拒写和请求取消均立即记录 uncertain 与失败审计；成功回放与被阻止的重试分别审计，均不重复执行 |
| rate limit / concurrency | PostgreSQL 限流计数与每用户写并发用例通过；所有副本使用共享数据库桶与租约；生产负载容量未做压力验收 |
| audit / health / ready / migration | 审计写入、清理、schema 回退/重建测试通过；容器 `/readyz` 返回 200，匿名 MCP 返回 401 + metadata challenge；生产日志与监控接入待部署 |
| Public Canvas | Node 22 实际 runtime 命令目录运行通过，Go bridge 和 PostgreSQL 状态跨用户隔离通过；真实 Canvas mutation 及恢复场景仍待外部验收 |
| 部署文档与产物 | `deploy/public/{Dockerfile,compose.yaml,Caddyfile}`；镜像构建、容器 migrate/start/readiness、Compose config 校验通过；`docs/public-mcp-deployment.md` 包含 secret、TLS、缓存、备份/恢复、迁移与验收命令 |
| 跨平台兼容 | macOS 测试/构建通过，Linux 容器构建/启动通过，Windows amd64 交叉编译通过；Windows 实机启动尚无证据 |
| 现有 PR 提交与状态 | 当前分支继续提交；PR 描述须保持与上述范围一致；外部门禁未完成前维持 Draft |

## 本次方向调整验证

2026-09-06，在独立 PostgreSQL 17 容器中执行：

- `TEST_DATABASE_URL=... npm test`：Node tests、全仓库 Go tests 与 vet 通过。
- `TEST_DATABASE_URL=... go test -race ./...`：通过；补充 code 发行锁回归测试后 Public 包 race 再次通过。
- `go build ./...` 与 Windows amd64 交叉编译：通过。
- 新镜像 Docker build、迁移 001+002、只读文件系统/非 root 启动与 readiness、匿名 MCP 401 discovery：通过。启动 smoke 使用公开 Google OIDC discovery 与虚构 client ID/secret，只验证发现和进程启动，**没有验证真实账号登录**。
- Compose 配置校验：通过，使用非敏感 fixture 值。
- Playwright 检查登录、绑定、授权三个页面的 375px/1280px 视口：CSS 加载、密码输入、时限选择、确认框、按钮状态与无横向溢出通过，并人工检查截图。使用实际 Go 模板生成的静态页面；不代替生产浏览器 OIDC 往返验收。

本次没有配置生产 OIDC client，没有向 Public 数据库写入真实 Access Key，也没有消耗小云雀生成积分。

## 调整前验证记录

使用独立测试容器 PostgreSQL 17，`TEST_DATABASE_URL` 指向 `127.0.0.1:55432/publicapp_test`，各集成测试创建独立 schema 并清理。测试中的凭据均为 fixture，没有写入真实用户凭据。

- `npm run prepare:canvas-runtime`：通过。
- `TEST_DATABASE_URL=... npm test`：Node tests、全仓库 Go tests、go vet 通过。
- `TEST_DATABASE_URL=... go test -race ./...`：通过，无 race；后续安全改动又执行 Public/MCP 包 race 测试。
- `go build ./...`：通过。
- `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ...`：通过，不能视为 Windows 实机验证。
- `docker build -f deploy/public/Dockerfile -t pippit-public:local .`：通过。
- `docker compose -f deploy/public/compose.yaml config --quiet`：使用非敏感 fixture 环境通过。
- 容器执行 `mcp migrate up`：通过；非 root、只读根文件系统、有限 tmpfs 下启动 Public：healthy；`GET /readyz` 返回 `{"ready":true}`；匿名 `/mcp` 返回 401 和受保护资源 metadata URL。
- 推送后 PR `statusCheckRollup` 为空；查询仓库 Actions 配置返回 HTTP 403（缺少 Actions policies 读取权限）。没有远端 CI 通过的证据，以上自动测试结果均来自实际本地/容器执行。
- `PIPPIT_TEST_LIVE_XYQ_IDENTITY=1 go test -v ./internal/publicapp -run '^TestLiveXiaoyunqueIdentity$' -count=1`：**失败**，HTTP 200、业务码 1015、subject 缺失，`upstream_identity_rejected`。只读使用现有网页登录状态；未打印或持久化真实凭据。

## 已退役设计证据

2026-09-06 补充：新增 job 恢复用例已在真实 PostgreSQL 17 下通过 race 测试；通过内存 MCP transport 实际枚举 local 工具，确认 local 目录不包含 Public 专用查询且原有工具没有删除。上游限制仍沿用下面 2026-09-05 的实测证据，未将模拟恢复测试算作真实网页登录或 ChatGPT 验收。

2026-09-05 获取当前小云雀网页登录页与它引用的官方静态资源：

- 页面：`https://xyq.jianying.com/cli/pippit-tool-login`，构建版本 `1.0.0.2435`。
- 登录代码：[page.835d5b491a.js](https://lf3-lv-buz.vlabstatic.com/obj/image-lvweb-buz/ies/pippit/platform_xyq/static/js/async/cli/pippit-tool-login/page.835d5b491a.js)。其 callback 校验要求完整 URL 等于 `http://127.0.0.1:<port>/pippit-tool/callback?state=<43字符>`，拒绝 HTTPS 和非 loopback hostname；因此本服务 `https://<issuer>/bind/callback?binding=...` 无法通过当前上游页面校验。
- 上游主代码 [main.f5e8e1a3b9.js](https://lf3-lv-buz.vlabstatic.com/obj/image-lvweb-buz/ies/pippit/platform_xyq/static/js/main.f5e8e1a3b9.js) 确实声明 GET `/api/biz/v1/user/info`，但实测仅带当前 CLI Access Key 时业务码 1015，无经过验证的 UID。不能以页面中存在接口作为 AK introspection 支持的证明。

以上结果解释了退役原方案的原因，不再构成手动 Access Key 方案的发布依赖。原 `/bind/start`、`/bind/callback` 与 Public `/user/info` 身份验证代码、live identity 测试已删除；local CLI loopback 登录保留。

## 剩余门禁

在稳定 HTTPS 域名配置真实 OIDC 登录提供方与 ChatGPT App，完成注册/登录/账号找回、两个用户各自持有并绑定可用 Access Key、工具及文件 schema 扫描、真实聊天图片交接、确认消耗积分后生成视频、查询返回上游 URL、验证生成媒体没有经过服务器、刷新/撤销/重新连接、真实 Canvas mutation，以及 Windows 基础启动。需要记录真实结果，不能用模拟数据勾选这些门禁。密钥不可用时应记录上游真实失败，不能把“密文已保存”当作“绑定验证成功”。
