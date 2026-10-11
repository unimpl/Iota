package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	iota "github.com/unimpl/Iota"
)

type cliCompactProvider struct{ requests []iota.Request }

func (p *cliCompactProvider) Stream(_ context.Context, request iota.Request, _ func(iota.Delta)) (iota.Response, error) {
	p.requests = append(p.requests, request)
	if strings.Contains(request.Messages[0].Content, "<turns>\n") {
		var inputs []struct {
			ID string `json:"id"`
		}
		_, body, _ := strings.Cut(request.Messages[0].Content, "<turns>\n")
		mapping, _, _ := strings.Cut(body, "\n</turns>")
		if err := json.Unmarshal([]byte(mapping), &inputs); err != nil {
			return iota.Response{}, err
		}
		var entries []struct {
			ID   string `json:"id"`
			Text string `json:"text"`
		}
		for _, input := range inputs {
			entries = append(entries, struct {
				ID   string `json:"id"`
				Text string `json:"text"`
			}{input.ID, "checkpoint"})
		}
		data, err := json.Marshal(struct {
			Turns []struct {
				ID   string `json:"id"`
				Text string `json:"text"`
			} `json:"turns"`
			Important []string `json:"important"`
		}{entries, []string{}})
		if err != nil {
			return iota.Response{}, err
		}
		return iota.Response{Content: string(data), StopReason: "stop"}, nil
	}
	return iota.Response{Content: "checkpoint", StopReason: "stop"}, nil
}

func TestCompactionConfigDefaultOverrideAndNoEnvironment(t *testing.T) {
	for _, value := range []int{0, 1, 5, -1} {
		config := fileConfig{}
		if value != 0 {
			config.KeepRecentTokens = &value
		}
		opts, err := parseOptionsWithConfig(nil, &bytes.Buffer{}, config, func(name string) (string, bool) {
			return "99", name == "IOTA_COMPACTION_KEEP_RECENT_TOKENS"
		})
		if value < 0 {
			if err == nil {
				t.Fatal("accepted negative retention")
			}
			continue
		}
		want := value
		if want == 0 {
			want = iota.DefaultKeepRecentTokens
		}
		if err != nil || opts.keepRecentTokens != want {
			t.Fatalf("value=%d options=%+v err=%v", value, opts, err)
		}
	}
	zero := 0
	if _, err := optionsFromConfig(fileConfig{KeepRecentTokens: &zero}); err == nil {
		t.Fatal("accepted explicitly zero retention")
	}
	home := t.TempDir()
	writeConfig(t, filepath.Join(home, ".iota", "config.toml"), "compaction_keep_recent_tokens = 7\n")
	config, err := loadConfigFrom(home, t.TempDir())
	if err != nil || config.KeepRecentTokens == nil || *config.KeepRecentTokens != 7 {
		t.Fatalf("config=%+v err=%v", config, err)
	}
	ratio := 0.35
	opts, err := optionsFromConfig(fileConfig{CompactionTargetRatio: &ratio})
	if err != nil || opts.compactionTargetRatio != ratio {
		t.Fatalf("target ratio=%v err=%v", opts.compactionTargetRatio, err)
	}
	invalidRatio := 1.0
	if _, err := optionsFromConfig(fileConfig{CompactionTargetRatio: &invalidRatio}); err == nil {
		t.Fatal("accepted a target ratio of 100%")
	}
}

func TestInteractiveCompactFocusIsNotSubmittedAsUserPrompt(t *testing.T) {
	provider := &cliCompactProvider{}
	agent, err := iota.New(iota.Config{Provider: provider, Model: "test", KeepRecentTokens: 60})
	if err != nil {
		t.Fatal(err)
	}
	for _, prompt := range []string{strings.Repeat("old context ", 1000), "recent one", "recent two", "recent three"} {
		if _, err := agent.Run(context.Background(), prompt, nil); err != nil {
			t.Fatal(err)
		}
	}
	recent := agent.Messages()[2:]
	stdin, err := os.CreateTemp(t.TempDir(), "input")
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if _, err := io.WriteString(stdin, "/compact 保留路径 和未完成任务\n/exit\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := interactive(agent, make(chan os.Signal), stdin, &stdout, &stderr, nil, nil); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	if len(provider.requests) != 5 || !strings.Contains(provider.requests[4].Messages[0].Content, "保留路径 和未完成任务") || len(provider.requests[4].Tools) != 0 {
		t.Fatalf("requests=%+v", provider.requests)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "context compacted") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if len(agent.Messages()) != len(recent)+1 || !agent.Messages()[0].ContextSummary {
		t.Fatal("command added a user turn or discarded recent messages")
	}
}

func TestPromptLoaderReadsUpdatedAgentsAfterCompaction(t *testing.T) {
	cwd := t.TempDir()
	path := filepath.Join(cwd, "AGENTS.md")
	writePromptFile(t, path, "original project rule")
	provider := &cliCompactProvider{}
	agent, err := iota.New(iota.Config{Provider: provider, Model: "test", KeepRecentTokens: 40, SystemPromptLoader: func(ctx context.Context) (string, error) { return loadSystemPrompt(cwd, "") }})
	if err != nil {
		t.Fatal(err)
	}
	for _, prompt := range []string{strings.Repeat("old ", 2000), "a", "b", "c"} {
		if _, err := agent.Run(context.Background(), prompt, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := agent.Compact(context.Background(), "focus", nil); err != nil {
		t.Fatal(err)
	}
	writePromptFile(t, path, "updated authoritative rule")
	if _, err := agent.Run(context.Background(), "next", nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(provider.requests[len(provider.requests)-1].SystemPrompt, "updated authoritative rule") {
		t.Fatal("AGENTS instructions were not reloaded")
	}
}
