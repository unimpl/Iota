package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	iota "github.com/unimpl/Iota"
	"github.com/unimpl/Iota/provider/openaicompat"
)

func TestPlanningOptionPrecedence(t *testing.T) {
	for _, item := range []struct {
		args []string
		env  string
		want string
	}{
		{want: "plan"},
		{env: "default", want: "default"},
		{args: []string{"--mode", "plan"}, env: "default", want: "plan"},
	} {
		var stderr bytes.Buffer
		opts, err := parseOptionsWithConfig(item.args, &stderr, fileConfig{Mode: "plan"}, func(name string) (string, bool) { return item.env, name == "IOTA_MODE" && item.env != "" })
		if err != nil || opts.mode != item.want || !opts.modeRequested {
			t.Fatalf("options=%+v err=%v", opts, err)
		}
	}
	if _, err := parseOptionsWithConfig([]string{"--mode", "unknown"}, &bytes.Buffer{}, fileConfig{}, func(string) (string, bool) { return "", false }); err == nil {
		t.Fatal("invalid mode accepted")
	}
}

func TestInteractivePlanningAndExecution(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct{ Role, Content string }
			Tools    []struct{ Function struct{ Name string } }
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		requests++
		var names []string
		for _, tool := range body.Tools {
			names = append(names, tool.Function.Name)
		}
		if requests <= 2 {
			if !strings.Contains(body.Messages[0].Content, "Collaboration mode: plan") || strings.Join(names, ",") != "read,list,save_plan" {
				t.Errorf("plan tools=%v messages=%+v", names, body.Messages)
			}
		} else if !strings.Contains(body.Messages[0].Content, "Collaboration mode: default") || strings.Contains(strings.Join(names, ","), "save_plan") {
			t.Errorf("default tools=%v messages=%+v", names, body.Messages)
		}
		if requests == 1 {
			arguments, err := json.Marshal(`{"content":"# Feature plan\nImplement and test."}`)
			if err != nil {
				t.Error(err)
				return
			}
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"save\",\"type\":\"function\",\"function\":{\"name\":\"save_plan\",\"arguments\":%s}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n", arguments)
		} else if requests == 3 {
			if !strings.Contains(body.Messages[len(body.Messages)-1].Content, "Implement the approved plan") {
				t.Error("approved plan was not submitted")
			}
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"progress\",\"type\":\"function\",\"function\":{\"name\":\"update_plan\",\"arguments\":\"{\\\"plan\\\":[{\\\"step\\\":\\\"Implement\\\",\\\"status\\\":\\\"completed\\\"}]}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
		} else {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		}
	}))
	defer server.Close()
	provider, err := openaicompat.New(openaicompat.Config{BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	tools, err := createTools(cwd, defaultTools, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	plansDir := filepath.Join(home, ".iota", "plans")
	agent, err := iota.New(iota.Config{Provider: provider, Model: "test", Tools: tools, PlansDir: plansDir, PlanTemplatePath: filepath.Join(plansDir, "template.md")})
	if err != nil {
		t.Fatal(err)
	}
	session, err := iota.NewSession(t.TempDir(), iota.SessionInfo{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	stdin, err := os.CreateTemp(t.TempDir(), "input")
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if _, err := stdin.WriteString("/plan Design the feature\n/execute\n/mode\n/exit\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := interactive(agent, make(chan os.Signal), stdin, &stdout, &stderr, session); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, &stderr)
	}
	if requests != 4 || agent.Mode() != iota.ModeDefault || agent.Collaboration().Progress == nil {
		t.Fatalf("requests=%d state=%+v stderr=%s", requests, agent.Collaboration(), &stderr)
	}
	if filepath.Dir(agent.Collaboration().Plan.Path) != plansDir {
		t.Fatal("plan was not saved in the user directory")
	}
	for _, text := range []string{"mode: plan", "plan saved:", "plan approved:", "[completed] Implement", "mode: default"} {
		if !strings.Contains(stderr.String(), text) {
			t.Fatalf("missing %q in %s", text, &stderr)
		}
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile(session.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"mode_changed", "plan_saved", "plan_approved", "plan_updated"} {
		if !strings.Contains(string(log), `"type":"`+kind+`"`) {
			t.Fatalf("missing %s in session", kind)
		}
	}
}

func TestRunResumesModeUnlessExplicitlyOverridden(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("IOTA_MODE", "")
	if err := os.Unsetenv("IOTA_MODE"); err != nil {
		t.Fatal(err)
	}
	for _, override := range []bool{false, true} {
		t.Run(fmt.Sprintf("override=%t", override), func(t *testing.T) {
			cwd := t.TempDir()
			session, err := iota.NewSession(t.TempDir(), iota.SessionInfo{})
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			provider, err := openaicompat.New(openaicompat.Config{})
			if err != nil {
				t.Fatal(err)
			}
			agent, err := iota.New(iota.Config{Provider: provider, Model: "test"})
			if err != nil {
				t.Fatal(err)
			}
			if err := session.SetMode(agent, iota.ModePlan, nil); err != nil {
				t.Fatal(err)
			}
			if err := session.Close(); err != nil {
				t.Fatal(err)
			}
			want := "plan"
			if override {
				want = "default"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct{ Messages []struct{ Content string } }
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				if !strings.Contains(body.Messages[0].Content, "Collaboration mode: "+want) {
					t.Errorf("wrong restored mode: %s", body.Messages[0].Content)
				}
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"answer\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			defer server.Close()
			stdin, err := os.Open(os.DevNull)
			if err != nil {
				t.Fatal(err)
			}
			defer stdin.Close()
			args := []string{"--model", "test", "--base-url", server.URL, "--cwd", cwd, "--tools", "none", "--resume", session.Path(), "-p", "continue"}
			if override {
				args = append(args, "--mode", "default")
			}
			var stdout, stderr bytes.Buffer
			if code := run(args, stdin, &stdout, &stderr); code != 0 || stdout.String() != "answer\n" {
				t.Fatalf("code=%d stdout=%s stderr=%s", code, &stdout, &stderr)
			}
		})
	}
}
