# 核心流程与类型关系

Iota 的一次运行是“用户消息 → 模型回复 → 工具结果 → 下一轮模型回复”。例如用户要求解释文件时，第一轮模型可以发起 `read` 调用；Agent 执行后将文件内容作为工具消息加入历史，第二轮模型据此给出解释。

本文的图对应实际的 Go 结构体、接口和函数。类图省略指针标记、锁和部分辅助字段，方法参数省略类型及返回值；实线表示持有或关联，虚线表示调用依赖或接口实现。

## 入口与初始化

CLI 的入口是 [`cmd/iota/main.go`](../cmd/iota/main.go) 中的 `run`。它先读取配置文件，再应用环境变量和命令行覆盖；随后检查模型和工作目录，加载系统提示、创建工具与 OpenAI 兼容 Provider，调用 `iota.New` 创建 Agent，再检查输入方式并初始化会话。SDK 调用方直接传入 `Config`。

[`config.go`](../config.go) 中的 `New` 检查 Provider、模型和轮数上限，并检查工具名称、重复定义和执行函数。每个工具的 JSON Schema（参数约束）在此时编译，运行时复用；schema 的 `$ref` 只允许引用当前文档。无效配置在执行前失败。

CLI 默认创建会话；指定 `--resume` 时先解析路径、UUID 或显式空值，再打开已有会话并恢复 Agent 历史。`--no-session` 时 Session 为 `nil`。`-p` 和管道输入各运行一次，交互输入重复调用同一个 Agent 和 Session；`/reset` 清空历史，`/exit` 结束输入循环。

`--mode`、`IOTA_MODE` 和 TOML 的 `mode` 选择内置的 `plan` 或 `default` 协作模式。恢复时先载入日志模式，只有显式模式配置才覆盖它。`/plan`、`/default` 和 `/mode` 通过 `Session.SetMode` 保存模式快照；`/execute` 通过 `Session.ApprovePlan` 读取实际计划文件、记录批准内容，再提交执行任务。

## Agent 的核心循环

下图对应 [`agent.go`](../agent.go) 的 `Agent.Run` 和 [`tool.go`](../tool.go) 的工具执行。一次模型请求算一轮；一轮中的多个工具按回复顺序执行。

```mermaid
flowchart TD
    Start["Agent.Run(ctx, prompt, emit)"] --> Busy{"同一 Agent 已在运行？"}
    Busy -->|是| Rejected["返回 ErrBusy；不修改历史"]
    Busy -->|否| User["标记 running；追加 user 消息<br/>发出 run_start、message_added"]
    User --> Limit{"还有可用轮数？"}
    Limit -->|否| MaxTurns["返回 ErrMaxTurns<br/>StopReason = max_turns"]
    Limit -->|是| Context{"ctx 已取消？"}
    Context -->|是| Aborted["返回取消或截止时间错误"]
    Context -->|否| Request["发出 turn_start<br/>构造 Request：模型、系统提示、历史副本、工具定义"]
    Request --> Encode["可选 RequestEncoder.EncodeRequest<br/>发出 model_request，含 RawRequest"]
    Encode -->|编码失败| Failed["返回错误"]
    Encode -->|成功或未实现接口| Stream["Provider.Stream<br/>Delta 转为文本、推理、工具片段等 Event"]
    Stream -->|请求或流解析失败| Failed
    Stream -->|完整 Response| Length{"StopReason 是 length？"}
    Length -->|是| Truncated["返回 ErrOutputLength"]
    Length -->|否| Validate["校验整批 ToolCall<br/>ID 非空且唯一、名称非空、Arguments 为有效 JSON"]
    Validate -->|无效| Failed
    Validate -->|有效| Assistant["发出 model_response<br/>追加 assistant 消息和完整 ToolCall"]
    Assistant --> Calls{"有工具调用？"}
    Calls -->|否| Answer["返回最终文本<br/>空 StopReason 使用 stop"]
    Calls -->|是| Before{"执行下一工具前，ctx 已取消？"}
    Before -->|是| CancelPending["为本批剩余调用补错误结果消息"]
    Before -->|否| Tool["发出 tool_start<br/>查找工具、校验参数 schema 与模式权限<br/>执行普通工具或内置计划工具"]
    Tool --> Result["追加 tool 消息，关联 ToolCallID<br/>失败设置 IsError；发出 tool_end"]
    Result --> After{"执行后，ctx 已取消？"}
    After -->|是| CancelPending
    After -->|否| More{"本批还有工具？"}
    More -->|是| Before
    More -->|否| Next["轮数递增"]
    Next --> Limit
    CancelPending --> Aborted
    MaxTurns --> Finish["返回前汇总本次新增 Messages<br/>清除 running；发出 run_end"]
    Aborted --> Finish
    Failed --> Finish
    Truncated --> Finish
    Answer --> Finish
```

