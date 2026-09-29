// Package iota 提供与模型服务商无关的轻量 Agent 执行循环。
//
// Agent 在内存中保存对话历史。每次 Run 向 Provider 请求回复，按顺序执行已经完整接收
// 且通过校验的工具调用，再将结果发回模型，直到得到最终回复。历史不会自动持久化。
package iota
