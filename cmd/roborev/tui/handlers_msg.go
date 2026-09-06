package tui

import (
	"errors"
	"fmt"
	"io"
	"log"
	"slices"
	"sort"
	"strings"
	"time"
	"uuid"

	tea "charm.land/bubbletea/v2"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/streamfmt"
	"go.kenn.io/roborev/internal/version"
)

// handleJobsMsg processes job list updates from the server.
func (m model) handleJobsMsg(msg jobsMsg) (tea.Model, tea.Cmd) {
	// Discard stale responses from before a filter change.
	if msg.seq < m.fetchSeq {
		m.paginateNav = 0
		m.loadingMore = false
		return m, nil
	}

	if msg.append || m.paginateNav == 0 {
		m.loadingMore = false
	}
	if !msg.append {
		m.loadingJobs = false
	}
	m.consecutiveErrors = 0

	m.hasMore = msg.hasMore

	m.updateDisplayNameCache(msg.jobs)

	if msg.append {
		m.jobs = append(m.jobs, msg.jobs...)
	} else {
		m.jobs = msg.jobs
	}
	m.queueColGen++

	// Clear pending closed states that server has confirmed
	for jobID, pending := range m.pendingClosed {
		found := false
		for i := range m.jobs {
			if m.jobs[i].ID == jobID {
				found = true
				serverState := m.jobs[i].Closed != nil && *m.jobs[i].Closed
				if serverState == pending.newState {
					delete(m.pendingClosed, jobID)
				}
				break
			}
		}
		// When hideClosed is active, closed jobs are filtered
		// out of the response. If a pending "mark closed" job is
		// absent from the response, that confirms the server absorbed
		// the change — clear it to prevent delta double-counting.
		if !found && m.hideClosed && pending.newState {
			delete(m.pendingClosed, jobID)
		}
	}

	if !msg.append {
		m.jobStats = msg.stats
		// Re-apply only unconfirmed pending deltas so that
		// rollback math stays correct without double-counting
		// entries the server has already absorbed.
		for _, pending := range m.pendingClosed {
			m.applyStatsDelta(pending.newState)
		}
	}

	// Apply any remaining pending closed changes to prevent flash
	for i := range m.jobs {
		if pending, ok := m.pendingClosed[m.jobs[i].ID]; ok {
			newState := pending.newState
			m.jobs[i].Closed = &newState
		}
	}

	// Selection management
	normalizePrevSelected := m.selectedJobID
	if m.selectedIsMember() {
		// A panel member is selected. Members are side-fetched and absent
		// from the parents-only m.jobs, so keep the id authoritative and
		// leave selectedIdx as a no-op hint; moveQueueSelection re-derives
		// the cursor from the flattened rows on the next nav key.
		m.selectedIdx = -1
	} else if len(m.jobs) == 0 {
		m.selectedIdx = -1
		if !m.isReviewAnchored() {
			m.selectedJobID = 0
		}
	} else if m.selectedJobID > 0 {
		found := false
		for i, job := range m.jobs {
			if job.ID == m.selectedJobID {
				m.selectedIdx = i
				found = true
				break
			}
		}

		if !found && m.isReviewAnchored() {
			// Review-rooted views: leave selectedIdx/selectedJobID
			// as-is so ←/→ navigation stays anchored to the displayed
			// review's position. normalizeSelectionIfHidden adjusts
			// on return to queue.
		} else if !found {
			m.selectedIdx = max(0, min(len(m.jobs)-1, m.selectedIdx))
			if len(m.activeRepoFilter) > 0 || m.hideClosed {
				idx := m.findNearestVisibleJob(m.selectedIdx)
				if idx >= 0 {
					m.selectedIdx = idx
					m.selectedJobID = m.jobs[idx].ID
				} else {
					m.selectedIdx = -1
					m.selectedJobID = 0
				}
			} else {
				m.selectedJobID = m.jobs[m.selectedIdx].ID
			}
		} else if !m.isJobVisible(m.jobs[m.selectedIdx]) && m.currentView == viewQueue {
			// Only adjust selection in queue view. In review/prompt/log
			// views, ←/→ navigation is relative to the viewed job's
			// position; normalizeSelectionIfHidden handles it on return
			// to queue.
			idx := m.findNearestVisibleJob(m.selectedIdx)
			if idx >= 0 {
				m.selectedIdx = idx
				m.selectedJobID = m.jobs[idx].ID
			} else {
				m.selectedIdx = -1
				m.selectedJobID = 0
			}
		}
	} else if m.currentView == viewReview && m.currentReview != nil && m.currentReview.Job != nil {
		targetID := m.currentReview.Job.ID
		for i, job := range m.jobs {
			if job.ID == targetID {
				m.selectedIdx = i
				m.selectedJobID = targetID
				break
			}
		}
		if m.selectedJobID == 0 {
			m.selectedIdx = 0
			m.selectedJobID = m.jobs[0].ID
		}
	} else {
		firstVisible := m.findFirstVisibleJob()
		if firstVisible >= 0 {
			m.selectedIdx = firstVisible
			m.selectedJobID = m.jobs[firstVisible].ID
		} else if len(m.activeRepoFilter) == 0 && len(m.jobs) > 0 {
			m.selectedIdx = 0
			m.selectedJobID = m.jobs[0].ID
		} else {
			m.selectedIdx = -1
			m.selectedJobID = 0
		}
	}

	// The selection reassignments above (a selected job that vanished from
	// the refreshed list, or became hidden by a filter) don't go through
	// followSelectionChange -- splitReconcileDetail below is the authority
	// for the detail pane's CONTENT on this path -- but the fix panel is
	// keyed to a job rather than to content, so it needs the same treatment
	// scheduleDetailFollow gives it on every other selection change.
	// splitActive() rather than layout: a tasks-origin review renders
	// full-screen with a selection that legitimately isn't the displayed
	// job's, and stacked mode can show one job's review while the queue
	// selection sits elsewhere.
	if m.splitActive() {
		m.closeFixPanelIfJobChanged()
	} else if m.reviewFixPanelPending &&
		m.selectedJobID != normalizePrevSelected &&
		m.fixPromptJobID == normalizePrevSelected {
		// Outside split only the PENDING half of the fix intent is
		// selection-bound: F was pressed with the selection on the intent's
		// job and no review on screen yet, so a normalization that moves the
		// selection off it abandons the request exactly like the
		// pending-open disarm below (same keying, same rule). An OPEN panel
		// is different outside split -- it is bound to the review displayed
		// full-screen, whose viewReview branch above re-syncs the selection
		// to it -- so it is deliberately left alone here.
		m.closeFixPanel()
	}
	// The pending-open intent gets the same treatment. Waiting for the
	// reactive clear (a message for the old job arriving with a
	// mismatched jobID) is not enough: when nothing is in flight for the
	// old job, nothing arrives, the intent waits for the selection to
	// come back, and the next response for that job -- typically
	// reconcile's follow fetch after a later refresh reselects it --
	// consumes it and opens the review unbidden.
	//
	// The abandonment rule, stated once: ANY selection change abandons an
	// intent bound to the job it moves off, whoever causes it -- user
	// navigation (followSelectionChange), a jobs refresh normalizing the
	// selection (here), a rollback, or the control socket. Keyed on the
	// intent's own job (normalizePrevSelected) so it cannot touch an
	// intent armed for a job the selection never sat on, and not gated on
	// layout -- an intent is a request to open a review, not a pane, and
	// a stacked queue Enter arms it identically.
	if m.pendingReviewOpenJobID != 0 &&
		m.selectedJobID != normalizePrevSelected &&
		m.pendingReviewOpenJobID == normalizePrevSelected {
		m.disarmPendingReviewOpen()
	}
	// ABANDONMENT bumper (see detailFollowGen's contract, tui.go): the
	// disarms above make an abandoned intent unservable, but an armed-era
	// ORDINARY dispatch is dangerous even with no intent left -- if the
	// user is still on its dispatch origin view when it lands gen-fresh,
	// openReviewView switches views with no fresh request. A refresh
	// normalization that moves the selection is the same abandonment
	// event as user navigation, so it dooms in-flight dispatches the same
	// way. A refresh that leaves the selection in place bumps nothing.
	if m.selectedJobID != normalizePrevSelected {
		m.detailFollowGen++
		// Same abandonment event, same request-scoped state: doom an
		// in-flight prompt response and release the reconcile suppression
		// slot for the abandoned era. See
		// abandonInFlightSelectionRequests (layout.go).
		m.abandonInFlightSelectionRequests()
	}

	// Auto-paginate when hide-closed hides too many jobs
	if m.currentView == viewQueue &&
		m.hideClosed &&
		m.canPaginate() &&
		len(m.getVisibleJobs()) < m.queueVisibleRows() {
		m.loadingMore = true
		cmds := []tea.Cmd{m.fetchMoreJobs()}
		var reconcileCmd tea.Cmd
		m, reconcileCmd = m.splitReconcileDetail()
		if reconcileCmd != nil {
			cmds = append(cmds, reconcileCmd)
		}
		return m, tea.Batch(cmds...)
	}

	// Carries the review and prompt arms' detail-follow command out of the
	// switch below: unlike the log arm, those arms' non-Done branches fall
	// through to this function's shared tail (splitReconcileDetail and the
	// SSE/panel refreshes), so the command has to be batched there rather
	// than returned early.
	var navFollowCmd tea.Cmd
	// Auto-navigate after pagination triggered from review/prompt view
	if msg.append && m.paginateNav != 0 && m.currentView == m.paginateNav {
		nav := m.paginateNav
		m.paginateNav = 0
		switch nav {
		case viewReview:
			nextIdx := m.stepVisibleJobIndex(1, eligibleReviewRow)
			if nextIdx >= 0 {
				// Same shape as stepReviewNav (handlers.go): the branches
				// below replace currentReview for the new job, so the
				// selection change must go through the shared transition
				// (dropping the old job's splitDetailErr and pending
				// intents). This jump changes the DISPLAYED review, so
				// outside split a panel bound to the previous job -- open
				// or pending -- must also close; followSelectionChange
				// only closes the pending half there, hence the explicit
				// close below.
				prevSelected := m.selectedJobID
				m.selectedIdx = nextIdx
				m.updateSelectedJobID()
				m.reviewScroll = 0
				job := m.jobs[nextIdx]
				m, navFollowCmd = m.followSelectionChange(prevSelected)
				if m.layout != layoutSplit {
					m.closeFixPanelIfJobChanged()
				}
				switch job.Status {
				case storage.JobStatusDone:
					cmd := m.dispatchReviewFetch(job.ID)
					return m, tea.Batch(navFollowCmd, cmd)
				case storage.JobStatusFailed:
					// Shared synthesized acceptance: also loads persisted
					// comments, which no review fetch can carry for a
					// row-less review. Folded into navFollowCmd so the
					// arm's fall-through bookkeeping below still runs.
					commentsCmd := m.acceptSynthesizedFailure(job.ID, synthesizeFailedReview(&job, m.currentReview))
					navFollowCmd = tea.Batch(navFollowCmd, commentsCmd)
				}
			}
		case viewKindPrompt:
			nextIdx := m.stepVisibleJobIndex(1, eligiblePromptRow)
			if nextIdx >= 0 {
				// Same shape as stepPromptNav (handlers.go): this arm
				// resumes prompt-view content nav past the previously
				// loaded page, and eligiblePromptRow admits running/queued
				// jobs, so the selection change needs the shared follow
				// transition (stop the old tail, drop splitDetailErr,
				// start the new job's) -- this early return skips the
				// splitReconcileDetail call at the bottom of the function.
				prevSelected := m.selectedJobID
				m.selectedIdx = nextIdx
				m.updateSelectedJobID()
				m.promptScroll = 0
				job := m.jobs[nextIdx]
				m, navFollowCmd = m.followSelectionChange(prevSelected)
				// This jump changes the displayed content, so outside
				// split a panel bound to the previous job -- open or
				// pending -- must close; followSelectionChange only
				// closes the pending half there.
				if m.layout != layoutSplit {
					m.closeFixPanelIfJobChanged()
				}
				if job.Status == storage.JobStatusDone {
					promptCmd := m.dispatchPromptFetch(job.ID)
					return m, tea.Batch(navFollowCmd, promptCmd)
				} else if (job.Status == storage.JobStatusRunning || job.Status == storage.JobStatusQueued) && job.Prompt != "" {
					m.currentReview = &storage.Review{
						Agent:  job.Agent,
						Prompt: job.Prompt,
						Job:    &job,
					}
				}
			}
		case viewLog:
			nextIdx := m.stepVisibleJobIndex(1, eligibleLogRow)
			if nextIdx >= 0 {
				// Same gap as stepLogNav (handlers.go): this auto-navigates
				// selectedJobID after a pagination fetch resumes log-view
				// content nav past the previously loaded page, but the log
				// view never touches currentReview, so the split detail pane
				// needs its own explicit follow here too -- this early
				// return skips the splitReconcileDetail call at the bottom
				// of this function entirely.
				prevSelected := m.selectedJobID
				m.selectedIdx = nextIdx
				m.updateSelectedJobID()
				job := m.jobs[nextIdx]
				var followCmd tea.Cmd
				m, followCmd = m.followSelectionChange(prevSelected)
				logModel, logCmd := m.openLogView(job, m.logFromView)
				return logModel, tea.Batch(followCmd, logCmd)
			}
		}
	} else if !msg.append && !m.loadingMore {
		m.paginateNav = 0
	}

	cmds := []tea.Cmd{m.consumeSSEPendingRefresh()}
	if navFollowCmd != nil {
		cmds = append(cmds, navFollowCmd)
	}
	for _, uuid := range m.staleExpandedPanelRuns() {
		cmds = append(cmds, m.fetchPanelMembers(uuid))
	}
	var reconcileCmd tea.Cmd
	m, reconcileCmd = m.splitReconcileDetail()
	if reconcileCmd != nil {
		cmds = append(cmds, reconcileCmd)
	}
	return m, tea.Batch(cmds...)
}

