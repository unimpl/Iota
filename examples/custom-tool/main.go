package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"

	iota "github.com/unimpl/Iota"
	"github.com/unimpl/Iota/provider/openaicompat"
)

// main 展示如何提供本地工具并让 Agent 把工具结果送回模型。
// 此处天气数据是固定示例，不会访问真实天气服务。
func main() {
	provider, err := openaicompat.New(openaicompat.Config{APIKey: os.Getenv("OPENAI_API_KEY")})
	if err != nil {
		log.Fatal(err)
	}
	weather := iota.Tool{
		Name:        "weather",
		Description: "Return a sample weather report for a city.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false}`),
		// 每次调用单独解析参数，再返回示例数据；无共享状态。
		Execute: func(_ context.Context, arguments json.RawMessage) (string, error) {
			// input 对应工具 schema 中必需的 city 字段。
			var input struct {
				City string `json:"city"`
			}
			if err := json.Unmarshal(arguments, &input); err != nil {
				return "", err
			}
			return input.City + ": sunny, 22 C", nil
		},
	}
	agent, err := iota.New(iota.Config{Provider: provider, Model: os.Getenv("IOTA_MODEL"), Tools: []iota.Tool{weather}})
	if err != nil {
		log.Fatal(err)
	}
	result, err := agent.Run(context.Background(), "What is the weather in Shanghai?", nil)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(result.Text)
}
