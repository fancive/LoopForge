package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
)

func TestDUCCConversationUsesOwnedTeamRunner(t *testing.T) {
	server, store, _ := newDaemonTestServer(t)
	root := t.TempDir()
	binary := filepath.Join(root, "ducc-fixture")
	script := `#!/bin/sh
set -eu
if [ "$1" = --version ]; then echo "DUCC fixture"; exit 0; fi
[ -z "${ANTHROPIC_API_KEY:-}" ] && [ -z "${CLAUDE_CODE_OAUTH_TOKEN:-}" ]
settings='' tools=unset source=unset prev=''
for arg in "$@"; do
  case "$prev" in
    --settings) settings="$arg" ;;
    --tools) tools="$arg" ;;
    --setting-sources) source="$arg" ;;
  esac
  prev="$arg"
done
[ -z "$tools" ] && [ -z "$source" ]
grep -Fq 'team-fixture-key' "$settings"
cat >/dev/null
printf '%s\n' '{"type":"result","subtype":"success","result":"DUCC_CHAT_OK"}'
`
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(root, "settings.json")
	if err := os.WriteFile(settings, []byte(`{"env":{"ANTHROPIC_BASE_URL":"https://oneapi.example.invalid","ANTHROPIC_API_KEY":"team-fixture-key"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Run the same HTTP-to-runner contract against an operator-supplied CLI
	// and oneAPI config when verifying an actual DUCC installation.
	if native := os.Getenv("AGENTFLOW_TEST_DUCC_BINARY"); native != "" {
		binary, settings = native, os.Getenv("AGENTFLOW_TEST_DUCC_SETTINGS")
		if settings == "" {
			t.Fatal("native DUCC acceptance requires AGENTFLOW_TEST_DUCC_SETTINGS")
		}
	}
	t.Setenv("ANTHROPIC_API_KEY", "ambient-personal-key")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "ambient-personal-token")
	daemon, err := New(Config{
		ServerURL: server.URL, BootstrapToken: "boot-secret", Name: "ducc-chat-fixture",
		StatePath: filepath.Join(root, "state.json"), DefaultWorkspaceRoot: filepath.Join(root, "workspaces"),
		HeartbeatInterval: time.Second, LongPollTimeout: time.Second, DisableAutoUpdate: true,
		DUCCBinary: binary, DUCCSettings: settings,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = daemon.Run(ctx) }()
	waitFor(t, 10*time.Second, "DUCC chat capability", func() bool {
		runners := store.Runners()
		return len(runners) == 1 && runners[0].SupportsDUCCChat()
	})
	user := domain.User{ID: 42, LoginName: "ducc-chat-fixture"}
	if err := store.PutUser(user); err != nil {
		t.Fatal(err)
	}
	token := "ducc-fixture-api-token"
	sum := sha256.Sum256([]byte(token))
	if err := store.PutAPIToken(domain.APIToken{ID: "ducc-fixture", UserID: user.ID, TokenHash: hex.EncodeToString(sum[:]), CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	runner := store.Runners()[0]
	target := domain.AssistantExecutionTarget{Pool: domain.ExecutionModeRunner, RunnerID: runner.ID}
	start := func(id string, target domain.AssistantExecutionTarget, want int) {
		t.Helper()
		body, err := json.Marshal(map[string]any{
			"sessionId": id, "executor": "ducc", "model": "ducc:default", "executionTarget": target,
			"messages": []domain.AssistantMessage{{Role: "user", Content: "Reply with exactly DUCC_CHAT_OK and nothing else."}},
		})
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequestWithContext(ctx, "POST", server.URL+"/api/assistant/generations", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != want {
			detail, _ := io.ReadAll(resp.Body)
			t.Fatalf("start %s: HTTP %d, want %d: %s", id, resp.StatusCode, want, detail)
		}
	}
	start("foreign-ducc", target, http.StatusBadRequest)
	start("cloud-ducc", domain.AssistantExecutionTarget{Pool: domain.ExecutionModeCloud}, http.StatusBadRequest)
	runner.OwnerUserID = user.ID
	if err := store.PutRunner(runner); err != nil {
		t.Fatal(err)
	}
	const sessionID = "team-ducc-chat"
	start(sessionID, target, http.StatusAccepted)
	waitFor(t, 15*time.Second, "DUCC chat terminal status", func() bool {
		gen, ok := store.AssistantGeneration(user.ID, sessionID)
		return ok && gen.Status != "running"
	})
	gen, _ := store.AssistantGeneration(user.ID, sessionID)
	session, _ := store.AssistantSession(user.ID, sessionID)
	var reply struct{ Message string }
	if gen.Status != "completed" || len(session.Messages) != 2 ||
		json.Unmarshal([]byte(session.Messages[1].Content), &reply) != nil || strings.TrimSpace(reply.Message) != "DUCC_CHAT_OK" ||
		session.Messages[1].Model != "ducc:default" || session.Execution().RunnerID != runner.ID {
		t.Fatalf("unexpected DUCC result: status=%q messages=%d expectedReply=%t correctRunner=%t",
			gen.Status, len(session.Messages), strings.TrimSpace(reply.Message) == "DUCC_CHAT_OK", session.Execution().RunnerID == runner.ID)
	}
}

func TestDUCCWorkflowUsesTeamRunnerSettings(t *testing.T) {
	server, store, eng := newDaemonTestServer(t)
	root := t.TempDir()
	binary := filepath.Join(root, "ducc-fixture")
	if err := os.WriteFile(binary, []byte(`#!/bin/sh
set -eu
if [ "$1" = "--version" ]; then printf 'DUCC fixture 1.0\n'; exit 0; fi
[ -z "${ANTHROPIC_API_KEY:-}" ] && [ -z "${CLAUDE_CODE_OAUTH_TOKEN:-}" ]
case "$HOME" in */.agentflow/private-home/ducc-*) ;; *) exit 2;; esac
settings=''
for arg in "$@"; do
  if [ "${previous:-}" = '--settings' ]; then settings="$arg"; break; fi
  previous="$arg"
done
[ -n "$settings" ]
grep -Fq 'team-fixture-key' "$settings"
grep -Fq 'https://oneapi.example.invalid' "$settings"
printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"submit_gate_result","arguments":{"decision":"pass","reason":"team fixture executed"}}}' |
  AGENTFLOW_GATE_RESULT_PATH=.agentflow/gate-results/work/attempt-1.json .agentflow/config/ducc-fixture/gate-mcp >/dev/null
