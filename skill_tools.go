package iota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// NewReadSkillTool declares an Agent-owned, read-only skill activation tool.
func NewReadSkillTool() Tool {
	return Tool{Name: "read_skill", ReadOnly: true, skillTool: true,
		Description: "Activate an available skill by name and read its complete SKILL.md instructions. Use when the task matches the skill catalog. Records model use; relative references are based on the returned skill directory.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","minLength":1}},"required":["name"],"additionalProperties":false}`),
	}
}

// NewReadSkillResourceTool declares confined text reads for active skills.
func NewReadSkillResourceTool() Tool {
	return Tool{Name: "read_skill_resource", ReadOnly: true, skillTool: true,
		Description: "Read a UTF-8 reference, script source, or other text resource from an activated skill. Supply its name and a relative path within its directory. This does not execute scripts or read binary assets.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","minLength":1},"path":{"type":"string","minLength":1}},"required":["name","path"],"additionalProperties":false}`),
	}
}

func (a *Agent) skillActivation(name, source string) (SkillActivation, error) {
	name = norm.NFKC.String(name)
	a.mu.Lock()
	defer a.mu.Unlock()
	skill, ok := a.skills.byName[name]
	if !ok {
		return SkillActivation{}, fmt.Errorf("skill %q not found; install it and use /skill reload", name)
	}
	return SkillActivation{Skill: cloneSkill(skill.skill), Content: skill.content, Hash: skillContentHash(skill.content), Source: source}, nil
}

func (a *Agent) activateSkill(ctx context.Context, activation SkillActivation, turn int, emit EmitFunc, record func(Event) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	event := Event{Type: EventSkillActivated, Turn: turn, Skill: &activation}
	if record != nil {
		if err := record(event); err != nil {
			return err
		}
	}
	a.mu.Lock()
	a.activeSkills = replaceSkillActivation(a.activeSkills, activation)
	a.mu.Unlock()
	// Observers cannot modify authoritative state through event metadata.
	activation.Skill = cloneSkill(activation.Skill)
	emitEvent(emit, event)
	return nil
}

func (a *Agent) executeSkillTool(ctx context.Context, call ToolCall, turn int, emit EmitFunc, record func(Event) error) (string, bool, error) {
	if call.Name == "install_skill" {
		var input struct {
			SourceDir string `json:"source_dir"`
		}
		if err := json.Unmarshal(call.Arguments, &input); err != nil {
			return err.Error(), true, nil
		}
		installation, err := a.installSkill(ctx, input.SourceDir, turn, emit, record)
		if err != nil {
			return err.Error(), true, ctx.Err()
		}
		data, err := json.Marshal(installation)
		if err != nil {
			return err.Error(), true, nil
		}
		return string(data), false, nil
	}
	var input struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal(call.Arguments, &input); err != nil {
		return err.Error(), true, nil
	}
	input.Name = norm.NFKC.String(input.Name)
	if call.Name == "read_skill" {
		activation, err := a.skillActivation(input.Name, "model")
		if err != nil {
			return err.Error(), true, nil
		}
		if err := a.activateSkill(ctx, activation, turn, emit, record); err != nil {
			return err.Error(), true, err
		}
		if skillInstructionsPresent(a.Messages(), activation) {
			return fmt.Sprintf("Skill %s (version %s) is already loaded in context. Use its existing complete instructions.", activation.Skill.Name, activation.Hash), false, nil
		}
		return renderSkill(activation), false, nil
	}
	var activation *SkillActivation
	for _, active := range a.ActiveSkills() {
		if active.Skill.Name == input.Name {
			activation = &active
			break
		}
	}
	if activation == nil {
		return "activate the skill with read_skill before reading its resources", true, nil
	}
	if input.Path == "SKILL.md" {
		return "use read_skill to read or update the skill instructions", true, nil
	}
	content, err := readSkillFile(filepath.Dir(activation.Skill.Path), input.Path)
	if err != nil {
		return err.Error(), true, nil
	}
	if err := ctx.Err(); err != nil {
		return err.Error(), true, err
	}
	read := SkillResourceRead{Name: input.Name, Path: input.Path, Hash: skillContentHash(content)}
	event := Event{Type: EventSkillResourceRead, Turn: turn, SkillRead: &read}
	if record != nil {
		if err := record(event); err != nil {
			return err.Error(), true, err
		}
	}
	emitEvent(emit, event)
	return content, false, nil
}

