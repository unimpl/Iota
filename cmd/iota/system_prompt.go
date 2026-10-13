package main

import "github.com/unimpl/Iota/internal/app"

const defaultSystemPrompt = app.DefaultSystemPrompt

func loadSystemPrompt(cwd, additional string) (string, error) {
	return app.LoadSystemPrompt(cwd, additional)
}
func loadSystemPromptFrom(cwd, home, additional string) (string, error) {
	return app.LoadSystemPromptFrom(cwd, home, additional)
}
