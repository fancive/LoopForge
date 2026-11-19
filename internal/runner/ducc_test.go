package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
)

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
