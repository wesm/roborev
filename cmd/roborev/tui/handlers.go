package tui

import (
	"fmt"
	"io"
	"time"

	tea "charm.land/bubbletea/v2"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/streamfmt"
)

// handleKeyMsg dispatches key events to view-specific handlers.
func (m model) handleKeyMsg(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Fix panel captures input when focused in review view
	if m.currentView == viewReview && m.reviewFixPanelOpen && m.reviewFixPanelFocused {
		return m.handleReviewFixPanelKey(msg)
	}

	// Modal views that capture most keys for typing
	switch m.currentView {
	case viewKindComment:
		return m.handleCommentKey(msg)
	case viewFilter:
		return m.handleFilterKey(msg)
	case viewLog:
		return m.handleLogKey(msg)
	case viewKindWorktreeConfirm:
		return m.handleWorktreeConfirmKey(msg)
	case viewTasks:
		return m.handleTasksKey(msg)
	case viewPatch:
		return m.handlePatchKey(msg)
	case viewColumnOptions:
		return m.handleColumnOptionsInput(msg)
	case viewReleaseNotes:
		return m.handleReleaseNotesKey(msg)
	case viewRerunAgent:
		return m.handleRerunAgentPickerKey(msg)
	}

	// Review-scoped actions must not fire against a review the detail pane
	// has already moved past (see guardStaleSplitDetailAction).
	if mm, blocked := m.guardStaleSplitDetailAction(msg); blocked {
		return mm, nil
	}

	// Global keys shared across queue/review/prompt/commitMsg/help views
	prevSelected := m.selectedJobID
	res, cmd := m.handleGlobalKey(msg)
	if mm, ok := res.(model); ok && mm.currentView == viewQueue {
		mm, followCmd := mm.followSelectionChange(prevSelected)
		if followCmd != nil {
			return mm, tea.Batch(cmd, followCmd)
		}
		return mm, cmd
	}
	return res, cmd
}

// guardStaleSplitDetailAction blocks the review-scoped action keys while
// the split detail pane is showing a review that is NOT the selected job's.
// Split's two panes advance independently: detail navigation (stepReviewNav's
// arrow keys) moves selectedJobID immediately and only replaces
// m.currentReview once the async fetch lands, so in that window every
// review-scoped key -- 'a' (close), 'c' (comment), 'F' (fix), 'p' (prompt),
// 'm' (commit message), 'y' (copy) -- would act on the PREVIOUS job's
// review while the list already highlights the new one. 'F' is the worst of
// them: it opens the inline fix panel over the newly selected job while
// bound to the old job's ID, so submitting starts a fix for the wrong job.
//
// The predicate is m.selectedReviewLoaded() (helpers.go) -- the same one
// tab (handleTabKey) and the detail-pane click (split_render.go) already
// use to refuse handing review actions to the wrong job -- so all three
// entry points agree by construction.
//
// Only the split DETAIL pane is gated: splitActive() excludes a
// tasks-origin review (rendered full-screen even on a split-capable
// terminal), whose displayed review legitimately has nothing to do with
// the queue selection, and list focus (currentView == viewQueue) acts on
// the selected JOB rather than on currentReview, so neither is affected.
//
// The three keys with side effects flash, because silently swallowing a
// close/comment/fix reads as a broken keyboard; the three read-only ones
// (prompt/commit message/copy) are silent, since a flash on every stray
// keypress during the sub-second load window would be noise.
func (m model) guardStaleSplitDetailAction(msg tea.KeyMsg) (model, bool) {
	if !m.splitActive() || m.currentView != viewReview || m.selectedReviewLoaded() {
		return m, false
	}
	switch msg.String() {
	case "a", "c", "F":
		m.setFlash("Review still loading", 2*time.Second, m.currentView)
		return m, true
	case "p", "m", "y":
		return m, true
	}
	return m, false
}

// mouseNavWithFollow invokes nav (handleUpKey/handleDownKey) and routes any
// resulting queue selection change through followSelectionChange, mirroring
// handleKeyMsg's viewQueue wrapper. Mouse events never pass through
// handleKeyMsg, so without this the non-split wheel paths would move the
// selection while skipping the abandonment chokepoint, leaving pending
// review/fix intents armed for a job the cursor already left.
func (m model) mouseNavWithFollow(nav func() (tea.Model, tea.Cmd)) (tea.Model, tea.Cmd) {
	prevSelected := m.selectedJobID
	res, cmd := nav()
	if mm, ok := res.(model); ok && mm.currentView == viewQueue {
		mm, followCmd := mm.followSelectionChange(prevSelected)
		if followCmd != nil {
			return mm, tea.Batch(cmd, followCmd)
		}
		return mm, cmd
	}
	return res, cmd
}