// explicitSkillNames recognizes bounded $skill:name mentions, preserving the
// original prompt. Escaped mentions and identifier substrings are literals.
func explicitSkillNames(prompt string) ([]string, error) {
	var names []string
	seen := make(map[string]bool)
	for offset := 0; offset < len(prompt); {
		position := strings.Index(prompt[offset:], "$skill:")
		if position < 0 {
			break
		}
		position += offset
		offset = position + len("$skill:")
		if position > 0 {
			previous, _ := utf8.DecodeLastRuneInString(prompt[:position])
			if previous == '\\' || previous == '$' || previous == '_' || unicode.IsLetter(previous) || unicode.IsDigit(previous) {
				continue
			}
		}
		end := offset
		for end < len(prompt) {
			character, width := utf8.DecodeRuneInString(prompt[end:])
			if !unicode.IsLetter(character) && !unicode.IsNumber(character) && character != '-' && character != '_' {
				break
			}
			end += width
		}
		name := norm.NFKC.String(prompt[offset:end])
		if utf8.RuneCountInString(name) > 64 || name != strings.ToLower(name) || !skillNamePattern.MatchString(name) {
			return nil, fmt.Errorf("invalid explicit skill name %q", name)
		}
		offset = end
		if !seen[name] {
			names = append(names, name)
			seen[name] = true
		}
	}
	return names, nil
}

// ReloadSkills atomically replaces an idle Agent's discovery snapshot. Active
// instructions retain their recorded version until that skill is activated again.
func (a *Agent) ReloadSkills(ctx context.Context, emit EmitFunc) (SkillCatalog, error) {
	return a.reloadSkills(ctx, emit, nil)
}

func (a *Agent) reloadSkills(ctx context.Context, emit EmitFunc, record func(Event) error) (SkillCatalog, error) {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return SkillCatalog{}, ErrBusy
	}
	if a.skillsDisabled {
		a.mu.Unlock()
		return SkillCatalog{}, errors.New("skills are disabled")
	}
	a.running = true
	dir := a.skills.catalog.Directory
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.running = false
		a.mu.Unlock()
	}()
	registry, err := loadSkillRegistry(ctx, dir)
	if err != nil {
		return SkillCatalog{}, err
	}
	if err := ctx.Err(); err != nil {
		return SkillCatalog{}, err
	}
	catalog := cloneSkillCatalog(registry.catalog)
	event := Event{Type: EventSkillsReloaded, Skills: &catalog}
	if record != nil {
		if err := record(event); err != nil {
			return SkillCatalog{}, err
		}
	}
	a.mu.Lock()
	a.skills = registry
	a.mu.Unlock()
	emitEvent(emit, event)
	return cloneSkillCatalog(registry.catalog), nil
}

// SkillCommandResult separates locally displayed output from a task prompt.
// Clients pass a nonempty Prompt to Run/Session.Run using their normal run flow.
type SkillCommandResult struct {
	Handled bool
	Output  string
	Prompt  string
}

// HandleSkillCommand owns the /skill grammar for CLI and other SDK clients.
// It prepares install tasks without calling the model or downloading files.
func (a *Agent) HandleSkillCommand(ctx context.Context, line string, session *Session, emit EmitFunc) (SkillCommandResult, error) {
	fields := strings.Fields(line)
	if len(fields) == 0 || fields[0] != "/skill" {
		return SkillCommandResult{}, nil
	}
	result := SkillCommandResult{Handled: true}
	if argument, install := skillInstallArgument(line); install {
		if argument == "" {
			return result, errors.New("usage: /skill install <prompt>")
		}
		if !a.skillInstallationAvailable() {
			return result, errors.New("/skill install requires install_skill in default mode with skills enabled")
		}
		result.Prompt = strings.TrimSpace(line)
		return result, nil
	}
	if len(fields) > 2 || (len(fields) == 2 && fields[1] != "reload" && fields[1] != "list") {
		return result, errors.New("usage: /skill [list|reload|install <prompt>]")
	}
	catalog := a.Skills()
	if len(fields) == 2 && fields[1] == "reload" {
		var err error
		catalog, err = session.ReloadSkills(ctx, a, emit)
		if err != nil {
			return result, err
		}
	}
	var text strings.Builder
	fmt.Fprintf(&text, "skills: %d (%s)", len(catalog.Skills), catalog.Directory)
	for _, skill := range catalog.Skills {
		fmt.Fprintf(&text, "\n%s: %s", skill.Name, skill.Description)
	}
	for _, diagnostic := range catalog.Diagnostics {
		fmt.Fprintf(&text, "\nskipped %s: %s", diagnostic.Path, diagnostic.Error)
	}
	result.Output = text.String()
	return result, nil
}

