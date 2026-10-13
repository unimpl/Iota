package main

import (
	iota "github.com/unimpl/Iota"
	"github.com/unimpl/Iota/internal/app"
	"time"
)

func createTools(cwd, list string, timeout time.Duration, onStatus ...func(string)) ([]iota.Tool, error) {
	return app.CreateTools(cwd, list, timeout, onStatus...)
}
