package runner

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"agentflow/internal/domain"
	"agentflow/internal/runtime"
)

func readDUCCSettings(path string) (map[string]json.RawMessage, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("DUCC OneAPI 配置需使用绝对路径")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > 64<<10 {
		return nil, errors.New("DUCC OneAPI 配置必须是仅 Runner 账号可读的普通文件")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("无法读取 DUCC OneAPI 配置")
	}
	var settings map[string]json.RawMessage
	var env map[string]string
	if json.Unmarshal(data, &settings) != nil || json.Unmarshal(settings["env"], &env) != nil {
		return nil, errors.New("DUCC OneAPI 配置缺少有效的 env")
	}
	baseURL, err := url.Parse(env["ANTHROPIC_BASE_URL"])
	withKey := strings.TrimSpace(env["ANTHROPIC_API_KEY"]) != ""
	withToken := strings.TrimSpace(env["ANTHROPIC_AUTH_TOKEN"]) != ""
	if err != nil || baseURL.Scheme != "https" || baseURL.Hostname() == "" || baseURL.User != nil || withKey == withToken {
		return nil, errors.New("DUCC OneAPI 需配置 HTTPS 地址和一种服务凭证")
	}
	return settings, nil
}

func (d *Daemon) duccBinary() (string, error) {
	path := d.cfg.DUCCBinary
	if !filepath.IsAbs(path) {
		return "", errors.New("需在团队 Runner 配置 DUCC CLI 的绝对路径")
	}
	binary, ok := lookupCLI(path)
	if !ok {
		return "", errors.New("团队 Runner 上的 DUCC CLI 不可执行")
	}
	return binary, nil
}

func (d *Daemon) detectDUCC(ctx context.Context) (domain.RunnerRuntime, bool) {
	binary, err := d.duccBinary()
	if err != nil {
		return domain.RunnerRuntime{}, false
	}
	if _, err := readDUCCSettings(d.cfg.DUCCSettings); err != nil {
		return domain.RunnerRuntime{}, false
	}
	version := probeVersion(ctx, binary, nil)
	if version == "" {
		return domain.RunnerRuntime{}, false
	}
	return domain.RunnerRuntime{Provider: domain.DUCCProvider, BinaryPath: binary, Version: version,
		Capabilities: []string{"non_interactive", domain.RuntimeCapabilityTeamChat}, Status: domain.RuntimeStatusOnline}, true
}

// bindDUCCConversation gives the existing native chat process the Runner's
// operator-owned settings. The control plane never receives the OneAPI key.
func (d *Daemon) bindDUCCConversation(command *runtime.ConversationCommand) error {
	binary, err := d.duccBinary()
	if err != nil {
		return err
	}
	if _, err := readDUCCSettings(d.cfg.DUCCSettings); err != nil {
		return err
	}
	if len(command.Argv) == 0 {
		return errors.New("invalid DUCC conversation command")
	}
	for i := 1; i < len(command.Argv)-1 && command.Argv[i] != "--"; i++ {
		if command.Argv[i] == "--settings" {
			command.Argv[0] = binary
			command.Argv[i+1] = d.cfg.DUCCSettings
			if command.Environment == nil {
				command.Environment = make(map[string]string)
			}
			for _, key := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"} {
				command.Environment[key] = ""
			}
			command.Environment["DISABLE_BAIDU_CLAUDE_UPDATE"] = "1"
			return nil
		}
	}
	return errors.New("missing DUCC conversation settings argument")
}

// bindDUCC keeps OneAPI settings and CLI session state on the dedicated Runner.
// The workspace may provide compiled hooks, which are the only task settings
// merged into the operator-supplied configuration.
func (d *Daemon) bindDUCC(task domain.RunnerTask, workspace string, argv []string) ([]string, map[string]string, func(), error) {
	settings, err := readDUCCSettings(d.cfg.DUCCSettings)
	if err != nil {
		return nil, nil, nil, err
	}
	args := make([]string, 0, len(argv)+2)
	// The final argument is the normalized positional task prompt.
	for i := 0; i < len(argv)-1; i++ {
		if argv[i] != "--settings" {
			args = append(args, argv[i])
			continue
		}
		i++
		if i >= len(argv)-1 || !filepath.IsLocal(argv[i]) {
			return nil, nil, nil, errors.New("DUCC 任务的 hook 配置路径无效")
		}
		var compiled map[string]json.RawMessage
		data, err := os.ReadFile(filepath.Join(workspace, argv[i]))
		if err != nil || json.Unmarshal(data, &compiled) != nil {
			return nil, nil, nil, errors.New("DUCC 任务的 hook 配置无法读取")
		}
		if hooks, ok := compiled["hooks"]; ok {
			settings["hooks"] = hooks
		}
	}

	homeID := sha256.Sum256([]byte(task.RunID + "\x00" + task.NodeID))
	home, err := runtime.PrepareRunnerDUCCHome(workspace, fmt.Sprintf("ducc-%x", homeID[:12]))
	if err != nil {
		return nil, nil, nil, err
	}
	controlDir, err := os.MkdirTemp("", "agentflow-ducc-")
	if err != nil {
		return nil, nil, nil, errors.New("无法创建 DUCC 任务的私有配置目录")
	}
	cleanup := func() { _ = os.RemoveAll(controlDir) }
	data, err := json.Marshal(settings)
	settingsPath := filepath.Join(controlDir, "settings.json")
	if err != nil || os.WriteFile(settingsPath, data, 0o600) != nil {
		cleanup()
		return nil, nil, nil, errors.New("无法写入 DUCC 任务的私有配置")
	}
	env := map[string]string{
		"HOME": home, "USERPROFILE": home, "CLAUDE_CONFIG_DIR": filepath.Join(home, ".claude"),
		"XDG_CONFIG_HOME":    filepath.Join(home, ".config"),
		"XDG_DATA_HOME":      filepath.Join(home, ".local", "share"),
		"XDG_CACHE_HOME":     filepath.Join(home, ".cache"),
		"ANTHROPIC_BASE_URL": "", "ANTHROPIC_API_KEY": "", "ANTHROPIC_AUTH_TOKEN": "", "CLAUDE_CODE_OAUTH_TOKEN": "",
		"DISABLE_BAIDU_CLAUDE_UPDATE": "1",
	}
	args = append(args, "--settings", settingsPath, "--", argv[len(argv)-1])
	return args, env, cleanup, nil
}
