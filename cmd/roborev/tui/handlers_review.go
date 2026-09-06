package tui

import (
	"fmt"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
)

func (m model) handlePromptKey() (tea.Model, tea.Cmd) {
	if job, ok := m.selectedJob(); m.currentView == viewQueue && ok {
		if job.Status == storage.JobStatusDone {
			m.promptFromQueue = true
			cmd := m.dispatchPromptFetch(job.ID)
			return m, cmd
		} else if (job.Status == storage.JobStatusRunning || job.Status == storage.JobStatusQueued) && job.Prompt != "" {
			jobCopy := *job
			m.currentReview = &storage.Review{
				Agent:  job.Agent,
				Prompt: job.Prompt,
				Job:    &jobCopy,
			}
			m.currentView = viewKindPrompt
			m.promptScroll = 0
			m.promptFromQueue = true
			return m, nil
		}
	} else if m.currentView == viewReview && m.currentReview != nil && m.currentReview.Prompt != "" {
		m.closeFixPanel()
		m.currentView = viewKindPrompt
		m.promptScroll = 0
		m.promptFromQueue = false
	} else if m.currentView == viewKindPrompt {
		if m.promptFromQueue {
			m.currentView = viewQueue
			m.promptScroll = 0
			m = m.preserveOrClearReviewOnQueueReturn()
		} else {
			m.currentView = viewReview
			m.promptScroll = 0
		}
	}
	return m, nil
}

func (m model) handleCloseKey() (tea.Model, tea.Cmd) {
	if m.currentView == viewReview && m.currentReview != nil && m.currentReview.ID > 0 {
		if m.currentReview.Job != nil && m.currentReview.Job.PanelRole == storage.PanelRoleMember {
			m.setFlash("Select the panel's synthesis row to close the panel", 3*time.Second, m.currentView)
			return m, nil
		}
		oldState := m.currentReview.Closed
		newState := !oldState
		m.closedSeq++
		seq := m.closedSeq
		m.currentReview.Closed = newState
		var jobID int64
		if m.currentReview.Job != nil {
			jobID = m.currentReview.Job.ID
			m.setJobClosed(jobID, newState)
			m.pendingClosed[jobID] = pendingState{newState: newState, seq: seq}
			m.applyStatsDelta(newState)
		} else {
			m.pendingReviewClosed[m.currentReview.ID] = pendingState{newState: newState, seq: seq}
		}
		return m, m.closeReview(m.currentReview.ID, jobID, newState, oldState, seq)
	} else if job, ok := m.selectedJob(); m.currentView == viewQueue && ok {
		if job.PanelRole == storage.PanelRoleMember {
			m.setFlash("Select the panel's synthesis row to close the panel", 3*time.Second, m.currentView)
			return m, nil
		}
		if job.Status == storage.JobStatusDone && job.Closed != nil {
			oldState := *job.Closed
			newState := !oldState
			m.closedSeq++
			seq := m.closedSeq
			restoreSelection := false
			*job.Closed = newState
			m.pendingClosed[job.ID] = pendingState{newState: newState, seq: seq}
			m.applyStatsDelta(newState)
			// Closing from the split list mutates the QUEUE row's job.Closed
			// above, but with list focus currentView stays viewQueue, so the
			// currentView==viewReview branch above (which flips
			// currentReview.Closed) never runs. Left alone, the detail
			// pane's header would keep showing the OLD closed state
			// indefinitely: splitReconcileDetail's Done-branch idempotency
			// check matches on JobID (and on
			// paneReviewSeenNonTerminal/FinishedAt) -- none of which this
			// close toggle changes, so reconciliation has no reason to
			// refetch and correct it. Flip it optimistically here too, keyed
			// by the SAME seq as the job's pendingClosed entry (no separate
			// pendingClosed-style map for the review side) so
			// handleClosedResultMsg's single rollback-on-failure path, gated
			// on that one seq, can't roll back one half without the other.
			if m.layout == layoutSplit && m.currentReview != nil && m.currentReview.JobID == job.ID {
				m.currentReview.Closed = newState
			}
			if m.hideClosed && newState {
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
					// Closing the LAST visible job: clear the selection like
					// the cancel twin below, or the list shows "No jobs"
					// while the detail pane stays actionable for the hidden
					// review. The rollback restores by msg.jobID
					// (selectJobByID), so it works from a cleared selection.
					m.selectedIdx = -1
					m.selectedJobID = 0
				}
				restoreSelection = true
			}
			return m, m.closeReviewInBackground(job.ID, newState, oldState, seq, restoreSelection)
		}
	}
	return m, nil
}