// staleExpandedPanelRuns returns the panel_run_uuids of expanded panels whose
// synthesis parent is still in m.jobs and whose cached members include at least
// one non-terminal (queued/running) row, so the cache should be refreshed.
// Collapsed and all-terminal panels are skipped; nil when nothing is expanded.
func (m model) staleExpandedPanelRuns() []uuid.UUID {
	if len(m.expandedPanels) == 0 {
		return nil
	}
	visible := make(map[uuid.UUID]bool, len(m.jobs))
	for i := range m.jobs {
		if u := m.jobs[i].PanelRunUUID; u != nil && m.jobs[i].IsSynthesisJob() {
			visible[*u] = true
		}
	}
	var runs []uuid.UUID
	for runUUID := range m.expandedPanels {
		if !visible[runUUID] {
			continue
		}
		for _, mem := range m.panelMembers[runUUID] {
			if mem.Status == storage.JobStatusQueued || mem.Status == storage.JobStatusRunning {
				runs = append(runs, runUUID)
				break
			}
		}
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].Compare(runs[j]) < 0 })
	return runs
}

// handlePanelMembersMsg caches side-fetched panel members on success. On error
// it flashes and leaves the panel uncached, so a later expand retries.
func (m model) handlePanelMembersMsg(msg panelMembersMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.setFlash("Couldn't load panel members — collapse and expand to retry",
			3*time.Second, viewQueue)
		return m, nil
	}
	m.panelMembers[msg.runUUID] = msg.members
	m.queueColGen++
	// Panel members are absent from the main jobs response, so THIS message
	// is the only carrier of a selected member's status transition -- the
	// jobs-refresh path that normally follows a transition with
	// splitReconcileDetail never sees it. Reconcile here when the selection
	// belongs to this run, or a member going queued->running/done leaves
	// the detail pane stalled (no log tail, no review fetch) until an
	// unrelated SSE event or the fallback poll. Safe for a selected
	// synthesis PARENT too: reconcile is idempotent against m.jobs state.
	if job, ok := m.selectedJob(); ok && job.PanelRunUUID != nil && *job.PanelRunUUID == msg.runUUID {
		return m.splitReconcileDetail()
	}
	return m, nil
}

// handleCostMsg stores the latest cost aggregate, discarding responses from
// before a filter change (same fetchSeq guard as handleJobsMsg). It records the
// response's fetchSeq so costSegmentText can hide the segment after a later
// filter change until the matching cost response arrives.
func (m model) handleCostMsg(msg costMsg) (tea.Model, tea.Cmd) {
	if msg.seq < m.fetchSeq {
		return m, nil
	}
	m.cost = msg.cost
	m.costSeq = msg.seq
	return m, nil
}

// handleStatusMsg processes daemon status updates.
func (m model) handleStatusMsg(msg statusMsg) (tea.Model, tea.Cmd) {
	if msg.gen < m.fetchGen {
		return m, nil // discard pre-reconnect response
	}
	m.loadingStatus = false
	m.status = msg.status
	m.consecutiveErrors = 0
	if m.status.Version != "" {
		m.daemonVersion = m.status.Version
		m.versionMismatch = m.daemonVersion != version.Version
	}
	if m.statusFetchedOnce && m.status.ConfigReloadCounter != m.lastConfigReloadCounter {
		m.setFlash("Config reloaded", 5*time.Second, m.currentView)
	}
	m.lastConfigReloadCounter = m.status.ConfigReloadCounter
	m.statusFetchedOnce = true
	if m.statusStale {
		m.statusStale = false
		m.loadingStatus = true
		return m, m.fetchStatus()
	}
	return m, nil
}

// handleRepoNamesMsg stores the display-name-to-root-paths mapping
// fetched from /api/repos at init, used by control socket set-filter.
func (m model) handleRepoNamesMsg(
	msg repoNamesMsg,
) (tea.Model, tea.Cmd) {
	if msg.names != nil {
		m.repoNames = msg.names
		m.repoIdentities = msg.identities
		if m.reconcileAutoRepoFilter() {
			return m, m.fetchJobs()
		}
	}
	return m, nil
}

// handleReposMsg processes repo list results for the filter modal.
func (m model) handleReposMsg(
	msg reposMsg,
) (tea.Model, tea.Cmd) {
	m.consecutiveErrors = 0
	refetchJobs := false

	// Refresh repoNames when the modal fetch was unfiltered (no
	// branch constraint), so newly registered repos are picked up.
	// Skip branch-filtered responses — they are a subset and would
	// clobber the authoritative mapping.
	if !msg.branchFiltered {
		names := make(map[string][]string, len(msg.repos))
		for _, r := range msg.repos {
			names[r.name] = r.rootPaths
		}
		m.repoNames = names
		m.repoIdentities = msg.identities
		refetchJobs = m.reconcileAutoRepoFilter()
	}

	// Build filterTree from repos (all collapsed, no children)
	m.filterTree = make([]treeFilterNode, len(msg.repos))
	for i, r := range msg.repos {
		m.filterTree[i] = treeFilterNode{
			name:      r.name,
			rootPaths: r.rootPaths,
			count:     r.count,
		}
	}
	// Move cwd repo to first position for quick access
	if m.cwdRepoRoot != "" && len(m.filterTree) > 1 {
		moveToFront(m.filterTree, func(n treeFilterNode) bool {
			return slices.Contains(n.rootPaths, m.cwdRepoRoot)
		})
	}
	m.rebuildFilterFlatList()
	// Pre-select active filter if any
	if len(m.activeRepoFilter) > 0 {
		for i, entry := range m.filterFlatList {
			if entry.repoIdx >= 0 && entry.branchIdx == -1 &&
				rootPathsMatch(
					m.filterTree[entry.repoIdx].rootPaths,
					m.activeRepoFilter,
				) {
				m.filterSelectedIdx = i
				break
			}
		}
	}
	// Auto-expand repo to branches when opened via 'b' key
	if m.filterBranchMode && len(m.filterTree) > 0 {
		targetIdx := 0
		if len(m.activeRepoFilter) > 0 {
			for i, node := range m.filterTree {
				if rootPathsMatch(
					node.rootPaths, m.activeRepoFilter,
				) {
					targetIdx = i
					goto foundTarget
				}
			}
		}
		if m.cwdRepoRoot != "" {
			for i, node := range m.filterTree {
				for _, p := range node.rootPaths {
					if p == m.cwdRepoRoot {
						targetIdx = i
						goto foundTarget
					}
				}
			}
		}
	foundTarget:
		m.filterTree[targetIdx].loading = true
		for i, entry := range m.filterFlatList {
			if entry.repoIdx == targetIdx &&
				entry.branchIdx == -1 {
				m.filterSelectedIdx = i
				break
			}
		}
		branchCmd := m.fetchBranchesForRepo(
			m.filterTree[targetIdx].rootPaths,
			targetIdx, true, m.filterSearchSeq,
		)
		if refetchJobs {
			return m, tea.Batch(m.fetchJobs(), branchCmd)
		}
		return m, branchCmd
	}
	// If user typed search before repos loaded, kick off fetches
	if cmd := m.fetchUnloadedBranches(); cmd != nil {
		if refetchJobs {
			return m, tea.Batch(m.fetchJobs(), cmd)
		}
		return m, cmd
	}
	if refetchJobs {
		return m, m.fetchJobs()
	}
	return m, nil
}

// handleRepoBranchesMsg processes branch list results for a repo.
func (m model) handleRepoBranchesMsg(
	msg repoBranchesMsg,
) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.err = msg.err
		m.filterBranchMode = false
		if msg.repoIdx >= 0 &&
			msg.repoIdx < len(m.filterTree) &&
			rootPathsMatch(
				m.filterTree[msg.repoIdx].rootPaths,
				msg.rootPaths,
			) {
			m.filterTree[msg.repoIdx].loading = false
			if !msg.expandOnLoad && m.filterSearch != "" &&
				msg.searchSeq == m.filterSearchSeq {
				m.filterTree[msg.repoIdx].fetchFailed = true
			}
		}
		if cmd := m.handleConnectionError(msg.err); cmd != nil {
			return m, cmd
		}
		return m, m.fetchUnloadedBranches()
	}
	// Verify filter view, repoIdx valid, and identity matches
	if m.currentView == viewFilter &&
		msg.repoIdx >= 0 &&
		msg.repoIdx < len(m.filterTree) &&
		rootPathsMatch(
			m.filterTree[msg.repoIdx].rootPaths,
			msg.rootPaths,
		) {
		m.consecutiveErrors = 0
		m.filterTree[msg.repoIdx].loading = false
		m.filterTree[msg.repoIdx].children = msg.branches
		if msg.expandOnLoad {
			m.filterTree[msg.repoIdx].expanded = true
		}
		// Move cwd branch to first position if this is cwd repo
		if m.cwdBranch != "" && len(msg.branches) > 1 {
			isCwdRepo := slices.Contains(
				m.filterTree[msg.repoIdx].rootPaths,
				m.cwdRepoRoot,
			)
			if isCwdRepo {
				moveToFront(
					m.filterTree[msg.repoIdx].children,
					func(b branchFilterItem) bool {
						return b.name == m.cwdBranch
					},
				)
			}
		}
		m.rebuildFilterFlatList()
		// Auto-position on first branch when opened via 'b'
		if m.filterBranchMode {
			m.filterBranchMode = false
			for i, entry := range m.filterFlatList {
				if entry.repoIdx == msg.repoIdx &&
					entry.branchIdx >= 0 {
					m.filterSelectedIdx = i
					break
				}
			}
		}
		if cmd := m.fetchUnloadedBranches(); cmd != nil {
			return m, cmd
		}
	}
	return m, nil
}

// handleBranchesMsg processes branch backfill completion.
func (m model) handleBranchesMsg(
	msg branchesMsg,
) (tea.Model, tea.Cmd) {
	m.consecutiveErrors = 0
	m.branchBackfillDone = true
	if msg.backfillCount > 0 {
		m.setFlash(fmt.Sprintf(
			"Backfilled branch info for %d jobs",
			msg.backfillCount,
		), 5*time.Second, viewFilter)
	}
	return m, nil
}

// handleFixJobsMsg processes fix job list results.
func (m model) handleFixJobsMsg(
	msg fixJobsMsg,
) (tea.Model, tea.Cmd) {
	if msg.gen < m.fetchGen {
		return m, nil // discard pre-reconnect response
	}
	m.loadingFixJobs = false
	if msg.err != nil {
		m.err = msg.err
	} else {
		m.fixJobs = msg.jobs
		m.taskColGen++
		if m.fixSelectedIdx >= len(m.fixJobs) &&
			len(m.fixJobs) > 0 {
			m.fixSelectedIdx = len(m.fixJobs) - 1
		}
	}
	// A state-mutating handler requested a refresh while this fetch was
	// in flight. The data we just received predates that mutation, so
	// dispatch a follow-up fetch to pick up the latest state.
	if m.fixJobsStale {
		m.fixJobsStale = false
		m.loadingFixJobs = true
		return m, m.fetchFixJobs()
	}
	return m, nil
}

// releaseReconcileFetch clears splitReconcileDetail's duplicate-dispatch
// suppression slot when the response to the dispatch that set it arrives,
// identified by that dispatch's own fetch epoch. Called
// unconditionally at the top of both review-response handlers, BEFORE any
// staleness check, because a response for a given dispatch arrives exactly
// once: if the arrival of the outstanding dispatch's own response didn't
// release the slot, nothing else would.
//
// Keyed on the epoch, NOT on jobID: an older response for the same job
// (e.g. a selection-follow fetch dispatched before reconcile's) would
// otherwise unlock the gate for a dispatch that is still in flight, the
// next jobs refresh would dispatch again and supersede it, and with
// fetches slower than the refresh interval nothing would ever land --
// suppression exists for exactly that liveness property, so releasing it
// on anything but the matching response defeats its only purpose. A real
// epoch value is never 0 and reconcileFetchSeq is 0 exactly when nothing
// is outstanding, so a 0-stamped message can never match. See
// reconcileFetchSeq's doc comment (tui.go) for the stuck-forever
// derivation this ordering satisfies.
func (m *model) releaseReconcileFetch(fetchSeq uint64) {
	if fetchSeq != 0 && m.reconcileFetchSeq == fetchSeq {
		m.reconcileFetchJobID = 0
		m.reconcileFetchSeq = 0
	}
}

