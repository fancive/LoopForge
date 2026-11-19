package runtime

import (
	"errors"
	"path/filepath"
	"slices"

	"agentflow/internal/domain"
)

// DUCC's native Claude CLI emits Anthropic stream-json. Its OneAPI
// settings are bound by the team Runner after compiling the SOP.
type duccProvider struct{ claudeProvider }

func (duccProvider) Kind() string { return domain.DUCCProvider }

func (p duccProvider) Prepare(in providerInvocation) (CommandSpec, error) {
	if in.Driver != "runner" || !filepath.IsAbs(in.Executor.Binary) {
		return CommandSpec{}, errors.New("DUCC + OneAPI 需在已配置团队服务身份的 Runner 上运行")
	}
	spec, err := p.claudeProvider.Prepare(in)
	if err != nil {
		return CommandSpec{}, err
	}
	spec.Provider = p.Kind()
	spec.CredentialBindings = slices.DeleteFunc(spec.CredentialBindings, func(b CredentialBinding) bool {
		return b == BindClaudeCode
	})
	return spec, nil
}

func (duccProvider) SandboxRequirements() SandboxRequirements {
	// The dedicated Runner container is the isolation boundary. The daemon
	// supplies a node-scoped HOME and only runs DUCC with the local driver.
	return SandboxRequirements{}
}
