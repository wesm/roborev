package tui

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	tea "charm.land/bubbletea/v2"

	"go.kenn.io/roborev/internal/storage"
)

// handleControlQuery dispatches a control query to the appropriate
// response builder and writes the result to the response channel.
func (m model) handleControlQuery(
	msg controlQueryMsg,
) (tea.Model, tea.Cmd) {
	var resp controlResponse
	switch msg.req.Command {
	case "get-state":
		resp = m.buildStateResponse()
	case "get-filter":
		resp = m.buildFilterResponse()
	case "get-jobs":
		resp = m.buildJobsResponse()
	case "get-selected":
		resp = m.buildSelectedResponse()
	default:
		resp = controlResponse{
			Error: fmt.Sprintf("unknown query: %s", msg.req.Command),
		}
	}
	msg.respCh <- resp
	return m, nil
}

// handleControlMutation dispatches a control mutation to the
// appropriate handler, writes the response, and returns the
// updated model with any side-effect tea.Cmd.
func (m model) handleControlMutation(
	msg controlMutationMsg,
) (tea.Model, tea.Cmd) {
	var resp controlResponse
	var cmd tea.Cmd

	switch msg.req.Command {
	case "set-filter":
		m, resp, cmd = m.handleCtrlSetFilter(msg.req.Params)
	case "clear-filter":
		m, resp, cmd = m.handleCtrlClearFilter(msg.req.Params)
	case "set-hide-closed":
		m, resp, cmd = m.handleCtrlSetHideClosed(msg.req.Params)
	case "select-job":
		m, resp, cmd = m.handleCtrlSelectJob(msg.req.Params)
	case "set-view":
		m, resp, cmd = m.handleCtrlSetView(msg.req.Params)
	case "close-review":
		m, resp, cmd = m.handleCtrlCloseReview(msg.req.Params)
	case "cancel-job":
		m, resp, cmd = m.handleCtrlCancelJob(msg.req.Params)
	case "rerun-job":
		m, resp, cmd = m.handleCtrlRerunJob(msg.req.Params)
	case "quit":
		m, resp, cmd = m.handleCtrlQuit()
	default:
		resp = controlResponse{
			Error: fmt.Sprintf("unknown mutation: %s", msg.req.Command),
		}
	}

	msg.respCh <- resp
	return m, cmd
}

// --- Query response builders ---

func (m model) buildStateResponse() controlResponse {
	focus := ""
	if m.layout == layoutSplit {
		focus = m.focus.String()
	}
	return controlResponse{
		OK: true,
		Data: stateSnapshot{
			View:            m.currentView.String(),
			RepoFilter:      copyStrings(m.activeRepoFilter),
			BranchFilter:    m.activeBranchFilter,
			LockedRepo:      m.lockedRepoFilter,
			LockedBranch:    m.lockedBranchFilter,
			HideClosed:      m.hideClosed,
			SelectedJobID:   m.selectedJobID,
			JobCount:        len(m.jobs),
			VisibleJobCount: len(m.getVisibleJobs()),
			Stats:           m.jobStats,
			Layout:          m.layout.String(),
			Focus:           focus,
		},
	}
}

func (m model) buildFilterResponse() controlResponse {
	type filterData struct {
		RepoFilter   []string `json:"repo_filter"`
		BranchFilter string   `json:"branch_filter"`
		LockedRepo   bool     `json:"locked_repo"`
		LockedBranch bool     `json:"locked_branch"`
		FilterStack  []string `json:"filter_stack"`
	}
	return controlResponse{
		OK: true,
		Data: filterData{
			RepoFilter:   copyStrings(m.activeRepoFilter),
			BranchFilter: m.activeBranchFilter,
			LockedRepo:   m.lockedRepoFilter,
			LockedBranch: m.lockedBranchFilter,
			FilterStack:  copyStrings(m.filterStack),
		},
	}
}