printf '%s\n' '{"type":"system","subtype":"init","session_id":"ducc-fixture-session"}'
printf '%s\n' '{"type":"result","subtype":"success","result":"DUCC team fixture completed"}'
`), 0o700); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(root, "settings.json")
	if err := os.WriteFile(settings, []byte(`{"env":{"ANTHROPIC_BASE_URL":"https://oneapi.example.invalid","ANTHROPIC_AUTH_TOKEN":"team-fixture-key"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_API_KEY", "ambient-personal-key")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "ambient-personal-token")
	daemon, err := New(Config{
		ServerURL: server.URL, BootstrapToken: "boot-secret",
		StatePath: filepath.Join(root, "state.json"), Name: "ducc-team-fixture",
		HeartbeatInterval: time.Second, LongPollTimeout: 2 * time.Second, DisableAutoUpdate: true,
		DefaultWorkspaceRoot: filepath.Join(root, "workspaces"),
		DUCCBinary:           binary, DUCCSettings: settings,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = daemon.Run(ctx) }()
	waitFor(t, 10*time.Second, "DUCC runner registration", func() bool {
		runners := store.Runners()
		return len(runners) == 1 && runners[0].Status == domain.RunnerStatusOnline && runners[0].SupportsProvider(domain.DUCCProvider)
	})
	runner := store.Runners()[0]
	runner.OwnerUserID = 42
	if err := store.PutRunner(runner); err != nil {
		t.Fatal(err)
	}
	if err := store.SetDefaultRunner(42, runner.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	agent := domain.AgentDefinition{ID: "ducc-fixture", Name: "DUCC fixture", ExecutorID: domain.DUCCProvider, Prompt: "{{input}}"}
	if err := store.PutAgent(agent); err != nil {
		t.Fatal(err)
	}
	wf := domain.WorkflowDefinition{
		ID: "ducc-e2e", Name: "DUCC e2e", SandboxProfileID: "local",
		Nodes: []domain.WorkflowNode{{ID: "work", Name: "Work", Type: "agent", AgentID: agent.ID, MaxAttempts: 1}},
	}
	if err := store.PutWorkflow(wf); err != nil {
		t.Fatal(err)
	}
	run, err := eng.StartWithExecutionContext(wf.ID, "team runner input", domain.RepositoryContext{}, domain.TelemetryContext{ResponsibleUserID: 42})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "DUCC run terminal status", func() bool {
		current, _ := store.Run(run.ID)
		return current.Status == "succeeded" || current.Status == "failed"
	})
	final, _ := store.Run(run.ID)
	node := final.Nodes["work"]
	if final.Status != "succeeded" || node == nil || node.GateResult == nil || node.GateResult.Decision != "pass" || !strings.Contains(node.Output, "DUCC team fixture completed") {
		t.Fatalf("DUCC run status=%q node=%+v", final.Status, node)
	}
	found := false
	for _, task := range store.RunnerTasks() {
		if task.RunID != run.ID || task.Provider != domain.DUCCProvider {
			continue
		}
		data, err := json.Marshal(task.Invocation)
		if err != nil {
			t.Fatal(err)
		}
		if task.Status != domain.RunnerTaskSuccess || strings.Contains(string(data), "team-fixture-key") || strings.Contains(string(data), "oneapi.example.invalid") {
			t.Fatal("unexpected DUCC runner task status or remote credential")
		}
		found = true
	}
	if !found {
		t.Fatal("no DUCC task reached the Runner")
	}

	if err := os.Chmod(settings, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := daemon.detectDUCC(ctx); ok {
		t.Fatal("Runner reported DUCC with a group/world-readable settings file")
	}
}