// acceptReview applies a review response that has been confirmed FRESHEST
// (its fetchSeq still equals m.reviewFetchSeq) to the displayed content.
// This is the single acceptance point for every dispatcher, follow or not,
// so anything that must happen exactly when currentReview is replaced
// lives here and cannot drift from the ordering logic.
func (m *model) acceptReview(msg reviewMsg) {
	m.consecutiveErrors = 0
	m.currentReview = msg.review
	m.currentResponses = msg.responses
	m.currentBranch = msg.branchName
	m.reviewScroll = 0
	// The review is now ACCEPTED -- clear here, not at dispatch
	// (splitReconcileDetail), so a fetch that instead FAILS
	// (handleReviewFollowErrMsg) leaves the observation outstanding and a
	// later reconcile pass retries. Scoped to the accepted job: an
	// observation for a DIFFERENT job's loaded review must survive this
	// acceptance. Within the same job the clear still wipes an
	// observation made AFTER dispatch (the job going running again while
	// this fetch was in flight); if that happened, this response may be
	// stale relative to the job's CURRENT state, and a later jobs
	// refresh's own FinishedAt/Output comparison is what would eventually
	// catch that, same as for any other staleness. See
	// paneReviewSeenNonTerminalJob's doc comment (tui.go).
	if m.paneReviewSeenNonTerminalJob == msg.jobID {
		m.paneReviewSeenNonTerminalJob = 0
	}
	// The inline fix panel is scoped to the job it was opened for
	// (fixPromptJobID). The detail pane now shows a DIFFERENT job, so a
	// panel left open (or pending) over it would submit a fix against the
	// job the user is no longer looking at -- the same hazard
	// scheduleDetailFollow closes for selection changes, closed here for
	// the other half of the split's two-speed model: the CONTENT changing
	// under a panel whose job didn't. Living on the acceptance path means
	// it can never drift from the ordering rule that decides which
	// content wins. The queue-'F' flow is unaffected: it sets
	// fixPromptJobID to the job it fetches, so the arriving review is
	// that same job and the panel is left alone (and opened just below).
	if (m.reviewFixPanelOpen || m.reviewFixPanelPending) && m.fixPromptJobID != msg.jobID {
		m.closeFixPanel()
	}
	// Consume a queue-initiated pending fix panel ('F' from the queue sets
	// reviewFixPanelPending and dispatches a fetch for that job). Done on
	// the shared acceptance path rather than only for non-follow
	// responses, so whichever dispatch's response actually lands first for
	// that job opens the panel -- a follow fetch overtaking the 'F' fetch
	// no longer strands the pending flag.
	//
	// Opening the panel WITHOUT the view/focus switch produces a panel the
	// keyboard cannot reach: handleKeyMsg routes to the panel only when
	// currentView == viewReview, and normalizeSplitState pins focus =
	// focusList while currentView is viewQueue -- so the pane would render
	// a panel that LOOKS focused while every keystroke went to the queue
	// ('f' opening the filter modal, 'a' closing the review, 'q' quitting,
	// ... instead of typing the fix prompt). That was only reachable once
	// a FOLLOW response could consume the pending flag (an ordinary
	// response performs the switch itself just below), which is exactly
	// what moving this consume onto the shared path enabled: split, list
	// focus, 'F' on a job whose review isn't loaded, and a jobs refresh
	// whose reconcile fetch overtakes the 'F' fetch.
	//
	// A guarded switch, not an unconditional one: the pending flag IS
	// explicit user intent to open the panel, but that intent doesn't
	// override the guard's actual purpose, which is about WHEN the intent
	// is served -- a user who pressed 'F' and then opened the comment
	// editor while the fetch was in flight is mid-typing in a view they
	// chose afterward, and yanking them into the review view is precisely
	// the surprise the guard exists to prevent (the transient-view case
	// has its own named test).
	//
	// Guarded on the origin of the request that ARMED the flag
	// (fixPromptOrigin), NOT on msg.dispatchedFrom: the accepting
	// response need not be the 'F' fetch at all -- any later dispatcher's
	// response can supersede it and consume the flag -- so the armed
	// origin is the only value that describes the user's actual request
	// no matter which dispatcher's response lands first.
	// jobID-only matching is sound here (no fixPromptSeq check needed,
	// unlike the two stale-rejection branches below): acceptReview only
	// ever runs for a message whose fetchSeq equals the CURRENT
	// m.reviewFetchSeq (handleReviewMsg's caller already established that
	// by not taking the superseded branch), and fixPromptSeq can never
	// exceed whatever was true of reviewFetchSeq at its own arm time -- so
	// it is provably <= msg.fetchSeq here. See fixPromptSeq's doc comment,
	// tui.go.
	if m.reviewFixPanelPending && m.fixPromptJobID == msg.jobID {
		origin := m.fixPromptOrigin
		m.reviewFixPanelPending = false
		m.reviewFixPanelOpen = true
		m.reviewFixPanelFocused = true
		m.fixPromptOrigin = 0
		m.fixPromptFollowRetried = false
		m.fixPromptSeq = 0
		m.openReviewViewFrom(origin)
	}
	// The plain "open the review view" intent -- pendingReviewOpenJobID
	// generalizes the pattern just above to every ordinary dispatch, not
	// only the queue-'F' fix-panel one. Consumed on the shared acceptance
	// path regardless of msg.follow, for the same reason: whichever
	// dispatcher's response lands this job's content FIRST performs the
	// transition. A follow response never switches views on its own (see
	// the early return just below), so without this consume a follow that
	// beat the superseded ordinary response would leave nothing to ever
	// open the review. See pendingReviewOpenJobID's doc comment, tui.go.
	// Redundant (harmless) with the fix-panel branch above when both are
	// armed for the same job -- openReviewViewFrom is idempotent.
	// jobID-only matching is sound here for the same reason as above
	// (pendingReviewOpenSeq is provably <= msg.fetchSeq at this point).
	if m.pendingReviewOpenJobID == msg.jobID {
		origin := m.pendingReviewOpenOrigin
		m.pendingReviewOpenJobID = 0
		m.pendingReviewOpenOrigin = 0
		m.pendingReviewOpenSeq = 0
		m.openReviewViewFrom(origin)
	}
}

// openReviewViewFrom performs the guarded switch into the review view for
// a request that originated on view `origin`. Split out from openReviewView
// so the pending fix-panel consume can pass the origin of the request that
// ARMED it rather than of whatever response happens to serve it -- see
// fixPromptOrigin's doc comment (tui.go).
func (m *model) openReviewViewFrom(origin viewKind) {
	if m.currentView == origin || m.currentView == viewReview {
		m.currentView = viewReview
		if m.layout == layoutSplit {
			m.focus = focusDetail
		}
	}
}

// openReviewView performs the view switch an ORDINARY (non-follow) review
// response carries: a view the user explicitly navigated to after the
// fetch was dispatched -- e.g. Enter on a done job from the queue, then
// quickly 'c' before the review lands -- must not be yanked into the
// review view out from under the user. An allowlist of "queue-origin"
// views (viewQueue/viewReview/viewTasks) is NOT sound here: viewQueue and
// viewTasks are mutually reachable via 'T'/Esc while a fetch from EITHER
// one is still in flight, so an allowlist would let a fetch dispatched
// from the queue resolve into viewReview even after the user explicitly
// switched to Tasks (and the mirror case, tasks -> queue). Instead,
// compare against the fetch's true dispatch origin, carried on the message
// itself (msg.dispatchedFrom, stamped by fetchReview from currentView at
// command-creation time). m.reviewFromView is NOT that origin -- it is the
// Esc RETURN target, and the two diverge exactly when the review view
// dispatches its own fetch: arrow-key review nav runs with currentView ==
// viewReview while reviewFromView still says viewQueue, so comparing
// against it re-opened the review for a user who had already escaped back
// to the queue. Only switch when the user is still on the exact origin
// view, or already in viewReview.
func (m *model) openReviewView(msg reviewMsg) {
	m.openReviewViewFrom(msg.dispatchedFrom)
}

// reviewIntentRescuable reports whether a gen-stale rejection -- jobID
// still equals m.selectedJobID, only detailFollowGen differs -- should be
// treated as CURRENT instead of discarded: this message is still the
// single freshest dispatch for the job (fetchSeq matches m.reviewFetchSeq,
// so nothing newer has superseded it) AND a pending intent is genuinely
// armed for it.
//
// Why the rescue exists: an incidental gen bump (maybeBootstrapDetail's
// same-selection bootstrap on a layout toggle or resize) is not
// abandonment, but its own debounced follow tick can be dropped (the
// layout flips back before it fires) or can take a branch that dispatches
// nothing -- leaving a bump with nothing behind it to ever serve a
// still-armed intent. The response that WOULD have served it is this one;
// discarding it would either silently drop an explicit open-review request
// or leave the intent stranded for some much later, unrelated event to
// "spring open" with whatever content loads then.
//
// The invariant that makes rescuing safe: any intent still armed when a
// gen-mismatched response for its job arrives here is (a) still bound to
// the currently-selected job (that is what put the message in the
// gen-mismatch branch rather than the jobID-mismatch one), and (b) has had
// no rerun of that job confirmed, THAT THIS TUI OBSERVED, since it was
// armed -- enforced twice: handleRerunResultMsg disarms both intents
// unconditionally on its own msg.jobID, and callers only reach this
// function after the per-job attempt gate has already accepted the
// message. "This TUI observed" is deliberate: a rerun triggered by the
// CLI, another TUI instance, or the daemon produces no rerunResultMsg
// here, so neither the disarm nor the attempt bump happens for it -- an
// input boundary shared by every rerun-driven invalidation in this file.
// Within that boundary, an intent still armed here has nothing this TUI
// knows of that could have invalidated it, so serving it is correct.
func (m model) reviewIntentRescuable(jobID int64, fetchSeq uint64) bool {
	if fetchSeq != m.reviewFetchSeq {
		return false
	}
	return (m.reviewFixPanelPending && m.fixPromptJobID == jobID) ||
		m.pendingReviewOpenJobID == jobID
}

// handleReviewMsg processes review fetch results. Acceptance is gated on
// ONE rule shared by every dispatcher: the response must still carry the
// newest fetch epoch (msg.fetchSeq == m.reviewFetchSeq). See
// m.reviewFetchSeq's doc comment (tui.go).
func (m model) handleReviewMsg(
	msg reviewMsg,
) (tea.Model, tea.Cmd) {
	m.releaseReconcileFetch(msg.fetchSeq)
	// Three staleness reasons, three treatments. msg.jobID !=
	// m.selectedJobID: the selection moved on since dispatch --
	// unconditional abandonment, content and both pending intents are
	// dropped below. msg.gen != m.detailFollowGen with the job unchanged:
	// content is stale and rejected, but a gen bump alone does not mean
	// the REQUEST was abandoned (an incidental same-job bootstrap bumps
	// too) -- the one exception, reviewIntentRescuable, is documented on
	// that function. msg.attempt != m.jobAttemptGen[msg.jobID]: a
	// confirmed rerun superseded the ATTEMPT this response belongs to;
	// its content must never resurrect over the rerun's own result, so
	// this is unconditional, unrescuable, and needs no disarm here
	// (handleRerunResultMsg disarmed inline, keyed on its own msg.jobID).
	if msg.jobID != m.selectedJobID {
		// The jobID mismatch alone proves abandonment:
		// dispatchReviewFetch always sets the armed field and
		// m.selectedJobID together, so a fresh re-arm of the SAME job can
		// never leave the field pointing at a job the selection has since
		// diverged from. The msg.fetchSeq >= <field>Seq conjunct is
		// defense-in-depth for that unstated invariant across the
		// dispatch call sites, not load-bearing.
		if m.reviewFixPanelPending && m.fixPromptJobID == msg.jobID &&
			msg.fetchSeq >= m.fixPromptSeq {
			m.reviewFixPanelPending = false
			m.fixPromptJobID = 0
			m.fixPromptOrigin = 0
			m.fixPromptFollowRetried = false
			m.fixPromptSeq = 0
		}
		if m.pendingReviewOpenJobID == msg.jobID &&
			msg.fetchSeq >= m.pendingReviewOpenSeq {
			m.pendingReviewOpenJobID = 0
			m.pendingReviewOpenOrigin = 0
			m.pendingReviewOpenSeq = 0
		}
		return m, nil
	}
	if msg.attempt != m.jobAttemptGen[msg.jobID] {
		// This response belongs to an ATTEMPT of this job that a
		// confirmed rerun has since superseded. Dropped outright and
		// unconditionally -- no rescue (that would serve content the user
		// explicitly replaced) and nothing to disarm
		// (handleRerunResultMsg disarmed both intents when it bumped, and
		// any intent armed since carries the post-bump stamp). See
		// m.jobAttemptGen's contract, clause 4 (tui.go), for why this
		// sits after the jobID gate and before the gen gate.
		return m, nil
	}
	if msg.gen != m.detailFollowGen {
		// Ordinarily just stale, dropped (see the function-level comment
		// above for why gen-mismatch-alone isn't abandonment). The one
		// exception -- reviewIntentRescuable -- treats this response as
		// CURRENT instead, falling through to the normal acceptance path
		// below exactly as if gen had matched; see its doc comment for the
		// full derivation.
		if !m.reviewIntentRescuable(msg.jobID, msg.fetchSeq) {
			return m, nil
		}
	}
	if msg.fetchSeq != m.reviewFetchSeq {
		// SUPERSEDED: a NEWER dispatch (from any call site, ordinary or
		// follow) has since gone out or already landed, so this older
		// response must not overwrite the fresher content -- the general
		// form of "a just-submitted comment disappears when an older
		// in-flight fetch lands after the comment refresh".
		//
		// reviewFixPanelPending is deliberately NOT cleared here (unlike
		// the jobID/gen rejection above): the job and attempt are still
		// current, only this particular request lost the race, and the
		// newer response for the same job will consume the pending flag
		// on the acceptance path.
		//
		// pendingReviewOpenJobID is left untouched for the same reason --
		// see its doc comment (tui.go) -- EXCEPT when this fallback below
		// actually performs the switch, in which case the intent it was
		// tracking has now been served.
		//
		// The fallback fires ONLY while the matching intent is still ARMED
		// (with the per-arm identity guard, so a stale message cannot serve
		// a fresher re-arm). An unconditional content-present switch would
		// REOPEN a review the user already left: a newer response consumes
		// the intent and opens the view, the user presses esc back to the
		// queue, and then this older response arrives -- content still
		// loaded, jobID still matching -- and would yank them back in with
		// no request outstanding. The armed intent is exactly the record of
		// "an open is still owed"; once it is consumed, a late response owes
		// nothing. The switch uses the INTENT's stored origin (the request
		// that armed it), not this stale message's own dispatch origin.
		//
		// Content-present gating still matters within that: acceptReview's
		// own consume normally serves the intent when a newer response
		// lands, so what remains for this fallback is the race where the
		// newer landing happened before this intent was armed, or content
		// arrived while the intent's own dispatch was superseded -- either
		// way it must never switch to an empty or foreign review.
		if !msg.follow && m.currentReview != nil && m.currentReview.JobID == msg.jobID &&
			m.pendingReviewOpenJobID == msg.jobID &&
			msg.fetchSeq >= m.pendingReviewOpenSeq {
			origin := m.pendingReviewOpenOrigin
			m.pendingReviewOpenJobID = 0
			m.pendingReviewOpenOrigin = 0
			m.pendingReviewOpenSeq = 0
			m.openReviewViewFrom(origin)
		}
		return m, nil
	}
	m.acceptReview(msg)
	if msg.follow {
		// A follow response updates the pane's content only -- no view
		// switch, no focus steal.
		return m, nil
	}
	m.openReviewView(msg)
	return m, nil
}