模型的工具调用片段可能把参数拆成多段，例如先到达 `{"path":`，再到达 `"README.md"}`。这些片段只用于观察；Agent 等待完整响应并校验整批调用后，才追加助手消息和执行工具。

两类失败的处理不同：缺失或重复调用 ID、无效 JSON、流异常和输出截断会终止运行；未知工具、参数不符合 schema、执行函数报错则成为 `IsError=true` 的工具消息，下一轮模型可以修正调用。工具自身的超时也按执行错误处理；运行上下文取消时停止后续调用，并补齐尚未执行的工具结果。

协作模式由 [`collaboration.go`](../collaboration.go) 维护，每轮请求动态组装模式提示和工具声明。规划模式允许 `Tool.ReadOnly` 工具和 `save_plan`，默认模式允许普通工具和 `update_progress`；执行入口重复检查模式，未声明的越界调用也不会执行。`save_plan` 把完整 Markdown 原子写入调用方提供的目录，文件名由 Agent 生成；`update_progress` 保存执行清单。保存计划不退出规划模式。

模式切换不删除对话历史，因此旧助手回复和工具结果仍可能描述此前的规划状态。当前请求的模式指令是实时依据：默认模式显式说明旧规划限制已结束，保存计划仅供参考，最新用户消息决定当前任务，并注明 `/execute` 的有效条件；不能依靠清空历史来切换行为。

规划指令由 [`iota/plans/template.md`](../iota/plans/template.md) 提供，通过 `go:embed` 随程序分发。SDK 未设置 `PlanTemplatePath` 时直接使用打包内容；CLI 将 `PlansDir` 和 `PlanTemplatePath` 分别设为 `~/.iota/plans` 与 `~/.iota/plans/template.md`。进入规划模式时检查并初始化模板，每次规划请求重新读取；缺失时发布完整的默认文件且不覆盖已有模板。读取失败会在调用 Provider 前终止。模板只引导澄清流程和文档结构，不取代工具权限检查；选项和自定义答案沿用正常消息与会话记录。

启用 Session 时，计划工具通过内部的可返回错误的记录函数提交 `mode_changed`、`plan_saved`、`plan_approved`、`progress_updated` 快照，写入并同步 JSONL 后才更新 Agent 状态和通知观察者。文件写入失败不会提交状态；日志记录失败会取消运行，保存工具会尝试恢复之前的文件。普通 `EmitFunc` 仍然只负责通知，不成为扩展接口。

`runtime_context.go` 将最新完整协作状态附加到请求消息末尾，标记为 `RuntimeContext`，不写进 `Agent.messages` 或 `message_added`。每个 run 的首轮默认模式请求额外附带意图判断和任务拆分提示；重试沿用同一轮阶段。system 保留模式、执行和历史查询规则，状态更新不改写长历史前缀。批准状态保存 plan ID 与正文 SHA-256，恢复不依赖模型从摘要猜测授权。

消息通过同一个可返回错误的记录函数写入并同步后再加入 Agent 历史。Session 在写入时分配原始事件 seq、run ID 和活动步骤 ID，并维护只含原始语义消息的历史索引。压缩替换有效上下文，不删除该索引；`history.go` 的历史工具只访问绑定 Session，并分页限制返回字节数。

`compaction_units.go` 将消息组织为完整工具批次，再优先按 run／步骤和输入预算分块。超限时可以压缩近期 run 内的旧批次；原子批次过大时生成有来源标记的首尾摘录，参数仍为有效 JSON。各块顺序更新同一份有总长度限制的摘要。检查点中的来源和分块元数据由程序生成，模型上下文中的来源区间有数量上限，完整元数据留在 JSONL。

