package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	iota "github.com/unimpl/Iota"
	"github.com/unimpl/Iota/internal/app"
)

type AgentFactory func(app.Settings, string, iota.UserInputHandler, func(string)) (*iota.Agent, error)

type Config struct {
	Settings app.Settings
	Home     string
	// PublicOrigin is the externally visible origin when a gateway terminates TLS.
	PublicOrigin string
	Static       fs.FS
	NewAgent     AgentFactory
}

// Server owns Agent lifetimes, independent of browsers and HTTP requests.
type Server struct {
	config   Config
	folder   string
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	sessions map[string]*workspace
	closing  bool
}

type workspace struct {
	mu           sync.Mutex
	session      *iota.Session
	agent        *iota.Agent
	cwd          string
	released     bool
	running      bool
	cancel       context.CancelFunc
	done         chan struct{}
	status       string
	operation    string
	lastError    string
	requests     map[string]commandResult
	pending      *pendingInput
	inputCounter uint64
}

type pendingInput struct {
	ID        string                `json:"id"`
	Request   iota.UserInputRequest `json:"request"`
	CallIndex int                   `json:"call_index"`
	CallCount int                   `json:"call_count"`
	answers   chan iota.UserInputResponse
}

type State struct {
	Open          bool                     `json:"open"`
	Occupied      bool                     `json:"occupied"`
	Status        string                   `json:"status"`
	RequestID     string                   `json:"request_id,omitempty"`
	Error         string                   `json:"error,omitempty"`
	CWD           string                   `json:"cwd"`
	Model         string                   `json:"model"`
	Collaboration *iota.CollaborationState `json:"collaboration,omitempty"`
	Skills        *iota.SkillCatalog       `json:"skills,omitempty"`
	Pending       *pendingInput            `json:"pending,omitempty"`
	PlanHash      string                   `json:"plan_hash,omitempty"`
	PlanError     string                   `json:"plan_error,omitempty"`
	PlanPreview   *iota.SavedPlan          `json:"plan_preview,omitempty"`
}

func New(config Config) (*Server, error) {
	if config.Home == "" {
		return nil, errors.New("home directory is required")
	}
	if config.PublicOrigin != "" {
		origin, err := url.Parse(config.PublicOrigin)
		if err != nil || (origin.Scheme != "http" && origin.Scheme != "https") || origin.Host == "" || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
			return nil, errors.New("public origin must be an http(s) origin without a path, query or credentials")
		}
	}
	if config.NewAgent == nil {
		config.NewAgent = app.NewAgent
	}
	if config.Settings.Model == "" {
		return nil, errors.New("model is required; use --model or IOTA_MODEL")
	}
	if config.Settings.NoSession {
		return nil, errors.New("web server requires session saving; set save_session=true or IOTA_SAVE_SESSION=true")
	}
	if config.Settings.MaxTurns <= 0 || config.Settings.Timeout <= 0 {
		return nil, errors.New("max turns and timeout must be positive")
	}
	if config.Settings.Mode != "default" && config.Settings.Mode != "plan" {
		return nil, errors.New("mode must be default or plan")
	}
	cwd, err := filepath.Abs(config.Settings.CWD)
	if err != nil {
		return nil, err
	}
	config.Settings.CWD = cwd
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("invalid working directory %q", cwd)
	}
	folder := filepath.Join(config.Home, ".iota", "sessions")
	if err := os.MkdirAll(folder, 0o700); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{config: config, folder: folder, ctx: ctx, cancel: cancel, sessions: make(map[string]*workspace)}, nil
}