// handleReviewFollowErrMsg processes a failed split-view follow fetch
// (fetchReviewFollow). Four kinds of stale response are dropped silently
// rather than recorded in m.splitDetailErr: a stale jobID (the selection
// has since moved on), a stale attempt (a rerun of this job confirmed
// since the fetch was dispatched -- never rescuable), a stale gen
// (another selection change since dispatch) UNLESS reviewIntentRescuable
// says nothing else is left to serve a still-armed intent, and a
// superseded fetchSeq (a newer review fetch, from any dispatcher, has
// since gone out or already landed). Anything else is recorded in
// m.splitDetailErr so renderDetailPane can surface it instead of leaving
// the "Loading review..." placeholder stuck forever.
func (m model) handleReviewFollowErrMsg(msg reviewFollowErrMsg) (tea.Model, tea.Cmd) {
	m.releaseReconcileFetch(msg.fetchSeq)
	// jobID and gen are checked together, mirroring handleReviewMsg's
	// success-path rejection exactly: a stale error from a fetch
	// dispatched for a job the user has since navigated away from (jobID
	// mismatch) or invalidated by a later selection change (gen mismatch)
	// must not clobber splitDetailErr for whatever is currently selected.
	if msg.jobID != m.selectedJobID {
		// Mirrors handleReviewMsg's matching branch: clearing either
		// pending intent requires the JOB to have genuinely moved on.
		// The msg.fetchSeq >= <field>Seq conjunct is defense-in-depth,
		// not load-bearing, same reasoning as there.
		if m.reviewFixPanelPending && m.fixPromptJobID == msg.jobID &&
			msg.fetchSeq >= m.fixPromptSeq {
			m.reviewFixPanelPending = false
			m.fixPromptJobID = 0
			m.fixPromptOrigin = 0
			m.fixPromptFollowRetried = false
			m.fixPromptSeq = 0
		}
		if m.pendingReviewOpenJobID == msg.jobID &&
			msg.fetchSeq >= m.pendingReviewOpenSeq {
			m.pendingReviewOpenJobID = 0
			m.pendingReviewOpenOrigin = 0
			m.pendingReviewOpenSeq = 0
		}
		return m, nil
	}
	if msg.attempt != m.jobAttemptGen[msg.jobID] {
		// Same attempt gate as handleReviewMsg: a FAILURE
		// belonging to a superseded attempt must not reach splitDetailErr
		// or resolve an intent, for the same reasons its success
		// counterpart must not reach currentReview.
		return m, nil
	}
	if msg.gen != m.detailFollowGen {
		// Same rescue reasoning as handleReviewMsg: a gen-mismatch-only
		// rejection (job unchanged) is ordinarily just dropped, UNLESS
		// reviewIntentRescuable says nothing else is left to serve a
		// still-armed intent -- in which case this FAILURE is treated as
		// current instead of silently discarded,
		// falling through to record it in splitDetailErr and resolve
		// whichever intent is armed (retry, for the fix panel; clear +
		// flash, for the plain open-review intent) exactly as the
		// un-rescued "current failure" path below already does.
		if !m.reviewIntentRescuable(msg.jobID, msg.fetchSeq) {
			return m, nil
		}
	}
	// Same ordering rule as the success path: a fetchSeq mismatch means a
	// NEWER dispatch has since gone out or already landed, and this is an
	// older, now-superseded error arriving late. Drop it without touching
	// splitDetailErr, which the newer request/response already owns.
	//
	// reviewFixPanelPending is deliberately left untouched here too, for
	// the same reason handleReviewMsg's mirror-image branch leaves it
	// alone on a superseded SUCCESS: the job/attempt is still current,
	// only this particular response lost the race, and the newer
	// dispatch's own eventual response -- success or (per the retry
	// below) failure -- is what resolves the pending flag.
	if msg.fetchSeq != m.reviewFetchSeq {
		return m, nil
	}
	m.splitDetailErr = msg.err
	// Deliberately does NOT clear paneReviewSeenNonTerminal: a fresh
	// attempt was genuinely observed and this fetch failed to pick it up,
	// so the next reconcile pass must still retry (see that field's doc
	// comment).

	// A pending queue-'F' request armed against this same
	// job normally gets served by acceptReview when SOME review response
	// for the job lands -- but reviewFixPanelPending is deliberately left
	// armed (just above, and in handleReviewMsg's fetchSeq-superseded
	// branch) whenever a NEWER dispatch has superseded an older one, on
	// the assumption that the newer dispatch will eventually resolve and
	// consume it. A follow fetch FAILING is that newer dispatch resolving
	// -- as an outcome acceptReview's success-only consume never sees. Left
	// unhandled, an already-loaded (merely stale) review for this job makes
	// splitReconcileDetail's Done-branch idempotency check skip re-fetching
	// entirely, so nothing else would ever try again: the panel stays armed
	// with nothing in flight to serve it, and the user's deliberate 'F' is
	// silently swallowed.
	//
	// The user's 'F' was a deliberate action and the review is very likely
	// still fetchable (this is far more often a transient hiccup than a
	// permanent failure), so retry once -- automatically, since the user
	// already asked for this by pressing 'F' and has no simpler way to ask
	// again while the panel isn't even open yet -- before giving up.
	// Bounded to exactly one retry so a persistently failing fetch can't
	// loop forever; the second failure clears the pending flag with
	// user-visible feedback instead of leaving it silently stranded.
	if m.reviewFixPanelPending && m.fixPromptJobID == msg.jobID {
		if !m.fixPromptFollowRetried {
			m.fixPromptFollowRetried = true
			cmd := m.dispatchReviewFollow(msg.jobID)
			// Re-stamp the identity at the RETRY's own dispatch,
			// mirroring the arm site (handlers_review.go). Otherwise
			// fixPromptSeq keeps pointing at the original (now-dead)
			// dispatch, and that dispatch's own long-in-flight, gen-stale
			// response would pass the `msg.fetchSeq >= fixPromptSeq`
			// guard and wrongly clear the retry still in flight.
			m.fixPromptSeq = m.reviewFetchSeq
			return m, cmd
		}
		m.reviewFixPanelPending = false
		m.fixPromptJobID = 0
		m.fixPromptOrigin = 0
		m.fixPromptFollowRetried = false
		m.fixPromptSeq = 0
		m.setWarningFlash(
			fmt.Sprintf("Could not load review to fix job #%d: %v", msg.jobID, msg.err),
			3*time.Second, m.currentView,
		)
		// The 'F' fetch is an ordinary dispatch, so dispatchReviewFetch
		// armed the plain open intent alongside this panel. The panel's
		// terminal failure resolves BOTH: left armed, the block below
		// would start its own retry chain against a review that just
		// failed twice, and a late success would pop the review open
		// after the user was already told the fix couldn't load.
		if m.pendingReviewOpenJobID == msg.jobID {
			m.pendingReviewOpenJobID = 0
			m.pendingReviewOpenOrigin = 0
			m.pendingReviewOpenSeq = 0
		}
	}
	// The plain "open the review" intent needs the same handling as the
	// fix panel above: a follow failure resolves the freshest dispatch for
	// this job, and left unhandled nothing would EVER resolve the intent --
	// reconcile cannot re-arm it on its own, because splitReconcileDetail
	// dispatches through dispatchReviewFollow, the one dispatcher that
	// deliberately never touches pendingReviewOpen* (a follow is not a
	// view-opening request). A later successful follow would populate the
	// pane but never switch the view, leaving the user's explicit
	// navigation permanently swallowed.
	//
	// Clearing OUTRIGHT here is wrong, though: the intent's own ORIGINATING
	// ordinary dispatch may still be in flight, and even when it later
	// SUCCEEDS its response arrives fetchSeq-superseded -- and with no
	// content loaded (precisely because this follow failed), the
	// superseded branch cannot serve the open. The user's deliberate
	// navigation would be swallowed despite its own request succeeding. So
	// retry once, exactly like the fix panel above: a fresh follow whose
	// success serves the intent via acceptReview's consume (which runs
	// regardless of msg.follow). Bounded by pendingReviewOpenRetried
	// (reset at each arm) so a persistently failing fetch can't loop; the
	// second failure clears with user-visible feedback.
	//
	// jobID-only matching (no fetchSeq guard needed) for the same reason as
	// acceptReview's consume: this code only runs for a message that is
	// current (passed both the jobID/gen and fetchSeq staleness checks
	// above), so pendingReviewOpenSeq is provably <= msg.fetchSeq here --
	// see pendingReviewOpenSeq's doc comment, tui.go.
	if m.pendingReviewOpenJobID == msg.jobID {
		if !m.pendingReviewOpenRetried {
			m.pendingReviewOpenRetried = true
			cmd := m.dispatchReviewFollow(msg.jobID)
			// Re-stamp the identity at the RETRY's own dispatch, mirroring
			// the fix panel's re-stamp above: otherwise pendingReviewOpenSeq
			// keeps pointing at the original (now-dead) dispatch, and that
			// dispatch's own long-in-flight, gen-stale response would pass
			// the `msg.fetchSeq >= pendingReviewOpenSeq` guard and wrongly
			// clear the retry still in flight.
			m.pendingReviewOpenSeq = m.reviewFetchSeq
			return m, cmd
		}
		// Terminal: clear AND surface a warning flash targeted at
		// pendingReviewOpenOrigin (the view the request was issued FROM,
		// e.g. the tasks view for its 'P' key) -- not m.currentView,
		// because the user may be elsewhere by now, and not splitDetailErr,
		// because a tasks-origin request never renders the split pane. The
		// origin view is the one feedback channel guaranteed visible if the
		// user is still on (or returns to) the view where they made the
		// request.
		origin := m.pendingReviewOpenOrigin
		m.pendingReviewOpenJobID = 0
		m.pendingReviewOpenOrigin = 0
		m.pendingReviewOpenSeq = 0
		m.setWarningFlash(
			fmt.Sprintf("Could not open review for job #%d: %v", msg.jobID, msg.err),
			3*time.Second, origin,
		)
	}
	return m, nil
}

