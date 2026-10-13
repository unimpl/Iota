package main

import "github.com/unimpl/Iota/internal/app"

const defaultTools = app.DefaultTools

type fileConfig = app.FileConfig

func loadConfigFrom(home, cwd string) (fileConfig, error) { return app.LoadConfigFrom(home, cwd) }
func optionsFromConfig(config fileConfig) (options, error) {
	s, err := app.SettingsFromConfig(config)
	return optionsFromSettings(s), err
}
func applyEnvironment(opts *options, lookup func(string) (string, bool)) error {
	s := sharedSettings(*opts)
	err := app.ApplyEnvironment(&s, lookup)
	updated := optionsFromSettings(s)
	updated.prompt, updated.resumeRequested = opts.prompt, opts.resumeRequested
	*opts = updated
	return err
}
func sharedSettings(opts options) app.Settings {
	return app.Settings{
		Model:                 opts.model,
		BaseURL:               opts.baseURL,
		APIKey:                opts.apiKey,
		CWD:                   opts.cwd,
		System:                opts.system,
		Tools:                 opts.tools,
		MaxTurns:              opts.maxTurns,
		Timeout:               opts.timeout,
		Resume:                opts.resume,
		Mode:                  opts.mode,
		ModeRequested:         opts.modeRequested,
		KeepRecentTokens:      opts.keepRecentTokens,
		CompactionTargetRatio: opts.compactionTargetRatio,
		NoSession:             opts.noSession,
	}
}
func optionsFromSettings(s app.Settings) options {
	return options{
		model:                 s.Model,
		baseURL:               s.BaseURL,
		apiKey:                s.APIKey,
		cwd:                   s.CWD,
		system:                s.System,
		tools:                 s.Tools,
		maxTurns:              s.MaxTurns,
		timeout:               s.Timeout,
		resume:                s.Resume,
		mode:                  s.Mode,
		modeRequested:         s.ModeRequested,
		keepRecentTokens:      s.KeepRecentTokens,
		compactionTargetRatio: s.CompactionTargetRatio,
		noSession:             s.NoSession,
	}
}
