# iota

iota 是一个精简的 Go Agent SDK，以及使用该 SDK 构建的普通命令行编程助手。它保留 Agent 的必要闭环：请求模型、校验并执行工具、把工具结果交回模型，再得到最终回答。

项目只支持 OpenAI 兼容 Chat Completions 协议。对话保存在内存中，不包含 TUI、会话持久化、自动压缩、插件、MCP 或多 Agent。

## 安装与 CLI

```sh
go install github.com/unimpl/Iota/cmd/iota@latest
```

至少配置模型名。官方 OpenAI 地址是默认地址，其他兼容服务可设置 `OPENAI_BASE_URL`：

```sh
export IOTA_MODEL=gpt-5-mini
export OPENAI_API_KEY=...

iota -p "解释这个仓库的结构"
printf '列出当前目录中的 Go 文件' | iota
iota
```

交互模式逐行接收任务，支持 `/reset` 和 `/exit`。模型文本写入 stdout，提示符、工具状态和错误写入 stderr。

主要选项：

```text
--model       模型名，默认读取 IOTA_MODEL
--base-url    API 根地址，默认读取 OPENAI_BASE_URL
--cwd         工具工作目录
--system      附加系统提示
--max-turns   每次运行最多模型轮数，默认 20
--timeout     每次模型请求及 bash 命令的默认超时，默认 2m
--tools       read,write,edit,bash 的逗号列表；none 表示禁用
```

CLI 默认启用四个工具，并把工作目录根部的 `AGENTS.md` 加入系统提示。工具使用当前进程权限；项目不提供沙箱或操作审批。

## SDK

```go
provider, err := openaicompat.New(openaicompat.Config{
    APIKey: os.Getenv("OPENAI_API_KEY"),
})
if err != nil {
    log.Fatal(err)
}

agent, err := iota.New(iota.Config{
    Provider: provider,
    Model:    os.Getenv("IOTA_MODEL"),
    Tools: []iota.Tool{
        tools.NewRead("."),
        tools.NewWrite("."),
        tools.NewEdit("."),
        tools.NewBash(".", 2*time.Minute),
    },
})
if err != nil {
    log.Fatal(err)
}

result, err := agent.Run(context.Background(), "解释 README", func(event iota.Event) {
    if event.Type == iota.EventTextDelta {
        fmt.Print(event.Text)
    }
})
```

SDK 不读取环境变量、`AGENTS.md` 或终端。调用者显式注入 Provider、模型、系统提示和工具。SDK 默认不注册工具。

`Run` 同步等待一次完整运行，同一个 Agent 不允许并发运行。工具调用只在完整模型响应返回后执行；未知工具、参数错误和工具失败会作为 tool result 交回模型。模型请求错误、响应异常结束、输出截断、取消或达到轮数上限会停止运行。

参见 [`examples/basic`](examples/basic) 和 [`examples/custom-tool`](examples/custom-tool)。

## 开发

```sh
go test ./...
go test -race ./...
go vet ./...
```

测试只使用假 Provider 和本地 HTTP 服务，不请求真实模型。