func (m model) handleMouseMsg(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if m.splitActive() {
		return m.handleSplitMouse(msg)
	}
	mouse := msg.Mouse()
	switch msg.(type) {
	case tea.MouseWheelMsg:
		switch mouse.Button {
		case tea.MouseWheelUp:
			if m.currentView == viewColumnOptions {
				if m.colOptionsIdx > 0 {
					m.colOptionsIdx--
				}
				return m, nil
			}
			if m.currentView == viewTasks {
				if m.fixSelectedIdx > 0 {
					m.fixSelectedIdx--
				}
				return m, nil
			}
			return m.mouseNavWithFollow(m.handleUpKey)
		case tea.MouseWheelDown:
			if m.currentView == viewColumnOptions {
				if m.colOptionsIdx < len(m.colOptionsList)-1 {
					m.colOptionsIdx++
				}
				return m, nil
			}
			if m.currentView == viewTasks {
				if m.fixSelectedIdx < len(m.fixJobs)-1 {
					m.fixSelectedIdx++
				}
				return m, nil
			}
			return m.mouseNavWithFollow(m.handleDownKey)
		default:
			return m, nil
		}
	case tea.MouseClickMsg:
		if mouse.Button != tea.MouseLeft {
			return m, nil
		}
		switch m.currentView {
		case viewQueue:
			prevSelected := m.selectedJobID
			m.handleQueueMouseClick(mouse.X, mouse.Y)
			return m.followSelectionChange(prevSelected)
		case viewTasks:
			m.handleTasksMouseClick(mouse.Y)
		case viewColumnOptions:
			return m.handleColumnOptionsMouseClick(mouse.Y)
		}
		return m, nil
	default:
		return m, nil
	}
}

// handleGlobalKey handles keys shared across queue, review, prompt, commit msg, and help views.
func (m model) handleGlobalKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if isSubmitKey(msg) {
		return m.handleEnterKey()
	}
	switch msg.String() {
	case "ctrl+c", "ctrl+d":
		return m.handleQuitKey()
	case "q":
		if m.noQuit && m.currentView == viewQueue {
			return m, nil
		}
		return m.handleQuitKey()
	case "home", "g":
		return m.handleHomeKey()
	case "up":
		return m.handleUpKey()
	case "j":
		return m.handlePrevKey()
	case "left":
		return m.handleLeftKey()
	case "down":
		return m.handleDownKey()
	case "k":
		return m.handleNextKey()
	case "right":
		return m.handleRightKey()
	case "pgup":
		return m.handlePageUpKey()
	case "pgdown":
		return m.handlePageDownKey()
	case "space":
		return m.handleToggleExpand()
	case "p":
		return m.handlePromptKey()
	case "i":
		if m.currentView == viewKindPrompt {
			m.promptCmdExpanded = !m.promptCmdExpanded
			return m, tea.ClearScreen
		}
		return m, nil
	case "a":
		return m.handleCloseKey()
	case "x":
		return m.handleCancelKey()
	case "r":
		return m.handleRerunKey()
	case "R":
		return m.handleRerunAgentKey()
	case "l", "t":
		return m.handleLogKey2()
	case "f":
		return m.handleFilterOpenKey()
	case "b":
		return m.handleBranchFilterOpenKey()
	case "h":
		return m.handleHideClosedKey()
	case "u":
		m.releaseNotesFromView = m.currentView
		m.currentView = viewReleaseNotes
		m.releaseNotesScroll = 0
		m.releaseNotesLoading = true
		m.releaseNotesErr = nil
		return m, m.fetchReleaseNotes()
	case "s":
		return m.handleToggleClassifyKey()
	case "c":
		return m.handleCommentOpenKey()
	case "y":
		return m.handleCopyKey()
	case "m":
		return m.handleCommitMsgKey()
	case "?":
		return m.handleHelpKey()
	case "esc":
		return m.handleEscKey()
	case "F":
		return m.handleFixKey()
	case "T":
		return m.handleToggleTasksKey()
	case "o":
		return m.handleColumnOptionsKey()
	case "D":
		return m.handleDistractionFreeKey()
	case "P":
		return m.handleTogglePauseKey()
	case "tab":
		return m.handleTabKey()
	case "L":
		return m.handleToggleLayoutKey()
	}
	return m, nil
}

