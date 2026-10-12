package iota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/text/unicode/norm"
)

const maxSkillBundleBytes = 128 * 1024 * 1024
const maxSkillBundleFiles = 4096

// SkillInstallation records copying a prepared local package into SkillsDir.
// It does not activate the skill, execute scripts, or download a source.
type SkillInstallation struct {
	Skill     Skill  `json:"skill"`
	SourceDir string `json:"source_dir"`
	Hash      string `json:"hash"`
}

// NewInstallSkillTool declares an Agent-owned installation tool. bash or the
// caller prepares the complete package in a local directory before calling it.
func NewInstallSkillTool() Tool {
	return Tool{Name: "install_skill", skillTool: true,
		Description: "Install a standard skill from a prepared local directory containing SKILL.md and its bundled resources. First download/extract the complete package into a temporary directory using bash (curl, wget, or git), or use a user-provided local directory. Supply an absolute source_dir. Validates, copies into the configured skills directory, records installation, and refreshes the catalog immediately. Existing skills are never overwritten. This does not download, execute scripts, install dependencies, or activate the skill. Use only when the user requests skill installation; available in default mode.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"source_dir":{"type":"string","minLength":1}},"required":["source_dir"],"additionalProperties":false}`),
	}
}

// InstallSkill installs a package on an idle Agent. Use Session.InstallSkill
// when persistence is enabled. The destination is always owned by the SDK.
func (a *Agent) InstallSkill(ctx context.Context, sourceDir string, emit EmitFunc) (SkillInstallation, error) {
	return a.installSkillIdle(ctx, sourceDir, emit, nil)
}

func (a *Agent) installSkillIdle(ctx context.Context, sourceDir string, emit EmitFunc, record func(Event) error) (SkillInstallation, error) {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return SkillInstallation{}, ErrBusy
	}
	a.running = true
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.running = false
		a.mu.Unlock()
	}()
	return a.installSkill(ctx, sourceDir, 0, emit, record)
}

func (a *Agent) installSkill(ctx context.Context, sourceDir string, turn int, emit EmitFunc, record func(Event) error) (installation SkillInstallation, installErr error) {
	if err := ctx.Err(); err != nil {
		return installation, err
	}
	if a.skillsDisabled || a.Mode() != ModeDefault {
		return installation, errors.New("skill installation requires enabled skills in default mode")
	}
	if !filepath.IsAbs(sourceDir) {
		return installation, errors.New("source_dir must be an absolute local skill directory")
	}
	sourceDir = filepath.Clean(sourceDir)
	source, err := os.OpenRoot(sourceDir)
	if err != nil {
		return installation, fmt.Errorf("open skill source: %w", err)
	}
	defer source.Close()
	content, err := readSkillText(source, "SKILL.md")
	if err != nil {
		return installation, fmt.Errorf("read source SKILL.md: %w", err)
	}
	skill, err := parseSkillMetadata(content)
	if err != nil {
		return installation, err
	}
	dir := a.Skills().Directory
	skill.Path = filepath.Join(dir, skill.Name, "SKILL.md")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return installation, err
	}
	destination, err := os.OpenRoot(dir)
	if err != nil {
		return installation, err
	}
	defer destination.Close()
	sourcePath, err := filepath.EvalSymlinks(sourceDir)
	if err != nil {
		return installation, err
	}
	destinationPath, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return installation, err
	}
	if relative, err := filepath.Rel(sourcePath, destinationPath); err == nil && (relative == "." || filepath.IsLocal(relative)) {
		return installation, errors.New("skills directory cannot be inside the source package")
	}
	entries, err := fs.ReadDir(destination.FS(), ".")
	if err != nil {
		return installation, err
	}
	for _, entry := range entries {
		if norm.NFKC.String(entry.Name()) == skill.Name {
			return installation, fmt.Errorf("skill destination %q already exists; installation never overwrites it", skill.Name)
		}
	}
	id, err := newUUID()
	if err != nil {
		return installation, err
	}
	stageName := ".install-" + id
	if err := destination.Mkdir(stageName, 0o700); err != nil {
		return installation, err
	}
	defer func() { installErr = errors.Join(installErr, destination.RemoveAll(stageName)) }()
	stage, err := destination.OpenRoot(stageName)
	if err != nil {
		return installation, err
	}
	defer stage.Close()
	if err := copySkillResources(ctx, source, stage, int64(len(content))); err != nil {
		return installation, err
	}
	manifest, err := stage.OpenFile("SKILL.md", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return installation, err
	}
	_, writeErr := io.WriteString(manifest, content)
	if err := errors.Join(writeErr, manifest.Close()); err != nil {
		return installation, err
	}
	if err := ctx.Err(); err != nil {
		return installation, err
	}
	// Reserve the destination exclusively. Publish the manifest last so another
	// registry scan never sees a skill whose resources are only partly copied.
	if err := destination.Mkdir(skill.Name, 0o700); err != nil {
		return installation, err
	}
	committed := false
	defer func() {
		if !committed {
			installErr = errors.Join(installErr, destination.RemoveAll(skill.Name))
		}
	}()
	resources, err := fs.ReadDir(stage.FS(), ".")
	if err != nil {
		return installation, err
	}
	for _, resource := range resources {
		if resource.Name() == "SKILL.md" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return installation, err
		}
		if err := destination.Rename(filepath.Join(stageName, resource.Name()), filepath.Join(skill.Name, resource.Name())); err != nil {
			return installation, err
		}
	}
	if err := destination.Rename(filepath.Join(stageName, "SKILL.md"), filepath.Join(skill.Name, "SKILL.md")); err != nil {
		return installation, err
	}
	registry, err := loadSkillRegistry(ctx, dir)
	if err != nil {
		return installation, err
	}
	registered, exists := registry.byName[skill.Name]
	if !exists || registered.content != content || registered.skill.Path != skill.Path {
		return installation, errors.New("installed skill could not be registered")
	}
	installation = SkillInstallation{Skill: cloneSkill(registered.skill), SourceDir: sourceDir, Hash: skillContentHash(content)}
	catalog := cloneSkillCatalog(registry.catalog)
	eventInstallation := installation
	eventInstallation.Skill = cloneSkill(installation.Skill)
	event := Event{Type: EventSkillInstalled, Turn: turn, SkillInstall: &eventInstallation, Skills: &catalog}
	if err := ctx.Err(); err != nil {
		return SkillInstallation{}, err
	}
	if record != nil {
		if err := record(event); err != nil {
			return SkillInstallation{}, err
		}
	}
	a.mu.Lock()
	a.skills = registry
	a.mu.Unlock()
	committed = true
	emitEvent(emit, event)
	return installation, nil
}

