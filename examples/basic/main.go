package main

import (
	"context"
	"fmt"
	"log"
	"os"

	iota "github.com/unimpl/Iota"
	"github.com/unimpl/Iota/provider/openaicompat"
)

func main() {
	provider, err := openaicompat.New(openaicompat.Config{
		BaseURL: os.Getenv("OPENAI_BASE_URL"),
		APIKey:  os.Getenv("OPENAI_API_KEY"),
	})
	if err != nil {
		log.Fatal(err)
	}
	agent, err := iota.New(iota.Config{Provider: provider, Model: os.Getenv("IOTA_MODEL")})
	if err != nil {
		log.Fatal(err)
	}
	result, err := agent.Run(context.Background(), "Reply with a short greeting.", func(event iota.Event) {
		if event.Type == iota.EventTextDelta {
			fmt.Print(event.Text)
		}
	})
	if err != nil {
		log.Fatal(err)
	}
	if result.Text != "" {
		fmt.Println()
	}
}