// handleToggleLayoutKey toggles between split and stacked layout, locking
// the user's preference so it survives subsequent resizes.
func (m model) handleToggleLayoutKey() (tea.Model, tea.Cmd) {
	if m.currentView != viewQueue && m.currentView != viewReview {
		return m, nil
	}
	target := layoutSplit
	if m.layout == layoutSplit {
		target = layoutStacked
	}
	// applyLayout below is a direct transition that bypasses resolveLayout,
	// so distraction-free's stacked override (see resolveLayout) must be
	// enforced here too or L would re-engage the split composition while
	// the title-plus-list-only contract is active.
	if target == layoutSplit && m.distractionFree {
		m.setFlash("Split layout is unavailable in distraction-free mode (press D to exit)",
			3*time.Second, m.currentView)
		return m, nil
	}
	if target == layoutSplit && pickLayout(m.width, m.height) != layoutSplit {
		m.setFlash(fmt.Sprintf("Terminal too small for split view (needs %dx%d)",
			splitMinWidth, splitMinHeight), 3*time.Second, m.currentView)
		return m, nil
	}
	m.layoutLocked = true
	m.preferredLayout = target
	m.applyLayout(target)
	return m.maybeBootstrapDetail()
}

// handleTogglePauseKey pauses or resumes daemon queue processing. Pause is a
// global daemon state, so this toggle and the [PAUSED] badge work from any
// view sharing the global keymap. The badge flips optimistically and is
// reconciled (or rolled back) once the daemon responds.
func (m model) handleTogglePauseKey() (tea.Model, tea.Cmd) {
	paused := !m.status.QueuePaused
	m.status.QueuePaused = paused
	if paused {
		m.setWarningFlash(
			"Queue paused - running jobs finish, no new jobs start",
			3*time.Second, m.currentView,
		)
	} else {
		m.setFlash("Queue resumed", 3*time.Second, m.currentView)
	}
	return m, m.togglePauseCmd(paused)
}

func (m model) handleQuitKey() (tea.Model, tea.Cmd) {
	// A tasks-origin review renders full-screen even in split (splitActive()
	// excludes it -- see its doc comment), so this split-pane-specific
	// "back to the queue list" shortcut must not fire for it; falling
	// through to the general viewReview branch below returns to
	// reviewFromView (viewTasks) instead, via the same logic that handles
	// every other full-screen review return.
	if m.layout == layoutSplit && m.currentView == viewReview && m.reviewFromView != viewTasks {
		if m.reviewFixPanelOpen {
			m.closeFixPanel()
			return m, nil
		}
		m.focus = focusList
		m.currentView = viewQueue
		// The review just left may now be hidden -- closing it with
		// hideClosed enabled removes its row -- which would leave the
		// selection pointing at a job with no highlighted row, so later
		// actions would target a job the user cannot see. Normalize
		// exactly as the stacked return path does, and refill the queue
		// when hideClosed pruned it (the same refill the stacked path
		// performs). The follow transition (followSelectionChange) is NOT
		// called here: handleKeyMsg's wrapper performs it after this
		// handler returns to viewQueue, against the prevSelected it
		// captured before dispatch, so calling it here too would
		// double-bump detailFollowGen and schedule a stale extra tick.
		m.normalizeSelectionIfHidden()
		if m.hideClosed && !m.loadingJobs {
			m.loadingJobs = true
			return m, m.fetchJobs()
		}
		return m, nil
	}
	if m.currentView == viewReview {
		returnTo := m.reviewFromView
		if returnTo == 0 {
			returnTo = viewQueue
		}
		m.closeFixPanel()
		m.currentView = returnTo
		m.currentReview = nil
		m.reviewScroll = 0
		m.paginateNav = 0
		if returnTo == viewQueue {
			m.normalizeSelectionIfHidden()
		}
		return m, nil
	}
	if m.currentView == viewKindPrompt {
		m.paginateNav = 0
		if m.promptFromQueue {
			m.currentView = viewQueue
			m.promptScroll = 0
			m = m.preserveOrClearReviewOnQueueReturn()
		} else {
			m.currentView = viewReview
			m.promptScroll = 0
		}
		return m, nil
	}
	if m.currentView == viewCommitMsg {
		m.currentView = m.commitMsgFromView
		m.commitMsgContent = ""
		m.commitMsgScroll = 0
		return m, nil
	}
	if m.currentView == viewHelp {
		m.currentView = m.helpFromView
		return m, nil
	}
	return m, tea.Quit
}

func (m model) handleHomeKey() (tea.Model, tea.Cmd) {
	switch m.currentView {
	case viewQueue:
		rows := m.visibleQueueRows()
		if len(rows) > 0 {
			m = m.moveSelectionToJobID(rows[0].job.ID)
		}
	case viewReview:
		m.reviewScroll = 0
	case viewKindPrompt:
		m.promptScroll = 0
	case viewCommitMsg:
		m.commitMsgScroll = 0
	case viewHelp:
		m.helpScroll = 0
	}
	return m, nil
}