// handleReviewErrMsg processes a failed ORDINARY (non-follow) review fetch
// (fetchReview's reviewErrMsg). Unlike a gen-stale rejection (the request
// may still be served by a later follow) or a fetchSeq-superseded response
// (a newer dispatch is already in flight to serve it), a fetch that itself
// FAILS is a first-party signal that THIS attempt cannot be served --
// nothing is left in flight for it. Typed with jobID/gen/seq so the
// staleness gates and the pending intents can be resolved; a generic,
// jobID-less error could not be tied back to either intent, leaving both
// silently armed indefinitely.
//
// Same staleness shape as handleReviewMsg/handleReviewFollowErrMsg
// (jobID rejection, attempt gate, gen rejection with rescue, then
// fetchSeq-superseded): a gen-mismatch-only rejection (job unchanged)
// ordinarily clears nothing -- the failure belongs to an attempt the
// model has moved past for CONTENT purposes (so m.err/reconnect-tracking
// below must not see it), but the underlying request may still be served
// by whatever caused the bump -- UNLESS reviewIntentRescuable says
// nothing else is left to serve a still-armed intent.
//
// Deliberate asymmetry with handleReviewFollowErrMsg: that handler retries
// once before giving up, this one does not -- there is no
// separately-typed content to protect by retrying (an ordinary fetch's
// only two possible outcomes for the caller are "opened" or "didn't"), and
// a pending fix panel armed alongside pendingReviewOpenJobID (as
// handleFixKey's queue-'F' path always arms both together) still gets
// visible feedback for free, riding on the flash below.
func (m model) handleReviewErrMsg(msg reviewErrMsg) (tea.Model, tea.Cmd) {
	if msg.jobID != m.selectedJobID {
		// Same defense-in-depth conjunct as handleReviewMsg's matching
		// branch.
		if m.reviewFixPanelPending && m.fixPromptJobID == msg.jobID &&
			msg.fetchSeq >= m.fixPromptSeq {
			m.closeFixPanel()
		}
		if m.pendingReviewOpenJobID == msg.jobID &&
			msg.fetchSeq >= m.pendingReviewOpenSeq {
			m.pendingReviewOpenJobID = 0
			m.pendingReviewOpenOrigin = 0
			m.pendingReviewOpenSeq = 0
		}
		return m, nil
	}
	if msg.attempt != m.jobAttemptGen[msg.jobID] {
		// Same attempt gate as the other two handlers.
		return m, nil
	}
	if msg.gen != m.detailFollowGen {
		// Same rescue reasoning as handleReviewMsg/handleReviewFollowErrMsg.
		if !m.reviewIntentRescuable(msg.jobID, msg.fetchSeq) {
			return m, nil
		}
	}
	if msg.fetchSeq != m.reviewFetchSeq {
		// SUPERSEDED: a newer dispatch (from any call site, ordinary or
		// follow) has since gone out or already landed for this job -- its
		// own eventual response, success or failure, is what resolves
		// either intent. Leave both armed.
		return m, nil
	}

	// Current, un-superseded, genuine failure. Mirrors the generic
	// errMsg handling (handleErrMsg) so the global error field and
	// connection-error/reconnect detection still apply even though this
	// failure is typed.
	m.err = msg.err
	reconnectCmd := m.handleConnectionError(msg.err)

	// The pending fix panel ('F') is resolved the same way a selection
	// change resolves it (closeFixPanel) -- this job's own dispatch is
	// what would have opened it, and it just failed outright.
	if m.reviewFixPanelPending && m.fixPromptJobID == msg.jobID {
		m.closeFixPanel()
	}
	// No silent clear for the plain "open the review" intent, matching
	// handleReviewFollowErrMsg's own resolve-on-failure treatment (see
	// its doc comment): clear AND flash on pendingReviewOpenOrigin, the
	// one feedback channel guaranteed visible regardless of where the
	// user has navigated to since making the request.
	if m.pendingReviewOpenJobID == msg.jobID {
		origin := m.pendingReviewOpenOrigin
		m.pendingReviewOpenJobID = 0
		m.pendingReviewOpenOrigin = 0
		m.pendingReviewOpenSeq = 0
		m.setWarningFlash(
			fmt.Sprintf("Could not open review for job #%d: %v", msg.jobID, msg.err),
			3*time.Second, origin,
		)
	}
	return m, reconnectCmd
}

// handlePromptMsg processes prompt fetch results.
//
// STALENESS IDENTITY, and why it is deliberately narrower than
// handleReviewMsg's:
//
// This handler writes m.currentReview -- the same field the split detail
// pane renders and the same field reconcile's idempotency check reads --
// from a /api/review payload, so it is a review-content writer like
// handleReviewMsg, and it is gated on four things:
//
//  0. promptSeq + dispatchedFrom: the prompt path's own request identity
//     (promptFetchSeq, bumped at dispatch and by followSelectionChange's
//     abandonment) and origin-view guard, so a slow response can neither
//     be re-accepted after navigating away and back to the same job nor
//     yank the user out of a transient view opened while it was in
//     flight. See the inline comments on the gates below.
//  1. jobID: the selection moved on since dispatch.
//  2. attempt (the per-job counter): a confirmed rerun has
//     superseded the attempt this response belongs to. This gate is
//     exactly as necessary here as on the review path -- 'p' on a done job
//     in stacked mode, a rerun of that job confirming while the fetch is in
//     flight, and the response lands with the jobID gate still passing (a
//     rerun reuses the job ID and doesn't move the selection): it
//     overwrites the nil handleRerunResultMsg just wrote with the PREVIOUS
//     attempt's review, and splitReconcileDetail/handleDetailFollowTick
//     then see currentReview.JobID already matching and skip fetching the
//     rerun's real result. Dropping is also COMPLETE here, unlike on the
//     review path where an intent must be resolved: after a rerun the job
//     is queued/running, and handlePromptKey renders a running job's prompt
//     synchronously from job.Prompt with no fetch at all, so pressing 'p'
//     again immediately shows the fresh attempt.
//  3. NOT the shared fetch epoch (reviewFetchSeq), and NOT
//     detailFollowGen. Both were considered and rejected on specifics, not
//     on "structural" grounds:
//     - The epoch cannot be joined in either direction. Stamping WITHOUT
//     bumping means any review fetch dispatched after this one --
//     splitReconcileDetail fires one on every jobs refresh -- silently
//     discards the user's explicit 'p', with no pendingPromptOpen field
//     to re-serve it (the review path only survives that because
//     pendingReviewOpenJobID exists to be re-served). Stamping WITH
//     bumping has the same swallow problem plus a new one: this handler
//     populates currentReview but deliberately CLEARS its siblings
//     (currentResponses/currentBranch, see below), so superseding an
//     in-flight follow fetch would leave the pane holding a review with
//     no comments, which reconcile's JobID-only idempotency check will
//     not repair. Joining the epoch therefore requires giving the prompt
//     view its own intent field first -- a real design change, not a
//     stamp. Nothing is lost meanwhile: the only fetches that race this
//     one are for the SAME job (the jobID gate rejects the rest) and
//     carry the same /api/review payload, so their ordering is benign.
//     - detailFollowGen would add nothing the jobID gate doesn't already
//     cover (it bumps on a genuine selection change, which moves jobID
//     too) and would wrongly discard this response after a mere layout
//     toggle, which bumps gen with the selection unchanged.
func (m model) handlePromptMsg(
	msg promptMsg,
) (tea.Model, tea.Cmd) {
	if msg.jobID != m.selectedJobID {
		return m, nil
	}
	if msg.attempt != m.jobAttemptGen[msg.jobID] {
		// A confirmed rerun superseded this attempt -- gate 2 in this
		// function's doc comment, and jobAttemptGen's contract (tui.go).
		return m, nil
	}
	if msg.promptSeq != m.promptFetchSeq {
		// The prompt path's own request identity: superseded by a newer
		// prompt dispatch, or abandoned by a selection change
		// (followSelectionChange bumps the counter). The jobID gate alone
		// cannot catch the abandoned case when the user navigates away and
		// BACK to the same job before this lands -- accepting it would pop
		// the prompt view open with no fresh keypress.
		return m, nil
	}
	if m.currentView != msg.dispatchedFrom && m.currentView != viewKindPrompt {
		// The user opened a different view (filter, tasks, help, ...)
		// while this fetch was in flight -- same dispatch-origin guard as
		// openReviewView. Dropped entirely rather than loaded without the
		// view switch: this handler clears currentReview's siblings below,
		// so a background content write would strand the split pane with a
		// review and no comments. The prompt is cheap to refetch ('p'
		// again); nothing needs re-serving.
		return m, nil
	}
	// currentResponses/currentBranch are siblings of currentReview,
	// normally kept in sync with it by whichever fetch populated them
	// (the split pane's own follow fetch, most often). This handler only
	// ever assigns currentReview -- if the review it's about to load is
	// for a DIFFERENT job than whatever is currently loaded, the siblings
	// belong to that other job and must be cleared here, or they persist
	// stale alongside this job's review. Concretely: split, job X
	// selected with X's review+comments loaded; 'p' opens the prompt;
	// stepPromptNav (<-/->) walks to job Y's prompt, which calls
	// fetchReviewForPrompt(Y) -- landing here with msg.review.JobID == Y
	// while currentResponses is still X's. Esc then goes through
	// preserveOrClearReviewOnQueueReturn, whose guard only checks
	// currentReview.JobID (now Y) against selectedJobID (now Y) and
	// retains it -- rendering Y's review with X's comments underneath.
	// Nothing else corrects this: reconcile's idempotency checks only
	// examine currentReview itself, never its siblings (see
	// preserveOrClearReviewOnQueueReturn's doc comment).
	if m.currentReview == nil || m.currentReview.JobID != msg.jobID {
		m.currentResponses = nil
		m.currentBranch = ""
	}
	m.consecutiveErrors = 0
	m.currentReview = msg.review
	m.currentView = viewKindPrompt
	m.promptScroll = 0
	return m, nil
}

// handleLogOutputMsg processes log output from the daemon.
func (m model) handleLogOutputMsg(
	msg logOutputMsg,
) (tea.Model, tea.Cmd) {
	// Drop stale responses from previous log sessions.
	if msg.seq != m.logFetchSeq {
		return m, nil
	}
	m.logLoading = false
	m.consecutiveErrors = 0
	// If the user navigated away while a fetch was in-flight, drop it.
	if m.currentView != viewLog {
		return m, nil
	}
	if msg.err != nil {
		if errors.Is(msg.err, errNoLog) {
			// Auto-design-router rows (running classify, terminal
			// skipped, failed/canceled classify) never produce a
			// streamed agent log because the classifier is a
			// one-shot SchemaAgent.Decide call. Stay in the log
			// view so renderLogView's classifyReasoningLines
			// header can show the verdict, skip reason, and any
			// classifier error — bouncing back to the queue
			// would hide that information behind a flash.
			job := m.logViewLookupJob()
			if job != nil && len(classifyReasoningLines(job, m.width)) > 0 {
				m.logLines = []logLine{}
				m.logStreaming = false
				return m, nil
			}
			flash := "No log available for this job"
			if job != nil &&
				job.Status == storage.JobStatusFailed &&
				job.Error != "" {
				flash = fmt.Sprintf(
					"Job #%d failed: %s",
					m.logJobID, job.Error,
				)
			}
			m.setFlash(flash, 5*time.Second, m.logFromView)
			m.currentView = m.logFromView
			m.logStreaming = false
			return m, nil
		}
		m.err = msg.err
		return m, nil
	}
	if m.currentView == viewLog {
		// Persist formatter state for incremental polls
		if msg.fmtr != nil {
			m.logFmtr = msg.fmtr
		}
		m.logAgent = msg.agent
		m.logSource = msg.source

		if msg.append {
			if len(msg.lines) > 0 {
				m.logLines = append(
					m.logLines, msg.lines...,
				)
			}
		} else {
			m.logLines = msg.lines
			if m.logLines == nil && !msg.hasMore {
				m.logLines = []logLine{}
			}
		}
		m.logOffset = msg.newOffset
		m.logStreaming = msg.hasMore
		if m.logFollow && len(m.logLines) > 0 {
			visibleLines := m.logVisibleLines()
			maxScroll := max(len(m.logLines)-visibleLines, 0)
			m.logScroll = maxScroll
		}
		if m.logStreaming {
			return m, tea.Tick(
				500*time.Millisecond,
				func(t time.Time) tea.Msg {
					return logTickMsg{}
				},
			)
		}
	}
	return m, nil
}

// paneLogMaxLines caps the split detail pane's buffered live log lines,
// trimming from the front once exceeded.
const paneLogMaxLines = 500