func (m model) handleCancelKey() (tea.Model, tea.Cmd) {
	job, ok := m.selectedJob()
	if m.currentView != viewQueue || !ok {
		return m, nil
	}
	if job.PanelRole == storage.PanelRoleMember {
		m.setFlash("Select the panel's synthesis row to cancel the panel", 3*time.Second, m.currentView)
		return m, nil
	}
	if job.Status == storage.JobStatusRunning || job.Status == storage.JobStatusQueued {
		oldStatus := job.Status
		oldFinishedAt := job.FinishedAt
		job.Status = storage.JobStatusCanceled
		now := time.Now()
		job.FinishedAt = &now
		// Canceled jobs are hidden when hideClosed is active
		restoreSelection := false
		if m.hideClosed {
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
		return m, m.cancelJob(
			job.ID, oldStatus, oldFinishedAt, restoreSelection,
		)
	}
	return m, nil
}

func (m model) handleRerunKey() (tea.Model, tea.Cmd) {
	job, ok := m.selectedJob()
	if m.currentView != viewQueue || !ok {
		return m, nil
	}
	if job.PanelRole == storage.PanelRoleMember {
		m.setFlash("Select the panel's synthesis row to rerun the panel", 3*time.Second, m.currentView)
		return m, nil
	}
	if job.Status == storage.JobStatusDone || job.Status == storage.JobStatusFailed || job.Status == storage.JobStatusCanceled {
		// A synthesis parent's row stays terminal while its rerun is in
		// flight (see below), so the status check above cannot suppress
		// a second press the way it does for an ordinary job. Without
		// this, a fast double-'r' spawns two full panel runs: the daemon
		// accepts both, since the status IT checks is this same
		// unchanged row. See panelRerunInFlight (tui.go).
		if job.IsSynthesisJob() && m.panelRerunInFlight[job.ID] {
			m.setFlash(
				fmt.Sprintf("Panel rerun already in progress for job #%d", job.ID),
				3*time.Second, m.currentView,
			)
			return m, nil
		}
		cmd := m.startRerun(job, "")
		return m, cmd
	}
	return m, nil
}

func rerunAgentEligible(job *storage.ReviewJob) bool {
	if job == nil || job.PanelRole != "" || job.IsSynthesisJob() || len(job.Experiments) > 0 {
		return false
	}
	if job.Status == storage.JobStatusCanceled && job.WorkerID != "" {
		return false
	}
	return job.Status == storage.JobStatusDone ||
		job.Status == storage.JobStatusFailed ||
		job.Status == storage.JobStatusCanceled ||
		job.Status == storage.JobStatusSkipped
}

func (m model) availableRerunAgents(job *storage.ReviewJob) ([]string, error) {
	repoPath := job.RepoPath
	if job.WorktreePath != "" {
		repoPath = job.WorktreePath
	}
	repoCfg, err := config.LoadRepoConfig(repoPath)
	if err != nil {
		return nil, err
	}
	cfg := m.globalCfg
	if cfg == nil {
		cfg = config.DefaultConfig()
	}
	current := agent.StorageNameFromConfig(agent.CanonicalName(job.Agent), repoCfg, cfg)
	var available []string
	for _, name := range agent.AvailableNamesFromConfig(repoCfg, cfg) {
		if name == "test" || agent.StorageNameFromConfig(agent.CanonicalName(name), repoCfg, cfg) == current {
			continue
		}
		selected, err := agent.GetAvailableExactWithConfigFromConfig(repoCfg, name, cfg)
		if err != nil || (job.JobType == storage.JobTypeClassify && !agent.IsSchemaAgent(selected)) {
			continue
		}
		if agent.ValidateStructuredReviewSelection(job.ReviewType, selected) == nil {
			available = append(available, name)
		}
	}
	return available, nil
}

func (m model) handleRerunAgentKey() (tea.Model, tea.Cmd) {
	job, ok := m.selectedJob()
	if m.currentView != viewQueue || !ok || !rerunAgentEligible(job) {
		return m, nil
	}
	options, err := m.availableRerunAgents(job)
	if err != nil {
		m.setWarningFlash(
			fmt.Sprintf("Cannot load rerun agents: %v", err),
			4*time.Second, viewQueue,
		)
		return m, nil
	}
	if len(options) == 0 {
		m.setFlash("No alternate agents available", 3*time.Second, viewQueue)
		return m, nil
	}
	m.rerunAgentJobID = job.ID
	m.rerunAgentOptions = options
	m.rerunAgentSelected = 0
	m.currentView = viewRerunAgent
	return m, nil
}

func (m *model) closeRerunAgentPicker() {
	m.currentView = viewQueue
	m.rerunAgentJobID = 0
	m.rerunAgentOptions = nil
	m.rerunAgentSelected = 0
}

func (m model) handleRerunAgentPickerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.closeRerunAgentPicker()
		return m, nil
	case "up", "k":
		if m.rerunAgentSelected > 0 {
			m.rerunAgentSelected--
		}
		return m, nil
	case "down", "j":
		if m.rerunAgentSelected < len(m.rerunAgentOptions)-1 {
			m.rerunAgentSelected++
		}
		return m, nil
	case "enter":
		return m.submitRerunAgentChoice()
	default:
		return m, nil
	}
}