func (m model) buildJobsResponse() controlResponse {
	visible := m.getVisibleJobs()
	snapshots := make([]jobSnapshot, len(visible))
	for i, job := range visible {
		snapshots[i] = makeJobSnapshot(job)
	}
	return controlResponse{OK: true, Data: snapshots}
}

func (m model) buildSelectedResponse() controlResponse {
	job, ok := m.selectedJob()
	if !ok {
		return controlResponse{
			OK:   true,
			Data: selectedSnapshot{},
		}
	}
	snap := makeJobSnapshot(*job)
	hasReview := job.Status == storage.JobStatusDone &&
		job.Closed != nil
	return controlResponse{
		OK: true,
		Data: selectedSnapshot{
			Job:       &snap,
			HasReview: hasReview,
		},
	}
}

func makeJobSnapshot(job storage.ReviewJob) jobSnapshot {
	snap := jobSnapshot{
		ID:      job.ID,
		Agent:   job.Agent,
		Status:  job.Status,
		Repo:    job.RepoPath,
		Branch:  job.Branch,
		GitRef:  job.GitRef,
		Subject: job.CommitSubject,
	}
	// Deep-copy pointer fields so the snapshot is safe to
	// marshal in a connection goroutine without racing with
	// mutations in the Bubble Tea update loop.
	if job.Closed != nil {
		c := *job.Closed
		snap.Closed = &c
	}
	if job.Verdict != nil {
		v := *job.Verdict
		snap.Verdict = &v
	}
	return snap
}

// copyStrings returns a shallow copy of a string slice so the
// snapshot doesn't alias the model's backing array.
func copyStrings(s []string) []string {
	if s == nil {
		return nil
	}
	cp := make([]string, len(s))
	copy(cp, s)
	return cp
}

// resolveRepoFilter resolves a repo name to root paths. The input
// can be a display name (e.g. "msgvault") or a literal root path
// (e.g. "/home/user/projects/msgvault"). Display names are resolved
// via repoNames (populated from /api/repos at init).
func (m model) resolveRepoFilter(name string) []string {
	// Check display name lookup (authoritative, from /api/repos).
	if paths, ok := m.repoNames[name]; ok {
		return paths
	}

	// Check if it matches a root path in the repo list.
	for _, paths := range m.repoNames {
		if slices.Contains(paths, name) {
			return []string{name}
		}
	}

	// Unknown name — accept as-is so callers that pass a path
	// before the repo list is loaded still work.
	return []string{name}
}

// --- Mutation handlers ---
// Each returns (updated model, response, cmd) so value-receiver
// mutations propagate through Bubble Tea's Update chain.

func (m model) handleCtrlSetFilter(
	raw json.RawMessage,
) (model, controlResponse, tea.Cmd) {
	var params struct {
		Repo   *string `json:"repo"`
		Branch *string `json:"branch"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return m, controlResponse{
			Error: "invalid params: " + err.Error(),
		}, nil
	}

	// Validate all lock constraints before mutating any state
	// so a partial-success / partial-error cannot occur.
	if params.Repo != nil && m.lockedRepoFilter {
		return m, controlResponse{
			Error: "repo filter is locked via --repo flag",
		}, nil
	}
	if params.Branch != nil && m.lockedBranchFilter {
		return m, controlResponse{
			Error: "branch filter is locked via --branch flag",
		}, nil
	}

	if params.Repo != nil {
		m.autoRepoFilter = false
		if *params.Repo == "" {
			m.activeRepoFilter = nil
			m.removeFilterFromStack(filterTypeRepo)
		} else {
			m.activeRepoFilter = m.resolveRepoFilter(*params.Repo)
			m.pushFilter(filterTypeRepo)
		}
	}

	if params.Branch != nil {
		m.activeBranchFilter = *params.Branch
		if *params.Branch == "" {
			m.removeFilterFromStack(filterTypeBranch)
		} else {
			m.pushFilter(filterTypeBranch)
		}
	}

	m.resetQueueForFilterChange()
	m.recomputeClassifyEffective()
	return m, controlResponse{OK: true}, m.fetchJobs()
}

func (m model) handleCtrlClearFilter(
	raw json.RawMessage,
) (model, controlResponse, tea.Cmd) {
	var params struct {
		Repo   bool `json:"repo"`
		Branch bool `json:"branch"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return m, controlResponse{
			Error: "invalid params: " + err.Error(),
		}, nil
	}

	// Validate all lock constraints before mutating any state.
	if params.Repo && m.lockedRepoFilter {
		return m, controlResponse{
			Error: "repo filter is locked via --repo flag",
		}, nil
	}
	if params.Branch && m.lockedBranchFilter {
		return m, controlResponse{
			Error: "branch filter is locked via --branch flag",
		}, nil
	}

	if params.Repo {
		m.autoRepoFilter = false
		m.activeRepoFilter = nil
		m.removeFilterFromStack(filterTypeRepo)
	}

	if params.Branch {
		m.activeBranchFilter = ""
		m.removeFilterFromStack(filterTypeBranch)
	}

	m.resetQueueForFilterChange()
	m.recomputeClassifyEffective()
	return m, controlResponse{OK: true}, m.fetchJobs()
}