// handleToggleExpand toggles the selected panel parent. No-op off the queue or
// on a non-parent row. Dispatches a member fetch the first time an uncached
// panel is expanded; a failed fetch leaves the panel uncached so a later expand
// retries.
func (m model) handleToggleExpand() (tea.Model, tea.Cmd) {
	if m.currentView != viewQueue {
		return m, nil
	}
	rows := m.visibleQueueRows()
	idx := m.selectedRowIndex(rows)
	if idx < 0 || !rows[idx].hasChildren {
		return m, nil
	}
	runUUID := rows[idx].job.PanelRunUUID
	if runUUID == nil {
		return m, nil
	}
	m.queueColGen++
	if m.expandedPanels[*runUUID] {
		delete(m.expandedPanels, *runUUID)
		return m, nil
	}
	m.expandedPanels[*runUUID] = true
	if _, ok := m.panelMembers[*runUUID]; !ok {
		return m, m.fetchPanelMembers(*runUUID)
	}
	return m, nil
}

func (m model) handleRightKey() (tea.Model, tea.Cmd) {
	if m.currentView != viewQueue {
		return m.handleNextKey()
	}
	rows := m.visibleQueueRows()
	idx := m.selectedRowIndex(rows)
	if idx < 0 || !rows[idx].hasChildren {
		return m.handleNextKey()
	}
	runUUID := rows[idx].job.PanelRunUUID
	if runUUID == nil {
		return m, nil
	}
	if m.expandedPanels[*runUUID] {
		return m, nil
	}
	m.queueColGen++
	m.expandedPanels[*runUUID] = true
	if _, ok := m.panelMembers[*runUUID]; !ok {
		return m, m.fetchPanelMembers(*runUUID)
	}
	return m, nil
}

func (m model) handleLeftKey() (tea.Model, tea.Cmd) {
	if m.currentView != viewQueue {
		return m.handlePrevKey()
	}
	rows := m.visibleQueueRows()
	idx := m.selectedRowIndex(rows)
	if idx < 0 {
		return m.handlePrevKey()
	}
	row := rows[idx]
	if row.hasChildren && row.job.PanelRunUUID != nil {
		if m.expandedPanels[*row.job.PanelRunUUID] {
			m.queueColGen++
			delete(m.expandedPanels, *row.job.PanelRunUUID)
		}
		return m, nil
	}
	if row.depth == 1 && row.job.PanelRunUUID != nil {
		for i := idx - 1; i >= 0; i-- {
			parent := rows[i]
			if parent.depth == 0 && parent.job.PanelRunUUID != nil && *parent.job.PanelRunUUID == *row.job.PanelRunUUID {
				m.queueColGen++
				delete(m.expandedPanels, *row.job.PanelRunUUID)
				m = m.moveSelectionToJobID(parent.job.ID)
				return m, nil
			}
		}
	}
	return m.handlePrevKey()
}

// queueNavUp moves the queue cursor one row toward newer entries, flashing
// the boundary when the move clamps in place. Shared by handleUpKey's
// viewQueue arm and the split list pane's wheel-up (handleSplitMouse), so
// keyboard and wheel navigation cannot drift apart.
func (m model) queueNavUp() (model, tea.Cmd) {
	prevID := m.selectedJobID
	m = m.moveQueueSelection(-1)
	// id unchanged after a clamped move ⇒ already at the top; re-flash the boundary hint.
	if m.selectedJobID == prevID {
		m.setFlash("No newer review", 2*time.Second, viewQueue)
	}
	return m, nil
}

// queueNavDown moves the queue cursor one row toward older entries,
// prefetching near the end of loaded data and resuming pagination at the
// bottom boundary. Shared by handleDownKey's viewQueue arm and the split
// list pane's wheel-down (handleSplitMouse), so wheel users can pull older
// pages exactly like keyboard users.
func (m model) queueNavDown() (model, tea.Cmd) {
	prevID := m.selectedJobID
	m = m.moveQueueSelection(+1)
	if m.selectedJobID != prevID {
		if cmd := m.maybePrefetch(m.selectedIdx); cmd != nil {
			return m, cmd
		}
	} else if m.canPaginate() {
		m.loadingMore = true
		return m, m.fetchMoreJobs()
	} else if !m.hasMore || m.activeBranchFilter == branchNone {
		m.setFlash("No older review", 2*time.Second, viewQueue)
	}
	return m, nil
}

func (m model) handleUpKey() (tea.Model, tea.Cmd) {
	switch m.currentView {
	case viewQueue:
		return m.queueNavUp()
	case viewReview:
		if m.reviewScroll > 0 {
			m.reviewScroll--
		}
	case viewKindPrompt:
		if m.promptScroll > 0 {
			m.promptScroll--
		}
	case viewCommitMsg:
		if m.commitMsgScroll > 0 {
			m.commitMsgScroll--
		}
	case viewHelp:
		if m.helpScroll > 0 {
			m.helpScroll--
		}
	}
	return m, nil
}