`Messages()` 返回完整历史的独立副本；`RunResult.Messages` 只包含本次运行新增的消息。再次调用 `Run` 会沿用已有历史。`Reset()` 清空历史，运行期间返回 `ErrBusy`。达到轮数上限时，最后一轮已返回的工具调用仍会执行，但不再请求下一轮模型。

## 核心结构体与接口

图中的 `OpenAIProvider` 对应 [`openaicompat.Provider`](../provider/openaicompat/provider.go)，用不同名称区分它与 SDK 的 `Provider` 接口。工具执行函数通过 `Tool.Execute` 字段注入；`read`、`write`、`edit`、`bash` 都由构造函数返回同一种 `Tool`。

```mermaid
classDiagram
    direction TB
    class Config {
        +Provider Provider
        +string Model
        +string SystemPrompt
        +Tool[] Tools
        +int MaxTurns
        +Mode Mode
        +string PlansDir
        +string PlanTemplatePath
    }
    class Agent {
        -string model
        -string systemPrompt
        -int maxTurns
        -bool running
        -Message[] messages
        -CollaborationState collaboration
        +Run(ctx, prompt, emit)
        +Messages()
        +Reset()
        +SetMode(mode, emit)
        +ApprovePlan(emit)
        +Collaboration()
    }
    class Provider {
        <<interface>>
        +Stream(ctx, request, emit)
    }
    class RequestEncoder {
        <<interface>>
        +EncodeRequest(request)
    }
    class OpenAIProvider {
        -string endpoint
        -string apiKey
        -http.Client client
        -time.Duration timeout
        +Stream(ctx, request, emit)
        +EncodeRequest(request)
    }
    class compiledTool {
        -Tool tool
        -jsonschema.Schema schema
    }
    class Tool {
        +string Name
        +string Description
        +json.RawMessage Schema
        +func Execute
        +bool ReadOnly
    }
    class Session {
        -os.File file
        -string id
        -string path
        -uint64 seq
        -Message[] messages
        -CollaborationState collaboration
        +Run(ctx, agent, prompt, emit)
        +Restore(agent)
        +Reset(agent)
        +Close()
        +Path()
        +SetMode(agent, mode, emit)
        +ApprovePlan(agent, emit)
    }
    class sessionRecord {
        +int Version
        +uint64 Sequence
        +string Timestamp
        +string SessionID
        +string RunID
        +string Type
        +json.RawMessage Payload
    }
    Config ..> Agent : New 创建
    Config --> Provider : 注入
    Config o-- Tool : Tools
    Agent --> Provider : 请求模型
    Agent ..> RequestEncoder : 可选的请求体记录
    Agent *-- compiledTool : tools
    compiledTool *-- Tool : tool
    OpenAIProvider ..|> Provider
    OpenAIProvider ..|> RequestEncoder
    Session ..> Agent : Run、Restore、Reset 的参数
    Session ..> sessionRecord : 按顺序写入 JSONL
```

`Session` 不持有 Agent，调用方每次将 Agent 传给它。`Session.Run` 会检查 Agent 的历史是否与会话一致，防止把另一段对话追加到当前文件；打开已有文件后必须先 `Restore`。同一 Agent 不允许并发运行，同一 Session 也拒绝同时运行、恢复、重置或关闭。

模型通信和观察使用 [`types.go`](../types.go) 中的以下数据类型：