// handlePaneLogOutputMsg processes live log output for the split detail
// pane's running-job tail. Mirrors handleLogOutputMsg but drives
// the pane's own paneLog* fields, independent of the full-screen log view.
func (m model) handlePaneLogOutputMsg(msg paneLogOutputMsg) (tea.Model, tea.Cmd) {
	// Drop stale responses from a previous tail session (selection moved
	// to a different job, or startPaneLog re-armed the same job). seq
	// alone is scheduleDetailFollow's chosen invalidation point (bumped on
	// every keyboard/mouse selection change), but other selection-mutation
	// paths -- e.g. the control socket's select-job command, handleJobsMsg
	// reassigning the selection when the tailed job vanishes from a
	// refresh, closedResultMsg's restoreSelection rollback, and various
	// transient-view-return paths -- mutate m.selectedJobID directly
	// without going through scheduleDetailFollow at all, so they never
	// bump paneLogSeq either. msg.jobID == m.paneLogJobID alone isn't
	// enough to catch those: paneLogJobID is only ever updated when a NEW
	// tail actually starts (startPaneLog), so it's just as stale as
	// selectedJobID would be if we didn't also check it here -- a late
	// response satisfies both the seq and paneLogJobID checks (neither
	// was invalidated) while m.selectedJobID has already moved on.
	// Requiring msg.jobID == m.selectedJobID too closes every such path at
	// the message level, regardless of how the selection changed
	// underneath it: an in-flight fetch for a job that's no longer both
	// the tail's own bookkeeping AND the current selection can never land.
	//
	// The seq/paneLogJobID check and the selectedJobID check are
	// deliberately split into two separate ifs (not one combined
	// condition) because they need DIFFERENT consequences on rejection:
	// a response that's ALSO stale by seq/paneLogJobID is rejected
	// outright, untouched -- it doesn't correspond to a live tail at
	// all, so there's nothing to invalidate (contrast
	// handlePaneLogTickMsg's second guard: a stale-seq tick must not
	// kill a LIVE tail). But a
	// response that DOES match the tail's own bookkeeping (seq and
	// paneLogJobID both current) yet disagrees with m.selectedJobID means
	// the selection moved to a DIFFERENT job while this tail was still
	// genuinely live -- returning here without touching paneLogStreaming
	// would leave it claiming an active tail with no poll chain behind
	// it (nothing re-arms paneLogTickMsg once this response is dropped).
	// splitReconcileDetail's Running-branch restart guard and
	// startPaneLog's already-tailing no-op guard would then both believe
	// the tail is still live if the selection later returns to this SAME
	// job via a path that also bypasses scheduleDetailFollow, so the
	// live log would never get restarted -- frozen on stale output
	// permanently.
	if msg.seq != m.paneLogSeq || msg.jobID != m.paneLogJobID {
		return m, nil
	}
	if msg.jobID != m.selectedJobID {
		m.paneLogSeq++
		m.paneLogStreaming = false
		return m, nil
	}
	if msg.err != nil {
		if errors.Is(msg.err, errNoLog) {
			// Job just started; the daemon hasn't written any log
			// output yet. Keep polling rather than giving up.
			seq := msg.seq
			return m, tea.Tick(2*time.Second, func(time.Time) tea.Msg {
				return paneLogTickMsg{seq: seq}
			})
		}
		m.paneLogStreaming = false
		m.splitDetailErr = msg.err
		return m, nil
	}
	// A successful fetch that passed the seq+jobID gates proves the pane is
	// healthy, so any splitDetailErr still set is stale -- it belongs to an
	// earlier tail or an earlier job. Clearing here (not just on the
	// hasMore==false completion path below) keeps renderDetailPane's
	// unconditional running-branch error from covering a live tail.
	m.splitDetailErr = nil
	if msg.fmtr != nil {
		m.paneLogFmtr = msg.fmtr
	}
	m.paneLogAgent = msg.agent
	m.paneLogSource = msg.source
	if msg.append {
		if len(msg.lines) > 0 {
			m.paneLogLines = append(m.paneLogLines, msg.lines...)
		}
	} else {
		// Non-incremental fetch: either the initial full fetch or a
		// server-side offset reset (log truncated/rotated). Replace
		// rather than append so stale pre-reset lines don't linger
		// mixed in with the replacement log.
		m.paneLogLines = msg.lines
	}
	if over := len(m.paneLogLines) - paneLogMaxLines; over > 0 {
		m.paneLogLines = m.paneLogLines[over:]
	}
	m.paneLogOffset = msg.newOffset
	m.paneLogStreaming = msg.hasMore
	if msg.hasMore {
		seq := msg.seq
		return m, tea.Tick(2*time.Second, func(time.Time) tea.Msg {
			return paneLogTickMsg{seq: seq}
		})
	}
	// Job finished streaming -- swap the pane over to the review. The
	// reviewMsg staleness gate (msg.jobID != m.selectedJobID) protects
	// against the cursor having moved on in the meantime. splitDetailErr
	// was already cleared above (this fetch succeeded), so no stale error
	// can show through for this job while the fresh fetch is in flight.
	// A one-shot dispatch triggered by the log stream ending, not
	// splitReconcileDetail's every-refresh dispatch, so it needs no
	// duplicate-dispatch suppression (it never touches
	// reconcileFetchJobID) -- but it is ordered like every other review
	// fetch: dispatchReviewFollow bumps the shared epoch and stamps it.
	cmd := m.dispatchReviewFollow(m.paneLogJobID)
	return m, cmd
}

// handlePaneLogTickMsg processes a poll tick for the split detail pane's
// live log tail. Stale seq, a layout change, or the selection
// having moved off the tailed job all silently stop the tail.
//
// Unlike handlePaneLogOutputMsg's seq+jobID+selectedJobID gate, this
// doesn't need a separate m.selectedJobID check added: `m.selectedJob()`
// below resolves the CURRENT selection fresh (not a cached field the way
// paneLogJobID is), so `job.ID != m.paneLogJobID` already means "the
// tailed job and the current selection disagree" -- it can't go stale the
// way msg.jobID == m.paneLogJobID alone could.
func (m model) handlePaneLogTickMsg(msg paneLogTickMsg) (tea.Model, tea.Cmd) {
	if msg.seq != m.paneLogSeq || m.layout != layoutSplit || !m.paneLogStreaming {
		return m, nil
	}
	job, ok := m.selectedJob()
	if !ok || job.ID != m.paneLogJobID || job.Status != storage.JobStatusRunning {
		// The selection moved or the job entered a state with no decoder
		// left to finalize. Stop claiming an active tail here --
		// leaving paneLogStreaming true with no poll chain behind it would
		// make both splitReconcileDetail's running-branch restart guard and
		// startPaneLog's already-tailing no-op guard believe the tail is
		// still alive, so a rerun that reuses this job ID and returns to
		// running would never get its tail restarted. Bumping paneLogSeq
		// also rejects any fetch response still in flight for the tail we
		// just abandoned. paneLogLines/paneLogOffset are left alone: the
		// last-known output stays visible until a restart replaces it.
		m.paneLogSeq++
		m.paneLogStreaming = false
		return m, nil
	}
	return m, m.fetchPaneLog(m.paneLogJobID)
}

// handleCommitMsgMsg processes commit message fetch results.
func (m model) handleCommitMsgMsg(
	msg commitMsgMsg,
) (tea.Model, tea.Cmd) {
	if msg.jobID != m.commitMsgJobID {
		return m, nil
	}
	if msg.err != nil {
		m.setFlash(msg.err.Error(), 2*time.Second, m.currentView)
		return m, nil
	}
	m.commitMsgContent = msg.content
	m.commitMsgScroll = 0
	m.currentView = viewCommitMsg
	return m, nil
}

// handleClosedResultMsg processes the result of a closed toggle API call.
func (m model) handleClosedResultMsg(msg closedResultMsg) (tea.Model, tea.Cmd) {
	// A failed close is rolled back by restoring the selection the
	// optimistic path moved away from -- a selection change like any
	// other, so it takes the shared detail-follow transition below.
	var followCmd tea.Cmd
	prevSelected := m.selectedJobID
	isCurrentRequest := false
	if msg.jobID > 0 {
		if pending, ok := m.pendingClosed[msg.jobID]; ok && pending.seq == msg.seq {
			isCurrentRequest = true
		}
	} else if msg.reviewView && msg.reviewID > 0 {
		if pending, ok := m.pendingReviewClosed[msg.reviewID]; ok && pending.seq == msg.seq {
			isCurrentRequest = true
		}
	}

	if msg.err != nil {
		if isCurrentRequest {
			if msg.reviewView {
				if m.currentReview != nil && m.currentReview.ID == msg.reviewID {
					m.currentReview.Closed = msg.oldState
				}
			}
			if msg.jobID > 0 {
				m.setJobClosed(msg.jobID, msg.oldState)
				delete(m.pendingClosed, msg.jobID)
				if msg.restoreSelection {
					m.selectJobByID(msg.jobID)
					m, followCmd = m.followSelectionChange(prevSelected)
				}
				// Reverse the optimistic stats delta
				m.applyStatsDelta(msg.oldState)
				// Mirror handleCloseKey's split-list optimistic flip: this
				// job-keyed path (msg.reviewView is false here) is the one
				// closeReviewInBackground uses for a close toggled from the
				// split list, which also flips currentReview.Closed
				// optimistically when it's loaded for this job. Roll that
				// back here too -- gated on the SAME isCurrentRequest/seq
				// check as the job rollback above, so job and review can't
				// roll back independently (one seq, one pass, both or
				// neither).
				if m.currentReview != nil && m.currentReview.JobID == msg.jobID {
					m.currentReview.Closed = msg.oldState
				}
			} else if msg.reviewID > 0 {
				delete(m.pendingReviewClosed, msg.reviewID)
			}
			flashView := viewQueue
			if msg.reviewView {
				flashView = viewReview
			}
			m.setWarningFlash(msg.err.Error(), 3*time.Second, flashView)
			m.err = msg.err
		}
	} else {
		if isCurrentRequest && msg.jobID == 0 && msg.reviewID > 0 {
			delete(m.pendingReviewClosed, msg.reviewID)
		}
	}
	return m, followCmd
}

// handleClosedToggleMsg processes closed state toggle messages.
func (m model) handleClosedToggleMsg(
	msg closedMsg,
) (tea.Model, tea.Cmd) {
	if m.currentReview != nil {
		m.currentReview.Closed = bool(msg)
	}
	return m, nil
}

// handleCancelResultMsg processes job cancellation results.
func (m model) handleCancelResultMsg(
	msg cancelResultMsg,
) (tea.Model, tea.Cmd) {
	var followCmd tea.Cmd
	if msg.err != nil {
		prevSelected := m.selectedJobID
		m.setJobStatus(msg.jobID, msg.oldState)
		m.setJobFinishedAt(msg.jobID, msg.oldFinishedAt)
		// Restoring the selection the optimistic cancel moved away from is
		// a selection change like any other -- same shared transition.
		if msg.restoreSelection {
			m.selectJobByID(msg.jobID)
			m, followCmd = m.followSelectionChange(prevSelected)
		}
		m.err = msg.err
	}
	return m, followCmd
}

// handlePauseResultMsg reconciles the queue-pause badge with the daemon. On
// success it refetches status so worker counts settle; on failure it rolls
// back the optimistic toggle and surfaces the error.
func (m model) handlePauseResultMsg(msg pauseResultMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.status.QueuePaused = !msg.paused
		verb := "pause"
		if !msg.paused {
			verb = "resume"
		}
		m.setWarningFlash(
			fmt.Sprintf("Failed to %s queue: %v", verb, msg.err),
			4*time.Second, m.currentView,
		)
		return m, nil
	}
	return m, m.fetchStatus()
}