// contentNavBoundary applies the direction-specific behavior when no eligible
// row exists. dir is the nav direction (-1 = newer, +1 = older), the single
// source of truth from which older is derived. The "newer" direction flashes;
// the "older" direction resumes pagination (parent-only — gated on
// selectedIdx >= 0 so a member never sets paginateNav, since the handlers_msg
// resume is parent-only) or flashes. The returned cmd is non-nil only when
// pagination was started.
//
// Intentional v1 limitation: older-nav from a selected member (selectedIdx ==
// -1) never paginates — it flashes the boundary instead. Resuming pagination
// from a member would require new parent-anchored resume state to re-find the
// member's panel after the page lands, which v1 does not implement. The
// selectedIdx >= 0 gate enforces this no-op (see TestMemberAtBoundaryDoesNotPaginate).
func (m *model) contentNavBoundary(view viewKind, dir int) tea.Cmd {
	older := dir > 0
	verb := "newer"
	if older {
		verb = "older"
	}
	label := "review"
	if view == viewLog {
		label = "log"
	}
	if older && m.canPaginate() && m.selectedIdx >= 0 {
		m.loadingMore = true
		m.paginateNav = view
		return m.fetchMoreJobs()
	}
	m.setFlash("No "+verb+" "+label, 2*time.Second, view)
	return nil
}

// resolveAbsentSelection handles a content-nav step whose selection left the
// flattened rows (present=false). An absent member flashes and stops (ok=false,
// nil cmd → flash-and-stay). An absent parent/normal job hidden by hide-closed
// or a filter falls back to the adjacent visible job (ok=true), or stops at the
// boundary via contentNavBoundary (ok=false, cmd set). The returned job is
// meaningful only when ok is true.
func (m *model) resolveAbsentSelection(
	view viewKind, dir int, eligible func(storage.ReviewJob) bool,
) (storage.ReviewJob, tea.Cmd, bool) {
	if m.selectedIsMember() {
		m.setFlash("Selection no longer visible", 2*time.Second, view)
		return storage.ReviewJob{}, nil, false
	}
	idx := m.stepVisibleJobIndex(dir, eligible)
	if idx < 0 {
		return storage.ReviewJob{}, m.contentNavBoundary(view, dir), false
	}
	return m.jobs[idx], nil, true
}

// stepReviewNav walks the flattened rows for the queue-origin review view in
// direction dir (-1 = newer, +1 = older) and opens the target review, or
// flashes/paginates at the boundary. Never indexes m.jobs by a stale index.
//
// Takes the shared detail-follow transition (followSelectionChange) like
// every other selection-moving site. It was long argued not to need one,
// because eligibleReviewRow admits only done/failed jobs and both branches
// below replace currentReview for the new job -- true for the CONTENT, and
// false for splitDetailErr: renderDetailPane's done branch
// renders that error whenever currentReview does not yet match the selected
// job, which is exactly the state this function creates for the duration of
// the dispatched fetch. So a reconcile follow that failed while job X was
// displayed left "Failed to load review: ..." rendering under job Y's status
// card the moment the user arrowed to it. Routing through the shared
// transition clears it (and stops any tail, and disarms the intent) instead
// of hand-clearing one field here.
func (m model) stepReviewNav(dir int) (tea.Model, tea.Cmd) {
	job, found, present := m.contentNavStep(dir, eligibleReviewRow)
	if !present {
		fallbackJob, cmd, ok := m.resolveAbsentSelection(viewReview, dir, eligibleReviewRow)
		if !ok {
			return m, cmd
		}
		job = fallbackJob
	} else if !found {
		return m, m.contentNavBoundary(viewReview, dir)
	}
	prevSelected := m.selectedJobID
	m.closeFixPanel()
	m = m.moveSelectionToJobID(job.ID)
	m.reviewScroll = 0
	// Before the dispatch: the gen bump must precede the fetch it stamps,
	// and disarmPendingReviewOpen must precede the re-arm below.
	var followCmd tea.Cmd
	m, followCmd = m.followSelectionChange(prevSelected)
	switch job.Status {
	case storage.JobStatusDone:
		cmd := m.dispatchReviewFetch(job.ID)
		return m, tea.Batch(followCmd, cmd)
	case storage.JobStatusFailed:
		// Shared synthesized acceptance: also loads persisted comments,
		// which no review fetch can carry for a row-less review.
		commentsCmd := m.acceptSynthesizedFailure(job.ID, synthesizeFailedReview(&job, m.currentReview))
		return m, tea.Batch(followCmd, commentsCmd)
	}
	return m, followCmd
}

