# Public MCP 验收记录

日期：2026-09-05。分支：`feature/go-mcp-server`。PR：[yuebanddd/cli#1](https://github.com/yuebanddd/cli/pull/1)，必须保持 Draft。当前结论：**自动化实现验证已取得进展，完整 Public App 尚未通过验收，禁止转 Ready。**

## 要求与当前证据

| 要求 | 当前证据与结论 |
|---|---|
| 多租户 Public MCP，保留 local CLI/MCP | `mcp serve --mode public` 独立入口；默认 local 不变；全仓库 Go/Node 测试通过；Docker Linux Public 启动与 401 discovery challenge 通过 |
| OAuth 2.1 + PKCE + refresh | PostgreSQL 集成验证浏览器 flow、错误 PKCE、code/callback 重放、refresh 轮换与重放整族撤销；metadata 与精确 resource/client/redirect/scope 检查已实现；真实 ChatGPT 连接待验证 |
| 每用户小云雀网页登录绑定 | **未完成外部验收，存在已证实上游阻碍**，详见下节；fixture 绑定流程通过不能替代真实验证 |
| 用户、OAuth、加密凭据、job metadata 使用 PostgreSQL | PostgreSQL 17 实测 migrations up/down/reapply/checksum readiness、密文无明文、AES-GCM 账号 AAD 与篡改拒绝；生成提交与终态 URL metadata 落库通过 |
| 不使用全局 credential | 双租户 MCP 测试设置恶意 `XYQ_ACCESS_KEY`，上游只收到当前 OAuth 用户凭据；进行中的 runner 在 family 被撤销后拒绝解析凭据；Public Node 环境不传凭据 |
| thread/run tenant isolation | 双用户读取/修改/查询隔离、run 与 thread 配对、嵌套 Canvas asset 输入检查、拒绝通过只读响应认领未知 thread；已归属 thread 中发现的嵌套 run 记录正确父级 |
| ChatGPT 输入临时缓存与 TTL | 正常/失败重复清理、残留 TTL、活动目录保留、数量/字节容量限制、MIME spoof、私网/重定向/混合 DNS 拒绝测试通过 |
| oaiusercontent + Fake-IP + SSRF | 默认拒绝 Fake-IP；仅特定域名 + HTTPS 443 + 198.18.0.0/15 的显式例外；TLS 开启、禁代理、非法域/IP/端口拒绝用例通过；真实 ChatGPT 文件与 Clash TUN 组合待验收 |
| 生成图片/视频不下载、不落地 | 完整 `pippit_query_result` MCP + PostgreSQL + mock 上游查询返回图/视频 URL；媒体监听器收到 0 次请求，缓存为空，响应无 output_path；Public 不注册下载工具；真实生成流程待验收 |
| 幂等 | 同 key 同参数两次请求只调用上游一次；参数冲突、并发重复、结果不确定不自动重复提交测试通过；保留 tombstone，30 天清理响应及结果 URL metadata |
| rate limit / concurrency | PostgreSQL 限流计数与每用户写并发用例通过；所有副本使用共享数据库桶与租约；生产负载容量未做压力验收 |
| audit / health / ready / migration | 审计写入、清理、schema 回退/重建测试通过；容器 `/readyz` 返回 200，匿名 MCP 返回 401 + metadata challenge；生产日志与监控接入待部署 |
| Public Canvas | Node 22 实际 runtime 命令目录运行通过，Go bridge 和 PostgreSQL 状态跨用户隔离通过；真实 Canvas mutation 及恢复场景仍待外部验收 |
| 部署文档与产物 | `deploy/public/{Dockerfile,compose.yaml,Caddyfile}`；镜像构建、容器 migrate/start/readiness、Compose config 校验通过；`docs/public-mcp-deployment.md` 包含 secret、TLS、缓存、备份/恢复、迁移与验收命令 |
| 跨平台兼容 | macOS 测试/构建通过，Linux 容器构建/启动通过，Windows amd64 交叉编译通过；Windows 实机启动尚无证据 |
| 现有 PR 提交与状态 | 当前分支继续提交；PR 描述须保持与上述范围一致；外部门禁未完成前维持 Draft |

## 已执行命令

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

## 上游阻碍与剩余门禁

2026-09-05 获取当前小云雀网页登录页与它引用的官方静态资源：

- 页面：`https://xyq.jianying.com/cli/pippit-tool-login`，构建版本 `1.0.0.2435`。
- 登录代码：[page.835d5b491a.js](https://lf3-lv-buz.vlabstatic.com/obj/image-lvweb-buz/ies/pippit/platform_xyq/static/js/async/cli/pippit-tool-login/page.835d5b491a.js)。其 callback 校验要求完整 URL 等于 `http://127.0.0.1:<port>/pippit-tool/callback?state=<43字符>`，拒绝 HTTPS 和非 loopback hostname；因此本服务 `https://<issuer>/bind/callback?binding=...` 无法通过当前上游页面校验。
- 上游主代码 [main.f5e8e1a3b9.js](https://lf3-lv-buz.vlabstatic.com/obj/image-lvweb-buz/ies/pippit/platform_xyq/static/js/main.f5e8e1a3b9.js) 确实声明 GET `/api/biz/v1/user/info`，但实测仅带当前 CLI Access Key 时业务码 1015，无经过验证的 UID。不能以页面中存在接口作为 AK introspection 支持的证明。

完成真实绑定需要小云雀支持生产 HTTPS callback，并提供能用该授权凭据独立校验用户身份的机制，或提供等价的正式上游 OAuth 流程。当前服务拒绝不可信 UID，未加入伪造成功或全局凭据回退。

上游条件满足后，仍须在稳定 HTTPS 域名与真实 ChatGPT App 配置中完成：两个用户独立绑定、工具及文件 schema 扫描、真实聊天图片交接、确认消耗积分后生成视频、查询返回上游 URL、验证生成媒体没有经过服务器、刷新/撤销/重新连接、真实 Canvas mutation，以及 Windows 基础启动。需要记录真实结果，不能用模拟数据勾选这些门禁。