func (m model) handleCtrlSetHideClosed(
	raw json.RawMessage,
) (model, controlResponse, tea.Cmd) {
	var params struct {
		HideClosed bool `json:"hide_closed"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return m, controlResponse{
			Error: "invalid params: " + err.Error(),
		}, nil
	}

	m.hideClosed = params.HideClosed
	m.resetQueueForFilterChange()
	return m, controlResponse{OK: true}, m.fetchJobs()
}

func (m model) handleCtrlSelectJob(
	raw json.RawMessage,
) (model, controlResponse, tea.Cmd) {
	var params struct {
		JobID int64 `json:"job_id"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return m, controlResponse{
			Error: "invalid params: " + err.Error(),
		}, nil
	}
	if params.JobID == 0 {
		return m, controlResponse{Error: "job_id is required"}, nil
	}

	for i := range m.jobs {
		if m.jobs[i].ID == params.JobID {
			if !m.isJobVisible(m.jobs[i]) {
				return m, controlResponse{
					Error: fmt.Sprintf(
						"job %d is hidden by current filters",
						params.JobID,
					),
				}, nil
			}
			prevSelected := m.selectedJobID
			m.selectedIdx = i
			m.selectedJobID = params.JobID
			// In split layout a selection change must go through the same
			// detail-follow path as keyboard/mouse navigation. Mutating
			// selectedIdx/selectedJobID alone left the detail pane showing
			// the PREVIOUS job's live tail (and any splitDetailErr from it)
			// until the next jobs refresh reconciled it, and left an
			// in-flight pane-log fetch for the old job able to land:
			// handlePaneLogOutputMsg's seq+jobID gates both still reference
			// the old job on this path, so nothing rejected it -- including
			// a successful response, which would clear an error belonging
			// to the newly selected job. scheduleDetailFollow bumps
			// paneLogSeq when the selection moved off the tailed job
			// (invalidating exactly those responses), clears
			// splitDetailErr, and arms the debounced fetch for the new job.
			mm, cmd := m.followSelectionChange(prevSelected)
			return mm, controlResponse{OK: true}, cmd
		}
	}
	return m, controlResponse{
		Error: fmt.Sprintf(
			"job %d not found in current view", params.JobID,
		),
	}, nil
}