func (m model) submitRerunAgentChoice() (tea.Model, tea.Cmd) {
	if m.rerunAgentSelected < 0 || m.rerunAgentSelected >= len(m.rerunAgentOptions) {
		m.closeRerunAgentPicker()
		return m, nil
	}
	var job *storage.ReviewJob
	for i := range m.jobs {
		if m.jobs[i].ID == m.rerunAgentJobID {
			job = &m.jobs[i]
			break
		}
	}
	if !rerunAgentEligible(job) {
		m.closeRerunAgentPicker()
		m.setWarningFlash("Job is no longer eligible for an alternate-agent rerun", 4*time.Second, viewQueue)
		return m, nil
	}
	selectedAgent := m.rerunAgentOptions[m.rerunAgentSelected]
	m.closeRerunAgentPicker()
	cmd := m.startRerun(job, selectedAgent)
	return m, cmd
}

func (m model) handleLogKey2() (tea.Model, tea.Cmd) {
	// From prompt view: view log for the job being viewed
	if m.currentView == viewKindPrompt && m.currentReview != nil && m.currentReview.Job != nil {
		job := m.currentReview.Job
		return m.openLogView(*job, m.reviewFromView)
	}

	job, ok := m.selectedJob()
	if m.currentView != viewQueue || !ok {
		return m, nil
	}
	switch job.Status {
	case storage.JobStatusQueued:
		m.setFlash("Job is queued - not yet running", 2*time.Second, viewQueue)
		return m, nil
	default:
		return m.openLogView(*job, viewQueue)
	}
}

func (m model) handleCommentOpenKey() (tea.Model, tea.Cmd) {
	if job, ok := m.selectedJob(); m.currentView == viewQueue && ok {
		if job.Status == storage.JobStatusDone || job.Status == storage.JobStatusFailed {
			if m.commentJobID != job.ID {
				m.commentText = ""
			}
			m.commentJobID = job.ID
			m.commentCommit = gitrepo.ShortSHA(job.GitRef)
			m.commentFromView = viewQueue
			m.currentView = viewKindComment
		}
		return m, nil
	} else if m.currentView == viewReview && m.currentReview != nil {
		if m.commentJobID != m.currentReview.JobID {
			m.commentText = ""
		}
		m.commentJobID = m.currentReview.JobID
		m.commentCommit = ""
		if m.currentReview.Job != nil {
			m.commentCommit = gitrepo.ShortSHA(
				m.currentReview.Job.GitRef)
		}
		m.commentFromView = viewReview
		m.currentView = viewKindComment
		return m, nil
	}
	return m, nil
}

func (m model) handleCopyKey() (tea.Model, tea.Cmd) {
	if m.currentView == viewReview && m.currentReview != nil && m.currentReview.Output != "" {
		return m, m.copyToClipboard(m.currentReview, m.currentResponses)
	} else if job, ok := m.selectedJob(); m.currentView == viewQueue && ok {
		if job.Status == storage.JobStatusDone || job.Status == storage.JobStatusFailed {
			jobCopy := *job
			return m, m.fetchReviewAndCopy(job.ID, &jobCopy)
		}
		var status string
		switch job.Status {
		case storage.JobStatusQueued:
			status = "queued"
		case storage.JobStatusRunning:
			status = "in progress"
		case storage.JobStatusCanceled:
			status = "canceled"
		default:
			status = string(job.Status)
		}
		m.setFlash(fmt.Sprintf("Job #%d is %s — no review to copy", job.ID, status), 2*time.Second, viewQueue)
		return m, nil
	}
	return m, nil
}