// stepPromptNav walks the flattened rows for the queue-origin prompt view in
// direction dir (-1 = newer, +1 = older) and opens the target prompt, or
// flashes/paginates at the boundary.
//
// Takes the shared detail-follow transition (followSelectionChange), same as
// stepLogNav. An earlier audit marked this site safe, but for the narrower
// question of whether a pending INTENT could be stranded here -- not for
// whether the split detail PANE follows the selection. It does not follow on
// its own: eligiblePromptRow admits RUNNING and QUEUED jobs, so prompt nav
// can move the selection from one running job to another, and for those the
// branch below writes only a synthetic prompt-only review. Nothing stops the
// pane's live log tail for the job navigated away from (paneLogSeq is bumped
// only by scheduleDetailFollow), nothing clears a splitDetailErr belonging to
// it, and nothing starts the NEW job's tail until an unrelated jobs refresh
// reconciles it -- so the previous job's buffered log or error kept rendering
// beneath the new job's status. The Done branch's fetchReviewForPrompt has
// the same gap: it writes currentReview through handlePromptMsg without
// touching any pane-follow state.
func (m model) stepPromptNav(dir int) (tea.Model, tea.Cmd) {
	job, found, present := m.contentNavStep(dir, eligiblePromptRow)
	if !present {
		fallbackJob, cmd, ok := m.resolveAbsentSelection(viewKindPrompt, dir, eligiblePromptRow)
		if !ok {
			return m, cmd
		}
		job = fallbackJob
	} else if !found {
		return m, m.contentNavBoundary(viewKindPrompt, dir)
	}
	prevSelected := m.selectedJobID
	m = m.moveSelectionToJobID(job.ID)
	m.promptScroll = 0
	var followCmd tea.Cmd
	m, followCmd = m.followSelectionChange(prevSelected)
	if job.Status == storage.JobStatusDone {
		// Batched, not returned in place of the prompt fetch: both must
		// run. Dispatched as a standalone statement so the returned m
		// carries the seq bump (dispatchReviewFetch's evaluation-order
		// pitfall, fetch.go).
		promptCmd := m.dispatchPromptFetch(job.ID)
		return m, tea.Batch(followCmd, promptCmd)
	}
	if (job.Status == storage.JobStatusRunning || job.Status == storage.JobStatusQueued) && job.Prompt != "" {
		m.currentReview = &storage.Review{
			Agent:  job.Agent,
			Prompt: job.Prompt,
			Job:    &job,
		}
	}
	return m, followCmd
}

// stepLogNav walks the flattened rows for the queue-origin log view in direction
// dir (-1 = newer, +1 = older) and opens the target log, or flashes/paginates
// at the boundary.
//
// Its target view (viewLog) never touches currentReview -- the log view's
// content lives entirely in separate fields (logLines, paneLog*) -- so
// without an explicit follow here the split detail pane would keep
// showing the job the user navigated away from until an unrelated jobs
// refresh reconciled it (see cmd/roborev/tui's PR review finding on
// handlers.go:27). followSelectionChange only gates on m.layout (not
// splitActive()), so this still arms the debounced follow while the log view
// is on screen in split layout; the resulting tick/fetch lands quietly in
// the background and the pane is already caught up by the time the user
// returns to the queue. In stacked layout (or tasks-origin log nav, which
// never reaches this function -- see handleNextKey/handlePrevKey's
// logFromView == viewTasks branch) followSelectionChange is a no-op.
func (m model) stepLogNav(dir int) (tea.Model, tea.Cmd) {
	job, found, present := m.contentNavStep(dir, eligibleLogRow)
	if !present {
		fallbackJob, cmd, ok := m.resolveAbsentSelection(viewLog, dir, eligibleLogRow)
		if !ok {
			return m, cmd
		}
		job = fallbackJob
	} else if !found {
		return m, m.contentNavBoundary(viewLog, dir)
	}
	prevSelected := m.selectedJobID
	m = m.moveSelectionToJobID(job.ID)
	m.logStreaming = false
	var followCmd tea.Cmd
	m, followCmd = m.followSelectionChange(prevSelected)
	logModel, logCmd := m.openLogView(job, m.logFromView)
	return logModel, tea.Batch(followCmd, logCmd)
}

func (m model) handleNextKey() (tea.Model, tea.Cmd) {
	switch m.currentView {
	case viewQueue:
		return m.moveQueueSelection(-1), nil
	case viewReview:
		return m.stepReviewNav(-1)
	case viewKindPrompt:
		return m.stepPromptNav(-1)
	case viewLog:
		if m.logFromView == viewTasks {
			return m.nextFixLog()
		}
		return m.stepLogNav(-1)
	}
	return m, nil
}

// nextFixLog steps to the next (newer) tasks-origin fix-job log, or flashes the
// boundary. Tasks-origin log nav is index-based over m.fixJobs and unchanged.
func (m model) nextFixLog() (tea.Model, tea.Cmd) {
	idx := m.findNextLoggableFixJob()
	if idx >= 0 {
		m.fixSelectedIdx = idx
		job := m.fixJobs[idx]
		m.logStreaming = false
		return m.openLogView(job, viewTasks)
	}
	m.setFlash("No newer log", 2*time.Second, viewLog)
	return m, nil
}

