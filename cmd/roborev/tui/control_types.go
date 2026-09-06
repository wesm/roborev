package tui

import (
	"encoding/json"

	tea "charm.land/bubbletea/v2"

	"go.kenn.io/roborev/internal/storage"
)

// controlRequest is the JSON envelope for incoming control commands.
type controlRequest struct {
	Command string          `json:"command"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// controlResponse is the JSON envelope for outgoing control responses.
type controlResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Data  any    `json:"data,omitempty"`
}

// controlQueryMsg is sent through tea.Program.Send to query TUI state.
// The Update handler writes the response to respCh.
type controlQueryMsg struct {
	req    controlRequest
	respCh chan<- controlResponse
}

// controlMutationMsg is sent through tea.Program.Send to mutate TUI state.
// The Update handler writes the response to respCh and returns side-effect cmds.
type controlMutationMsg struct {
	req    controlRequest
	respCh chan<- controlResponse
}

// controlSocketReadyMsg is sent to the model after the control
// listener starts successfully, so the model can update runtime
// metadata on reconnect without advertising a dead socket.
type controlSocketReadyMsg struct {
	socketPath string
}

// stateSnapshot is the data payload for the get-state query.
type stateSnapshot struct {
	View            string           `json:"view"`
	RepoFilter      []string         `json:"repo_filter"`
	BranchFilter    string           `json:"branch_filter"`
	LockedRepo      bool             `json:"locked_repo"`
	LockedBranch    bool             `json:"locked_branch"`
	HideClosed      bool             `json:"hide_closed"`
	SelectedJobID   int64            `json:"selected_job_id"`
	JobCount        int              `json:"job_count"`
	VisibleJobCount int              `json:"visible_job_count"`
	Stats           storage.JobStats `json:"stats"`
	Layout          string           `json:"layout"`
	Focus           string           `json:"focus"`
}

// jobSnapshot is a lightweight job representation for the get-jobs query.
type jobSnapshot struct {
	ID      int64             `json:"id"`
	Agent   string            `json:"agent"`
	Status  storage.JobStatus `json:"status"`
	Repo    string            `json:"repo"`
	Branch  string            `json:"branch"`
	GitRef  string            `json:"git_ref"`
	Subject string            `json:"subject"`
	Closed  *bool             `json:"closed,omitempty"`
	Verdict *string           `json:"verdict,omitempty"`
}

// selectedSnapshot is the data payload for the get-selected query.
type selectedSnapshot struct {
	Job       *jobSnapshot `json:"job,omitempty"`
	HasReview bool         `json:"has_review"`
}

// String returns the display name of a viewKind.
func (v viewKind) String() string {
	switch v {
	case viewQueue:
		return "queue"
	case viewReview:
		return "review"
	case viewKindPrompt:
		return "prompt"
	case viewFilter:
		return "filter"
	case viewKindComment:
		return "comment"
	case viewCommitMsg:
		return "commit-msg"
	case viewHelp:
		return "help"
	case viewLog:
		return "log"
	case viewTasks:
		return "tasks"
	case viewKindWorktreeConfirm:
		return "worktree-confirm"
	case viewPatch:
		return "patch"
	case viewColumnOptions:
		return "column-options"
	case viewReleaseNotes:
		return "release-notes"
	case viewRerunAgent:
		return "rerun-agent"
	default:
		return "unknown"
	}
}

// parseViewKind converts a string to a viewKind.
// Returns -1 if the string is not a recognized view.
func parseViewKind(s string) viewKind {
	switch s {
	case "queue":
		return viewQueue
	case "tasks":
		return viewTasks
	default:
		return -1
	}
}

// controlQueryCommands are commands that only read state.
var controlQueryCommands = map[string]bool{
	"get-state":    true,
	"get-filter":   true,
	"get-jobs":     true,
	"get-selected": true,
}

// controlMutationCommands are commands that modify state.
var controlMutationCommands = map[string]bool{
	"set-filter":      true,
	"clear-filter":    true,
	"set-hide-closed": true,
	"select-job":      true,
	"set-view":        true,
	"close-review":    true,
	"cancel-job":      true,
	"rerun-job":       true,
	"quit":            true,
}

// isControlCommand returns true and the message type for known commands.
func isControlCommand(cmd string) (isQuery bool, isMutation bool) {
	return controlQueryCommands[cmd], controlMutationCommands[cmd]
}

// Ensure control messages satisfy tea.Msg at compile time.
var (
	_ tea.Msg = controlQueryMsg{}
	_ tea.Msg = controlMutationMsg{}
)