func (m model) handleCommitMsgKey() (tea.Model, tea.Cmd) {
	if job, ok := m.selectedJob(); m.currentView == viewQueue && ok {
		m.commitMsgFromView = m.currentView
		m.commitMsgJobID = job.ID
		m.commitMsgContent = ""
		m.commitMsgScroll = 0
		jobCopy := *job
		return m, m.fetchCommitMsg(&jobCopy)
	} else if m.currentView == viewReview && m.currentReview != nil && m.currentReview.Job != nil {
		job := m.currentReview.Job
		m.commitMsgFromView = m.currentView
		m.commitMsgJobID = job.ID
		m.commitMsgContent = ""
		m.commitMsgScroll = 0
		return m, m.fetchCommitMsg(job)
	}
	return m, nil
}

// handleFixKey opens the fix prompt modal for the currently selected job.
func (m model) handleFixKey() (tea.Model, tea.Cmd) {
	if m.currentView != viewQueue && m.currentView != viewReview {
		return m, nil
	}
	if !m.tasksWorkflowEnabled() {
		m.setFlash(m.tasksDisabledMessage(), 3*time.Second, m.currentView)
		return m, nil
	}

	// Get the selected job
	var job storage.ReviewJob
	if m.currentView == viewReview {
		if m.currentReview == nil || m.currentReview.Job == nil {
			return m, nil
		}
		job = *m.currentReview.Job
	} else if sel, ok := m.selectedJob(); ok {
		job = *sel
	} else {
		return m, nil
	}

	// Only allow fix on completed review jobs (not fix jobs —
	// fix-of-fix chains are not supported).
	if job.IsFixJob() {
		m.setFlash("Cannot fix a fix job", 2*time.Second, m.currentView)
		return m, nil
	}
	if job.Status != storage.JobStatusDone {
		m.setFlash("Can only fix completed reviews", 2*time.Second, m.currentView)
		return m, nil
	}

	if m.currentView == viewReview {
		// Open inline fix panel within review view
		m.fixPromptJobID = job.ID
		m.fixPromptText = ""
		m.reviewFixPanelOpen = true
		m.reviewFixPanelFocused = true
		return m, nil
	}

	// Fetch the review and open the inline fix panel when it loads
	m.fixPromptJobID = job.ID
	m.fixPromptText = ""
	m.reviewFixPanelPending = true
	m.fixPromptFollowRetried = false
	// The origin of this fix REQUEST, for the consume-time view-switch
	// decision -- recorded rather than assumed, since the response that
	// eventually serves it may come from an entirely different dispatcher
	// (see fixPromptOrigin's doc comment, tui.go).
	m.fixPromptOrigin = m.currentView
	m.reviewFromView = viewQueue
	m.selectedJobID = job.ID
	cmd := m.dispatchReviewFetch(job.ID)
	// This request's own identity (see fixPromptSeq's doc
	// comment, tui.go): read AFTER dispatchReviewFetch, which is what just
	// bumped m.reviewFetchSeq to the value stamped on this dispatch.
	m.fixPromptSeq = m.reviewFetchSeq
	return m, cmd
}

// handleReviewFixPanelKey handles key input when the inline fix panel is focused.
func (m model) handleReviewFixPanelKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.reviewFixPanelOpen = false
		m.reviewFixPanelFocused = false
		m.fixPromptText = ""
		m.fixPromptJobID = 0
		// Complete the reset to match fixPromptSeq's doc claim (tui.go):
		// "reset alongside fixPromptJobID wherever that is." These three
		// sites only ever run on an already-OPEN panel (reviewFixPanelPending
		// is false by the time reviewFixPanelOpen is true, so fixPromptSeq/
		// Origin/FollowRetried are already inert leftovers from the arm
		// cycle here) -- harmless today, but zeroing them keeps every
		// fixPromptJobID-clearing site in this file uniform instead of
		// leaving a partial-reset shape for a future change to trip on.
		m.fixPromptOrigin = 0
		m.fixPromptSeq = 0
		m.fixPromptFollowRetried = false
		return m, nil
	case "tab":
		m.reviewFixPanelFocused = false
		return m, nil
	case "enter":
		if !m.tasksWorkflowEnabled() {
			m.reviewFixPanelOpen = false
			m.reviewFixPanelFocused = false
			m.fixPromptText = ""
			m.fixPromptJobID = 0
			m.fixPromptOrigin = 0
			m.fixPromptSeq = 0
			m.fixPromptFollowRetried = false
			m.setFlash(m.tasksDisabledMessage(), 3*time.Second, viewReview)
			return m, nil
		}
		jobID := m.fixPromptJobID
		prompt := m.fixPromptText
		m.reviewFixPanelOpen = false
		m.reviewFixPanelFocused = false
		m.fixPromptText = ""
		m.fixPromptJobID = 0
		m.fixPromptOrigin = 0
		m.fixPromptSeq = 0
		m.fixPromptFollowRetried = false
		m.currentView = viewTasks
		return m, m.triggerFix(jobID, prompt, "")
	case "backspace":
		if len(m.fixPromptText) > 0 {
			runes := []rune(m.fixPromptText)
			m.fixPromptText = string(runes[:len(runes)-1])
		}
		return m, nil
	default:
		if len(keyRunes(msg)) > 0 {
			for _, r := range keyRunes(msg) {
				if unicode.IsPrint(r) {
					m.fixPromptText += string(r)
				}
			}
		}
		return m, nil
	}
}

