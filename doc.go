// Package iota provides a small, provider-independent agent execution loop.
//
// An Agent owns an in-memory message history. Each Run asks a Provider for an
// assistant response, executes complete and validated tool calls in order, and
// sends their results back to the Provider until it produces a final response.
package iota