// Close cancels every operation, waits for durable cleanup, and releases locks.
func (s *Server) Close() error {
	s.mu.Lock()
	s.closing = true
	s.cancel()
	workspaces := make([]*workspace, 0, len(s.sessions))
	for _, w := range s.sessions {
		workspaces = append(workspaces, w)
	}
	s.mu.Unlock()
	var result error
	for _, w := range workspaces {
		w.mu.Lock()
		done := w.done
		w.mu.Unlock()
		if done != nil {
			<-done
		}
		w.mu.Lock()
		w.released = true
		result = errors.Join(result, w.session.Close())
		w.mu.Unlock()
	}
	return result
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
		sendJSON(w, http.StatusOK, struct {
			Model string `json:"model"`
			CWD   string `json:"cwd"`
			Mode  string `json:"mode"`
		}{s.config.Settings.Model, s.config.Settings.CWD, s.config.Settings.Mode})
	})
	mux.HandleFunc("GET /api/sessions", s.list)
	mux.HandleFunc("POST /api/sessions", s.create)
	mux.HandleFunc("POST /api/sessions/{name}/open", s.open)
	mux.HandleFunc("POST /api/sessions/{name}/close", s.release)
	mux.HandleFunc("DELETE /api/sessions/{name}", s.remove)
	mux.HandleFunc("GET /api/sessions/{name}/state", s.state)
	mux.HandleFunc("GET /api/sessions/{name}/history", s.history)
	mux.HandleFunc("GET /api/sessions/{name}/events", s.events)
	mux.HandleFunc("POST /api/sessions/{name}/commands", s.command)
	mux.HandleFunc("POST /api/sessions/{name}/stop", s.stop)
	mux.HandleFunc("POST /api/sessions/{name}/answers", s.answer)
	if s.config.Static != nil {
		mux.Handle("GET /", http.FileServer(http.FS(s.config.Static)))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if strings.HasPrefix(r.URL.Path, "/api/") && r.Method != http.MethodGet {
			origin := s.config.PublicOrigin
			if origin == "" {
				origin = "http://" + r.Host
			}
			if r.Header.Get("X-Iota-Client") != "web" || (r.Header.Get("Origin") != "" && r.Header.Get("Origin") != origin) {
				fail(w, http.StatusForbidden, errors.New("same-origin client request required"))
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

func sendJSON(w http.ResponseWriter, status int, value interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func fail(w http.ResponseWriter, status int, err error) {
	sendJSON(w, status, struct {
		Error string `json:"error"`
	}{err.Error()})
}

func decode(w http.ResponseWriter, r *http.Request, value interface{}) bool {
	reader := http.MaxBytesReader(w, r.Body, 1024*1024)
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		fail(w, http.StatusBadRequest, err)
		return false
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		fail(w, http.StatusBadRequest, errors.New("expected one JSON object"))
		return false
	}
	return true
}

func (s *Server) path(w http.ResponseWriter, r *http.Request) (string, bool) {
	path, err := sessionPath(s.folder, r.PathValue("name"))
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, os.ErrNotExist) {
			status = http.StatusNotFound
		}
		fail(w, status, err)
		return "", false
	}
	return path, true
}

func (s *Server) owned(w http.ResponseWriter, r *http.Request) *workspace {
	s.mu.Lock()
	owned := s.sessions[r.PathValue("name")]
	closing := s.closing
	s.mu.Unlock()
	if closing {
		fail(w, http.StatusServiceUnavailable, errors.New("server is shutting down"))
		return nil
	}
	if owned == nil {
		fail(w, http.StatusConflict, errors.New("open the session before continuing"))
	}
	return owned
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	entries, err := os.ReadDir(s.folder)
	if err != nil {
		fail(w, 500, err)
		return
	}
	summaries := make([]sessionSummary, 0)
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !sessionName.MatchString(entry.Name()) {
			continue
		}
		path := filepath.Join(s.folder, entry.Name())
		summary, err := readSummary(path)
		if err != nil {
			continue
		}
		summary.Occupied, err = iota.SessionOccupied(path)
		if err != nil {
			fail(w, 500, err)
			return
		}
		s.mu.Lock()
		summary.Open = s.sessions[entry.Name()] != nil
		s.mu.Unlock()
		summaries = append(summaries, summary)
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].CreatedAt == summaries[j].CreatedAt {
			return summaries[i].Name > summaries[j].Name
		}
		return summaries[i].CreatedAt > summaries[j].CreatedAt
	})
	sendJSON(w, 200, summaries)
}