| 类型 | 主要字段 | 在流程中的作用 |
| --- | --- | --- |
| `Message` | `Role`、`Content`、`ToolCalls`、`ToolCallID`、`ToolName`、`IsError` | 历史中的 user、assistant、tool 消息；工具结果通过调用 ID 配对 |
| `ToolCall` | `ID`、`Name`、`Arguments` | 完整工具调用；参数保留为原始 JSON |
| `ToolDefinition` | `Name`、`Description`、`Schema` | 模型可见的工具说明，不包含本地执行函数 |
| `Request` | `Mode`、`Model`、`SystemPrompt`、`Messages`、`Tools` | 单轮模型请求；模式体现在系统提示和工具集合中，`Mode` 字段用于事件记录 |
| `Response` | `Content`、`ToolCalls`、`StopReason`、`Usage` | Provider 汇总的完整回复，决定返回答案或执行工具 |
| `Delta` | `Text`、`Reasoning`、`ToolCall`、`FinishReason`、`StreamDone`、`Usage`、`RawChunk` | Provider 的流片段，Agent 将其转换成事件 |
| `ToolCallDelta` | `Index`、`ID`、`Name`、`Arguments` | 单个工具调用的片段，Provider 按 Index 拼接 |
| `Usage` | `PromptTokens`、`CompletionTokens`、`TotalTokens` | 服务端返回的令牌用量，可以缺席 |
| `Event` / `EmitFunc` | 事件类型与对应载荷 / 同步回调 | 统一驱动终端展示、会话记录和 SDK 观察者 |
| `RunResult` | `Text`、`Messages`、`Turns`、`StopReason` | 本次运行结果，与返回的 error 一起交给调用方 |
| `SessionInfo` | `Model`、`CWD` | 新会话的来源信息，不用于还原运行配置 |

## 模型通信、事件与保存顺序

下图以启用会话保存、第一轮调用 `read`、第二轮给出答案为例。省略初始化和重复事件，仅展示消息与控制顺序。

```mermaid
sequenceDiagram
    participant Caller as CLI 或 SDK
    participant Session
    participant Agent
    participant Provider as openaicompat.Provider
    participant Tool as read 工具
    Caller->>Session: Run(ctx, agent, prompt, emit)
    Session->>Session: 检查历史一致，生成 run ID
    Session->>Agent: Run(ctx, prompt, 包装后的 emit)
    Agent-->>Session: run_start、message_added(user)
    Session->>Session: 写入 JSONL，再转发给调用方
    Agent-->>Session: turn_start、model_request
    Agent->>Provider: Stream(Request, Delta 回调)
    loop 服务端流片段
        Provider-->>Agent: Delta：文本、推理、工具调用片段等
        Agent-->>Session: 对应 Event，保留 RawChunk
        Session->>Session: 写入 JSONL，再转发给调用方
    end
    Provider-->>Agent: 完整 Response，包含 read 调用
    Agent->>Agent: 检查截断、校验整批调用
    Agent-->>Session: model_response、message_added(assistant)
    Agent-->>Session: tool_start
    Agent->>Tool: Execute(ctx, Arguments)
    Tool-->>Agent: 文本结果或错误
    Agent-->>Session: message_added(tool)、tool_end
    Agent->>Provider: 下一轮 Stream，包含完整历史与工具结果
    Provider-->>Agent: Delta 回调及最终 Response，无工具调用
    Agent-->>Session: model_response、message_added(assistant)、run_end
    Agent-->>Session: RunResult、error
    Session->>Session: 同步文件，合并运行错误与保存错误
    Session-->>Caller: RunResult、error
```

OpenAI 兼容 Provider 将系统提示放在请求的 system 消息中，编码工具定义，并向 `/chat/completions` 发送流式请求。SSE（服务端逐条推送数据的格式）中的工具参数按 Index 汇总；只有收到 `[DONE]` 且工具调用完整时才返回成功。缺少结束标记、调用索引不连续或参数不是有效 JSON 都会报错。每次 `Stream` 单独应用请求超时。

`finish_reason`、`[DONE]` 和 `model_response` 分别表示服务端的停止原因、流结束标记和 Agent 接受完整回复，它们不是同一个阶段。文本可先展示给用户；只有通过完整响应校验的 assistant 消息才进入可恢复历史。系统提示单独保存在运行配置中，不属于 `Agent.messages`。

`EmitFunc` 同步调用，会阻塞当前运行。启用 Session 后，每条事件先写文件，再转发给调用方；保存失败会保留首个错误并取消运行，返回时合并运行错误与保存错误。`Session.Run` 为 `nil` 接收者时直接调用 `Agent.Run`，保留相同的事件回调。

会话文件每行是一条 `sessionRecord`，包含版本、递增序号、时间、session ID、可选 run ID、类型和载荷。`message_added` 同时更新 Session 的完整消息列表，其他流片段只作为过程记录。`NewSession` 写 `session_start`，`Reset` 写 `session_reset` 并清空两边历史，`Close` 写 `session_end`、同步并关闭文件。