func (m model) handleDownKey() (tea.Model, tea.Cmd) {
	switch m.currentView {
	case viewQueue:
		return m.queueNavDown()
	case viewReview:
		m.reviewScroll++
		if m.mdCache != nil && m.reviewScroll > m.mdCache.lastReviewMaxScroll {
			m.reviewScroll = m.mdCache.lastReviewMaxScroll
		}
	case viewKindPrompt:
		m.promptScroll++
		if m.mdCache != nil && m.promptScroll > m.mdCache.lastPromptMaxScroll {
			m.promptScroll = m.mdCache.lastPromptMaxScroll
		}
	case viewCommitMsg:
		m.commitMsgScroll++
	case viewHelp:
		m.helpScroll = min(m.helpScroll+1, m.helpMaxScroll())
	}
	return m, nil
}

func (m model) handlePrevKey() (tea.Model, tea.Cmd) {
	switch m.currentView {
	case viewQueue:
		return m.prevQueueSelection()
	case viewReview:
		return m.stepReviewNav(+1)
	case viewKindPrompt:
		return m.stepPromptNav(+1)
	case viewLog:
		if m.logFromView == viewTasks {
			return m.prevFixLog()
		}
		return m.stepLogNav(+1)
	}
	return m, nil
}

// prevQueueSelection moves the queue cursor to the older (higher-index) row,
// prefetching when near the end or resuming pagination at the bottom boundary.
func (m model) prevQueueSelection() (tea.Model, tea.Cmd) {
	prevID := m.selectedJobID
	m = m.moveQueueSelection(+1)
	if m.selectedJobID != prevID {
		if cmd := m.maybePrefetch(m.selectedIdx); cmd != nil {
			return m, cmd
		}
	} else if m.canPaginate() {
		m.loadingMore = true
		return m, m.fetchMoreJobs()
	}
	return m, nil
}

// prevFixLog steps to the previous (older) tasks-origin fix-job log, or flashes
// the boundary. Tasks-origin log nav is index-based over m.fixJobs and unchanged.
func (m model) prevFixLog() (tea.Model, tea.Cmd) {
	idx := m.findPrevLoggableFixJob()
	if idx >= 0 {
		m.fixSelectedIdx = idx
		job := m.fixJobs[idx]
		m.logStreaming = false
		return m.openLogView(job, viewTasks)
	}
	m.setFlash("No older log", 2*time.Second, viewLog)
	return m, nil
}

func (m model) handlePageUpKey() (tea.Model, tea.Cmd) {
	pageSize := max(1, m.height-10)
	switch m.currentView {
	case viewQueue:
		return m.moveQueueSelection(-pageSize), nil
	case viewReview:
		if m.mdCache != nil && m.reviewScroll > m.mdCache.lastReviewMaxScroll {
			m.reviewScroll = m.mdCache.lastReviewMaxScroll
		}
		m.reviewScroll = max(0, m.reviewScroll-pageSize)
		return m, tea.ClearScreen
	case viewKindPrompt:
		if m.mdCache != nil && m.promptScroll > m.mdCache.lastPromptMaxScroll {
			m.promptScroll = m.mdCache.lastPromptMaxScroll
		}
		m.promptScroll = max(0, m.promptScroll-pageSize)
		return m, tea.ClearScreen
	case viewHelp:
		m.helpScroll = max(0, m.helpScroll-pageSize)
	}
	return m, nil
}

func (m model) handlePageDownKey() (tea.Model, tea.Cmd) {
	pageSize := max(1, m.height-10)
	switch m.currentView {
	case viewQueue:
		rows := m.visibleQueueRows()
		idx := m.selectedRowIndex(rows)
		reachedEnd := idx >= 0 && idx+pageSize >= len(rows)
		m = m.moveQueueSelection(+pageSize)
		if reachedEnd && m.canPaginate() {
			m.loadingMore = true
			return m, m.fetchMoreJobs()
		}
		if cmd := m.maybePrefetch(m.selectedIdx); cmd != nil {
			return m, cmd
		}
	case viewReview:
		m.reviewScroll += pageSize
		if m.mdCache != nil && m.reviewScroll > m.mdCache.lastReviewMaxScroll {
			m.reviewScroll = m.mdCache.lastReviewMaxScroll
		}
		return m, tea.ClearScreen
	case viewKindPrompt:
		m.promptScroll += pageSize
		if m.mdCache != nil && m.promptScroll > m.mdCache.lastPromptMaxScroll {
			m.promptScroll = m.mdCache.lastPromptMaxScroll
		}
		return m, tea.ClearScreen
	case viewHelp:
		m.helpScroll = min(m.helpScroll+pageSize, m.helpMaxScroll())
	}
	return m, nil
}

