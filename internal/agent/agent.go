package agent

import (
	"context"
	"fmt"
	"os/exec"
)

// Agent defines the interface for code review agents
type Agent interface {
	// Name returns the agent identifier (e.g., "codex", "claude-code")
	Name() string

	// Review runs a code review and returns the output
	Review(ctx context.Context, repoPath, commitSHA, prompt string) (output string, err error)
}

// CommandAgent is an agent that uses an external command
type CommandAgent interface {
	Agent
	// CommandName returns the executable command name
	CommandName() string
}

// Registry holds available agents
var registry = make(map[string]Agent)

// Register adds an agent to the registry
func Register(a Agent) {
	registry[a.Name()] = a
}

// Get returns an agent by name
func Get(name string) (Agent, error) {
	a, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown agent: %s", name)
	}
	return a, nil
}

// Available returns the names of all registered agents
func Available() []string {
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	return names
}

// IsAvailable checks if an agent's command is installed on the system
func IsAvailable(name string) bool {
	a, ok := registry[name]
	if !ok {
		return false
	}

	// Check if agent implements CommandAgent interface
	if ca, ok := a.(CommandAgent); ok {
		_, err := exec.LookPath(ca.CommandName())
		return err == nil
	}

	// Non-command agents (like test) are always available
	return true
}

// GetAvailable returns an available agent, trying the requested one first,
// then falling back to alternatives. Returns error only if no agents available.
func GetAvailable(preferred string) (Agent, error) {
	// Try preferred agent first
	if preferred != "" && IsAvailable(preferred) {
		return Get(preferred)
	}

	// Fallback order: codex, claude-code, gemini, copilot
	fallbacks := []string{"codex", "claude-code", "gemini", "copilot"}
	for _, name := range fallbacks {
		if name != preferred && IsAvailable(name) {
			return Get(name)
		}
	}

	// List what's actually available for error message
	var available []string
	for name := range registry {
		if IsAvailable(name) {
			available = append(available, name)
		}
	}

	if len(available) == 0 {
		return nil, fmt.Errorf("no agents available (install one of: codex, claude-code, gemini, copilot)")
	}

	return Get(available[0])
}
