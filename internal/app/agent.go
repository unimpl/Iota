package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	iota "github.com/unimpl/Iota"
	"github.com/unimpl/Iota/provider/openaicompat"
)

// NewAgent builds identical model, tool and instruction defaults for both frontends.
// home is explicit so a server does not accidentally read another user's resources.
func NewAgent(settings Settings, home string, input iota.UserInputHandler, status func(string)) (*iota.Agent, error) {
	cwd, err := filepath.Abs(settings.CWD)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(cwd)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("invalid working directory %q", cwd)
	}
	if settings.MaxTurns <= 0 || settings.Timeout <= 0 {
		return nil, errors.New("max turns and timeout must be positive")
	}
	prompt, err := LoadSystemPromptFrom(cwd, home, settings.System)
	if err != nil {
		return nil, err
	}
	selected, err := CreateTools(cwd, settings.Tools, settings.Timeout, status)
	if err != nil {
		return nil, err
	}
	provider, err := openaicompat.New(openaicompat.Config{BaseURL: settings.BaseURL, APIKey: settings.APIKey, Timeout: settings.Timeout})
	if err != nil {
		return nil, err
	}
	plans := filepath.Join(home, ".iota", "plans")
	return iota.New(iota.Config{
		Provider: provider, Model: settings.Model, SystemPrompt: prompt, Tools: selected,
		MaxTurns: settings.MaxTurns, Mode: iota.Mode(settings.Mode), PlansDir: plans,
		PlanTemplatePath: filepath.Join(plans, "template.md"), SkillsDir: filepath.Join(home, ".iota", "skills"),
		KeepRecentTokens: settings.KeepRecentTokens, CompactionTargetRatio: settings.CompactionTargetRatio,
		UserInputHandler: input,
		SystemPromptLoader: func(ctx context.Context) (string, error) {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			return LoadSystemPromptFrom(cwd, home, settings.System)
		},
	})
}