func (m model) handleHelpKey() (tea.Model, tea.Cmd) {
	if m.currentView == viewHelp {
		m.currentView = m.helpFromView
		return m, nil
	}
	if m.currentView == viewQueue || m.currentView == viewReview || m.currentView == viewKindPrompt || m.currentView == viewLog {
		m.helpFromView = m.currentView
		m.currentView = viewHelp
		m.helpScroll = 0
		return m, nil
	}
	return m, nil
}

func (m model) handleEscKey() (tea.Model, tea.Cmd) {
	// See handleQuitKey's matching comment: a tasks-origin review must
	// fall through to the general viewReview branch below (which returns
	// to reviewFromView == viewTasks) instead of this split-pane-specific
	// "back to the queue list" shortcut.
	if m.layout == layoutSplit && m.currentView == viewReview && m.reviewFromView != viewTasks {
		if m.reviewFixPanelOpen {
			m.closeFixPanel()
			return m, nil
		}
		m.focus = focusList
		m.currentView = viewQueue
		// The review just left may now be hidden -- closing it with
		// hideClosed enabled removes its row -- which would leave the
		// selection pointing at a job with no highlighted row, so later
		// actions would target a job the user cannot see. Normalize
		// exactly as the stacked return path does, and refill the queue
		// when hideClosed pruned it (the same refill the stacked path
		// performs). The follow transition (followSelectionChange) is NOT
		// called here: handleKeyMsg's wrapper performs it after this
		// handler returns to viewQueue, against the prevSelected it
		// captured before dispatch, so calling it here too would
		// double-bump detailFollowGen and schedule a stale extra tick.
		m.normalizeSelectionIfHidden()
		if m.hideClosed && !m.loadingJobs {
			m.loadingJobs = true
			return m, m.fetchJobs()
		}
		return m, nil
	}
	if m.currentView == viewQueue && len(m.filterStack) > 0 {
		popped := m.popFilter()
		if popped == filterTypeRepo || popped == filterTypeBranch {
			m.resetQueueForFilterChange()
			return m, m.fetchJobs()
		}
		return m, nil
	} else if m.currentView == viewQueue && m.hideClosed {
		m.hideClosed = false
		m.resetQueueForFilterChange()
		return m, m.fetchJobs()
	} else if m.currentView == viewReview {
		// If fix panel is open (unfocused), esc closes it rather than leaving the review
		if m.reviewFixPanelOpen {
			m.closeFixPanel()
			return m, nil
		}
		m.closeFixPanel()
		returnTo := m.reviewFromView
		if returnTo == 0 {
			returnTo = viewQueue
		}
		m.currentView = returnTo
		m.currentReview = nil
		m.reviewScroll = 0
		m.paginateNav = 0
		if returnTo == viewQueue {
			m.normalizeSelectionIfHidden()
			if m.hideClosed && !m.loadingJobs {
				m.loadingJobs = true
				return m, m.fetchJobs()
			}
		}
	} else if m.currentView == viewKindPrompt {
		m.paginateNav = 0
		if m.promptFromQueue {
			m.currentView = viewQueue
			m.promptScroll = 0
			m = m.preserveOrClearReviewOnQueueReturn()
		} else {
			m.currentView = viewReview
			m.promptScroll = 0
		}
	} else if m.currentView == viewCommitMsg {
		m.currentView = m.commitMsgFromView
		m.commitMsgContent = ""
		m.commitMsgScroll = 0
	} else if m.currentView == viewHelp {
		m.currentView = m.helpFromView
	}
	return m, nil
}

// openLogView opens the log view for a job of any status.
// Running jobs stream with follow; completed jobs show a static view.
func (m model) openLogView(
	job storage.ReviewJob, fromView viewKind,
) (tea.Model, tea.Cmd) {
	m.logReviewAnchored = m.isReviewAnchored()
	m.logJobID = job.ID
	m.logAgent = job.Agent
	m.logSource = job.Source
	m.logLines = nil
	m.logScroll = 0
	m.logFromView = fromView
	m.currentView = viewLog
	m.logOffset = 0
	m.logFmtr = streamfmt.NewWithWidth(
		io.Discard, m.width, m.glamourStyle,
		decoderForJobLog(m.logAgent, m.logSource),
	)
	m.logFetchSeq++
	m.logLoading = true

	if job.Status == storage.JobStatusRunning {
		m.logStreaming = true
		m.logFollow = true
	} else {
		m.logStreaming = false
		m.logFollow = false
	}

	return m, tea.Batch(tea.ClearScreen, m.fetchJobLog(job.ID))
}

// handleConnectionError tracks consecutive connection errors and triggers reconnection.
func (m *model) handleConnectionError(err error) tea.Cmd {
	if isConnectionError(err) {
		m.consecutiveErrors++
		if m.consecutiveErrors >= 3 && !m.reconnecting {
			m.reconnecting = true
			return m.tryReconnect()
		}
	}
	return nil
}
