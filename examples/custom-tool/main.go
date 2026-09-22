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

func main() {
	provider, err := openaicompat.New(openaicompat.Config{APIKey: os.Getenv("OPENAI_API_KEY")})
	if err != nil {
		log.Fatal(err)
	}
	weather := iota.Tool{
		Name:        "weather",
		Description: "Return a sample weather report for a city.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false}`),
		Execute: func(_ context.Context, arguments json.RawMessage) (string, error) {
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
