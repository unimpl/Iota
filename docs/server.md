# Server 架构与协议

CLI 直接使用 Go SDK。React 客户端通过 HTTP 操作 Server；Server 为每份打开的会话持有一个独立 Agent 和 Session。模型和工具循环只有 SDK 一份实现。

`internal/app` 共用配置解析、默认工具、系统提示、项目规则和规划命令。CLI 保留原来的参数、终端输出与工作目录选择。Server 恢复会话时，从 `Session.Info().CWD` 初始化文件工具、bash 与项目提示；不修改进程全局工作目录。未记录 CWD 的 SDK 会话使用 Server 的默认目录。

## 运行与配置

```sh
go run ./cmd/iota-server --listen 127.0.0.1:9281
```

配置优先级与 CLI 相同：用户 TOML（缺失时读取启动目录 TOML）、环境变量、显式参数。支持 `--model`、`--base-url`、`--cwd`、`--system`、`--tools`、`--max-turns`、`--timeout`、`--mode`。显式模式配置会覆盖恢复出的模式，与 CLI 一致。配置里的 `resume` 不会自动打开 Web 会话，由界面选择。Web 需要 `save_session=true`。

React 静态文件随 Go 程序嵌入。Node 只用于开发与更新资产：

```sh
cd web
npm ci --ignore-scripts
npm run check
node build.mjs
```

`web/dist` 是生成文件，需要与源码一起保存，确保 `go install` 无需 Node。开发时使用 `node build.mjs --watch`，并以 `--web-dir web/dist` 启动 Server；刷新页面可见最新资产。

## 会话所有权

新建或打开会话先获取操作系统独占锁，再读取和恢复日志。锁保持到 `Session.Close()`；进程退出会自动释放，包括异常退出。稳定的锁文件放在会话目录旁的隐藏目录，如 `~/.iota/.sessions-locks`，不会加入会话列表。锁文件保留，不通过删除锁文件接管会话。

浏览日志是独立的只读路径，不调用会修复中断工具的 `OpenSession()`。CLI、Server 和原查看器的删除入口都使用 SDK 的占用规则。占用冲突返回 HTTP 409。

浏览器与 Server 内的 Agent 生命周期独立。空闲会话可关闭；运行中的会话先停止，等待 SDK 清理和落盘，再关闭。切换页面、刷新和断开 SSE 不停止任务。Server 正常退出会取消所有任务、等待清理并释放会话。

异常退出后只恢复已保存内容，不自动重跑工具。未完成的 Web 请求标记为 `interrupted`；用户先检查历史和文件状态，再提交新的任务。损坏或不完整的 JSONL 继续明确报错，不自动截断。

## HTTP 接口

所有路径相对 `/api`。写操作使用 JSON，并带 `X-Iota-Client: web`。提供 Origin 的请求必须匹配本机 Origin，或 `--origin` 配置的公开 Origin。凭据不返回浏览器。

| 方法与路径 | 行为 |
| --- | --- |
| `GET /config` | 返回当前模型、默认工作目录和模式 |
| `GET /sessions` | 列出共享会话，包括占用和本 Server 的打开状态 |
| `POST /sessions` | 用 `{cwd, mode?}` 创建并打开会话 |
| `POST /sessions/{name}/open` | 获取独占锁，创建 Agent 并恢复状态；已由当前 Server 打开时复用实例 |
| `POST /sessions/{name}/close` | 关闭空闲会话并释放锁 |
| `DELETE /sessions/{name}` | 删除未被占用的会话日志 |
| `GET /sessions/{name}/state` | 运行状态、协作状态、技能、待回答问题和当前计划预览 |
| `GET /sessions/{name}/history` | 最近 3 次 run 的消息、全部 run 的轻量索引与实时游标；`?before=runID` 加载之前 3 次，`?run=runID` 加载指定 run |
| `GET /sessions/{name}/events?after=N&interval_ms=1000` | 合并的 SSE 事件与当前状态，重连优先使用 `Last-Event-ID` |
| `POST /sessions/{name}/commands` | 接受操作并在后台执行 |
| `POST /sessions/{name}/stop` | 请求取消当前操作 |
| `POST /sessions/{name}/answers` | 回答当前等待的问题，在原 Run 中继续 |

会话路径只接受 SDK 生成的文件名，不接受任意文件路径或符号链接。请求体最多 1 MiB，并拒绝未知字段和多个 JSON 值。

### 提交操作

```json
{"request_id":"unique-id","action":"run","prompt":"解释项目结构"}
```

`action` 支持 `run`、`compact`、`reset`、`mode`、`execute`、`skills`、`skills_reload`、`skill_install`。`run` 也解释 `/plan [任务]`、`/default [任务]`、`/mode`、`/execute`、`/compact`、`/reset` 和 `/skill` 命令。`mode` 使用 `mode` 字段；安装和压缩使用 `prompt` 字段。

新操作返回 202。一个会话同一时间只执行一个操作；忙时新请求返回 409，不排队。请求 ID、参数和最终结果保存在同一 JSONL 中。使用相同 ID 和相同参数重试返回已有结果；相同 ID 携带不同参数返回 409。恢复后不会因为 HTTP 重试重复执行工具。