func (m model) handleCtrlSetView(
	raw json.RawMessage,
) (model, controlResponse, tea.Cmd) {
	var params struct {
		View string `json:"view"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return m, controlResponse{
			Error: "invalid params: " + err.Error(),
		}, nil
	}

	v := parseViewKind(params.View)
	if v < 0 {
		return m, controlResponse{
			Error: fmt.Sprintf(
				"invalid view %q (valid: queue, tasks)", params.View,
			),
		}, nil
	}

	if v == viewTasks && !m.tasksWorkflowEnabled() {
		return m, controlResponse{
			Error: "tasks workflow is disabled",
		}, nil
	}

	var cmd tea.Cmd
	if v == viewQueue {
		// Same repair as the keyboard exits: the tasks flow may have moved
		// the selection to a fix job no queue row resolves -- see
		// exitTasksToQueue. A no-op when the selection still resolves
		// (including when this call didn't come from the tasks view).
		m, cmd = m.exitTasksToQueue()
	} else {
		m.currentView = v
		cmd = m.startFetchFixJobs()
	}
	return m, controlResponse{OK: true}, cmd
}

func (m model) handleCtrlCloseReview(
	raw json.RawMessage,
) (model, controlResponse, tea.Cmd) {
	var params struct {
		JobID  int64 `json:"job_id"`
		Closed *bool `json:"closed"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return m, controlResponse{
			Error: "invalid params: " + err.Error(),
		}, nil
	}
	if params.JobID == 0 {
		return m, controlResponse{
			Error: "job_id is required",
		}, nil
	}

	// Find the job
	var job *storage.ReviewJob
	for i := range m.jobs {
		if m.jobs[i].ID == params.JobID {
			job = &m.jobs[i]
			break
		}
	}
	if job == nil {
		return m, controlResponse{
			Error: fmt.Sprintf("job %d not found", params.JobID),
		}, nil
	}
	if job.Status != storage.JobStatusDone || job.Closed == nil {
		return m, controlResponse{
			Error: fmt.Sprintf(
				"job %d has no review to close", params.JobID,
			),
		}, nil
	}

	oldState := *job.Closed
	newState := !oldState
	if params.Closed != nil {
		newState = *params.Closed
	}
	if newState == oldState {
		return m, controlResponse{OK: true}, nil
	}

	m.closedSeq++
	seq := m.closedSeq
	*job.Closed = newState
	m.pendingClosed[params.JobID] = pendingState{
		newState: newState, seq: seq,
	}
	m.applyStatsDelta(newState)
	// Mirrors handleCloseKey's split-list optimistic flip (R9): without
	// this, the control-socket close route leaves the split detail pane's
	// header showing the stale closed state until the user reselects, the
	// same staleness R9 fixed for the key path. Keyed by the same seq as
	// pendingClosed so handleClosedResultMsg's rollback-on-failure path
	// rolls both back together; the rollback itself is effectively a
	// no-op here in the sense that it just reverts to oldState, same as
	// the key path.
	if m.currentReview != nil && m.currentReview.JobID == params.JobID {
		m.currentReview.Closed = newState
	}

	// The hideClosed branch below hides the job just closed and moves the
	// selection to an adjacent row. That is a selection change like any
	// other and must take the shared detail-follow transition, or the
	// detail pane (and a fix panel still bound to the just-closed job)
	// stays pointed at it while the list highlights something else.
	prevSelected := m.selectedJobID
	restoreSelection := false
	if m.hideClosed && newState &&
		m.selectedJobID == params.JobID {
		idx := m.findPrevVisibleJob(m.selectedIdx)
		if idx < 0 {
			idx = m.findNextVisibleJob(m.selectedIdx)
		}
		if idx < 0 {
			idx = m.findFirstVisibleJob()
		}
		if idx >= 0 {
			m.selectedIdx = idx
			m.updateSelectedJobID()
			restoreSelection = true
		} else {
			m.selectedIdx = -1
			m.selectedJobID = 0
			restoreSelection = true
		}
	}

	closeCmd := m.closeReviewInBackground(
		params.JobID, newState, oldState, seq, restoreSelection,
	)
	m, followCmd := m.followSelectionChange(prevSelected)
	return m, controlResponse{OK: true}, tea.Batch(closeCmd, followCmd)
}

