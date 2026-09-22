# iota：精简 Go Agent SDK 与普通 CLI

## 目标与范围

在仓库根目录建立 module `github.com/unimpl/Iota`，根包名为 `iota`，Go 基线为 1.25。SDK 位于仓库根层，`cmd/iota` 只使用公开 SDK。

项目按三个边界组织：

- 根包 `iota`：消息、模型 Provider、工具、事件和 Agent 执行循环。
- `provider/openaicompat`：OpenAI 兼容 Chat Completions 流式协议。
- `tools`：`read`、`write`、`edit`、`bash` 四个编程工具。
- `cmd/iota`：单次调用、标准输入和内存对话三种 CLI 用法。
- `examples`：基本调用和自定义工具示例。

典型执行流程为“用户消息 → 模型工具调用 → 参数校验和工具执行 → 工具结果返回模型 → 最终回答”。第一版不实现 TUI、会话持久化、自动压缩、steering/follow-up、插件、MCP、多 Agent、远程服务、图片或供应商专有推理协议。

## SDK 与执行行为

公共入口包括：

- `New(Config) (*Agent, error)`：注入 Provider、模型、系统提示、工具和轮数限制。
- `Agent.Run(ctx, prompt, emit) (RunResult, error)`：同步完成一次可能包含多个 turn 的运行。
- `Agent.Messages()`：读取历史副本。
- `Agent.Reset()`：仅在空闲时清空对话。
- `Provider.Stream(ctx, Request, emitDelta)`：执行一次模型请求。
- `Tool`：名称、描述、JSON Schema 和执行函数。

执行规则：

- 同一个 Agent 不允许并发运行，冲突返回 `ErrBusy`。
- 完整模型响应返回后才执行工具；同批工具按响应顺序执行。
- 工具 schema 在初始化时编译，参数在产生副作用前校验。
- 未知工具、非法参数和工具错误变成关联原调用 ID 的 tool result。
- 模型错误、协议错误、响应截断、取消或达到轮数限制结束运行。
- 不自动重试模型请求或工具；默认每次运行最多 20 个 turn。
- 取消通过 `context.Context` 传播，并为尚未执行的已登记调用补齐取消结果。
- 流式增量可展示，但失败响应的部分 assistant 消息不进入历史。
- 事件回调同步且有序，不使用后台事件队列。

历史只保存在内存中，不自动压缩或丢弃。SDK 不隐式读取环境变量、文件或终端。

## 模型、工具与 CLI

模型适配使用标准库 HTTP 和 SSE，支持文本、function tool calls、分片参数、停止原因及可选用量。Base URL、API key、模型和 HTTP client 均由调用方提供；默认请求超时 120 秒，不自动重试。工具参数校验使用固定版本的 `github.com/santhosh-tekuri/jsonschema/v6`，并拒绝外部 schema 引用。

内置工具行为：

- `read`：读取 UTF-8 文本，支持 1-based offset 和行数限制。
- `write`：创建或覆盖文件，自动创建父目录。
- `edit`：仅替换唯一的精确文本匹配。
- `bash`：在绑定的 cwd 使用 `bash -c`，返回合并输出、退出码和超时信息。

工具输出默认限制为 2,000 行或 50 KiB；read 保留开头，bash 使用有界缓冲保留末尾。bash 取消或超时时终止进程组。工具使用当前进程权限，不提供沙箱或审批。

CLI 行为：

- `iota -p "任务"` 单次执行。
- 管道输入整体作为一次任务。
- 直接运行进入逐行内存对话，支持 `/reset` 和 `/exit`。
- `-p` 与管道输入冲突时报用法错误。
- 模型文本写 stdout，提示符、工具状态和错误写 stderr。
- 支持模型、服务地址、cwd、系统提示、轮数、超时和工具选择参数。
- 模型优先使用参数，其次 `IOTA_MODEL`；地址读取 `OPENAI_BASE_URL`，凭据读取 `OPENAI_API_KEY`。
- CLI 默认启用四个工具，并读取 cwd 根部的 `AGENTS.md`；不搜索父目录或子目录。

## 验收

测试覆盖直接回答、工具闭环、多次同名调用、未知工具、非法参数、工具失败、分片协议、缺失用量、HTTP 错误、异常 EOF、并发拒绝、轮数限制、取消配对、输出截断、四个工具和 CLI 输入装配。

- `go test ./...`
- `go test -race ./...`
- `go vet ./...`
- 所有 Go 文件通过 `gofmt`

自动测试使用假 Provider 或本地 HTTP 服务，不访问真实模型服务。项目不自动提交或推送。