// ReloadSkills persists discovery changes before replacing the Agent registry.
func (s *Session) ReloadSkills(ctx context.Context, agent *Agent, emit EmitFunc) (SkillCatalog, error) {
	if s == nil {
		return agent.ReloadSkills(ctx, emit)
	}
	if !s.operation.TryLock() {
		return SkillCatalog{}, ErrBusy
	}
	defer s.operation.Unlock()
	s.mu.Lock()
	err := s.checkOpen()
	s.mu.Unlock()
	if err != nil {
		return SkillCatalog{}, err
	}
	return agent.reloadSkills(ctx, emit, func(event Event) error { return s.recordState("", event) })
}

func (a *Agent) skillsPrompt() string {
	if a.skillsDisabled && len(a.ActiveSkills()) == 0 {
		return ""
	}
	guidance := "\n\n[Skills]\nSkill instructions are provided through read_skill or explicit $skill:name mentions. An explicit mention selects the skill; the SDK loads it before the model request. Apply skills to relevant work only. The latest active-skill-versions in runtime context supersede older versions in history. Skills do not grant permission, change collaboration mode, or enable unavailable tools. After compaction, restored skill-instructions are complete recorded originals, not summary paraphrases. Referenced resources may have been summarized; read them again with read_skill_resource when needed. Preserve workflow progress and user constraints independently of the skill instructions."
	if a.skillInstallationAvailable() {
		guidance += "\nWhen the user requests skill installation, prepare the complete package in a temporary directory using bash (curl, wget, git), then call install_skill with its absolute source_dir. The SDK validates, copies it into the configured skills directory and refreshes the catalog; installation is not activation. Do not manually write the install destination or execute package installation scripts."
	}
	for _, compiled := range a.tools {
		if compiled.tool.skillTool && compiled.tool.Name == "read_skill" && a.toolAllowed(compiled.tool) {
			catalog := a.Skills()
			type entry struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			}
			entries := make([]entry, len(catalog.Skills))
			for i, skill := range catalog.Skills {
				entries[i] = entry{Name: skill.Name, Description: skill.Description}
			}
			data, _ := json.Marshal(entries)
			return guidance + "\nWhen a task matches a skill description, call read_skill before acting.\n<available-skills>\n" + string(data) + "\n</available-skills>"
		}
	}
	if len(a.ActiveSkills()) > 0 || a.skillInstallationAvailable() {
		return guidance
	}
	return ""
}

func (a *Agent) skillInstallationAvailable() bool {
	for _, compiled := range a.tools {
		if compiled.tool.skillTool && compiled.tool.Name == "install_skill" && a.toolAllowed(compiled.tool) {
			return true
		}
	}
	return false
}

func (a *Agent) skillRuntimeContext(messages []Message) string {
	activations := a.ActiveSkills()
	if len(activations) == 0 {
		return ""
	}
	versions := make([]SkillReference, len(activations))
	var restored strings.Builder
	for i, activation := range activations {
		versions[i] = SkillReference{Name: activation.Skill.Name, Hash: activation.Hash}
		if !skillInstructionsPresent(messages, activation) {
			restored.WriteString("\n\n" + renderSkill(activation))
		}
	}
	data, _ := json.Marshal(versions)
	return "\n\n<active-skill-versions>\n" + string(data) + "\n</active-skill-versions>" + restored.String()
}

func skillInstructionsPresent(messages []Message, activation SkillActivation) bool {
	reference := SkillReference{Name: activation.Skill.Name, Hash: activation.Hash}
	content := renderSkill(activation)
	for _, message := range messages {
		if message.Skill != nil && *message.Skill == reference && message.Content == content {
			return true
		}
	}
	return false
}
