# Session SDK 示例

在仓库根目录运行，模型和兼容服务配置与 [`basic`](../basic) 示例相同：

```sh
export IOTA_MODEL=your-model
export OPENAI_API_KEY=your-api-key
# 使用其他兼容服务时设置 OPENAI_BASE_URL。
go run ./examples/session -dir ./sessions
```

程序向模型发送两次请求：先要求记住数字 `42`，关闭会话，再用一个新 Agent 恢复历史并询问该数字。标准输出显示两次回复，标准错误显示会话文件路径和恢复的消息数。

[`main.go`](main.go) 展示的 SDK 调用顺序为：

1. `NewSession` 创建 JSONL 文件，`Session.Run` 保存第一段对话。
2. `Session.Close` 同步并关闭文件。
3. `iota.New` 创建没有历史的新 Agent，`OpenSession` 打开原文件。
4. `Session.Restore` 将文件中的完整消息复制到新 Agent。
5. `Session.Run` 继续对话并追加到原文件，退出时再次关闭会话。

示例保留会话文件。恢复已有会话时，Provider、模型、系统提示和工具由调用方重新配置；`Restore` 只恢复消息历史。关闭会话的延迟调用也会在运行失败时执行，关闭错误与运行错误一起返回。