// handleRerunResultMsg processes job re-run results.
func (m model) handleRerunResultMsg(
	msg rerunResultMsg,
) (tea.Model, tea.Cmd) {
	// Release the in-flight slot FIRST, before every early return below, so
	// a rerun that fails, or one whose result is otherwise dropped, can
	// never leave the job permanently blocked. See panelRerunInFlight's doc
	// comment (tui.go) for the no-leak argument this is half of.
	delete(m.panelRerunInFlight, msg.jobID)
	if msg.err != nil {
		// Restore the optimistic mutation -- but only if there WAS one.
		// For a panel synthesis parent the dispatchers skip
		// the mutation entirely, so there is nothing to undo, and doing it
		// anyway is not the same-value no-op it looks like: the snapshot
		// was taken before the request went out, and anything that touched
		// this row in the meantime -- a close/unclose toggle, or a jobs
		// refresh landing fresh server state -- would be overwritten with
		// pre-rerun values. Only the error is surfaced for that case.
		if !msg.spawnsNewRun {
			m.setJobStatus(msg.jobID, msg.oldState)
			m.setJobStartedAt(msg.jobID, msg.oldStartedAt)
			m.setJobFinishedAt(msg.jobID, msg.oldFinishedAt)
			m.setJobError(msg.jobID, msg.oldError)
			m.mutateJob(msg.jobID, func(job *storage.ReviewJob) {
				if msg.agent != "" {
					job.Agent = msg.oldAgent
				}
				job.Closed = msg.oldClosed
				job.Verdict = msg.oldVerdict
			})
		}
		m.err = msg.err
		return m, nil
	}
	// PANEL SYNTHESIS EXCEPTION. Everything below this point
	// rests on "a rerun reuses the same job ID", which is false for a panel
	// synthesis parent: internal/daemon/server.go routes those to
	// rerunPanelRun, which clones the members and the synthesis row into a
	// BRAND-NEW run under a fresh panel_run_uuid with new job IDs and
	// leaves the original run -- including this job's row and its review --
	// intact as history. 'r' reaches this case: handleRerunKey blocks only
	// PanelRoleMember, and the control socket blocks nothing.
	//
	// So for this job, a confirmed rerun is NOT abandonment, and running
	// the block below would be wrong three ways: it clears a review that is
	// still current (forcing a refetch and closing an open fix panel), it
	// bumps jobAttemptGen and so drops a still-correct in-flight fetch
	// (which then only heals on the next reconcile pass), and it cancels
	// the user's pending open/fix intent with a "Job #N is rerunning"
	// flash that is simply untrue of the job they asked about.
	//
	// This is stated as a CODE exception rather than a documented one
	// precisely because jobAttemptGen's clause 4 ("nothing needs to be
	// disarmed on the rejection path") is derived from clause 2 ("a bump is
	// always abandonment"): leaving clause 2 with an unwritten exception
	// would leave clause 4 resting on a premise that does not hold. The
	// predicate is evaluated at DISPATCH, where the job is provably in
	// hand, and carried on the message -- see rerunSnapshot.spawnsNewRun
	// (actions.go) for why a lookup here would not be reliable.
	//
	// The new run's own jobs are ordinary rows with their own IDs; nothing
	// here needs to know about them, and the next jobs refresh brings them
	// in like any other newly enqueued work.
	//
	// Nor was this job's ROW optimistically mutated: both dispatchers skip
	// the re-queue write for a synthesis parent, since the
	// same fact that makes the attempt survive -- the daemon re-runs
	// nothing here -- makes that write wrong from the instant it is made.
	// So there is nothing to undo, and a flash is what confirms the action
	// instead: without the row visibly changing, an accepted rerun would
	// otherwise be completely silent until the new run's rows arrive on the
	// next refresh.
	if msg.spawnsNewRun {
		m.setFlash(
			fmt.Sprintf("Panel rerun queued for job #%d -- a new run will appear shortly", msg.jobID),
			3*time.Second, m.currentView,
		)
		return m, nil
	}
	if msg.agent != "" {
		m.mutateJob(msg.jobID, func(job *storage.ReviewJob) {
			job.Agent = msg.agent
		})
	}
	// Rerun confirmed: a rerun reuses the same job ID, so once the job
	// finishes again `currentReview.JobID == job.ID` would keep matching
	// even though currentReview still holds the PREVIOUS attempt's review
	// -- splitReconcileDetail and handleDetailFollowTick both treat that
	// match as "already loaded, nothing to fetch" and would leave the
	// stale review displayed indefinitely. Clear it here, at the point the
	// rerun is confirmed accepted (not on the earlier optimistic dispatch,
	// so a failed rerun request -- the msg.err branch above -- leaves the
	// old review in place). Guarded on JobID match so this can't disrupt a
	// loaded review for a DIFFERENT job (including stacked mode, where
	// currentReview is normally nil for the queue view anyway).
	if m.currentReview != nil && m.currentReview.JobID == msg.jobID {
		m.currentReview = nil
		m.currentResponses = nil
		m.reviewScroll = 0
		m.splitDetailErr = nil
		// The inline fix panel is scoped to whatever review is displayed
		// (fixPromptJobID is stamped from the loaded review's job when the
		// panel opens), so a review invalidated here can leave a stale
		// panel behind: once normalizeSplitState repairs the view back to
		// the queue and the user opens a DIFFERENT review, the old panel
		// would render over it, and submitting would target the reran job
		// instead of the one on screen. closeFixPanel resets
		// reviewFixPanelOpen/Focused/Pending and fixPromptJobID together.
		m.closeFixPanel()
	}
	// Invalidate every in-flight review fetch for THIS job, whether or not
	// it is the selected one. The counter is keyed by job, so bumping it
	// here says nothing about an in-flight fetch for any OTHER job -- which
	// is what lets this be unconditional (a jobAttemptGen bumper; see that
	// field's contract, tui.go, clauses 1 and 2).
	//
	// What this covers: a follow fetch in flight for the selected job while
	// currentReview is still nil (the debounced follow hasn't resolved yet)
	// and a rerun of that job confirms; and equally a regular (non-follow)
	// fetch dispatched before the rerun (queue Enter, stepReviewNav, tasks
	// 'P', ...). Left un-invalidated, either response later lands looking
	// perfectly current, populates currentReview with the OLD attempt's
	// content, and blocks the rerun's real result from ever being fetched
	// -- splitReconcileDetail/handleDetailFollowTick see currentReview.JobID
	// already matching and skip refetching.
	//
	// The per-job counter (not the global detailFollowGen) is what
	// invalidates these: it has no cross-job cost, so it needs no
	// selection gate and covers every job in both layouts. A global bump
	// here would invalidate legitimate in-flight fetches for whatever
	// unrelated job is currently selected, and a selection-gated bump
	// would miss reruns of unselected jobs entirely.
	if m.jobAttemptGen == nil {
		m.jobAttemptGen = make(map[int64]uint64)
	}
	m.jobAttemptGen[msg.jobID]++
	// Same reasoning as scheduleDetailFollow's matching clear
	// (belt-and-suspenders, not strictly load-bearing now that both
	// handlers release the slot on any response for the job -- see
	// reconcileFetchJobID's doc comment, tui.go): clear the suppression
	// slot immediately, rather than waiting for its doomed in-flight
	// response to arrive and self-resolve, so a fresh dispatch once the
	// rerun completes isn't unnecessarily delayed. Keyed on the SLOT's own
	// job, not on the selection: the slot tracks one specific job's
	// outstanding reconcile fetch, so clearing it for an unrelated job's
	// rerun would only cost a duplicate dispatch.
	if m.reconcileFetchJobID == msg.jobID {
		m.reconcileFetchJobID = 0
		m.reconcileFetchSeq = 0
	}
	// Disarm BOTH pending intents for THIS job, keyed on msg.jobID alone
	// and never on the selection. A confirmed rerun of job X abandons any
	// intent armed for X regardless of what is selected right now: the
	// user asked for a fresh attempt, so whatever content an in-flight
	// 'F'/'P'/Enter dispatch for X was waiting to serve belongs to the
	// superseded attempt. A selection-gated disarm would skip exactly the
	// reruns that target an unselected job (the control socket can target
	// any job by ID; TUI 'r' can race the selection moving), leaving the
	// intent armed and rescuable -- a later incidental gen bump would
	// then get the previous attempt's review served for a job that is
	// currently re-running. Per-job disarms have no cross-job cost: both
	// fields are keyed on jobID, so clearing them for X cannot affect an
	// intent armed for any other job.
	if m.reviewFixPanelPending && m.fixPromptJobID == msg.jobID {
		m.closeFixPanel()
	}
	if m.pendingReviewOpenJobID == msg.jobID {
		origin := m.pendingReviewOpenOrigin
		m.pendingReviewOpenJobID = 0
		m.pendingReviewOpenOrigin = 0
		m.pendingReviewOpenSeq = 0
		m.setWarningFlash(
			fmt.Sprintf("Job #%d is rerunning -- open request canceled", msg.jobID),
			3*time.Second, origin,
		)
	}
	return m, nil
}

// handleFailedCommentsMsg applies persisted comments fetched for a
// synthesized failed-job review (see failedCommentsMsg). Accepted only
// while that job's synthetic review is still the loaded, selected content:
// a persisted review's responses arrive with its own epoch-ordered fetch
// and must not be stomped by this side channel, and a response for a job
// the user has navigated away from is simply stale.
func (m model) handleFailedCommentsMsg(msg failedCommentsMsg) (tea.Model, tea.Cmd) {
	if msg.seq != m.failedCommentsSeq {
		// A newer comments fetch has since gone out: synthesized reviews
		// all share ID 0, so without this identity an OLDER dispatch's
		// response (navigate away and back re-dispatches, and the
		// post-success refetch always re-dispatches) could land last and
		// overwrite the newer result or a just-appended comment.
		return m, nil
	}
	if m.currentReview == nil || m.currentReview.JobID != msg.jobID ||
		m.currentReview.ID != 0 || m.selectedJobID != msg.jobID {
		return m, nil
	}
	if msg.err != nil {
		// Comments are additive context on a failure review; a failed
		// load keeps the review itself fully usable, so no error surface
		// beyond leaving the existing (possibly locally-appended) state.
		return m, nil
	}
	m.currentResponses = msg.responses
	return m, nil
}

// handleCommentResultMsg processes comment submission results.
func (m model) handleCommentResultMsg(
	msg commentResultMsg,
) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.err = msg.err
	} else {
		if m.commentJobID == msg.jobID {
			m.commentText = ""
			m.commentJobID = 0
		}
		// A synthesized failed-job review has no persisted review row
		// (ID == 0): the refresh fetch below would 404 against
		// /api/review and the just-created comment would never appear.
		// The daemon stored the comment keyed by the job, so append what
		// was posted directly; a later real review fetch (e.g. after a
		// rerun completes) supersedes this local copy with server state.
		// No selection gate needed: unlike the fetch branches below, a
		// local append advances no ordering epoch, so it cannot supersede
		// anything.
		if m.currentReview != nil && m.currentReview.JobID == msg.jobID &&
			m.currentReview.ID == 0 {
			m.currentResponses = append(m.currentResponses, storage.Response{
				JobID:     &msg.jobID,
				Responder: msg.responder,
				Response:  msg.comment,
				CreatedAt: time.Now(),
			})
			// Reconcile the optimistic append with server truth, and --
			// via the seq bump -- doom any pre-post comments fetch still
			// in flight, whose response would otherwise land WITHOUT the
			// just-submitted comment and wipe the append.
			cmd := m.dispatchFailedCommentsFetch(msg.jobID)
			return m, cmd
		}
		// splitActive() -- not the coarser m.layout == layoutSplit -- is
		// the knob: it encodes "the split pane is actually rendering",
		// including the tasks-origin exclusion. A tasks-origin review is
		// rendered full-screen even on a split-capable terminal, and the
		// follow path's failures land only in m.splitDetailErr, which
		// only the split pane renders -- routed down the follow path, a
		// failed comment refresh on a tasks-origin review would be
		// completely silent. splitActive() routes it to the else branch,
		// whose plain fetchReview failures surface through the ordinary
		// full-screen error mechanism. For every non-tasks-origin case
		// splitActive() reduces to m.layout == layoutSplit here, so only
		// the tasks-origin review behaves differently.
		if m.splitActive() {
			// A one-shot dispatch triggered by this single
			// commentResultMsg, not splitReconcileDetail's every-refresh
			// dispatch, so it needs no duplicate-dispatch suppression --
			// but it is ordered like every other review fetch
			// (dispatchReviewFollow bumps the shared epoch and stamps
			// it), which is what stops an OLDER fetch dispatched before
			// the comment from landing after it and wiping the fresh
			// comment back out of currentResponses. Gated on msg.jobID ==
			// m.selectedJobID: the comment refresh is the only one of
			// fetchReviewFollow's split-pane dispatchers not already tied
			// to the selection, and a dispatch for a no-longer-selected
			// job would advance the epoch and supersede a legitimate
			// fetch for the job that IS selected. splitActive()
			// already constrains currentView to viewQueue/viewReview
			// (list or detail focus, queue-origin only), so this branch
			// needs no separate currentView check of its own.
			if m.currentReview != nil && m.currentReview.JobID == msg.jobID &&
				msg.jobID == m.selectedJobID {
				cmd := m.dispatchReviewFollow(msg.jobID)
				return m, cmd
			}
		} else if m.currentView == viewReview &&
			m.currentReview != nil &&
			m.currentReview.JobID == msg.jobID {
			// Stacked mode, OR a tasks-origin review rendered full-screen
			// even while layout == layoutSplit (see above):
			// a plain non-follow fetch (ordered by the same epoch as
			// every other review fetch, but not follow-TAGGED), whose
			// failures surface through the ordinary full-screen error
			// mechanism rather than m.splitDetailErr (which only the
			// split detail pane renders, and which neither of these two
			// cases displays).
			//
			// Gated on msg.jobID == m.selectedJobID as well as on the
			// review being the DISPLAYED one. The selection gate has a
			// real cost -- an unrelated reselect (e.g. the control
			// socket's select-job, which in stacked mode moves
			// selectedJobID without touching currentReview) means the
			// displayed review's comments don't refresh -- but an
			// ungated dispatch would be worse than useless: every review
			// fetch advances the shared ordering epoch, so a dispatch
			// whose response the jobID gate is guaranteed to drop still
			// supersedes a concurrent, legitimate fetch for the job that
			// IS selected (stacked, comment on X, right-arrow to Y
			// before the POST resolves: X's refresh at epoch N+1 dooms
			// Y's fetch at N, and both are dropped, with no reconcile in
			// stacked to heal it). Since the dispatch could never be
			// accepted in this state, gating it loses nothing.
			if msg.jobID == m.selectedJobID {
				cmd := m.dispatchReviewFetch(msg.jobID)
				return m, cmd
			}
		}
	}
	return m, nil
}

// handleClipboardResultMsg processes clipboard copy results.
func (m model) handleClipboardResultMsg(
	msg clipboardResultMsg,
) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.err = fmt.Errorf("copy failed: %w", msg.err)
		m.setWarningFlash(clipboardErrorMessage(msg.err), 4*time.Second, msg.view)
	} else {
		m.setFlash("Copied to clipboard", 2*time.Second, msg.view)
	}
	return m, nil
}

// clipboardErrorMessage returns a user-friendly flash message for a
// clipboard write failure. The atotto/clipboard library returns a verbose
// "No clipboard utilities available..." error when no clipboard tool is
// installed; we substitute a shorter, actionable hint in that case.
func clipboardErrorMessage(err error) string {
	if strings.Contains(err.Error(), "No clipboard utilities available") {
		return "Copy failed: install xclip, wl-clipboard, or xsel"
	}
	return fmt.Sprintf("Copy failed: %v", err)
}

// handleSavePatchResultMsg processes save-patch-to-file results.
func (m model) handleSavePatchResultMsg(msg savePatchResultMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.err = fmt.Errorf("save patch failed: %w", msg.err)
	} else {
		m.setFlash("Saved to "+msg.path, 3*time.Second, viewPatch)
	}
	return m, nil
}

