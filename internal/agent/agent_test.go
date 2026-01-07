package agent

import (
	"testing"
)

func TestAgentRegistry(t *testing.T) {
	// Check that all agents are registered
	agents := []string{"codex", "claude-code", "gemini", "copilot", "opencode", "test"}
	for _, name := range agents {
		a, err := Get(name)
		if err != nil {
			t.Fatalf("Failed to get %s agent: %v", name, err)
		}
		if a.Name() != name {
			t.Errorf("Expected name '%s', got '%s'", name, a.Name())
		}
	}

	// Check unknown agent
	_, err := Get("unknown-agent")
	if err == nil {
		t.Error("Expected error for unknown agent")
	}
}

func TestAvailableAgents(t *testing.T) {
	agents := Available()
	// We have 6 agents: codex, claude-code, gemini, copilot, opencode, test
	if len(agents) < 6 {
		t.Errorf("Expected at least 6 agents, got %d: %v", len(agents), agents)
	}

	expected := map[string]bool{
		"codex":       false,
		"claude-code": false,
		"gemini":      false,
		"copilot":     false,
		"opencode":    false,
		"test":        false,
	}

	for _, a := range agents {
		if _, ok := expected[a]; ok {
			expected[a] = true
		}
	}

	for name, found := range expected {
		if !found {
			t.Errorf("Expected %s in available agents", name)
		}
	}
}