`execute` 和文本 `/execute` 必须携带 `state.plan_hash`。这是当前展示计划正文的 SHA-256；SDK 再次读取文件、校验版本、持久化批准，然后使用同一份批准内容开始执行。过期版本拒绝执行；`state.plan_preview` 提供最新文件内容，`plan_error` 表示预览不可用。计划预览不修改保存的协作状态。

### 用户提问

`state.pending` 包含稳定提问 ID、问题格式、问题列表与调用进度。刷新后读取同一个 pending，不启动新 Run。回答格式如下：

```json
{"id":"pending-id","response":{"answers":{"question-id":{"source":"selected","option_id":"option-id"}}}}
```

自由答案使用 `source=custom` 和 `value`；空的自由答案显式使用 `source=unanswered`。所有问题必须明确回答。无效答案返回 400，并继续等待；过期或重复答案返回 409。推荐选项只有在用户提交回答时才生效。停止任务通过单独的取消接口处理。

### SSE 与历史

历史接口先扫描日志建立轻量索引，再只投影所选 run 的完整回复、思考与工具调用/结果。默认返回最近 3 次 run 的 `items`；`before` 为排他的 run ID，返回之前最多 3 次 run；`run` 返回单个指定 run，两者不可同时使用。未知 run 或查询参数返回 400。`loaded_runs` 标识本页加载的 run。全部 run 按时间正序放在 `runs` 中，每项包含 `id`、`start_seq`、`end_seq`、`started_at`、`title`（最多 80 字符）、`preview`（最终文字回复摘要，最多 160 字符）和 `status`。较旧 run 的思考与工具正文不会进入默认响应。

正在生成的回复保留已保存的部分内容和工具状态。首次扫描限定在请求开始时的文件大小，消息投影不超过首次扫描的 `cursor`；未完成的最后一行不计入游标。所有历史页的 `cursor`、`run`、`turn`、`action` 和协作状态均指向日志末尾，而非本页末尾。可用 `at=N` 把索引、内容和状态限制在已接收的正整数游标，超过日志末尾则返回 400。Web 补载时使用当前 SSE 游标，避免较新的快照内容再被实时片段重复追加。浏览历史不会打开 Agent 或修复工具。

客户端加载最近历史，再从 `cursor` 订阅实时事件。快照的 `run`、`turn` 与 `action` 用于接续生成中的回复和操作。向上滚动加载更早的 run，并保持当前消息的位置；左侧刻度提供全部任务的标题、摘要预览与跳转，未加载的任务按需获取。跳转产生的历史空隙提供补载按钮。补载按消息 ID 去重、按 `seq` 排序，保留已有实时内容与 SSE 游标。历史接口不提供原始事件分页，增量使用 SSE。

`interval_ms` 控制普通更新的最小发送间隔，单位为毫秒，范围 50–10000，默认 1000；Web 显式使用 1000。同一窗口内按事件类型、run、turn 拼接文本和思考片段，不跨轮次或工具边界合并。开始、结束、完整消息、工具开始/结束、计划与协作变化、操作结果立即发送，并带上之前尚未发送的片段；等待回答、取消和失败等 `state` 变化也立即发送，不受普通间隔限制。关键事件会重新开始普通更新的计时间隔。

SSE 使用 `event: records`，数据为 `{"records":[...],"cursor":N}`。合并记录的 `seq` 是最后一个来源片段的序号，批内按序号排序；SSE `id` 与 `cursor` 相同，还覆盖被省略的 Provider 记录。只有省略记录时可发送 `records:[]` 来推进游标。客户端整批应用后推进游标，重复批次直接忽略；重连使用最后收到的一整批 ID，尚未发送的片段从日志重新读取。`state` 没有独立日志游标，在之前的内容发送完毕后发布。

聊天传输保留消息、文本、思考、工具、协作状态和操作结果；重复的模型请求与原始 Provider 数据不发送给聊天界面。事件序号保持不变，原始 JSONL 与独立查看器仍保留完整记录。压缩改变模型上下文，不删除 Web 中的历史。流式回复最终由完整消息替换，避免重复展示。

每条 SSE 连接独立读取日志，不在同步 SDK 回调中发送网络数据。关键 SDK 事件与状态变化通过不阻塞的通知唤醒读取器；CLI 写入的共享日志使用最长 250 ms 的轮询作为补充。响应带 `X-Accel-Buffering: no`，空闲时发送心跳。

## 网关部署

Server 提供 HTTP；TLS 和访问限制由网关处理。Server 与 CLI 必须看到同一份文件系统和用户目录才能共享配置、会话、计划、skills 和项目文件。

```sh
go run ./cmd/iota-server --listen 127.0.0.1:9281 --origin https://iota.example.com
```

网关保留流式响应，关闭 SSE 缓冲，并允许长连接。公开 Origin 不含路径和末尾斜杠。Server 不信任任意 `X-Forwarded-*` 请求头来决定 Origin。