func (m model) handleCtrlCancelJob(
	raw json.RawMessage,
) (model, controlResponse, tea.Cmd) {
	var params struct {
		JobID int64 `json:"job_id"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return m, controlResponse{
			Error: "invalid params: " + err.Error(),
		}, nil
	}
	if params.JobID == 0 {
		return m, controlResponse{
			Error: "job_id is required",
		}, nil
	}

	var job *storage.ReviewJob
	for i := range m.jobs {
		if m.jobs[i].ID == params.JobID {
			job = &m.jobs[i]
			break
		}
	}
	if job == nil {
		return m, controlResponse{
			Error: fmt.Sprintf("job %d not found", params.JobID),
		}, nil
	}

	if job.Status != storage.JobStatusRunning &&
		job.Status != storage.JobStatusQueued {
		return m, controlResponse{
			Error: fmt.Sprintf(
				"job %d is %s, can only cancel running/queued jobs",
				params.JobID, job.Status,
			),
		}, nil
	}

	oldStatus := job.Status
	oldFinishedAt := job.FinishedAt
	job.Status = storage.JobStatusCanceled
	now := time.Now()
	job.FinishedAt = &now

	// Same as the close path above: canceling the selected job hides it
	// when hideClosed is on, so the selection move below has to take the
	// shared detail-follow transition.
	prevSelected := m.selectedJobID
	restoreSelection := false
	if m.hideClosed && m.selectedJobID == params.JobID {
		idx := m.findPrevVisibleJob(m.selectedIdx)
		if idx < 0 {
			idx = m.findNextVisibleJob(m.selectedIdx)
		}
		if idx < 0 {
			idx = m.findFirstVisibleJob()
		}
		if idx >= 0 {
			m.selectedIdx = idx
			m.updateSelectedJobID()
		} else {
			m.selectedIdx = -1
			m.selectedJobID = 0
		}
		restoreSelection = true
	}

	cancelCmd := m.cancelJob(
		params.JobID, oldStatus, oldFinishedAt,
		restoreSelection,
	)
	m, followCmd := m.followSelectionChange(prevSelected)
	return m, controlResponse{OK: true}, tea.Batch(cancelCmd, followCmd)
}

func (m model) handleCtrlRerunJob(
	raw json.RawMessage,
) (model, controlResponse, tea.Cmd) {
	var params struct {
		JobID int64 `json:"job_id"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return m, controlResponse{
			Error: "invalid params: " + err.Error(),
		}, nil
	}
	if params.JobID == 0 {
		return m, controlResponse{
			Error: "job_id is required",
		}, nil
	}

	var job *storage.ReviewJob
	for i := range m.jobs {
		if m.jobs[i].ID == params.JobID {
			job = &m.jobs[i]
			break
		}
	}
	if job == nil {
		return m, controlResponse{
			Error: fmt.Sprintf("job %d not found", params.JobID),
		}, nil
	}

	if job.Status != storage.JobStatusDone &&
		job.Status != storage.JobStatusFailed &&
		job.Status != storage.JobStatusCanceled {
		return m, controlResponse{
			Error: fmt.Sprintf(
				"job %d is %s, can only rerun done/failed/canceled jobs",
				params.JobID, job.Status,
			),
		}, nil
	}

	// Same suppression as the keyboard path: a synthesis
	// parent's row stays terminal while its rerun is in flight, so the
	// status check above cannot stop a second request from spawning a
	// second full panel run. An explicit error, rather than a silent
	// success, so a scripted caller can tell the two apart.
	if job.IsSynthesisJob() && m.panelRerunInFlight[job.ID] {
		return m, controlResponse{
			Error: fmt.Sprintf(
				"panel rerun already in flight for job %d", job.ID,
			),
		}, nil
	}

	cmd := m.startRerun(job, "")
	return m, controlResponse{OK: true}, cmd
}

func (m model) handleCtrlQuit() (model, controlResponse, tea.Cmd) {
	return m, controlResponse{OK: true}, tea.Quit
}
