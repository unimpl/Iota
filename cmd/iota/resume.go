package main

import "github.com/unimpl/Iota/internal/app"

func resolveResumePath(value string) (string, error)     { return app.ResolveResumePath(value) }
func findResumeSession(dir, uuid string) (string, error) { return app.FindResumeSession(dir, uuid) }
