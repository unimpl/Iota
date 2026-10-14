package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	iota "github.com/unimpl/Iota"
	"github.com/unimpl/Iota/internal/app"
)

var requestID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

type commandRequest struct {
	ID       string `json:"request_id"`
	Action   string `json:"action"`
	Prompt   string `json:"prompt,omitempty"`
	Mode     string `json:"mode,omitempty"`
	PlanHash string `json:"plan_hash,omitempty"`
}

type commandResult struct {
	ID       string `json:"request_id"`
	Status   string `json:"status"`
	Error    string `json:"error,omitempty"`
	Output   string `json:"output,omitempty"`
	Action   string `json:"action"`
	Prompt   string `json:"prompt,omitempty"`
	Mode     string `json:"mode,omitempty"`
	PlanHash string `json:"plan_hash,omitempty"`
}

func (s *Server) command(w http.ResponseWriter, r *http.Request) {
	var command commandRequest
	if !decode(w, r, &command) {
		return
	}
	if !requestID.MatchString(command.ID) {
		fail(w, 400, errors.New("request_id must contain 1-128 letters, digits, underscores or hyphens"))
		return
	}
	switch command.Action {
	case "run", "skill_install":
		if strings.TrimSpace(command.Prompt) == "" {
			fail(w, 400, errors.New("prompt is required"))
			return
		}
	case "mode":
		if command.Mode != "plan" && command.Mode != "default" {
			fail(w, 400, errors.New("mode must be plan or default"))
			return
		}
	case "reset", "compact", "execute", "skills", "skills_reload":
	default:
		fail(w, 400, errors.New("unknown command action"))
		return
	}
	owned := s.owned(w, r)
	if owned == nil {
		return
	}
	owned.mu.Lock()
	defer owned.mu.Unlock()
	if owned.released || s.ctx.Err() != nil {
		fail(w, 409, errors.New("session is closed"))
		return
	}
	if previous, exists := owned.requests[command.ID]; exists {
		if previous.Action != command.Action || previous.Prompt != command.Prompt || previous.Mode != command.Mode || previous.PlanHash != command.PlanHash {
			fail(w, 409, errors.New("request_id was already used for a different command"))
			return
		}
		sendJSON(w, 200, previous)
		return
	}
	if owned.running {
		fail(w, 409, iota.ErrBusy)
		return
	}
	if command.Action == "execute" && (owned.agent.Mode() != iota.ModePlan || owned.agent.Collaboration().Plan == nil) {
		fail(w, 409, errors.New("execution requires plan mode and a saved plan"))
		return
	}
	payload, err := json.Marshal(command)
	if err == nil {
		err = owned.session.AppendMetadata("client_request", payload)
	}
	if err != nil {
		fail(w, 500, err)
		return
	}
	result := commandResult{ID: command.ID, Status: "running", Action: command.Action, Prompt: command.Prompt, Mode: command.Mode, PlanHash: command.PlanHash}
	owned.requests[command.ID] = result
	owned.operation, owned.status, owned.lastError = command.ID, "running", ""
	owned.running = true
	ctx, cancel := context.WithCancel(s.ctx)
	owned.cancel = cancel
	owned.done = make(chan struct{})
	owned.notify()
	go s.execute(ctx, owned, command)
	sendJSON(w, http.StatusAccepted, result)
}

func (s *Server) execute(ctx context.Context, owned *workspace, command commandRequest) {
	var output string
	var err error
	switch command.Action {
	case "run":
		output, err = s.runPrompt(ctx, owned, command.Prompt, command.PlanHash)
	case "reset":
		err = owned.session.Reset(owned.agent)
	case "compact":
		_, err = owned.session.Compact(ctx, owned.agent, command.Prompt, owned.publish)
	case "mode":
		err = owned.session.SetMode(owned.agent, iota.Mode(command.Mode), owned.publish)
	case "execute":
		var plan iota.SavedPlan
		plan, err = owned.session.ApprovePlanVersion(owned.agent, command.PlanHash, owned.publish)
		if err == nil {
			_, err = owned.session.Run(ctx, owned.agent, app.ApprovedPlanPrompt(plan), owned.publish)
		}
	case "skills", "skills_reload", "skill_install":
		line := "/skill list"
		if command.Action == "skills_reload" {
			line = "/skill reload"
		}
		if command.Action == "skill_install" {
			line = "/skill install " + command.Prompt
		}
		var result iota.SkillCommandResult
		result, err = owned.agent.HandleSkillCommand(ctx, line, owned.session, owned.publish)
		output = result.Output
		if err == nil && result.Prompt != "" {
			_, err = owned.session.Run(ctx, owned.agent, result.Prompt, owned.publish)
		}
	}
	result := commandResult{ID: command.ID, Status: "completed", Output: output, Action: command.Action, Prompt: command.Prompt, Mode: command.Mode, PlanHash: command.PlanHash}
	if err != nil {
		result.Status, result.Error = "failed", err.Error()
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			result.Status = "aborted"
		}
	}
	payload, marshalErr := json.Marshal(result)
	if marshalErr == nil {
		marshalErr = owned.session.AppendMetadata("client_result", payload)
	}
	if marshalErr != nil {
		result.Status = "failed"
		result.Error = errors.Join(err, marshalErr).Error()
	}
	owned.mu.Lock()
	defer owned.mu.Unlock()
	owned.cancel()
	owned.cancel = nil
	owned.pending = nil
	owned.requests[command.ID] = result
	owned.status, owned.lastError = result.Status, result.Error
	owned.running = false
	close(owned.done)
	owned.notify()
}

