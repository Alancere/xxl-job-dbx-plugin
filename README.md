# DBX XXL-JOB 插件

在 DBX 内连接 [XXL-JOB](https://github.com/xuxueli/xxl-job) 分布式任务调度平台的管理端（xxl-job-admin），完成日常调度运维，而不需要在浏览器里再登录一次管理台。

插件走 DBX 的 manifest v1 插件平台：一个 Go 原生 Sidecar 负责所有与管理端的 HTTP 会话，一个沙箱工作台负责界面。因为 XXL-JOB 管理端没有 Token API、只有表单登录 + `XXL_JOB_LOGIN_IDENTITY` Cookie，所以登录、Cookie 保持、会话过期重登都在 Sidecar 内完成，工作台只能通过 DBX Host Bridge 调用，拿不到管理端地址、账号或 Cookie。

## 功能

| 模块 | 能力 |
| --- | --- |
| 连接 | 表单登录管理端；http/https；自定义上下文路径；自签名证书；SSH/SOCKS 等 DBX 传输层（使用 `runtime.host/port`）；`自动探测管理端路径` 连接动作 |
| 执行器 | 分组列表、AppName / 名称 / 注册方式 / 在线注册地址（分组本身仍在管理端维护） |
| 任务 | 分页列表、按分组与状态过滤、按任务描述 / JobHandler / 负责人搜索、详情、新建、编辑、删除 |
| 调度 | 启停、手动触发（可覆盖任务参数与执行地址，留空自动按注册地址路由）、Cron / 固定频率下次执行时间预览 |
| 日志 | 调度日志分页（任务 ID、调度结果、时间范围）、增量加载日志正文、终止运行中的执行 |
| GLUE | 读取并保存 GLUE 源码（Shell / Python / NodeJS / PHP / PowerShell / Groovy） |
| 报表 | 任务总览（分组数、任务数、运行中、已停止）、按天调度趋势（成功 / 失败 / 运行中）、执行器分组明细 |
| AI | 为内置 DBX AI 助手提供只读（分组、任务、日志）与需确认（触发任务）的工具 |

## 目录

```text
xxl-job/
├── manifest.json          # manifest v1：连接提供者、工作台、MCP 工具
├── assets/                # 插件与连接图标
├── backend/               # Go Sidecar（stdio JSON-RPC v1）
│   ├── main.go            # 生命周期、RPC 分发、AI 工具
│   ├── client.go          # 管理端 HTTP 会话（登录 / Cookie / 过期重登）
│   ├── api.go             # 分组、任务、调度、日志、GLUE、报表
│   └── xxljob_test.go     # 内置模拟管理端的单元测试 + 协议端到端测试
├── ui/index.html          # 沙箱工作台（内联样式与脚本，无构建步骤）
├── package.mjs            # 交叉编译 + 生成 .dbxp（不依赖 cargo）
├── smoke.mjs              # 测试 → 打包 → 校验产物 → 真实 Sidecar 联调
└── README.md
```

## 构建与安装

```bash
# 1) 架构与单元测试
cd plugins/xxl-job/backend && go test ./...

# 2) 打包当前平台（输出 plugins/xxl-job/dist/*.dbxp）
node plugins/xxl-job/package.mjs

# 3) 交叉打包（多平台发布）
DBX_PLUGIN_TARGET=linux-x64 node plugins/xxl-job/package.mjs
DBX_PLUGIN_TARGET=windows-x64 node plugins/xxl-job/package.mjs

# 4) 本地覆盖安装测试：用高于商店的版本号，产物输出到独立目录
DBX_PLUGIN_VERSION=1.0.5 DBX_PLUGIN_OUTPUT_DIR=/tmp/xxljob-local node plugins/xxl-job/package.mjs
```

`package.mjs` 用 `go build` 交叉编译 Sidecar，并按 DBX 的包规则写出 ZIP 容器（Deflate、UTF-8 文件名、Unix 权限、无目录条目、`checksums.json` 覆盖全部文件）。产物是一个**未签名**的候选包，安装时需要：

- 打开 **插件中心 → 允许安装未签名开发包**，安装 `dist/alancere.xxljob-<version>-<target>.dbxp`；或
- 用 `dbx-plugin-packager sign` 由仓库密钥签名后再安装（正式发布流程见 `plugins/RELEASING.md`）。

本地覆盖测试要注意版本号：已安装商店版时，用 `DBX_PLUGIN_VERSION` 指定一个更高的版本（例如 `1.0.5`），否则同版本可能被宿主拒绝安装。

一次完整的本地校验（测试 → 打包 → 校验每个文件的 SHA-256 与可执行位 → 启动真实 Sidecar 连接模拟管理端）：

```bash
node plugins/xxl-job/smoke.mjs
```

## 新建连接

| 字段 | 说明 |
| --- | --- |
| 连接名称 | 显示在侧边栏的名称 |
| 主机 | 管理端地址。可以填 `127.0.0.1`，也可以直接粘贴 `http://10.0.0.2:8080/xxl-job-admin`（会解析出协议、端口与上下文路径） |
| 端口 | 管理端端口，默认 `8080` |
| 管理端账号 / 密码 | 管理端登录账号。密码保存在 DBX 密钥库，工作台不会收到 |
| 协议 | `http` 或 `https` |
| 管理端上下文路径 | 默认 `/xxl-job-admin`；部署在根路径时填 `/` |
| 连接超时（秒） | 登录与探测的超时，默认 15；DBX 的 RPC 截止时间与此保持一致 |
| 跳过 TLS 校验 | https 使用自签名证书时打开 |

连接表单里的 **自动探测管理端路径** 会依次尝试 `http/https` × 常见上下文路径（`/xxl-job-admin`、`/`、`/xxl-job` 等）的 `/toLogin` 页面，把命中的配置回填到表单，避免上下文路径填错时反复试错。

保存前点 **测试** 即可验证登录与权限；保存后从侧边栏打开连接进入工作台。

## 工作台

- **任务管理**：左侧按执行器分组过滤，顶部按状态 / 描述 / JobHandler / 负责人搜索。行内可直接触发、启停、查看日志、编辑、删除。
- **编辑任务**：分组、描述、负责人、报警邮件、调度类型与 Cron/固定频率（支持下次执行时间预览）、运行模式、JobHandler、任务参数、路由策略、子任务、调度过期策略、阻塞处理策略、超时与重试。GLUE 模式下可再打开 **GLUE 源码** 编辑并保存。
- **调度日志**：按任务 ID、调度结果、时间范围查询；`日志` 打开增量加载的日志正文，可刷新、继续加载、终止执行。
- **运行报表**：任务总览、按天调度趋势（成功 / 失败 / 运行中）与执行器分组明细，支持近 7/14/30 天。

## 常见问题

**连接时报 `POST http://host:port/xxx/login 返回 HTTP 404`**

管理端没有部署在这个上下文路径上，或者端口不对（xxl-job-admin 默认 8080）。插件会在这条错误里直接给出探测结果，例如：

```text
XXL-JOB 登录失败：POST http://10.10.50.5:9090/xxl-job-admin/login 返回 HTTP 404。
已探测到管理端实际位于 http://10.10.50.5:9090/，请把“管理端上下文路径”改为 /（端口 9090）后重试
```

按提示改字段即可；也可以直接点表单里的 **自动探测管理端路径**，它会回填协议与上下文路径。若只是点「保存并连接」，插件会在同一主机上自动找到能登录的管理端并连上，同时在工作台顶部提示应把上下文路径改成什么。

**任何认证方式都能连：直接复用浏览器会话（推荐先用这一招）**

管理端的登录方式千差万别（XXL-SSO、网关 JWT、定制 token……），但**只要能登录的浏览器会话，插件就能用**：

1. 浏览器登录管理端，打开任务列表页；
2. F12 → Network → 点任意一个 XHR 请求（例如 `jobinfo/pageList`）→ Request Headers → 复制 `Cookie` 的值（如果走的是 `Authorization`，就复制它）；
3. 粘到连接配置的 **浏览器会话 Cookie**（或 **自定义认证请求头**，例如 `Authorization: Bearer ey...`）；
4. 点 **测试**。

插件会跳过登录、把这份凭证原样发到管理端，再用 `/jobinfo/pageList` 校验是否真的登录成功；失败会明确告诉你“连接凭证未被管理端接受”，而不是继续猜。Cookie / 请求头与密码一样保存在 DBX 密钥库；登录态在管理端过期后重新复制一次即可。

**探测结果显示 `/jobinfo/pageList` 返回 200 但带 `com.xxl.sso.core` 异常**

这是最常见的一种“非经典”部署：业务接口还是经典 xxl-job-admin 的，但登录被 **XXL-SSO** 接管（`/login`、`/toLogin` 都是 404，`/api/login` 提示 `The access token is wrong.`）。此时不需要换插件，只要在连接配置里填写 **Access Token（SSO / OpenAPI）**：

- 填管理端配置的 `xxl.job.accessToken`（或 SSO 下发的 token）；
- 插件会把它作为 `XXL-JOB-ACCESS-TOKEN` 请求头发送，并在登录时调用 `POST /api/login`（`{"accessToken": "..."}`）换取会话 Cookie，然后用 `/jobinfo/pageList` 校验是否真的登录成功；
- 校验不通过不会假装连上，而是原样返回 token 被拒绝的原因。

**管理端是带 `#/` 路由的新版/定制前端**

浏览器地址形如 `http://host:9090/xxl-job-admin/#/xxl-job-admin/dashboard` 时，`#` 之后的部分只在前端生效，服务端只知道 `GET /xxl-job-admin/`，而且这类前端通常把接口放在自己的 REST 路径下（例如 `/xxl-job-admin/api/...`），经典控制台的 `POST /login`、`/jobinfo/pageList` 并不存在 —— 这正是本插件报 `HTTP 404` 的原因。请点连接表单里的 **检查管理端接口**，插件会：探测各候选接口的状态码与响应内容；再读取管理端首页与它加载的脚本，列出前端真正调用的接口路径与认证线索（`accessToken`、`Authorization` 等）。把这份报告贴给插件作者即可精确适配。

**登录失败提示账号或密码错误**

用浏览器登录管理端确认账号；XXL-JOB 管理端没有角色体系，但账号必须存在。若地址是反向代理且改了路径，请确认 `X-Forwarded-*` 与上下文路径配置一致。

**工作台空白 / 提示“连接会话已失效”**

Sidecar 进程重启或连接被关闭后会出现，点提示里的 **重新连接** 即可；若管理端登录态过期，插件会自动重登一次，仍失败再点 **重新登录**。

## 管理端兼容性

插件适配两代 xxl-job-admin 控制台接口，连接时自动识别：

| 用途 | 2.x（1.9–2.4） | 3.x（3.0+） |
| --- | --- | --- |
| 登录 | `POST /login` | `POST /auth/doLogin` |
| 登录态 | Cookie `XXL_JOB_LOGIN_IDENTITY` | Cookie `xxl_job_login_token` |
| 响应信封 | `{code,msg,content}` | `{code,data,msg,success}` |
| 分页参数 | `start`/`length` | `offset`/`pagesize` |
| 新建任务 | `POST /jobinfo/add` | `POST /jobinfo/insert` |
| 删除任务 | `POST /jobinfo/remove` | `POST /jobinfo/delete`（参数 `ids[]`） |
| 启停任务 | `POST /jobinfo/start|stop`（参数 `id`） | 同上（参数 `ids[]`） |
| 日志正文 | `GET /joblog/logDetailCat` | `POST /joblog/logDetailCat` |
| GLUE 页面 | `GET /jobcode?id=` | `GET /jobcode?jobId=` |
| 报表 | `GET /chartInfo`（日期） | `POST /chartInfo`（日期时间） |

分组、任务分页、触发、下次执行时间、日志分页、终止、GLUE 保存等接口路径两代一致。识别方式：先按 2.x 登录，命中 `404` 再退回 3.x 的 `/auth/doLogin`；账号密码错误会立即返回，不做多余的第二次尝试。

分页接口里 `jobGroup` 在 2.x 是原生 `int`，缺失时 Spring 会返回 `HTTP 200` + `{"code":500,"msg":"java.lang.IllegalStateException: Optional int parameter 'jobGroup' ..."}`；插件发出的每个分页/登录校验请求都显式带上该参数。3.x 则要求 `jobDesc`、`executorHandler`、`author` 三个筛选参数都出现。时间字段在不同版本里形态不同（2.4.1 返回 `2026-10-21T03:42:19.000+00:00` 这样的 ISO 字符串，老版本返回毫秒时间戳），工作台两种都会按本地时区显示；3.x 报表的字段名也做了归一化。

已知限制：

- **任务详情**：控制台没有稳定的单任务查询接口，插件先尝试 `GET /jobinfo/detailById`（较新版本），不可用时回退到分页扫描；任务参数超过 500×40 条时回退会失败并提示。
- **执行日志正文**：2.4.1 起 `/joblog/logDetailCat` 由管理端转发给执行器读取，调度未路由到执行器时（触发失败、执行器地址为空）没有正文可读；工作台会禁用该行的「日志」按钮并说明原因。
- **启动任务**：调度类型为「不调度（NONE）」的任务无法启动，管理端返回“当前调度类型禁止启动”；工作台对这类任务禁用「启动」，需先改为固定频率或 Cron。
- **GLUE 源码读取**：控制台没有 JSON 接口，插件从 `/jobcode` 页面提取 `glueSource` 文本域；页面结构变化时会提示改用管理端网页编辑（保存接口不受影响）。
- **认证方式**：按优先级支持三种——① 浏览器会话 Cookie 或自定义认证头（原样复用，适合自建/SSO 登录）；② XXL-SSO access token（先 `POST /api/login` 换取会话，并把 `XXL-JOB-ACCESS-TOKEN` 带在每个请求上）；③ 管理端自带的表单登录（2.x `/login`，3.x 回退 `/auth/doLogin`）。注意 `xxl.job.accessToken` 本身是执行器与调度中心之间的共享凭据，只有在管理端把它当作 OpenAPI/SSO 守卫时才需要填到连接配置里。
- **管理员账号**：登录用户需要在管理端拥有对应权限；无权限的操作会原样返回管理端的错误信息。
- **执行器分组维护**：工作台只读展示分组与注册地址，新增/修改分组请仍在管理端完成。

## 协议方法

Sidecar 在 `connection/test`、`connection/connect`、`connection/disconnect` 之外，向工作台暴露以下方法（均需 `connectionId`）：

```text
xxljob/overview             { days }                            连接概览 + 分组 + 统计 + 趋势
xxljob/dashboard            { days }                            统计 + 调度趋势 + 分组
xxljob/groups               { }                                 执行器分组
xxljob/jobs                 { groupId, status, searchField,    任务分页（status: -1 全部 /
                              keyword, start, length }           0 已停止 / 1 运行中）
xxljob/job                  { id }                              任务详情
xxljob/job/save             { job }                             新建 / 更新
xxljob/job/remove|start|stop{ id }
xxljob/job/trigger          { id, executorParam, addressList }
xxljob/job/nextTriggerTime  { scheduleType, scheduleConf }
xxljob/logs                 { groupId, jobId, status, filterTime, start, length }
xxljob/log                  { logId, fromLineNum }
xxljob/log/kill             { logId }
xxljob/code                 { id }                              GLUE 源码
xxljob/code/save            { id, glueType, source }
xxljob/relogin              { }                                 重新登录管理端
```

错误码：`-32010` 连接会话失效（UI 会提示重新连接）、`-32020` 管理端业务失败、`-32021` 登录失败/过期、`-32022` 网络错误、`-32023` 未找到。

## 安全

- 管理端密码以 `password` 绑定存入 DBX 密钥库，`config_json` 中只有占位符；工作台只拿到 `connectionId`。
- 沙箱工作台没有网络权限（`connect-src 'none'`），所有管理端请求都由 Sidecar 发起。
- Cookie 只存在于 Sidecar 进程内存中，随 `connection/disconnect` 丢弃。
- AI 工具只在用户于插件中心启用插件、且对应连接已打开时可用；`xxljob_trigger_job` 会执行真实任务，因此未标记只读，调用前需要用户确认。