// handleSSEEventMsg processes real-time events from the daemon's NDJSON stream.
// Triggers an immediate data refresh and re-subscribes for the next event.
// If a fetch is already in flight, sets a pending flag so the refresh runs
// after the current load completes (avoiding stale data).
func (m model) handleSSEEventMsg() (tea.Model, tea.Cmd) {
	if m.loadingMore || m.loadingJobs {
		m.ssePendingRefresh = true
		return m, waitForSSE(m.sseCh, m.sseStop)
	}
	m.loadingJobs = true
	cmds := []tea.Cmd{
		m.fetchJobs(),
		waitForSSE(m.sseCh, m.sseStop),
	}
	// SSE events signal daemon state changes — use the stale-aware
	// helpers so a skipped fetch gets retried after the in-flight one.
	if cmd := m.requestFetchStatus(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	if m.tasksWorkflowEnabled() && (m.currentView == viewTasks || m.hasActiveFixJobs()) {
		if cmd := m.requestFetchFixJobs(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	return m, tea.Batch(cmds...)
}

// consumeSSEPendingRefresh returns the full SSE refresh command set if
// an event arrived while a fetch was in flight, then clears the flag.
// Returns nil if no refresh is pending.
func (m *model) consumeSSEPendingRefresh() tea.Cmd {
	if !m.ssePendingRefresh {
		return nil
	}
	m.ssePendingRefresh = false
	m.loadingJobs = true
	cmds := []tea.Cmd{m.fetchJobs()}
	if cmd := m.requestFetchStatus(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	if m.tasksWorkflowEnabled() && (m.currentView == viewTasks || m.hasActiveFixJobs()) {
		if cmd := m.requestFetchFixJobs(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	return tea.Batch(cmds...)
}

// handleReconnectMsg processes daemon reconnection attempts.
func (m model) handleReconnectMsg(msg reconnectMsg) (tea.Model, tea.Cmd) {
	m.reconnecting = false
	if msg.err != nil {
		return m, nil
	}

	if msg.endpoint != m.endpoint {
		m.endpoint = msg.endpoint
		m.client = msg.endpoint.HTTPClient(10 * time.Second)
		m.api = newDaemonAPI(msg.endpoint, m.client)
		// Update runtime metadata so external tools see the
		// new daemon address after reconnect.
		if m.controlSocket != "" {
			rtInfo := buildTUIRuntimeInfo(
				m.controlSocket, m.endpoint.ConfigAddr(),
			)
			if err := WriteTUIRuntime(rtInfo); err != nil {
				log.Printf(
					"warning: failed to update runtime info: %v",
					err,
				)
			}
		}
	}

	// Restart SSE subscription on any successful reconnect, not just
	// endpoint changes. The old goroutine may be stuck in backoff
	// after a same-address daemon restart.
	if m.sseStop != nil {
		close(m.sseStop)
		m.sseCh = make(chan struct{}, 1)
		m.sseStop = make(chan struct{})
		go startSSESubscription(m.endpoint, m.sseCh, m.sseStop)
	}

	m.consecutiveErrors = 0
	m.err = nil
	if msg.version != "" {
		m.daemonVersion = msg.version
	}
	m.clearFetchFailed()
	m.fetchGen++ // invalidate pre-reconnect status/fix-jobs responses
	m.fetchSeq++ // invalidate pre-reconnect jobs responses
	m.loadingJobs = true
	m.loadingMore = false
	cmds := []tea.Cmd{m.fetchJobs(), m.fetchRepoNames()}
	// Force fetches on reconnect — previous in-flight requests
	// were against the old connection and will fail or be stale.
	m.loadingStatus = true
	m.statusStale = false
	cmds = append(cmds, m.fetchStatus())
	m.loadingFixJobs = false
	m.fixJobsStale = false
	if m.tasksWorkflowEnabled() {
		m.loadingFixJobs = true
		cmds = append(cmds, m.fetchFixJobs())
	}
	if cmd := m.fetchUnloadedBranches(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	if m.sseCh != nil {
		cmds = append(cmds, waitForSSE(m.sseCh, m.sseStop))
	}
	return m, tea.Batch(cmds...)
}

// handleWindowSizeMsg processes terminal resize events.
// maybeResizeRefill dispatches a jobs refetch when the (resized) terminal
// can show more rows than are loaded and more data is available. In split
// layout the list pane's row budget (splitGeometry) differs from
// queueVisibleRows' full-screen chrome reservation, so use the pane's own
// capacity there or a resize into a tall split pane can leave rows unfilled
// even though more data is available (hasMore). Shared by
// handleWindowSizeMsg's two resize arms (the pane-tail restart and the
// plain fall-through) so the tail-restart early return can't skip the
// refill. Mutates loadingJobs when it dispatches.
func (m *model) maybeResizeRefill() tea.Cmd {
	if m.loadingMore || m.loadingJobs ||
		len(m.jobs) == 0 || !m.hasMore ||
		m.activeBranchFilter == branchNone {
		return nil
	}
	visibleRows := m.queueVisibleRows()
	if m.layout == layoutSplit {
		visibleRows = m.queuePaneRowCapacity()
	}
	if visibleRows+queuePrefetchBuffer > len(m.jobs) {
		m.loadingJobs = true
		return m.fetchJobs()
	}
	return nil
}

func (m model) handleWindowSizeMsg(
	msg tea.WindowSizeMsg,
) (tea.Model, tea.Cmd) {
	m.width = msg.Width
	m.height = msg.Height
	m.heightDetected = true

	var followCmd tea.Cmd
	if next := m.resolveLayout(); next != m.layout {
		m.applyLayout(next)
		m, followCmd = m.maybeBootstrapDetail()
	}

	// Width change while tailing a running job's live log in the split
	// detail pane requires restarting the tail at the new pane width --
	// otherwise the persistent paneLogFmtr keeps wrapping at the stale
	// width for the rest of the session. Gated on splitActive() (layout
	// split AND currentView queue/review) rather than bare
	// layout==layoutSplit: a transient view (e.g. viewLog) can be open on
	// top of split, and stealing the early return here would skip that
	// view's own resize re-render below, leaving IT at the stale width
	// instead. When the pane isn't the visible thing (transient view
	// open), don't fetch now -- just invalidate the tail (bump seq, stop
	// streaming) so it doesn't keep polling at the stale width in the
	// background; startPaneLog restarts it cleanly on the next follow
	// tick once the pane is visible again.
	if m.paneLogStreaming {
		if m.splitActive() {
			if job, ok := m.selectedJob(); ok &&
				job.ID == m.paneLogJobID &&
				job.Status == storage.JobStatusRunning {
				m.paneLogSeq++
				m.paneLogOffset = 0
				m.paneLogLines = nil
				m.paneLogFmtr = streamfmt.NewWithWidth(
					io.Discard, m.paneLogWidth(), m.glamourStyle,
					decoderForJobLog(m.paneLogAgent, m.paneLogSource),
				)
				// The same resize that re-widths the tail can also have
				// grown the list pane past the loaded rows -- batch the
				// refill check rather than skipping it, or the list stays
				// underfilled until the next SSE event or fallback poll.
				refillCmd := m.maybeResizeRefill()
				return m, tea.Batch(followCmd, m.fetchPaneLog(job.ID), refillCmd)
			}
		} else if m.layout == layoutSplit {
			// A transient view is covering the pane, so restarting the
			// tail now would poll invisibly at the wrong width. Invalidate
			// it and mark it paused; the Update tail resumes (via
			// splitReconcileDetail) as soon as the split panes are visible
			// again, rather than waiting for the next jobs refresh.
			m.paneLogSeq++
			m.paneLogStreaming = false
			m.paneLogPaused = true
		}
	}

	// If terminal can show more jobs than we have, re-fetch to fill.
	refillCmd := m.maybeResizeRefill()

	// Width change in log view requires full re-render
	if m.currentView == viewLog {
		m.logOffset = 0
		m.logLines = nil
		m.logFmtr = streamfmt.NewWithWidth(
			io.Discard, msg.Width, m.glamourStyle,
			decoderForJobLog(m.logAgent, m.logSource),
		)
		m.logFetchSeq++
		m.logLoading = true
		return m, tea.Batch(followCmd, m.fetchJobLog(m.logJobID), refillCmd)
	}

	if refillCmd != nil {
		return m, tea.Batch(followCmd, refillCmd)
	}

	return m, followCmd
}

// handleTickMsg processes periodic tick events for adaptive polling.
func (m model) handleTickMsg(
	_ tickMsg,
) (tea.Model, tea.Cmd) {
	// Skip job refresh while pagination or another refresh is in flight
	if m.loadingMore || m.loadingJobs {
		cmds := []tea.Cmd{m.tick()}
		if cmd := m.startFetchStatus(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		return m, tea.Batch(cmds...)
	}
	cmds := []tea.Cmd{m.tick(), m.fetchJobs()}
	if cmd := m.startFetchStatus(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	if m.tasksWorkflowEnabled() && (m.currentView == viewTasks || m.hasActiveFixJobs()) {
		if cmd := m.startFetchFixJobs(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	return m, tea.Batch(cmds...)
}

// handleLogTickMsg processes log stream polling ticks.
func (m model) handleLogTickMsg(
	_ logTickMsg,
) (tea.Model, tea.Cmd) {
	if m.currentView == viewLog && m.logStreaming &&
		m.logJobID > 0 && !m.logLoading {
		m.logLoading = true
		return m, m.fetchJobLog(m.logJobID)
	}
	return m, nil
}

// handleUpdateCheckMsg processes version update check results.
func (m model) handleUpdateCheckMsg(
	msg updateCheckMsg,
) (tea.Model, tea.Cmd) {
	m.updateAvailable = msg.version
	m.updateIsDevBuild = msg.isDevBuild
	return m, nil
}

// handleJobsErrMsg processes job fetch errors.
func (m model) handleJobsErrMsg(
	msg jobsErrMsg,
) (tea.Model, tea.Cmd) {
	if msg.seq < m.fetchSeq {
		return m, nil
	}
	m.err = msg.err
	m.loadingJobs = false
	if cmd := m.handleConnectionError(msg.err); cmd != nil {
		return m, cmd
	}
	return m, m.consumeSSEPendingRefresh()
}

// handlePaginationErrMsg processes pagination fetch errors.
func (m model) handlePaginationErrMsg(
	msg paginationErrMsg,
) (tea.Model, tea.Cmd) {
	if msg.seq < m.fetchSeq {
		m.loadingMore = false
		m.paginateNav = 0
		return m, nil
	}
	m.err = msg.err
	m.loadingMore = false
	m.paginateNav = 0
	if cmd := m.handleConnectionError(msg.err); cmd != nil {
		return m, cmd
	}
	return m, m.consumeSSEPendingRefresh()
}

// handleErrMsg processes generic error messages.
func (m model) handleStatusErrMsg(
	msg statusErrMsg,
) (tea.Model, tea.Cmd) {
	if msg.gen < m.fetchGen {
		return m, nil // discard pre-reconnect error
	}
	m.loadingStatus = false
	m.err = msg.err
	if m.statusStale {
		m.statusStale = false
		m.loadingStatus = true
		return m, m.fetchStatus()
	}
	if cmd := m.handleConnectionError(msg.err); cmd != nil {
		return m, cmd
	}
	return m, nil
}

func (m model) handleErrMsg(
	msg errMsg,
) (tea.Model, tea.Cmd) {
	m.err = msg
	if cmd := m.handleConnectionError(msg); cmd != nil {
		return m, cmd
	}
	return m, nil
}

// handleFixTriggerResultMsg processes fix job trigger results.
func (m model) handleFixTriggerResultMsg(
	msg fixTriggerResultMsg,
) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.err = msg.err
		m.setFlash(fmt.Sprintf(
			"Fix failed: %v", msg.err,
		), 3*time.Second, viewTasks)
	} else if msg.warning != "" {
		m.setFlash(msg.warning, 5*time.Second, viewTasks)
		return m, m.requestFetchFixJobs()
	} else {
		m.setFlash(fmt.Sprintf(
			"Fix job #%d enqueued", msg.job.ID,
		), 3*time.Second, viewTasks)
		return m, m.requestFetchFixJobs()
	}
	return m, nil
}

// handlePatchResultMsg processes patch fetch results.
func (m model) handlePatchResultMsg(
	msg patchMsg,
) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.setFlash(fmt.Sprintf(
			"Patch fetch failed: %v", msg.err,
		), 3*time.Second, viewTasks)
	} else {
		m.patchText = msg.patch
		m.patchJobID = msg.jobID
		m.patchScroll = 0
		m.currentView = viewPatch
	}
	return m, nil
}

// handleApplyPatchResultMsg processes patch application results.
func (m model) handleApplyPatchResultMsg(
	msg applyPatchResultMsg,
) (tea.Model, tea.Cmd) {
	if msg.needWorktree {
		m.worktreeConfirmJobID = msg.jobID
		m.worktreeConfirmBranch = msg.branch
		m.currentView = viewKindWorktreeConfirm
		return m, nil
	}
	if msg.rebase {
		m.setFlash(fmt.Sprintf(
			"Patch for job #%d doesn't apply cleanly"+
				" - triggering rebase", msg.jobID,
		), 5*time.Second, viewTasks)
		cmds := []tea.Cmd{m.triggerRebase(msg.jobID)}
		if cmd := m.requestFetchFixJobs(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		return m, tea.Batch(cmds...)
	} else if msg.commitFailed {
		detail := fmt.Sprintf(
			"Job #%d: %v", msg.jobID, msg.err,
		)
		if msg.worktreeDir != "" {
			detail += fmt.Sprintf(
				" (worktree kept at %s)", msg.worktreeDir,
			)
		}
		m.setFlash(detail, 8*time.Second, viewTasks)
	} else if msg.err != nil {
		m.setFlash(fmt.Sprintf(
			"Apply failed: %v", msg.err,
		), 3*time.Second, viewTasks)
	} else {
		m.setFlash(fmt.Sprintf(
			"Patch from job #%d applied and committed",
			msg.jobID,
		), 3*time.Second, viewTasks)
		cmds := []tea.Cmd{}
		if cmd := m.requestFetchFixJobs(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		if msg.parentJobID > 0 {
			cmds = append(
				cmds,
				m.markParentClosed(msg.parentJobID),
			)
		}
		return m, tea.Batch(cmds...)
	}
	return m, nil
}
