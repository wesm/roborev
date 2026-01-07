package agent

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

// OpenCodeAgent runs code reviews using the OpenCode CLI
type OpenCodeAgent struct {
	Command string // The opencode command to run (default: "opencode")
}

// NewOpenCodeAgent creates a new OpenCode agent
func NewOpenCodeAgent(command string) *OpenCodeAgent {
	if command == "" {
		command = "opencode"
	}
	return &OpenCodeAgent{Command: command}
}

func (a *OpenCodeAgent) Name() string {
	return "opencode"
}

func (a *OpenCodeAgent) CommandName() string {
	return a.Command
}

func (a *OpenCodeAgent) Review(ctx context.Context, repoPath, commitSHA, prompt string) (string, error) {
	// OpenCode CLI supports a headless invocation via `opencode run [message..]`.
	// We run it from the repo root so it can use project context, and pass the full
	// roborev prompt as the message.
	//
	// Helpful reference:
	//   opencode --help
	//   opencode run --help
	args := []string{"run", "--format", "default", prompt}

	cmd := exec.CommandContext(ctx, a.Command, args...)
	cmd.Dir = repoPath

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// opencode sometimes prints failures/usage to stdout; include both streams.
		return "", fmt.Errorf(
			"opencode failed: %w\nstdout: %s\nstderr: %s",
			err,
			stdout.String(),
			stderr.String(),
		)
	}

	output := stdout.String()
	if len(output) == 0 {
		return "No review output generated", nil
	}

	return output, nil
}

func init() {
	Register(NewOpenCodeAgent(""))
}