func (s *Server) runPrompt(ctx context.Context, owned *workspace, prompt, planHash string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	line := strings.TrimSpace(prompt)
	if line == "/execute" {
		plan, err := owned.session.ApprovePlanVersion(owned.agent, planHash, owned.publish)
		if err != nil {
			return "", err
		}
		_, err = owned.session.Run(ctx, owned.agent, app.ApprovedPlanPrompt(plan), owned.publish)
		return "", err
	}
	if line == "/reset" {
		return "", owned.session.Reset(owned.agent)
	}
	fields := strings.Fields(line)
	if len(fields) > 0 && fields[0] == "/compact" {
		_, err := owned.session.Compact(ctx, owned.agent, strings.TrimSpace(strings.TrimPrefix(line, "/compact")), owned.publish)
		return "", err
	}
	skill, err := owned.agent.HandleSkillCommand(ctx, line, owned.session, owned.publish)
	if skill.Handled {
		if err == nil && skill.Prompt != "" {
			_, err = owned.session.Run(ctx, owned.agent, skill.Prompt, owned.publish)
		}
		return skill.Output, err
	}
	handled, next, output, err := app.PlanningCommand(line, owned.agent, owned.session, owned.publish)
	if handled {
		if err == nil && next != "" {
			_, err = owned.session.Run(ctx, owned.agent, next, owned.publish)
		}
		return output, err
	}
	_, err = owned.session.Run(ctx, owned.agent, prompt, owned.publish)
	return "", err
}

func (owned *workspace) userInput(ctx context.Context, request iota.UserInputRequest) (iota.UserInputResponse, error) {
	owned.mu.Lock()
	owned.inputCounter++
	pending := &pendingInput{ID: fmt.Sprintf("%s-%d", owned.operation, owned.inputCounter), Request: request, CallIndex: request.CallIndex, CallCount: request.CallCount, answers: make(chan iota.UserInputResponse, 1)}
	owned.pending = pending
	owned.status = "waiting_input"
	owned.notify()
	owned.mu.Unlock()
	defer func() {
		owned.mu.Lock()
		owned.pending = nil
		if owned.status != "cancelling" {
			owned.status = "running"
		}
		owned.notify()
		owned.mu.Unlock()
	}()
	select {
	case <-ctx.Done():
		return iota.UserInputResponse{Cancelled: true}, ctx.Err()
	case response := <-pending.answers:
		return response, nil
	}
}

func (s *Server) stop(w http.ResponseWriter, r *http.Request) {
	owned := s.owned(w, r)
	if owned == nil {
		return
	}
	owned.mu.Lock()
	defer owned.mu.Unlock()
	if owned.running && owned.cancel != nil {
		owned.status = "cancelling"
		owned.cancel()
		owned.notify()
	}
	w.WriteHeader(204)
}

func (s *Server) answer(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ID       string                 `json:"id"`
		Response iota.UserInputResponse `json:"response"`
	}
	if !decode(w, r, &input) {
		return
	}
	owned := s.owned(w, r)
	if owned == nil {
		return
	}
	owned.mu.Lock()
	defer owned.mu.Unlock()
	pending := owned.pending
	if pending == nil || pending.ID != input.ID || owned.status != "waiting_input" {
		fail(w, 409, errors.New("question is no longer waiting for an answer"))
		return
	}
	if err := validateAnswers(pending.Request, input.Response); err != nil {
		fail(w, 400, err)
		return
	}
	pending.answers <- input.Response
	owned.pending = nil
	owned.status = "running"
	owned.notify()
	w.WriteHeader(204)
}

func validateAnswers(request iota.UserInputRequest, response iota.UserInputResponse) error {
	if response.Cancelled {
		return errors.New("use the stop operation to cancel")
	}
	if len(response.Answers) != len(request.Questions) {
		return errors.New("answer every question explicitly")
	}
	for _, question := range request.Questions {
		answer, exists := response.Answers[question.ID]
		if !exists {
			return fmt.Errorf("missing answer for %s", question.ID)
		}
		valid := false
		switch answer.Source {
		case "custom":
			valid = answer.OptionID == "" && strings.TrimSpace(answer.Value) != ""
		case "unanswered":
			valid = request.Format == iota.UserInputFreeform && answer.OptionID == "" && answer.Value == ""
		case "selected", "recommended":
			for _, option := range question.Options {
				if request.Format == iota.UserInputStructured && answer.OptionID == option.ID && (answer.Source != "recommended" || option.ID == question.RecommendedOptionID) {
					valid = true
				}
			}
		}
		if !valid {
			return fmt.Errorf("invalid answer for %s", question.ID)
		}
	}
	return nil
}