func (s *Server) attach(session *iota.Session) (*workspace, error) {
	settings := s.config.Settings
	if session.Info().CWD != "" {
		settings.CWD = session.Info().CWD
	}
	owned := &workspace{session: session, cwd: settings.CWD, status: "idle", requests: make(map[string]commandResult)}
	agent, err := s.config.NewAgent(settings, s.config.Home, owned.userInput, nil)
	if err != nil {
		return nil, err
	}
	owned.agent = agent
	if err := session.Restore(agent); err != nil {
		return nil, err
	}
	if err := session.SetSummaryDir(s.folder); err != nil {
		return nil, err
	}
	if settings.ModeRequested {
		if err := session.SetMode(agent, iota.Mode(settings.Mode), nil); err != nil {
			return nil, err
		}
	}
	unresolved := make(map[string]bool)
	err = walkRecords(session.Path(), func(record Record) bool {
		if record.Type == "client_request" {
			var command commandRequest
			if json.Unmarshal(record.Payload, &command) == nil {
				unresolved[command.ID] = true
				owned.operation = command.ID
				owned.requests[command.ID] = commandResult{ID: command.ID, Status: "interrupted", Error: "server stopped before recording the result; inspect history before continuing", Action: command.Action, Prompt: command.Prompt, Mode: command.Mode, PlanHash: command.PlanHash}
			}
		} else if record.Type == "client_result" {
			var result commandResult
			if json.Unmarshal(record.Payload, &result) == nil {
				delete(unresolved, result.ID)
				owned.requests[result.ID] = result
			}
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	for id := range unresolved {
		result := owned.requests[id]
		payload, err := json.Marshal(result)
		if err != nil {
			return nil, err
		}
		if err := session.AppendMetadata("client_result", payload); err != nil {
			return nil, err
		}
	}
	if result, exists := owned.requests[owned.operation]; exists && result.Status == "interrupted" {
		owned.status, owned.lastError = "interrupted", result.Error
	}
	return owned, err
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var input struct {
		CWD  string `json:"cwd"`
		Mode string `json:"mode"`
	}
	if !decode(w, r, &input) {
		return
	}
	if input.Mode != "" && input.Mode != "plan" && input.Mode != "default" {
		fail(w, 400, errors.New("mode must be default or plan"))
		return
	}
	if input.CWD == "" {
		input.CWD = s.config.Settings.CWD
	}
	cwd, err := filepath.Abs(input.CWD)
	if err != nil {
		fail(w, 400, err)
		return
	}
	info, err := os.Stat(cwd)
	if err != nil || !info.IsDir() {
		fail(w, 400, fmt.Errorf("invalid working directory %q", cwd))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		fail(w, 503, errors.New("server is shutting down"))
		return
	}
	session, err := iota.NewSession(s.folder, iota.SessionInfo{Model: s.config.Settings.Model, CWD: cwd})
	if err != nil {
		fail(w, 500, err)
		return
	}
	owned, err := s.attach(session)
	if err != nil {
		session.Close()
		fail(w, 400, err)
		return
	}
	if input.Mode != "" {
		if err := session.SetMode(owned.agent, iota.Mode(input.Mode), nil); err != nil {
			session.Close()
			fail(w, 400, err)
			return
		}
	}
	name := filepath.Base(session.Path())
	s.sessions[name] = owned
	sendJSON(w, 201, struct {
		Name string `json:"name"`
	}{name})
}

func (s *Server) open(w http.ResponseWriter, r *http.Request) {
	path, ok := s.path(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		fail(w, 503, errors.New("server is shutting down"))
		return
	}
	name := r.PathValue("name")
	if s.sessions[name] != nil {
		sendJSON(w, 200, struct {
			Open bool `json:"open"`
		}{true})
		return
	}
	session, err := iota.OpenSession(path)
	if err != nil {
		status := 400
		if errors.Is(err, iota.ErrSessionOccupied) {
			status = 409
		}
		fail(w, status, err)
		return
	}
	owned, err := s.attach(session)
	if err != nil {
		session.Close()
		fail(w, 400, err)
		return
	}
	s.sessions[name] = owned
	sendJSON(w, 200, struct {
		Open bool `json:"open"`
	}{true})
}

func (s *Server) release(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := r.PathValue("name")
	owned := s.sessions[name]
	if owned == nil {
		w.WriteHeader(204)
		return
	}
	owned.mu.Lock()
	defer owned.mu.Unlock()
	if owned.running {
		fail(w, 409, iota.ErrBusy)
		return
	}
	owned.released = true
	err := owned.session.Close()
	delete(s.sessions, name)
	if err != nil {
		fail(w, 500, err)
		return
	}
	w.WriteHeader(204)
}

func (s *Server) remove(w http.ResponseWriter, r *http.Request) {
	path, ok := s.path(w, r)
	if !ok {
		return
	}
	if err := iota.DeleteSession(path); err != nil {
		status := 500
		if errors.Is(err, iota.ErrSessionOccupied) {
			status = 409
		}
		fail(w, status, err)
		return
	}
	w.WriteHeader(204)
}

func (s *Server) snapshot(name, path string) State {
	s.mu.Lock()
	owned := s.sessions[name]
	s.mu.Unlock()
	if owned == nil {
		occupied, _ := iota.SessionOccupied(path)
		return State{Occupied: occupied, Status: "closed", Model: s.config.Settings.Model}
	}
	owned.mu.Lock()
	defer owned.mu.Unlock()
	state := owned.agent.Collaboration()
	var hash, planError string
	var planPreview *iota.SavedPlan
	if state.Mode == iota.ModePlan && state.Plan != nil {
		preview, err := owned.agent.PlanPreview()
		if err != nil {
			planError = err.Error()
		} else {
			planPreview = &preview
			hash = fmt.Sprintf("%x", sha256.Sum256([]byte(preview.Content)))
		}
	}
	skills := owned.agent.Skills()
	var pending *pendingInput
	if owned.pending != nil {
		copy := *owned.pending
		pending = &copy
	}
	return State{Open: !owned.released, Occupied: !owned.released, Status: owned.status, RequestID: owned.operation, Error: owned.lastError, CWD: owned.cwd, Model: s.config.Settings.Model, Collaboration: &state, Skills: &skills, Pending: pending, PlanHash: hash, PlanError: planError, PlanPreview: planPreview}
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	path, ok := s.path(w, r)
	if !ok {
		return
	}
	sendJSON(w, 200, s.snapshot(r.PathValue("name"), path))
}