协作事件的 `collaboration` 载荷是完整快照：`mode`、可选的 `plan`（ID、路径、正文）、`approval`（plan ID 与正文哈希）和 `progress`（清单 ID、版本、解释、稳定步骤 ID 及状态）。运行中的计划事件使用该次 run ID，用户切换或批准的事件不启动模型运行。恢复严格校验这些快照，重放后替换两边协作状态。重置同时清空计划、批准和进度，但保留模式和磁盘文件。

## 会话恢复

下图对应 [`session_restore.go`](../session_restore.go) 的 `OpenSession` 和 [`session.go`](../session.go) 的 `Restore`。

```mermaid
flowchart TD
    Open["OpenSession(path)"] --> Read["逐行读取 JSONL"]
    Read --> Valid{"完整记录、合法 JSON、版本与序号正确<br/>首条是 session_start，session ID 一致？"}
    Valid -->|否| Error["关闭文件并返回错误"]
    Valid -->|是| Replay["重放 message_added<br/>遇到 session_reset 清空已恢复历史"]
    Replay --> Pairs{"消息角色、调用和结果顺序有效？"}
    Pairs -->|否| Error
    Pairs -->|是| Seek["移动到文件末尾，准备追加"]
    Seek --> Pending{"末尾有缺失结果的工具调用？"}
    Pending -->|是| Repair["按调用顺序追加 IsError=true 的 tool 消息<br/>说明执行中断；不重跑工具"]
    Pending -->|否| Ready["返回 Session"]
    Repair --> Ready
    Ready --> Restore["Session.Restore(agent)"]
    Restore --> Busy{"Session 忙、Agent 正在运行<br/>或会话已关闭、保存出错？"}
    Busy -->|是| Reject["返回错误"]
    Busy -->|否| History["复制完整消息，替换 Agent 历史"]
    History --> Continue["Session.Run 继续对话，追加到原文件"]
```

例如日志以 `assistant: read(call-1)` 结束，却没有对应的 `tool(call-1)`，打开会话时会补一条中断错误结果。这样下一轮模型能看到完整的调用与结果配对；已经可能产生副作用的工具不会再次执行。

恢复替换消息历史和协作状态。Provider、模型、系统提示、工具和轮数上限由当前 `Config` 决定。计划路径必须属于当前 `PlansDir`，缺失文件可从日志正文恢复，已有普通文件保留人工编辑。CLI 的计划目录属于用户而非工作目录，因此切换目录不改变计划路径；SDK 调用方仍需显式配置匹配的 `PlansDir`。损坏或不完整的记录会导致恢复失败；工具结果必须按助手调用顺序配对。会话文件可以跨多次运行追加，run ID 区分各次输入。

## 对照源码

| 文件 | 对照内容 |
| --- | --- |
| [`cmd/iota/main.go`](../cmd/iota/main.go)、[`options.go`](../cmd/iota/options.go)、[`config.go`](../cmd/iota/config.go) | CLI 配置覆盖、组件初始化、输入和会话选择 |
| [`cmd/iota/execute.go`](../cmd/iota/execute.go)、[`output.go`](../cmd/iota/output.go)、[`interactive.go`](../cmd/iota/interactive.go) | 信号取消、终端颜色与事件分段、交互循环和重置 |
| [`agent.go`](../agent.go)、[`messages.go`](../messages.go) | 核心循环、取消结果补齐、历史副本与重置 |
| [`config.go`](../config.go)、[`tool.go`](../tool.go)、[`types.go`](../types.go) | Agent 构造、工具校验、接口与数据类型 |
| [`collaboration.go`](../collaboration.go)、[`plan_file.go`](../plan_file.go) | 模式状态、计划工具、原子文件保存、用户批准与计划恢复 |
| [`plan_template.go`](../plan_template.go)、[`iota/plans/template.md`](../iota/plans/template.md) | 规划模板的打包、读取、初始化和对话澄清规则 |
| [`provider/openaicompat/provider.go`](../provider/openaicompat/provider.go) | HTTP 编码、流解析、工具片段拼接与请求超时 |
| [`session.go`](../session.go)、[`session_restore.go`](../session_restore.go) | 事件保存、恢复校验、历史同步和关闭 |
| [`tools`](../tools) | 内置 read、write、edit、bash 工具的执行函数 |