func copySkillResources(ctx context.Context, source, destination *os.Root, total int64) error {
	count := 0
	return fs.WalkDir(source.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == "." || path == "SKILL.md" {
			return nil
		}
		if entry.Name() == ".git" {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if count++; count > maxSkillBundleFiles {
			return errors.New("skill package exceeds 4096 files and directories")
		}
		if entry.IsDir() {
			return destination.Mkdir(filepath.FromSlash(path), 0o700)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("skill package contains a link or special file: %s", path)
		}
		input, err := source.Open(filepath.FromSlash(path))
		if err != nil {
			return err
		}
		defer input.Close()
		info, err := input.Stat()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("skill resource is not a regular file: %s", path)
		}
		if info.Size() > maxSkillBundleBytes-total {
			return errors.New("skill package exceeds 128 MiB")
		}
		output, err := destination.OpenFile(filepath.FromSlash(path), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600|info.Mode().Perm()&0o111)
		if err != nil {
			return err
		}
		written, copyErr := io.Copy(output, io.LimitReader(input, maxSkillBundleBytes-total+1))
		closeErr := output.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			return err
		}
		total += written
		if total > maxSkillBundleBytes {
			return errors.New("skill package exceeds 128 MiB")
		}
		return ctx.Err()
	})
}

// InstallSkill saves the installation event and refreshes the Agent catalog.
// Resume replays the event as evidence; it never copies or reinstalls packages.
func (s *Session) InstallSkill(ctx context.Context, agent *Agent, sourceDir string, emit EmitFunc) (SkillInstallation, error) {
	if s == nil {
		return agent.InstallSkill(ctx, sourceDir, emit)
	}
	if !s.operation.TryLock() {
		return SkillInstallation{}, ErrBusy
	}
	defer s.operation.Unlock()
	s.mu.Lock()
	err := s.checkOpen()
	s.mu.Unlock()
	if err != nil {
		return SkillInstallation{}, err
	}
	return agent.installSkillIdle(ctx, sourceDir, emit, func(event Event) error { return s.recordState("", event) })
}

func skillInstallArgument(prompt string) (string, bool) {
	fields := strings.Fields(prompt)
	if len(fields) < 2 || fields[0] != "/skill" || fields[1] != "install" {
		return "", false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(prompt), "/skill"))
	return strings.TrimSpace(strings.TrimPrefix(rest, "install")), true
}

const skillInstallPrompt = `[Skill installation request]
The latest user message requests installation of a standard skill. Follow its prompt to choose the requested source; if needed, use available tools to discover and inspect a suitable existing skill. Use bash with curl, wget, git, and extraction commands to prepare the complete skill directory in a temporary location. Include its scripts, references, and assets, not just SKILL.md. For a user-provided local skill directory, use that directory directly.
Call install_skill with the absolute source_dir containing SKILL.md to validate, copy into the SDK-owned skills directory, and refresh the catalog. Do not manually copy into the destination or use download/install scripts as the installation step. Report the actual installed name, source, and path after observing success. Installation does not activate the skill or start unrelated work. A $skill:name mentioned in this installation request names the desired package; it is not activation of an already-installed skill.`