// handleTabKey shifts focus to the fix panel when it is open in review view.
// In split layout, tab from the list pane (with a review loaded) moves
// focus to the detail pane instead.
func (m model) handleTabKey() (tea.Model, tea.Cmd) {
	if m.layout == layoutSplit && m.currentView == viewQueue {
		if !m.selectedReviewLoaded() {
			// Either nothing is loaded, or the loaded review belongs to a
			// job other than the one currently highlighted (e.g. the
			// cursor moved to a running/queued job before the follow-fetch
			// for it landed) -- entering detail focus would hand review
			// actions (close/comment/fix) to the wrong job.
			return m, nil
		}
		// This transition (unlike handleReviewMsg's guarded switch) doesn't
		// go through a fresh fetchReview dispatch -- m.currentReview here
		// was already loaded by the split's background follow-fetch
		// (fetchReviewFollow, which never touches reviewFromView). Stamp
		// it explicitly so splitActive()'s tasks-origin exclusion can't
		// misfire on a stale value left over from a previously-viewed
		// tasks-origin review.
		m.reviewFromView = viewQueue
		m.focus = focusDetail
		m.currentView = viewReview
		return m, nil
	}
	if m.currentView == viewReview && m.reviewFixPanelOpen && !m.reviewFixPanelFocused {
		m.reviewFixPanelFocused = true
	}
	return m, nil
}

// handleToggleTasksKey switches between queue and tasks view.
func (m model) handleToggleTasksKey() (tea.Model, tea.Cmd) {
	if !m.tasksWorkflowEnabled() {
		m.setFlash(m.tasksDisabledMessage(), 3*time.Second, m.currentView)
		return m, nil
	}
	if m.currentView == viewTasks {
		return m.exitTasksToQueue()
	}
	if m.currentView == viewQueue {
		m.currentView = viewTasks
		return m, m.startFetchFixJobs()
	}
	return m, nil
}

// closeFixPanel resets all inline fix panel state. Call this when
// leaving review view or navigating to a different review.
// closeFixPanelIfJobChanged drops an open or pending inline fix panel that
// is bound to a job other than the currently selected one. The panel is
// keyed to fixPromptJobID, so it does not follow the pane's content the way
// currentReview does -- left alone over a different job, submitting would
// start a fix for the job the user is no longer looking at. Shared by
// scheduleDetailFollow (the selection-change transition) and handleJobsMsg
// (whose vanished/hidden-selection reassignment doesn't go through it,
// since splitReconcileDetail is the content authority on that path).
// A panel still on the selected job -- including a same-job no-op "move" --
// is left alone.
// markPanelRerunInFlight records that a panel rerun has been dispatched for
// jobID, so a second dispatch for the same job is suppressed until its
// result lands. See panelRerunInFlight's doc comment (tui.go) -- including
// why writing through a value receiver's shared map is the intended
// behaviour here, and the argument that every entry is removed again.
func (m *model) markPanelRerunInFlight(jobID int64) {
	if m.panelRerunInFlight == nil {
		m.panelRerunInFlight = make(map[int64]bool)
	}
	m.panelRerunInFlight[jobID] = true
}

func (m *model) closeFixPanelIfJobChanged() {
	if (m.reviewFixPanelOpen || m.reviewFixPanelPending) && m.fixPromptJobID != m.selectedJobID {
		m.closeFixPanel()
	}
}

func (m *model) closeFixPanel() {
	m.reviewFixPanelOpen = false
	m.reviewFixPanelFocused = false
	m.reviewFixPanelPending = false
	m.fixPromptText = ""
	m.fixPromptJobID = 0
	m.fixPromptOrigin = 0
	m.fixPromptFollowRetried = false
	m.fixPromptSeq = 0
}
