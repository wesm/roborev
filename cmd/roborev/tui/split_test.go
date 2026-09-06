package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
)

func splitModel(opts ...testModelOption) model {
	base := []testModelOption{
		withCurrentView(viewQueue),
		withDimensions(150, 40),
		withTestJobs(testQueueJobs()...),
		withSelection(1, 2),
	}
	m := initTestModel(append(base, opts...)...)
	m.layout = layoutSplit
	return m
}

func TestRenderSplitShowsBothPanes(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withReview(splitTestReview()))
	out := m.renderSplit()
	assert.Contains(out, "JobID")         // list pane header
	assert.Contains(out, "first finding") // detail pane body
	assert.Contains(out, "│")             // pane borders present
	// Every screen line fits the terminal width.
	for line := range strings.SplitSeq(out, "\n") {
		assert.LessOrEqual(lipgloss.Width(line), 150)
	}
}

func TestViewContentDispatchesToSplit(t *testing.T) {
	m := splitModel(withReview(splitTestReview()))
	assert.Contains(t, m.viewContent(), "first finding")

	// Stacked mode still renders the plain queue.
	m.layout = layoutStacked
	assert.NotContains(t, m.viewContent(), "first finding")
}

func TestSplitViewMouseCaptureFollowsFocusedPane(t *testing.T) {
	m := splitModel(withReview(splitTestReview()))

	assert.Equal(t, tea.MouseModeCellMotion, m.View().MouseMode)

	m.currentView = viewReview
	m.focus = focusDetail
	assert.Equal(t, tea.MouseModeNone, m.View().MouseMode)
}

func TestDetailPaneStates(t *testing.T) {
	assert := assert.New(t)

	// Failed job: error text.
	m := splitModel(withSelection(2, 1))
	m.currentReview = nil
	lines := strings.Join(m.renderDetailPane(88, 25), "\n")
	assert.Contains(lines, "boom")

	// Running job: status card.
	m = splitModel(withSelection(0, 3))
	m.currentReview = nil
	m.jobs[0].ReviewType = "security"
	lines = stripANSI(strings.Join(m.renderDetailPane(88, 25), "\n"))
	assert.Contains(lines, "running")
	assert.Contains(lines, "codex")
	assert.Contains(lines, "Review type: security")

	// Done job, review not yet fetched: loading placeholder.
	m = splitModel(withSelection(1, 2))
	m.currentReview = nil
	lines = strings.Join(m.renderDetailPane(88, 25), "\n")
	assert.Contains(lines, "Loading")
}

// TestSplitInfoLineStaleReview covers a stale m.currentReview: focus is on
// the detail pane, but the loaded review belongs to job 2 while selection has
// since moved to job 3 (running, no review at all). renderDetailPane
// correctly falls back to the status card in this case; splitInfoLine must
// not compute a scroll indicator from the stale review either.
func TestSplitInfoLineStaleReview(t *testing.T) {
	m := splitModel(withReview(splitTestReview()), withSelection(0, 3))
	m.focus = focusDetail

	footerRows := m.splitFooterRows()
	footerLines := len(reflowHelpRows(footerRows, m.width))
	g := splitGeometry(m.width, m.height, footerLines)

	info := m.splitInfoLine(g)
	assert.NotContains(t, info, "of")
}

func TestStateSnapshotIncludesLayout(t *testing.T) {
	m := splitModel()
	resp := m.buildStateResponse()
	snap, ok := resp.Data.(stateSnapshot)
	require.True(t, ok)
	assert.Equal(t, "split", snap.Layout)
	assert.Equal(t, "list", snap.Focus)

	m.layout = layoutStacked
	snap = m.buildStateResponse().Data.(stateSnapshot)
	assert.Equal(t, "stacked", snap.Layout)
	assert.Empty(t, snap.Focus)
}

func mouseClickAt(x, y int) tea.MouseMsg {
	return tea.MouseClickMsg(tea.Mouse{
		X:      x,
		Y:      y,
		Button: tea.MouseLeft,
	})
}

func mouseWheelAt(x, y int, btn tea.MouseButton) tea.MouseMsg {
	return tea.MouseWheelMsg(tea.Mouse{
		X:      x,
		Y:      y,
		Button: btn,
	})
}

// splitFirstDataRowY renders the split screen and locates the on-screen row
// index of the first queue data row (identified by a marker unique to it,
// e.g. a job's GitRef) rather than hardcoding the chrome offset, so a future
// change to the title/border/header chrome desyncs this test loudly instead
// of silently.
func splitFirstDataRowY(t *testing.T, m model, marker string) int {
	t.Helper()
	lines := strings.Split(m.renderSplit(), "\n")
	idx := -1
	for i, line := range lines {
		if strings.Contains(line, marker) {
			idx = i
			break
		}
	}
	require.NotEqual(t, -1, idx, "marker not found in rendered split output")
	return idx
}

func TestSplitMouseClickSelectsAndFocuses(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withReview(splitTestReview()))
	g := splitGeometry(150, 40, len(reflowHelpRows(m.splitFooterRows(), 150)))

	// Click a list row: selects it, keeps/sets list focus.
	firstDataY := splitFirstDataRowY(t, m, "cccc333") // job 3's GitRef, first visible row
	res, _ := m.handleSplitMouse(mouseClickAt(5, firstDataY))
	got := res.(model)
	assert.Equal(focusList, got.focus)
	assert.Equal(int64(3), got.selectedJobID) // first visible row

	// Click in the detail pane while the loaded review still belongs to
	// job 2 (the follow-fetch for job 3 hasn't landed): no-op, per the
	// stale-review guard (Finding 2, selectedReviewLoaded).
	res, _ = got.handleSplitMouse(mouseClickAt(g.listOuterW+5, 10))
	stale := res.(model)
	assert.Equal(focusList, stale.focus, "must not enter detail focus with a stale review for a different job")
	assert.Equal(viewQueue, stale.currentView)

	// Select the row matching the loaded review's job (2): detail click
	// now focuses detail.
	dataY2 := splitFirstDataRowY(t, got, "bbbb222")
	res, _ = got.handleSplitMouse(mouseClickAt(5, dataY2))
	got = res.(model)
	res, _ = got.handleSplitMouse(mouseClickAt(g.listOuterW+5, 10))
	got = res.(model)
	assert.Equal(focusDetail, got.focus)
	assert.Equal(viewReview, got.currentView)
}

func TestSplitMouseWheelScrollsPaneUnderCursor(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withReview(splitTestReview()))
	m.reviewScroll = 5
	g := splitGeometry(150, 40, len(reflowHelpRows(m.splitFooterRows(), 150)))

	// Wheel over detail pane scrolls the review, regardless of focus.
	res, _ := m.handleSplitMouse(mouseWheelAt(g.listOuterW+5, 10, tea.MouseWheelUp))
	assert.Less(res.(model).reviewScroll, 5)

	// Wheel over list pane moves the queue selection.
	prev := m.selectedJobID
	res, _ = m.handleSplitMouse(mouseWheelAt(5, 10, tea.MouseWheelDown))
	assert.NotEqual(prev, res.(model).selectedJobID)
}

// TestSplitMouseUnchangedSelectionPreservesScroll:
// scheduleDetailFollow (which zeroes reviewScroll) must only fire when a
// list-side click or wheel actually moves the selection -- not on a click on
// the already-selected row, nor on a wheel at a boundary where
// moveQueueSelection clamps in place. Otherwise scrolling the detail pane and
// then incidentally re-clicking/re-wheeling the same list row would silently
// discard the reader's scroll position.
func TestSplitMouseUnchangedSelectionPreservesScroll(t *testing.T) {
	assert := assert.New(t)

	// (a) Wheel up while already at the topmost row: selection clamps in
	// place, reviewScroll (and detailFollowGen) must be untouched.
	m := splitModel(withSelection(0, 3)) // job 3 is the first/topmost row
	m.reviewScroll = 42
	prevGen := m.detailFollowGen
	prevSelected := m.selectedJobID

	res, cmd := m.handleSplitMouse(mouseWheelAt(5, 10, tea.MouseWheelUp))
	got := res.(model)
	assert.Nil(cmd)
	assert.Equal(prevSelected, got.selectedJobID)
	assert.Equal(42, got.reviewScroll)
	assert.Equal(prevGen, got.detailFollowGen)

	// (b) Click the already-selected row: same guarantees.
	m2 := splitModel(withReview(splitTestReview())) // selection on job 2
	m2.reviewScroll = 42
	prevGen2 := m2.detailFollowGen
	firstDataY := splitFirstDataRowY(t, m2, "bbbb222") // job 2's GitRef, its own row

	res2, cmd2 := m2.handleSplitMouse(mouseClickAt(5, firstDataY))
	got2 := res2.(model)
	assert.Nil(cmd2)
	assert.Equal(int64(2), got2.selectedJobID)
	assert.Equal(42, got2.reviewScroll)
	assert.Equal(prevGen2, got2.detailFollowGen)

	// (c) Click a DIFFERENT row: follow scheduled (gen bumped) and
	// scheduleDetailFollow's reviewScroll reset takes effect.
	firstDataY3 := splitFirstDataRowY(t, m2, "cccc333") // job 3's row, different from selection
	res3, cmd3 := m2.handleSplitMouse(mouseClickAt(5, firstDataY3))
	got3 := res3.(model)
	assert.NotNil(cmd3)
	assert.Equal(int64(3), got3.selectedJobID)
	assert.Equal(0, got3.reviewScroll)
	assert.Greater(got3.detailFollowGen, prevGen2)
}

// TestSplitMouseWheelOverListDisarmsPendingReviewOpen covers roborev item
// (5): handleSplitMouse's wheel-in-list branch is one of the two direct
// scheduleDetailFollow callers (split_render.go) that must call
// disarmPendingReviewOpen when the wheel genuinely moves the selection.
func TestSplitMouseWheelOverListDisarmsPendingReviewOpen(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withReview(splitTestReview())) // job 2 selected
	cmd := m.dispatchReviewFetch(2)
	require.NotNil(cmd)
	require.Equal(int64(2), m.pendingReviewOpenJobID)

	res, followCmd := m.handleSplitMouse(mouseWheelAt(5, 10, tea.MouseWheelDown))
	got := res.(model)
	require.NotNil(followCmd, "sanity: the wheel must have moved the selection")
	assert.NotEqual(int64(2), got.selectedJobID)
	assert.Equal(int64(0), got.pendingReviewOpenJobID,
		"a genuine mouse-wheel selection change must disarm the pending-open intent")
}

// TestSplitMouseClickOnDifferentRowDisarmsPendingReviewOpen is the click
// counterpart to TestSplitMouseWheelOverListDisarmsPendingReviewOpen,
// covering handleSplitMouse's other direct scheduleDetailFollow call site.
func TestSplitMouseClickOnDifferentRowDisarmsPendingReviewOpen(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withReview(splitTestReview())) // job 2 selected
	cmd := m.dispatchReviewFetch(2)
	require.NotNil(cmd)
	require.Equal(int64(2), m.pendingReviewOpenJobID)

	firstDataY3 := splitFirstDataRowY(t, m, "cccc333") // job 3's row, different from selection
	res, followCmd := m.handleSplitMouse(mouseClickAt(5, firstDataY3))
	got := res.(model)
	require.NotNil(followCmd)
	require.Equal(int64(3), got.selectedJobID)
	assert.Equal(int64(0), got.pendingReviewOpenJobID,
		"a genuine mouse-click selection change must disarm the pending-open intent")
}

// TestSplitWheelDownPaginatesAtBottom: the split list pane's wheel routes
// through the shared queueNavDown helper, so a wheel-down that clamps at the
// last loaded row resumes pagination exactly like keyboard navigation
// (previously the wheel called moveQueueSelection bare and wheel users could
// never load older jobs past the final loaded row).
func TestSplitWheelDownPaginatesAtBottom(t *testing.T) {
	assert := assert.New(t)

	m := splitModel(withSelection(2, 1)) // job 1, the last loaded row
	m.hasMore = true
	m.loadingJobs = false
	res, cmd := m.handleSplitMouse(mouseWheelAt(5, 10, tea.MouseWheelDown))
	got := res.(model)
	assert.Equal(int64(1), got.selectedJobID, "clamped in place at the bottom")
	assert.True(got.loadingMore)
	assert.NotNil(cmd)

	// Nothing more to load: flash the boundary like the keyboard path, no fetch.
	m = splitModel(withSelection(2, 1))
	m.hasMore = false
	m.loadingJobs = false
	res, cmd = m.handleSplitMouse(mouseWheelAt(5, 10, tea.MouseWheelDown))
	got = res.(model)
	assert.False(got.loadingMore)
	assert.Nil(cmd)
	assertFlashMessage(t, got, viewQueue, "No older review")
}

// TestSplitWheelDownPrefetchesNearEnd: a wheel-down that moves the selection
// near the end of loaded data triggers the same prefetch as keyboard
// navigation (maybePrefetch), batched with the detail-follow cmd.
func TestSplitWheelDownPrefetchesNearEnd(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withReview(splitTestReview())) // job 2 selected
	m.hasMore = true
	m.loadingJobs = false

	res, cmd := m.handleSplitMouse(mouseWheelAt(5, 10, tea.MouseWheelDown))
	got := res.(model)
	assert.Equal(int64(1), got.selectedJobID)
	assert.True(got.loadingMore, "moving within prefetch range of the end must start a page fetch")
	assert.NotNil(cmd)
}

// TestSplitExternalRerunBlocksStaleReviewActions: an EXTERNAL rerun (another
// client) arrives as a plain jobs-refresh status change -- no local
// handleRerunResultMsg runs to clear the loaded review. Once
// splitReconcileDetail observes the selected job back in queued/running,
// selectedReviewLoaded must report the loaded review stale so tab and the
// detail-pane click stop handing review actions to the replaced attempt.
func TestSplitExternalRerunBlocksStaleReviewActions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withReview(splitTestReview())) // job 2 (done) selected
	require.True(m.selectedReviewLoaded(), "sanity: fresh review is actionable")

	// External rerun observed: job 2 back to queued on a jobs refresh.
	m.jobs[1].Status = storage.JobStatusQueued
	got, _ := m.splitReconcileDetail()
	require.Equal(int64(2), got.paneReviewSeenNonTerminalJob)
	assert.False(got.selectedReviewLoaded())

	// Tab must refuse to enter detail focus on the stale review.
	res, _ := got.handleTabKey()
	tabbed := res.(model)
	assert.Equal(viewQueue, tabbed.currentView)
	assert.Equal(focusList, tabbed.focus)

	// A detail-pane click must refuse as well.
	g := splitGeometry(got.width, got.height,
		len(reflowHelpRows(got.splitFooterRows(), got.width)))
	res, _ = got.handleSplitMouse(mouseClickAt(g.listOuterW+5, 10))
	clicked := res.(model)
	assert.Equal(viewQueue, clicked.currentView)
	assert.Equal(focusList, clicked.focus)

	// The rerun completing does not restore actionability by itself: the
	// flag stays set until the fresh review is ACCEPTED (handleReviewMsg).
	later := splitTestFinishedAt.Add(time.Minute)
	got.jobs[1].Status = storage.JobStatusDone
	got.jobs[1].FinishedAt = &later
	assert.False(got.selectedReviewLoaded())
}

// TestDetailPaneHidesStaleAttemptReview: the done-job branch renders the
// review only when selectedReviewLoaded confirms attempt freshness. A
// stale attempt's review (same reused job ID) must show the loading state
// while the refetch is in flight -- and must not mask splitDetailErr when
// that refetch fails, or obsolete output would display indefinitely.
func TestDetailPaneHidesStaleAttemptReview(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withReview(splitTestReview())) // job 2 (done) selected
	require.Contains(t, strings.Join(m.renderDetailPane(88, 25), "\n"), "first finding",
		"sanity: a fresh review renders")

	// A rerun of job 2 was observed; the loaded review is a previous
	// attempt's.
	m.paneReviewSeenNonTerminalJob = 2
	lines := strings.Join(m.renderDetailPane(88, 25), "\n")
	assert.NotContains(lines, "first finding", "a stale attempt's review must not render")
	assert.Contains(lines, "Loading review...")

	// The refetch fails: the error must show, not the stale review.
	m.splitDetailErr = errors.New("connection refused")
	lines = strings.Join(m.renderDetailPane(88, 25), "\n")
	assert.NotContains(lines, "first finding")
	assert.Contains(lines, "Failed to load review")
}

// TestTasksExitRestoresQueueSelection: opening a task's review points
// selectedJobID at the fix job (absent from m.jobs); every tasks-to-queue
// exit must repair the selection or the queue renders no highlighted row
// and the split pane "No job selected" until a refresh heals it.
func TestTasksExitRestoresQueueSelection(t *testing.T) {
	assert := assert.New(t)

	taskSelected := func() model {
		m := splitModel()
		m.tasksEnabled = true
		m.currentView = viewTasks
		m.selectedJobID = 99 // fix job: absent from m.jobs/panelMembers
		return m
	}

	// esc from the tasks view.
	res, cmd := taskSelected().handleTasksKey(keySpecialMsg(tea.KeyEscape))
	got := res.(model)
	assert.Equal(viewQueue, got.currentView)
	assert.Equal(int64(2), got.selectedJobID, "restored from the untouched selectedIdx")
	assert.NotNil(cmd, "the shared follow transition must re-point the split pane")

	// Control-socket set-view queue.
	got, _, _ = taskSelected().handleCtrlSetView(json.RawMessage(`{"view":"queue"}`))
	assert.Equal(viewQueue, got.currentView)
	assert.Equal(int64(2), got.selectedJobID)

	// A selection that still resolves is left alone -- including a
	// side-fetched panel member, which selectedIdx-based repair would
	// wrongly clobber.
	m := splitModel()
	m.tasksEnabled = true
	m.currentView = viewTasks
	m.panelMembers[testUUID("u1")] = []storage.ReviewJob{{ID: 77, PanelRunUUID: testUUIDPtr("u1")}}
	m.selectedJobID = 77
	m.selectedIdx = -1
	res, _ = m.handleTasksKey(keySpecialMsg(tea.KeyEscape))
	got = res.(model)
	assert.Equal(int64(77), got.selectedJobID, "a resolvable member selection must survive the exit")
}

// TestCrossJobRerunObservationDoesNotBlockFailedReview: the non-terminal
// observation is scoped per job. Observing job 2's external rerun and then
// selecting the FAILED job 1 (whose failure review handleDetailFollowTick
// synthesizes from current state) must not mark job 1's fresh review stale
// -- previously a global flag bled across the selection change and blocked
// detail focus until the next jobs refresh. Job 2's own observation
// survives for when the user returns to it.
func TestCrossJobRerunObservationDoesNotBlockFailedReview(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withReview(splitTestReview())) // job 2 (done) selected, review loaded

	m.jobs[1].Status = storage.JobStatusQueued
	m, _ = m.splitReconcileDetail()
	require.Equal(int64(2), m.paneReviewSeenNonTerminalJob)

	// Select the failed job 1; the debounced follow synthesizes its review.
	m = m.moveSelectionToJobID(1)
	m, _ = m.scheduleDetailFollow()
	res, _ := m.handleDetailFollowTick(detailFollowTickMsg{gen: m.detailFollowGen})
	m = res.(model)
	require.NotNil(m.currentReview)
	require.Equal(int64(1), m.currentReview.JobID)

	assert.True(m.selectedReviewLoaded(), "job 1's synthesized failure review is current")
	res, _ = m.handleTabKey()
	assert.Equal(viewReview, res.(model).currentView, "tab must enter detail focus")
	assert.Equal(int64(2), m.paneReviewSeenNonTerminalJob,
		"job 2's observation survives for when the user returns to it")

	// A same-job observation IS cleared by the tick's synchronous rebuild:
	// that rebuild is an acceptance for the failed job itself.
	m.paneReviewSeenNonTerminalJob = 1
	res, _ = m.handleDetailFollowTick(detailFollowTickMsg{gen: m.detailFollowGen})
	assert.Equal(int64(0), res.(model).paneReviewSeenNonTerminalJob)
}

// TestSelectedReviewLoadedDetectsMissedRerunCompletion: if the rerun's whole
// queued/running window fell between two jobs refreshes,
// paneReviewSeenNonTerminal was never set -- the completion-timestamp
// comparison (reviewJobCompletionChanged) is the fallback that still marks
// the loaded review stale.
func TestSelectedReviewLoadedDetectsMissedRerunCompletion(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withReview(splitTestReview())) // job 2 (done) selected
	assert.True(m.selectedReviewLoaded())

	later := splitTestFinishedAt.Add(time.Minute)
	m.jobs[1].FinishedAt = &later
	assert.False(m.selectedReviewLoaded(),
		"a completion newer than the loaded review's snapshot means a rerun landed")
}

// TestSplitExternalRerunClosesFixPanel: a fix panel bound to a job observed
// back in queued/running is bound to an attempt being replaced -- and a
// FOCUSED panel keeps capturing every keystroke (handleKeyMsg's panel
// capture runs before any staleness guard) even though renderDetailPane now
// shows a status card. splitReconcileDetail must close it, mirroring
// handleRerunResultMsg's close on the local-rerun path.
func TestSplitExternalRerunClosesFixPanel(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withReview(splitTestReview())) // job 2 (done) selected
	m.reviewFixPanelOpen = true
	m.reviewFixPanelFocused = true
	m.fixPromptJobID = 2
	m.fixPromptText = "half-typed instructions"

	m.jobs[1].Status = storage.JobStatusQueued
	got, _ := m.splitReconcileDetail()
	assert.False(got.reviewFixPanelOpen)
	assert.False(got.reviewFixPanelFocused)
	assert.Equal(int64(0), got.fixPromptJobID)
	assert.Empty(got.fixPromptText)

	// A PENDING panel intent for the reran job is abandoned the same way.
	m = splitModel(withReview(splitTestReview()))
	m.reviewFixPanelPending = true
	m.fixPromptJobID = 2
	m.jobs[1].Status = storage.JobStatusRunning
	got, _ = m.splitReconcileDetail()
	assert.False(got.reviewFixPanelPending)
	assert.Equal(int64(0), got.fixPromptJobID)

	// A panel bound to a DIFFERENT job is left alone.
	m = splitModel(withReview(splitTestReview()))
	m.reviewFixPanelOpen = true
	m.fixPromptJobID = 1
	m.jobs[1].Status = storage.JobStatusQueued
	got, _ = m.splitReconcileDetail()
	assert.True(got.reviewFixPanelOpen)
	assert.Equal(int64(1), got.fixPromptJobID)
}

// TestSplitSameRowClickUnfocusesFixPanel: a list-pane click on the
// already-selected row moves currentView to viewQueue but takes
// followSelectionChange's unchanged-selection no-op, so nothing closes or
// unfocuses the panel -- leaving it rendered as an active input box while
// every keystroke lands on the queue keymap ('q' quits, 'a' closes the
// review). normalizeSplitState's focus invariant (repair 3) must clear the
// focus; the panel itself stays open with its typed text preserved.
func TestSplitSameRowClickUnfocusesFixPanel(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withReview(splitTestReview())) // job 2 selected, review loaded
	m.currentView = viewReview
	m.focus = focusDetail
	m.reviewFixPanelOpen = true
	m.reviewFixPanelFocused = true
	m.fixPromptJobID = 2
	m.fixPromptText = "half-typed"

	firstDataY := splitFirstDataRowY(t, m, "bbbb222") // job 2's own row
	res, _ := m.handleSplitMouse(mouseClickAt(5, firstDataY))
	got := res.(model).normalizeSplitState() // Update applies this before rendering

	assert.Equal(viewQueue, got.currentView)
	assert.False(got.reviewFixPanelFocused,
		"a focused panel must not survive the move off viewReview")
	assert.True(got.reviewFixPanelOpen, "the panel and its text are preserved")
	assert.Equal("half-typed", got.fixPromptText)
}

// TestPendingFixPanelConsumedUnderTransientViewIsNotLeftFocused: when
// acceptReview consumes the pending queue-'F' panel while a transient view
// is open, openReviewViewFrom declines the switch -- the panel must not be
// left FOCUSED (handleKeyMsg would never route keys to it outside
// viewReview, misrouting keystrokes typed into the apparently-active input
// to whatever view is current). normalizeSplitState's focus invariant
// clears it; the open panel itself survives for tab to re-focus later.
func TestPendingFixPanelConsumedUnderTransientViewIsNotLeftFocused(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := initTestModel(
		withCurrentView(viewQueue),
		withDimensions(150, 40),
		withTestJobs(testQueueJobs()...),
		withSelection(1, 2), // job 2: done
	)
	m.tasksEnabled = true

	res, cmd := m.handleFixKey()
	m = res.(model)
	require.NotNil(cmd)
	require.True(m.reviewFixPanelPending)

	res, _ = m.handleCommentOpenKey()
	m = res.(model)
	require.Equal(viewKindComment, m.currentView)

	res, _ = m.handleReviewMsg(reviewMsg{review: splitTestReview(), jobID: 2, fetchSeq: m.reviewFetchSeq})
	got := res.(model).normalizeSplitState() // Update applies this before rendering

	assert.Equal(viewKindComment, got.currentView, "still not yanked out of the comment editor")
	assert.True(got.reviewFixPanelOpen)
	assert.False(got.reviewFixPanelFocused,
		"a panel the keyboard cannot reach must not render as focused")
}

// TestFixPanelPaneLinesLongInputKeepsHelpLine: lipgloss v2's Width is
// border-box, so the input caps must keep the box content within boxW-2 or
// the box wraps past its 3-line budget and the 5-line pane cap silently
// drops the help line.
func TestFixPanelPaneLinesLongInputKeepsHelpLine(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withReview(splitTestReview()))
	m.reviewFixPanelOpen = true
	m.fixPromptText = strings.Repeat("x", 300)

	m.reviewFixPanelFocused = true
	lines := m.renderReviewFixPanelPaneLines(96)
	assert.Len(lines, reviewFixPanelPaneReserve)
	assert.Contains(lines[len(lines)-1], "esc: cancel",
		"help line must survive a long input")

	m.reviewFixPanelFocused = false
	lines = m.renderReviewFixPanelPaneLines(96)
	assert.Len(lines, reviewFixPanelPaneReserve)
	assert.Contains(lines[len(lines)-1], "tab: focus fix panel",
		"help line must survive a long input")
}

// TestResizeRefillsDuringPaneTailRestart: the resize handler's pane-tail
// restart previously early-returned before the "terminal grew, refill the
// job list" check, leaving the list pane underfilled until the next SSE
// event or fallback poll. The refill must be batched with the tail restart.
func TestResizeRefillsDuringPaneTailRestart(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withSelection(0, 3)) // job 3: running
	m.paneLogJobID, m.paneLogStreaming = 3, true
	m.hasMore = true
	m.loadingJobs = false

	res, cmd := m.handleWindowSizeMsg(tea.WindowSizeMsg{Width: 180, Height: 60})
	got := res.(model)
	assert.True(got.loadingJobs,
		"a resize that grows the pane past the loaded rows must refill even while restarting the tail")
	assert.NotNil(cmd)
	assert.True(got.paneLogStreaming, "the tail restart itself still happens")
}

// TestSplitEngageKeepsTasksOriginReviewScroll: a tasks-origin fix-job review
// renders full-screen even when split engages (splitActive excludes it) and
// its job never resolves in m.jobs -- maybeBootstrapDetail must not
// schedule a follow for the unrendered pane, since scheduleDetailFollow
// zeroes the reviewScroll of the review the user is reading.
func TestSplitEngageKeepsTasksOriginReviewScroll(t *testing.T) {
	assert := assert.New(t)
	m := splitModel()
	m.currentView = viewReview
	m.reviewFromView = viewTasks
	m.currentReview = &storage.Review{JobID: 99, Output: "fix job review"}
	m.selectedJobID = 99 // fix job: absent from m.jobs/panelMembers
	m.reviewScroll = 7

	got, cmd := m.maybeBootstrapDetail()
	assert.Nil(cmd)
	assert.Equal(7, got.reviewScroll, "the displayed review's scroll must survive split engaging")
}

// TestPanelMembersMsgReconcilesSelectedMemberDetail: panel members are
// absent from the main jobs response, so the members side-fetch is the only
// carrier of a selected member's status transition -- handlePanelMembersMsg
// must reconcile the detail pane itself or the pane stalls (no review
// fetch, no log tail) until an unrelated SSE event or the fallback poll.
func TestPanelMembersMsgReconcilesSelectedMemberDetail(t *testing.T) {
	assert := assert.New(t)
	finishedAt := splitTestFinishedAt
	queuedMember := storage.ReviewJob{
		ID: 42, PanelRunUUID: testUUIDPtr("u1"), PanelRole: storage.PanelRoleMember,
		Status: storage.JobStatusQueued,
	}
	doneMember := queuedMember
	doneMember.Status = storage.JobStatusDone
	doneMember.FinishedAt = &finishedAt

	m := splitModel()
	m.panelMembers[testUUID("u1")] = []storage.ReviewJob{queuedMember}
	m.selectedJobID = 42
	m.selectedIdx = -1

	res, cmd := m.handlePanelMembersMsg(panelMembersMsg{
		runUUID: testUUID("u1"), members: []storage.ReviewJob{doneMember},
	})
	got := res.(model)
	assert.NotNil(cmd, "the member's queued->done transition must dispatch its review fetch now")
	assert.Equal(int64(42), got.reconcileFetchJobID)
}

// TestSplitReconcileSyncsExternallyToggledClosed: Closed changes
// independently of completion (another TUI or the CLI), arriving as
// refreshed m.jobs state with no new FinishedAt -- the reconcile fast path
// must sync it into the loaded review, or the [CLOSED] badge goes stale and
// the next local toggle submits the already-current state.
func TestSplitReconcileSyncsExternallyToggledClosed(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withReview(splitTestReview())) // job 2 done, review loaded, Closed=false
	closed := true
	m.jobs[1].Closed = &closed

	got, cmd := m.splitReconcileDetail()
	assert.Nil(cmd, "a closed-state change alone must not refetch")
	assert.True(got.currentReview.Closed)

	// And back: an external unclose syncs too.
	unclosed := false
	got.jobs[1].Closed = &unclosed
	got, cmd = got.splitReconcileDetail()
	assert.Nil(cmd)
	assert.False(got.currentReview.Closed)
}

// TestFollowFailureRetriesPendingReviewOpen covers the roborev repro
// "ordinary request -> newer follow failure -> ordinary success": the
// follow failure must not clear the pending-open intent while its
// originating dispatch can still be served -- it retries once, and the
// retry's success serves the explicit open.
func TestFollowFailureRetriesPendingReviewOpen(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel() // job 2 (done) selected
	m.currentView = viewTasks

	cmd := m.dispatchReviewFetch(2) // the explicit request (tasks 'P')
	require.NotNil(cmd)
	seqOrdinary := m.reviewFetchSeq
	require.Equal(int64(2), m.pendingReviewOpenJobID)

	_ = m.dispatchReviewFollow(2) // a newer reconcile follow supersedes it

	// The follow FAILS: the intent must survive, with one retry dispatched.
	res, retryCmd := m.handleReviewFollowErrMsg(reviewFollowErrMsg{
		jobID: 2, err: errors.New("boom"),
		gen: m.detailFollowGen, fetchSeq: m.reviewFetchSeq,
	})
	m = res.(model)
	assert.NotNil(retryCmd, "first follow failure with an armed open intent must retry")
	require.Equal(int64(2), m.pendingReviewOpenJobID,
		"the intent must survive the first follow failure")
	assert.Equal(m.reviewFetchSeq, m.pendingReviewOpenSeq,
		"identity re-stamped at the retry's own dispatch")

	// The ORIGINAL ordinary success lands, superseded: dropped, but the
	// intent stays armed for the retry to serve.
	res, _ = m.handleReviewMsg(reviewMsg{
		review: splitTestReview(), jobID: 2,
		gen: m.detailFollowGen, fetchSeq: seqOrdinary, dispatchedFrom: viewTasks,
	})
	m = res.(model)
	require.Equal(int64(2), m.pendingReviewOpenJobID)

	// The retry's success serves the explicit open.
	res, _ = m.handleReviewMsg(reviewMsg{
		review: splitTestReview(), jobID: 2, follow: true,
		gen: m.detailFollowGen, fetchSeq: m.reviewFetchSeq,
	})
	m = res.(model)
	assert.Equal(viewReview, m.currentView, "the user's explicit open must be served")
	assert.Equal(int64(0), m.pendingReviewOpenJobID)
	require.NotNil(m.currentReview)
}

// TestSecondFollowFailureClearsPendingReviewOpen: the retry is bounded --
// a second follow failure clears the intent with a warning flash targeted
// at the origin view.
func TestSecondFollowFailureClearsPendingReviewOpen(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel()
	m.currentView = viewTasks

	require.NotNil(m.dispatchReviewFetch(2))
	_ = m.dispatchReviewFollow(2)

	res, retryCmd := m.handleReviewFollowErrMsg(reviewFollowErrMsg{
		jobID: 2, err: errors.New("boom"),
		gen: m.detailFollowGen, fetchSeq: m.reviewFetchSeq,
	})
	m = res.(model)
	require.NotNil(retryCmd)

	res, cmd := m.handleReviewFollowErrMsg(reviewFollowErrMsg{
		jobID: 2, err: errors.New("boom again"),
		gen: m.detailFollowGen, fetchSeq: m.reviewFetchSeq,
	})
	m = res.(model)
	assert.Nil(cmd)
	assert.Equal(int64(0), m.pendingReviewOpenJobID, "second failure is terminal")
	assertFlashMessage(t, m, viewTasks, "Could not open review for job #2: boom again")
}

// splitTestFinishedAt is the completion time shared by testQueueJobs' done
// job and splitTestReview's embedded Job snapshot. Production done/failed
// jobs always carry FinishedAt (the daemon stamps it on every completion),
// and selectedReviewLoaded's attempt-freshness check treats a nil as stale
// -- so fixtures must model it, and the two copies must MATCH for the
// loaded review to count as current (a job timestamp strictly after the
// review's snapshot means a rerun completed since the review was fetched).
var splitTestFinishedAt = time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)

func splitTestReview() *storage.Review {
	verdictP := "P"
	finishedAt := splitTestFinishedAt
	return &storage.Review{
		ID: 10, JobID: 2, Agent: "codex",
		Output: "## Findings\n\n1. first finding\n2. second finding\n",
		Job: &storage.ReviewJob{
			ID: 2, GitRef: "bbbb222", RepoName: "repoA",
			Agent: "codex", Verdict: &verdictP, FinishedAt: &finishedAt,
		},
	}
}

func testQueueJobs() []storage.ReviewJob {
	verdictP := "P"
	finishedAt := splitTestFinishedAt
	return []storage.ReviewJob{
		{
			ID: 3, GitRef: "cccc333", Branch: "main", RepoName: "repoA",
			Agent: "codex", Status: storage.JobStatusRunning,
		},
		{
			ID: 2, GitRef: "bbbb222", Branch: "feat/x", RepoName: "repoA",
			Agent: "codex", Status: storage.JobStatusDone, Verdict: &verdictP,
			FinishedAt: &finishedAt,
		},
		{
			ID: 1, GitRef: "aaaa111", Branch: "main", RepoName: "repoB",
			Agent: "claude-code", Status: storage.JobStatusFailed, Error: "boom",
			FinishedAt: &finishedAt,
		},
	}
}

func TestRenderQueueTableAtNarrowWidth(t *testing.T) {
	m := initTestModel(
		withCurrentView(viewQueue),
		withDimensions(150, 40),
		withTestJobs(testQueueJobs()...),
		withSelection(1, 2),
	)
	rows := m.visibleQueueRows()
	require.NotEmpty(t, rows)
	visCols := m.visibleColumns()
	cw := m.queueContentWidths(rows, visCols, false, false)

	lines := m.renderQueueTable(rows, 48, 10, visCols, cw)
	require.NotEmpty(t, lines)
	joined := strings.Join(lines, "\n")
	assert.Contains(t, joined, "JobID")
	assert.NotContains(t, joined, "\x1b[K")
}

func TestQueuePaneColumnsNarrowing(t *testing.T) {
	assert := assert.New(t)
	m := initTestModel(
		withCurrentView(viewQueue),
		withDimensions(150, 40),
		withTestJobs(testQueueJobs()...),
		withSelection(0, 3),
	)
	rows := m.visibleQueueRows()
	cw := m.queueContentWidths(rows, m.visibleColumns(), false, false)

	// Wide pane: everything visible fits.
	wide := m.queuePaneColumns(200, cw)
	assert.ElementsMatch(m.visibleColumns(), wide)

	// Narrow pane: columns dropped from the end of columnOrder,
	// core columns always survive.
	narrow := m.queuePaneColumns(30, cw)
	assert.Contains(narrow, colSel)
	assert.Contains(narrow, colJobID)
	assert.Contains(narrow, colRef)
	assert.Less(len(narrow), len(wide))

	// A user-hidden column stays hidden even when it would fit.
	m.hiddenColumns = map[int]bool{colAgent: true}
	cols := m.queuePaneColumns(200, cw)
	assert.NotContains(cols, colAgent)
}

// TestQueuePaneColumnsDropOrder pins down the drop ORDER explicitly: when a
// pane width forces exactly one column to drop, it must be the LAST entry of
// m.columnOrder among the currently-visible columns, not the first. Widths
// are supplied directly (not derived from real rows) so the exact boundary
// between "everything fits" and "one drop" is deterministic.
func TestQueuePaneColumnsDropOrder(t *testing.T) {
	assert := assert.New(t)
	m := initTestModel(withCurrentView(viewQueue))
	m.columnOrder = []int{colRef, colBranch, colRepo, colAgent}
	m.hiddenColumns = map[int]bool{}

	cw := map[int]int{
		colJobID:  5,
		colRef:    6,
		colBranch: 6,
		colRepo:   6,
		colAgent:  6,
	}

	wide := m.queuePaneColumns(36, cw)
	assert.ElementsMatch([]int{colSel, colJobID, colRef, colBranch, colRepo, colAgent}, wide)

	// One char narrower than the exact fit forces exactly one drop.
	narrow := m.queuePaneColumns(35, cw)
	assert.Len(narrow, len(wide)-1)

	// The dropped column must be colAgent (last in columnOrder), not an
	// earlier-configured one — a front-first-dropping regression would drop
	// colRef/colBranch/colRepo instead and fail these assertions.
	assert.NotContains(narrow, colAgent)
	assert.Contains(narrow, colRef)
	assert.Contains(narrow, colBranch)
	assert.Contains(narrow, colRepo)
}

func TestRenderQueuePaneBody(t *testing.T) {
	assert := assert.New(t)
	m := initTestModel(
		withCurrentView(viewQueue),
		withDimensions(150, 40),
		withTestJobs(testQueueJobs()...),
		withSelection(1, 2),
	)
	lines := m.renderQueuePaneBody(48, 20)
	assert.Len(lines, 20)
	joined := strings.Join(lines, "\n")
	assert.Contains(joined, "2") // selected job id visible
	assert.NotContains(joined, "\x1b[K")

	// Empty queue renders a placeholder, still exactly innerH lines.
	m.jobs = nil
	lines = m.renderQueuePaneBody(48, 20)
	assert.Len(lines, 20)
}

func TestRenderReviewPaneBody(t *testing.T) {
	assert := assert.New(t)
	m := initTestModel(
		withCurrentView(viewQueue),
		withDimensions(150, 40),
		withReview(splitTestReview()),
	)
	lines := m.renderReviewPaneBody(88, 25)
	assert.Len(lines, 25)
	joined := strings.Join(lines, "\n")
	assert.Contains(joined, "Review #2")
	assert.Contains(joined, "Review type: default")
	assert.Contains(joined, "Verdict: Pass")
	assert.Contains(joined, "first finding")
	assert.NotContains(joined, "\x1b[K")
}

func TestRenderReviewPaneBodyScrolls(t *testing.T) {
	rev := splitTestReview()
	// Content must be non-repeating: a periodic body (e.g. the same line
	// repeated) can make two different scroll offsets land on the same
	// phase and render byte-identical windows, which would make this test
	// pass or fail by accident rather than by actually exercising scroll.
	var sb strings.Builder
	for i := range 60 {
		fmt.Fprintf(&sb, "line %d of output\n\n", i)
	}
	rev.Output = sb.String()
	m := initTestModel(withDimensions(150, 40), withReview(rev))

	top := strings.Join(m.renderReviewPaneBody(88, 10), "\n")
	m.reviewScroll = 20
	scrolled := strings.Join(m.renderReviewPaneBody(88, 10), "\n")
	assert.NotEqual(t, top, scrolled)

	// Scroll far past the end: clamps, never panics.
	m.reviewScroll = 10000
	assert.Len(t, m.renderReviewPaneBody(88, 10), 10)
}

func TestWindowResizeSwitchesLayout(t *testing.T) {
	assert := assert.New(t)
	m := initTestModel(withCurrentView(viewQueue), withDimensions(80, 24),
		withTestJobs(testQueueJobs()...), withSelection(1, 2))

	res, _ := m.handleWindowSizeMsg(tea.WindowSizeMsg{Width: 150, Height: 40})
	m = res.(model)
	assert.Equal(layoutSplit, m.layout)
	assert.Equal(focusList, m.focus)

	res, _ = m.handleWindowSizeMsg(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = res.(model)
	assert.Equal(layoutStacked, m.layout)
	assert.Equal(viewQueue, m.currentView)
}

// TestWindowResizeRestartsPaneLogAtNewWidth:
// an active pane log tail's persistent formatter keeps its old width across
// incremental polls, so a resize must restart the tail (bumped seq, reset
// offset/lines, rebuilt formatter, a fresh fetch) or the live log stays
// wrapped for the previous pane size.
func TestWindowResizeRestartsPaneLogAtNewWidth(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withSelection(0, 3)) // job 3: running
	m.paneLogJobID, m.paneLogSeq, m.paneLogStreaming = 3, 5, true
	m.paneLogLines = []logLine{{text: "stale-width line"}}
	m.paneLogOffset = 42

	res, cmd := m.handleWindowSizeMsg(tea.WindowSizeMsg{Width: 180, Height: 45})
	got := res.(model)
	assert.Equal(layoutSplit, got.layout)
	assert.Greater(got.paneLogSeq, uint64(5))
	assert.Equal(int64(0), got.paneLogOffset)
	assert.Empty(got.paneLogLines)
	assert.NotNil(got.paneLogFmtr)
	assert.NotNil(cmd)
}

// TestWindowResizeLeavesPaneLogAloneInStacked covers the other half of
// Finding A: pane log state is only meaningful in split (it's not rendered
// in stacked), so a resize landing in/staying in stacked layout must not
// touch it.
func TestWindowResizeLeavesPaneLogAloneInStacked(t *testing.T) {
	assert := assert.New(t)
	m := initTestModel(
		withCurrentView(viewQueue),
		withDimensions(80, 24), // below the split breakpoint
		withTestJobs(testQueueJobs()...),
		withSelection(0, 3),
	)
	m.layout = layoutStacked
	m.preferredLayout = layoutStacked
	m.paneLogJobID, m.paneLogSeq, m.paneLogStreaming = 3, 5, true
	m.paneLogLines = []logLine{{text: "line"}}

	res, _ := m.handleWindowSizeMsg(tea.WindowSizeMsg{Width: 100, Height: 30})
	got := res.(model)
	assert.Equal(layoutStacked, got.layout)
	assert.Equal(uint64(5), got.paneLogSeq)
	assert.Equal([]logLine{{text: "line"}}, got.paneLogLines)
}

// TestWindowResizeRunsLogViewBehindActivePaneLog:
// a transient full-screen log view (viewLog) open on top of
// split layout must still get its own resize re-render even while a
// background pane log tail is streaming for a different, still-running
// job. The pane restart branch must not steal the early return meant for
// viewLog's re-render below it (splitActive() is false here since
// currentView is viewLog, not queue/review); it should instead just
// invalidate the stale-width background tail (bump seq, stop streaming)
// without touching the buffered lines or issuing its own fetch.
func TestWindowResizeRunsLogViewBehindActivePaneLog(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withSelection(0, 3)) // job 3: running
	m.paneLogJobID, m.paneLogSeq, m.paneLogStreaming = 3, 5, true
	m.paneLogLines = []logLine{{text: "pane tail line"}}

	m.currentView = viewLog
	m.logJobID = 1
	m.logLines = []logLine{{text: "full log line"}}
	m.logFetchSeq = 2

	res, cmd := m.handleWindowSizeMsg(tea.WindowSizeMsg{Width: 180, Height: 45})
	got := res.(model)

	// viewLog's own resize re-render ran (existing behavior).
	assert.Greater(got.logFetchSeq, uint64(2))
	assert.Nil(got.logLines)
	assert.True(got.logLoading)
	assert.NotNil(cmd)

	// The pane restart branch invalidated the background tail instead of
	// firing its own fetch or clobbering the buffered lines.
	assert.Equal(uint64(6), got.paneLogSeq)
	assert.False(got.paneLogStreaming)
	assert.Equal([]logLine{{text: "pane tail line"}}, got.paneLogLines)
}

func TestToggleLayoutKey(t *testing.T) {
	assert := assert.New(t)
	m := splitModel()
	m.width, m.height = 150, 40

	res, _ := m.handleToggleLayoutKey()
	m = res.(model)
	assert.Equal(layoutStacked, m.layout)
	assert.True(m.layoutLocked)

	res, _ = m.handleToggleLayoutKey()
	m = res.(model)
	assert.Equal(layoutSplit, m.layout)

	// Too small: stays stacked, flashes.
	m.layout = layoutStacked
	m.preferredLayout = layoutStacked
	m.width, m.height = 100, 30
	res, _ = m.handleToggleLayoutKey()
	m = res.(model)
	assert.Equal(layoutStacked, m.layout)
	assert.NotEmpty(m.flashMessage)
}

// TestBootstrapDetailPreservesScrollOnMatchingReview covers entering split
// (via L) when a full-screen review is already open and matches the current
// selection: nothing needs fetching, so maybeBootstrapDetail must not
// schedule a follow tick or reset reviewScroll.
func TestBootstrapDetailPreservesScrollOnMatchingReview(t *testing.T) {
	assert := assert.New(t)
	m := initTestModel(
		withCurrentView(viewReview),
		withDimensions(150, 40),
		withTestJobs(testQueueJobs()...),
		withSelection(1, 2),
		withReview(splitTestReview()), // JobID 2, matches selection
	)
	m.layout = layoutStacked
	m.preferredLayout = layoutStacked
	m.reviewScroll = 15
	prevGen := m.detailFollowGen

	res, cmd := m.handleToggleLayoutKey()
	got := res.(model)
	assert.Equal(layoutSplit, got.layout)
	assert.Equal(15, got.reviewScroll)
	assert.Equal(prevGen, got.detailFollowGen)
	assert.Nil(cmd)
}

// TestBootstrapDetailSchedulesWhenNoMatchingReview covers the still-needed
// bootstrap path: no review loaded for the current selection, so entering
// split must schedule a follow tick as before.
func TestBootstrapDetailSchedulesWhenNoMatchingReview(t *testing.T) {
	assert := assert.New(t)
	m := initTestModel(
		withCurrentView(viewQueue),
		withDimensions(150, 40),
		withTestJobs(testQueueJobs()...),
		withSelection(1, 2),
	)
	m.layout = layoutStacked
	m.preferredLayout = layoutStacked
	prevGen := m.detailFollowGen

	res, cmd := m.handleToggleLayoutKey()
	got := res.(model)
	assert.Equal(layoutSplit, got.layout)
	assert.Greater(got.detailFollowGen, prevGen)
	assert.NotNil(cmd)
}

func TestSplitFocusKeys(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withReview(splitTestReview()))

	// tab: list -> detail (only when a review is loaded).
	res, _ := m.handleTabKey()
	m = res.(model)
	assert.Equal(focusDetail, m.focus)
	assert.Equal(viewReview, m.currentView)

	// esc: detail -> list, review kept for the pane.
	res, _ = m.handleEscKey()
	m = res.(model)
	assert.Equal(focusList, m.focus)
	assert.Equal(viewQueue, m.currentView)
	assert.NotNil(m.currentReview)

	// q in detail focus: back to list, not quit.
	m.focus = focusDetail
	m.currentView = viewReview
	res, cmd := m.handleQuitKey()
	m = res.(model)
	assert.Equal(focusList, m.focus)
	assert.Nil(cmd)

	// tab with no review loaded: no-op.
	m.currentReview = nil
	m.focus = focusList
	m.currentView = viewQueue
	res, _ = m.handleTabKey()
	m = res.(model)
	assert.Equal(focusList, m.focus)
}

func TestReviewMsgFocusesDetailInSplit(t *testing.T) {
	m := splitModel()
	res, _ := m.handleReviewMsg(reviewMsg{review: splitTestReview(), jobID: 2})
	got := res.(model)
	assert.Equal(t, focusDetail, got.focus)
	assert.Equal(t, viewReview, got.currentView)
}

func TestFollowScheduledOnCursorMove(t *testing.T) {
	assert := assert.New(t)
	m := splitModel() // selection on job 2
	prevGen := m.detailFollowGen

	res, cmd := m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyDown})
	got := res.(model)
	assert.NotEqual(int64(2), got.selectedJobID) // cursor moved
	assert.Greater(got.detailFollowGen, prevGen)
	assert.NotNil(cmd)

	// A second immediate move bumps the gen again — the first tick is stale.
	gen1 := got.detailFollowGen
	res, _ = got.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyUp})
	got = res.(model)
	assert.Greater(got.detailFollowGen, gen1)
}

func TestFollowTickFetchesForDoneJob(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withSelection(1, 2)) // job 2: done
	m.detailFollowGen = 7

	// Stale tick: dropped.
	_, cmd := m.handleDetailFollowTick(detailFollowTickMsg{gen: 6})
	assert.Nil(cmd)

	// Current tick: fetch issued.
	_, cmd = m.handleDetailFollowTick(detailFollowTickMsg{gen: 7})
	assert.NotNil(cmd)

	// Review already loaded for this job: no refetch.
	m.currentReview = splitTestReview() // JobID 2
	_, cmd = m.handleDetailFollowTick(detailFollowTickMsg{gen: 7})
	assert.Nil(cmd)
}

func TestFollowTickSynthesizesFailedReview(t *testing.T) {
	m := splitModel(withSelection(2, 1)) // job 1: failed
	m.detailFollowGen = 1
	res, _ := m.handleDetailFollowTick(detailFollowTickMsg{gen: 1})
	got := res.(model)
	require.NotNil(t, got.currentReview)
	assert.Equal(t, int64(1), got.currentReview.JobID)
	assert.Contains(t, got.currentReview.Output, "boom")
}

func TestFailedReviewDoesNotChangeColourAfterDetailFollow(t *testing.T) {
	m := splitModel(withSelection(2, 1)) // job 1: failed
	m.currentReview = nil

	before := strings.Join(m.renderDetailPane(88, 25), "\n")
	assert.Contains(t, before, "boom")
	assert.NotContains(t, before, failStyle.Render("boom"))

	m.detailFollowGen = 1
	res, _ := m.handleDetailFollowTick(detailFollowTickMsg{gen: 1})
	after := strings.Join(res.(model).renderDetailPane(88, 25), "\n")
	assert.Contains(t, after, "boom")
	assert.NotContains(t, after, failStyle.Render("boom"))
}

func TestFollowReviewMsgKeepsListFocus(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withSelection(1, 2))
	res, _ := m.handleReviewMsg(reviewMsg{review: splitTestReview(), jobID: 2, follow: true})
	got := res.(model)
	assert.Equal(focusList, got.focus)
	assert.Equal(viewQueue, got.currentView)
	assert.NotNil(got.currentReview)
}

// TestResizeAcrossBreakpointPreservesCommentEditor covers a resize that
// crosses the split breakpoint while the comment editor is open: applyLayout
// must update m.layout but leave the transient view and its in-progress
// text alone.
func TestResizeAcrossBreakpointPreservesCommentEditor(t *testing.T) {
	assert := assert.New(t)
	m := splitModel()
	m.width, m.height = 150, 40
	m.currentView = viewKindComment
	m.commentText = "in progress comment"

	res, _ := m.handleWindowSizeMsg(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = res.(model)
	assert.Equal(viewKindComment, m.currentView)
	assert.Equal("in progress comment", m.commentText)
	assert.Equal(layoutStacked, m.layout)
}

// TestResizeAcrossBreakpointPreservesLogView is the same as
// TestResizeAcrossBreakpointPreservesCommentEditor but for the log view.
func TestResizeAcrossBreakpointPreservesLogView(t *testing.T) {
	assert := assert.New(t)
	m := splitModel()
	m.width, m.height = 150, 40
	m.currentView = viewLog
	m.logLines = nil

	res, _ := m.handleWindowSizeMsg(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = res.(model)
	assert.Equal(viewLog, m.currentView)
	assert.Equal(layoutStacked, m.layout)
}

// TestTransientViewExitReconcilesSplitFocus covers a transient view (help)
// that outlives a resize crossing the breakpoint twice (so the layout
// changes underneath it while it's open), then exits via esc. Update()'s
// normalizeSplitState step must reconcile focus against the resulting view
// with no panic, for both the queue and the review destinations.
func TestTransientViewExitReconcilesSplitFocus(t *testing.T) {
	assert := assert.New(t)

	// Exit into viewQueue: focus reconciles to focusList.
	m := splitModel()
	m.width, m.height = 150, 40
	m.helpFromView = viewQueue
	m.currentView = viewHelp

	res, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = res.(model)
	assert.Equal(viewHelp, m.currentView)
	assert.Equal(layoutStacked, m.layout)

	res, _ = m.Update(tea.WindowSizeMsg{Width: 150, Height: 40})
	m = res.(model)
	assert.Equal(viewHelp, m.currentView)
	assert.Equal(layoutSplit, m.layout)

	m, cmd := pressSpecial(m, tea.KeyEscape)
	assert.Nil(cmd)
	assert.Equal(viewQueue, m.currentView)
	assert.Equal(focusList, m.focus)

	// Exit into viewReview with a review loaded: focus reconciles to
	// focusDetail.
	m2 := splitModel(withReview(splitTestReview()))
	m2.helpFromView = viewReview
	m2.currentView = viewHelp
	m2.focus = focusList

	m2, _ = pressSpecial(m2, tea.KeyEscape)
	assert.Equal(viewReview, m2.currentView)
	assert.Equal(focusDetail, m2.focus)
}

// TestStartPaneLogInitializesState covers the running-job branch of
// handleDetailFollowTick: startPaneLog resets the pane's log state
// and kicks off the first fetch.
func TestStartPaneLogInitializesState(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withSelection(0, 3)) // job 3: running
	job, _ := m.selectedJob()
	res, cmd := m.startPaneLog(*job)
	got := res.(model)
	assert.Equal(int64(3), got.paneLogJobID)
	assert.True(got.paneLogStreaming)
	assert.NotNil(cmd)
}

// TestPaneLogWidthUsesDetailPaneNotTerminalWidth pins startPaneLog's and
// fetchPaneLog's shared width source (paneLogWidth) to the split detail
// pane's inner width, not the full terminal width -- a regression here
// wraps log text for the terminal and then hard-truncates it to the pane.
func TestPaneLogWidthUsesDetailPaneNotTerminalWidth(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withSelection(0, 3))
	footerRows := m.splitFooterRows()
	g := splitGeometry(m.width, m.height, len(reflowHelpRows(footerRows, m.width)))
	assert.Equal(g.detailInnerW, m.paneLogWidth())
	assert.NotEqual(m.width, m.paneLogWidth())
}

// TestPaneLogOutputAppendsAndSchedulesTick covers the streaming path: a
// stale seq is dropped, a live seq appends lines and schedules the next
// poll, and the appended text shows up in the rendered detail pane.
func TestPaneLogOutputAppendsAndSchedulesTick(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withSelection(0, 3))
	m.paneLogJobID, m.paneLogSeq, m.paneLogStreaming = 3, 5, true

	// Stale seq: dropped.
	res, cmd := m.handlePaneLogOutputMsg(paneLogOutputMsg{jobID: 3, seq: 4, lines: []logLine{{text: "old"}}})
	assert.Empty(res.(model).paneLogLines)
	assert.Nil(cmd)

	// Live seq: appended, tick scheduled while running.
	res, cmd = m.handlePaneLogOutputMsg(paneLogOutputMsg{
		jobID: 3, seq: 5, hasMore: true, append: true, lines: []logLine{{text: "Analyzing diff..."}},
	})
	got := res.(model)
	assert.Len(got.paneLogLines, 1)
	assert.NotNil(cmd)

	// Render shows the tail.
	out := strings.Join(got.renderDetailPane(88, 25), "\n")
	assert.Contains(out, "Analyzing diff...")
}

// TestPaneLogOutputReplacesOnNonIncrementalFetch covers a server-side log
// offset reset (truncation/rotation): fetchPaneLog re-fetches the complete
// log from offset 0 and sets append=false, so the pane must replace its
// buffered lines rather than mix stale pre-reset lines in with the
// replacement log. append=true still appends, and
// the buffer still trims to the 500-line cap.
func TestPaneLogOutputReplacesOnNonIncrementalFetch(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withSelection(0, 3))
	m.paneLogJobID, m.paneLogSeq, m.paneLogStreaming = 3, 5, true
	m.paneLogLines = []logLine{{text: "stale line 1"}, {text: "stale line 2"}}

	// append=false: replaces, stale lines gone.
	res, _ := m.handlePaneLogOutputMsg(paneLogOutputMsg{
		jobID: 3, seq: 5, hasMore: true, append: false, lines: []logLine{{text: "fresh line"}},
	})
	got := res.(model)
	assert.Equal([]logLine{{text: "fresh line"}}, got.paneLogLines)

	// append=true: appends onto the (now fresh-only) buffer.
	res, _ = got.handlePaneLogOutputMsg(paneLogOutputMsg{
		jobID: 3, seq: 5, hasMore: true, append: true, lines: []logLine{{text: "more"}},
	})
	got = res.(model)
	assert.Equal([]logLine{{text: "fresh line"}, {text: "more"}}, got.paneLogLines)

	// append=true past the cap: trims from the front.
	big := make([]logLine, paneLogMaxLines+10)
	for i := range big {
		big[i] = logLine{text: fmt.Sprintf("line %d", i)}
	}
	res, _ = got.handlePaneLogOutputMsg(paneLogOutputMsg{jobID: 3, seq: 5, hasMore: true, append: true, lines: big})
	got = res.(model)
	assert.Len(got.paneLogLines, paneLogMaxLines)
	assert.Equal(fmt.Sprintf("line %d", len(big)-1), got.paneLogLines[len(got.paneLogLines)-1].text)
}

// TestPaneLogCompletionTriggersReviewFetch covers the running->done swap:
// once the job stops streaming, the pane reconciles via a review fetch.
func TestPaneLogCompletionTriggersReviewFetch(t *testing.T) {
	m := splitModel(withSelection(0, 3))
	m.paneLogJobID, m.paneLogSeq, m.paneLogStreaming = 3, 5, true
	// hasMore=false: job stopped streaming -- reconcile via a review fetch.
	res, cmd := m.handlePaneLogOutputMsg(paneLogOutputMsg{jobID: 3, seq: 5, hasMore: false})
	assert.False(t, res.(model).paneLogStreaming)
	assert.NotNil(t, cmd)
}

// TestPaneLogTickRefetchesWhileRunning covers handlePaneLogTickMsg: a
// current-seq tick for the still-running, still-selected job re-issues the
// fetch; a stale seq or a job that's moved on is a silent no-op.
func TestPaneLogTickRefetchesWhileRunning(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withSelection(0, 3))
	m.paneLogJobID, m.paneLogSeq, m.paneLogStreaming = 3, 5, true

	// Stale seq: no-op.
	_, cmd := m.handlePaneLogTickMsg(paneLogTickMsg{seq: 4})
	assert.Nil(cmd)

	// Current seq, job still running and selected: refetch.
	_, cmd = m.handlePaneLogTickMsg(paneLogTickMsg{seq: 5})
	assert.NotNil(cmd)

	// Selection moved to a different job: no-op.
	m.paneLogJobID = 999
	_, cmd = m.handlePaneLogTickMsg(paneLogTickMsg{seq: 5})
	assert.Nil(cmd)
}

// TestSplitReconcileDetailOnJobsUpdate covers handleJobsMsg's running->done
// swap: when the highlighted job in split view finishes but the
// pane still shows something else (e.g. a stale log tail), the jobs refresh
// triggers a review fetch to swap the pane over.
func TestSplitReconcileDetailOnJobsUpdate(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withSelection(0, 3)) // job 3: running
	m.paneLogJobID, m.paneLogStreaming = 3, true

	doneJobs := testQueueJobs()
	doneJobs[0].Status = storage.JobStatusDone // job 3 finished
	res, cmd := m.handleJobsMsg(jobsMsg{jobs: doneJobs, stats: storage.JobStats{}})
	got := res.(model)
	assert.Equal(int64(3), got.selectedJobID)
	assert.NotNil(cmd)
}

// TestPaneLogTickClearsStreamingWhenJobStopsRunning covers
// handlePaneLogTickMsg's second guard: when the tailed job stops
// running out from under a poll tick (it flips to failed/canceled/done in
// m.jobs, e.g. via an SSE push landing between ticks), the tick must stop
// claiming an active tail rather than just silently dropping the fetch.
// Pre-fix, paneLogStreaming stayed true here, which made both
// splitReconcileDetail's running-branch restart guard and startPaneLog's
// already-tailing no-op guard believe the tail was still alive -- freezing
// the pane permanently once a rerun reused this job ID and returned it to
// running (see TestSplitReconcileDetailRestartsTailAfterRerunReusesJobID).
func TestPaneLogTickClearsStreamingWhenJobStopsRunning(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withSelection(0, 3)) // job 3: running, selected
	m.paneLogJobID, m.paneLogSeq, m.paneLogStreaming = 3, 5, true

	m.jobs[0].Status = storage.JobStatusFailed // job 3 stopped running

	res, cmd := m.handlePaneLogTickMsg(paneLogTickMsg{seq: 5})
	got := res.(model)
	assert.Nil(cmd)
	assert.False(got.paneLogStreaming)
	assert.Greater(got.paneLogSeq, uint64(5))
}

// TestPaneLogTickStaleSeqLeavesActiveTailAlone is the negative case for the
// fix above: a stale-seq tick (superseded by a later restart, e.g. via
// scheduleDetailFollow or startPaneLog) must not touch a live tail's state.
// The first guard alone handles staleness and returns before reaching the
// job-status check, so a live tail's paneLogStreaming/paneLogSeq must be
// untouched.
func TestPaneLogTickStaleSeqLeavesActiveTailAlone(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withSelection(0, 3)) // job 3: running, selected
	m.paneLogJobID, m.paneLogSeq, m.paneLogStreaming = 3, 5, true

	res, cmd := m.handlePaneLogTickMsg(paneLogTickMsg{seq: 4}) // stale
	got := res.(model)
	assert.Nil(cmd)
	assert.True(got.paneLogStreaming)
	assert.Equal(uint64(5), got.paneLogSeq)
}

// TestSplitReconcileDetailRestartsTailAfterRerunReusesJobID is the freeze
// repro for the handlePaneLogTickMsg fix above: a tailed job stops running
// (the tick fix clears paneLogStreaming), then a rerun reuses the same job
// ID and the job returns to running in a later jobs refresh.
// splitReconcileDetail's running-branch restart guard must see the cleared
// paneLogStreaming and restart the tail. Pre-fix, handlePaneLogTickMsg left
// paneLogStreaming true, so the guard (paneLogJobID == job.ID &&
// paneLogStreaming) stayed satisfied and the tail was never restarted,
// freezing the pane on stale output indefinitely.
func TestSplitReconcileDetailRestartsTailAfterRerunReusesJobID(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withSelection(0, 3)) // job 3: running, selected
	m.paneLogJobID, m.paneLogSeq, m.paneLogStreaming = 3, 5, true
	m.jobs[0].Status = storage.JobStatusFailed // job 3 stopped running

	res, _ := m.handlePaneLogTickMsg(paneLogTickMsg{seq: 5})
	m = res.(model)
	require.False(t, m.paneLogStreaming)
	seqAfterTick := m.paneLogSeq

	// Rerun reuses job ID 3, back to running.
	m.jobs[0].Status = storage.JobStatusRunning
	res2, cmd := m.handleJobsMsg(jobsMsg{jobs: m.jobs, stats: storage.JobStats{}})
	got := res2.(model)
	assert.NotNil(cmd)
	assert.True(got.paneLogStreaming)
	assert.Greater(got.paneLogSeq, seqAfterTick)
}

// TestSplitReconcileDetailSynthesizesFailedReviewAndStopsTail covers
// splitReconcileDetail's JobStatusFailed case: when
// the selected job transitions running->failed during a jobs refresh, the
// pane must get a synthesized failed review (so it renders through the
// scrollable review-body path instead of the card+wrapped-error fallback)
// and any active tail for that job must be stopped so it can't keep polling
// a job that no longer exists in running state.
func TestSplitReconcileDetailSynthesizesFailedReviewAndStopsTail(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withSelection(0, 3)) // job 3: running, selected
	m.paneLogJobID, m.paneLogSeq, m.paneLogStreaming = 3, 5, true

	failedJobs := testQueueJobs()
	failedJobs[0].Status = storage.JobStatusFailed
	failedJobs[0].Error = "agent crashed"

	res, _ := m.handleJobsMsg(jobsMsg{jobs: failedJobs, stats: storage.JobStats{}})
	got := res.(model)

	require.NotNil(t, got.currentReview)
	assert.Equal(int64(3), got.currentReview.JobID)
	assert.Contains(got.currentReview.Output, "agent crashed")
	assert.False(got.paneLogStreaming)
	assert.Greater(got.paneLogSeq, uint64(5))
}

// TestFailedReviewFromReconcileRendersThroughReviewPaneBody covers the
// render side of the same fix: the synthesized failed review must be picked
// up by renderDetailPane's currentReview-match branch and rendered via
// renderReviewPaneBody (scrollable, focusable) -- not the card+wrapped-error
// fallback that only shows until the next selection change.
func TestFailedReviewFromReconcileRendersThroughReviewPaneBody(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withSelection(0, 3)) // job 3: running, selected
	m.paneLogJobID, m.paneLogStreaming = 3, true

	failedJobs := testQueueJobs()
	failedAt := splitTestFinishedAt
	failedJobs[0].Status = storage.JobStatusFailed
	failedJobs[0].Error = "agent crashed"
	failedJobs[0].FinishedAt = &failedAt
	res, _ := m.handleJobsMsg(jobsMsg{jobs: failedJobs, stats: storage.JobStats{}})
	got := res.(model)

	lines := strings.Join(got.renderDetailPane(88, 25), "\n")
	assert.Contains(lines, "agent crashed")
	// "Review #<id>" is only emitted by renderReviewPaneBody's title line,
	// never by the card+wrapped-error fallback -- confirms the pane-body
	// path, not the card path, rendered this.
	assert.Contains(lines, "Review #3")
}

// TestSplitReconcileDetailFailedIdempotent covers the idempotency
// requirement of the same fix: once the failed review has been synthesized
// and the tail stopped, a second identical jobsMsg must not rebuild the
// review or churn paneLogSeq again (mirroring the Done branch's existing
// currentReview-match guard).
func TestSplitReconcileDetailFailedIdempotent(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withSelection(0, 3)) // job 3: running, selected
	m.paneLogJobID, m.paneLogSeq, m.paneLogStreaming = 3, 5, true

	failedJobs := testQueueJobs()
	failedAt := splitTestFinishedAt
	failedJobs[0].Status = storage.JobStatusFailed
	failedJobs[0].Error = "agent crashed"
	failedJobs[0].FinishedAt = &failedAt
	m.jobs = failedJobs

	got, cmd := m.splitReconcileDetail()
	require.NotNil(t, got.currentReview)
	assert.NotNil(cmd, "the rebuild dispatches the persisted-comments fetch")
	assert.False(got.paneLogStreaming)
	firstReview := got.currentReview
	seqAfterFirst := got.paneLogSeq

	got2, cmd2 := got.splitReconcileDetail()
	assert.Nil(cmd2)
	assert.Same(firstReview, got2.currentReview)
	assert.Equal(seqAfterFirst, got2.paneLogSeq)
}

// TestSplitReconcileDetailReplacesStaleFailedReviewFromRerun covers
// splitReconcileDetail's Failed branch: job IDs are reused across reruns,
// so an idempotency
// guard on JobID match alone could treat an OLD attempt's synthesized
// review as "already current" when a NEW failure for the same job ID
// arrives -- e.g. a rerun's Running/Failed observations outrunning
// handleRerunResultMsg's own currentReview-invalidating clear. A stale
// review carrying different error text, with its tail still marked
// active, must be replaced with the fresh synthesis and the tail stopped.
func TestSplitReconcileDetailReplacesStaleFailedReviewFromRerun(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withSelection(2, 1)) // job 1: failed, current error "boom"
	m.currentReview = synthesizeFailedReview(&storage.ReviewJob{
		ID: 1, Agent: "claude-code", Error: "stale error from a previous attempt",
	}, nil)
	m.paneLogJobID, m.paneLogSeq, m.paneLogStreaming = 1, 5, true

	got, cmd := m.splitReconcileDetail()
	assert.NotNil(cmd, "the rebuild dispatches the persisted-comments fetch")
	require.NotNil(got.currentReview)
	assert.Contains(got.currentReview.Output, "boom", "must rebuild with the CURRENT job.Error, not keep the stale attempt's text")
	assert.NotContains(got.currentReview.Output, "stale error from a previous attempt")
	assert.False(got.paneLogStreaming, "a failed job's tail must be stopped")
	assert.Greater(got.paneLogSeq, uint64(5))
}

// TestSplitReconcileDetailFailedStopsTailEvenWhenReviewAlreadyCurrent covers
// part 1 of the same fix in isolation: even when the loaded review already
// reflects the CURRENT failure (no rebuild needed), an active tail for that
// job must still be stopped -- stopping it only on the rebuild path
// would let a tail that was (however it got there) still marked active
// when the review already looked current survive this branch
// indefinitely.
func TestSplitReconcileDetailFailedStopsTailEvenWhenReviewAlreadyCurrent(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withSelection(2, 1)) // job 1: failed, "boom"
	m.currentReview = synthesizeFailedReview(&m.jobs[2], nil)
	firstReview := m.currentReview
	m.paneLogJobID, m.paneLogSeq, m.paneLogStreaming = 1, 5, true // tail somehow still active

	got, cmd := m.splitReconcileDetail()
	assert.Nil(cmd)
	assert.Same(firstReview, got.currentReview, "an already-current review must not be rebuilt")
	assert.False(got.paneLogStreaming, "a failed job's tail must be stopped even when the review needs no rebuild")
	assert.Greater(got.paneLogSeq, uint64(5))
}

// TestSplitReconcileDetailFailedIdempotentWhenErrorUnchanged is part 2's
// non-regression/idempotency case: when the loaded review's Output already
// matches what synthesizeFailedReview would build for the job's current
// state (same job ID, same error text) and no tail is active, reconcile
// must not rebuild the review or churn paneLogSeq.
func TestSplitReconcileDetailFailedIdempotentWhenErrorUnchanged(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withSelection(2, 1)) // job 1: failed, "boom"
	m.currentReview = synthesizeFailedReview(&m.jobs[2], nil)
	firstReview := m.currentReview
	m.paneLogJobID, m.paneLogSeq, m.paneLogStreaming = 1, 5, false

	got, cmd := m.splitReconcileDetail()
	assert.Nil(cmd)
	assert.Same(firstReview, got.currentReview)
	assert.Equal(uint64(5), got.paneLogSeq)
	assert.False(got.paneLogStreaming)
}

// TestSplitReconcileDetailRefetchesDoneReviewWhenCompletionChanged covers
// the symmetric question raised alongside the Failed-branch fix: does the
// Done branch have the same stale-same-job-ID hole? Within a single
// session, a self-initiated rerun is protected by handleRerunResultMsg's
// clear/attempt bump on confirm, which fires well before a real
// review job could plausibly complete again. But that machinery is
// session-local -- it only fires for a rerun THIS session dispatched and
// saw confirmed. A rerun triggered by another client sharing the same
// daemon (another TUI instance, the CLI, a CI poller) is invisible to it:
// this session never sees a rerunResultMsg, so currentReview is never
// cleared, and the next jobs refresh reporting the job done again would
// hit the JobID-only guard and leave the OLD review shown indefinitely.
// This IS reachable, so the Done branch gets the same class of fix:
// job.FinishedAt (stamped fresh by the daemon on every completion,
// already tracked in memory, no new persisted state) distinguishes this
// completion from a previous one when the loaded review's embedded Job
// disagrees with the freshly-polled job.
func TestSplitReconcileDetailRefetchesDoneReviewWhenCompletionChanged(t *testing.T) {
	assert := assert.New(t)
	oldFinish := time.Now().Add(-time.Hour)
	m := splitModel(withSelection(1, 2)) // job 2: done
	oldReview := splitTestReview()
	oldReview.Job.FinishedAt = &oldFinish
	m.currentReview = oldReview

	newFinish := time.Now()
	jobs := testQueueJobs()
	jobs[1].FinishedAt = &newFinish // job 2 completed again (e.g. an external rerun)
	m.jobs = jobs

	got, cmd := m.splitReconcileDetail()
	assert.NotNil(cmd, "a job that completed again (different FinishedAt) must trigger a fresh fetch even though currentReview.JobID still matches")
	assert.Same(oldReview, got.currentReview, "the stale review isn't replaced synchronously -- the fetch does that once it lands")
}

// TestSplitReconcileDetailDoneIdempotentWhenCompletionUnchanged is the
// non-regression counterpart: when the loaded review's embedded
// job.FinishedAt matches the freshly-polled job's FinishedAt (the review
// already reflects this exact completion), reconcile must not refetch.
func TestSplitReconcileDetailDoneIdempotentWhenCompletionUnchanged(t *testing.T) {
	assert := assert.New(t)
	finish := time.Now()
	m := splitModel(withSelection(1, 2)) // job 2: done
	review := splitTestReview()
	review.Job.FinishedAt = &finish
	m.currentReview = review

	jobs := testQueueJobs()
	jobs[1].FinishedAt = &finish
	m.jobs = jobs

	got, cmd := m.splitReconcileDetail()
	assert.Nil(cmd, "matching FinishedAt means the loaded review already reflects this completion -- no refetch needed")
	assert.Same(review, got.currentReview)
}

// TestSplitReconcileDetailDoneNoRefetchWhenPolledSnapshotIsOlder:
// reviewJobCompletionChanged must not be a plain (symmetric) inequality,
// which would fire whenever the freshly-polled job's FinishedAt DIFFERED
// from the loaded review's, in either direction.
// selectedJob falls through to m.panelMembers for a panel member, a cache
// refreshed only while a member is queued/running and frozen once all
// members go terminal -- so the polled snapshot can be OLDER than the one
// already embedded in the loaded review (e.g. currentReview was fetched at
// a completion the frozen snapshot predates: an external rerun of a
// member, review opened via stacked Enter, then L into split). With a
// plain inequality this fires on EVERY jobs refresh forever -- a reconcile
// loop: one fetchReviewFollow per SSE event and per poll, each landing
// resetting reviewScroll to 0, i.e. a permanently unscrollable pane. This
// is the loop repro, and must NOT refetch under the fixed, forward-only
// comparison.
func TestSplitReconcileDetailDoneNoRefetchWhenPolledSnapshotIsOlder(t *testing.T) {
	assert := assert.New(t)
	newer := time.Now()
	older := newer.Add(-time.Hour)
	m := splitModel(withSelection(1, 2)) // job 2: done
	review := splitTestReview()
	review.Job.FinishedAt = &newer // the loaded review reflects the NEWER completion
	m.currentReview = review

	jobs := testQueueJobs()
	jobs[1].FinishedAt = &older // freshly-polled snapshot is OLDER (e.g. a frozen panelMembers cache)
	m.jobs = jobs

	got, cmd := m.splitReconcileDetail()
	assert.Nil(cmd, "a polled snapshot older than the loaded review must never trigger a refetch -- this is the reconcile-loop repro")
	assert.Same(review, got.currentReview)
}

// ---------------------------------------------------------------------------
// Integration sweep: cross-cutting behaviors across the split feature.
// ---------------------------------------------------------------------------

// TestArrowKeysStepReviewNavInSplitDetailFocus: with
// focus on the detail pane (currentView == viewReview while split is
// active), left/right route through handleLeftKey/handleRightKey ->
// stepReviewNav, which must move BOTH the displayed review and the queue
// cursor (selectedJobID/selectedIdx) -- keeping list/detail selection in
// sync even though the list pane isn't focused.
func TestArrowKeysStepReviewNavInSplitDetailFocus(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	// testQueueJobs order: job 3 (running, idx0), job 2 (done, idx1),
	// job 1 (failed, idx2).
	m := splitModel(withReview(splitTestReview()), withSelection(1, 2))
	m.focus = focusDetail
	m.currentView = viewReview

	// Left = older: job 2 -> job 1 (failed, synthesized inline, no fetch).
	got, _ := pressSpecial(m, tea.KeyLeft)
	assert.Equal(int64(1), got.selectedJobID)
	assert.Equal(2, got.selectedIdx) // testQueueJobs()[2] is job 1
	require.NotNil(got.currentReview)
	assert.Equal(int64(1), got.currentReview.JobID)
	assert.Equal(focusDetail, got.focus)
	assert.Equal(viewReview, got.currentView)

	// Right = newer: job 1 -> job 2 (done, triggers a fetch); the cursor
	// moves immediately, before the fetch resolves.
	got2, cmd2 := pressSpecial(got, tea.KeyRight)
	assert.Equal(int64(2), got2.selectedJobID)
	assert.Equal(1, got2.selectedIdx)
	assert.NotNil(cmd2)
	assert.Equal(focusDetail, got2.focus)
	assert.Equal(viewReview, got2.currentView)
}

// TestSplitTransientRoundTripPrompt covers sweep item 2: from split detail
// focus, 'p' opens the full-screen prompt view (splitActive() requires
// currentView in {viewQueue, viewReview}), and 'esc' returns to viewReview
// with focus/layout/currentReview all intact.
func TestSplitTransientRoundTripPrompt(t *testing.T) {
	assert := assert.New(t)
	rev := splitTestReview()
	rev.Prompt = "review this diff"
	m := splitModel(withReview(rev))
	m.focus = focusDetail
	m.currentView = viewReview

	got, _ := pressKey(m, 'p')
	assert.Equal(viewKindPrompt, got.currentView)
	assert.False(got.splitActive())
	assert.Contains(got.viewContent(), "review this diff")

	got2, _ := pressSpecial(got, tea.KeyEscape)
	assert.Equal(viewReview, got2.currentView)
	assert.Equal(focusDetail, got2.focus)
	assert.Equal(layoutSplit, got2.layout)
	require.NotNil(t, got2.currentReview)
	assert.True(got2.splitActive())
	assert.Contains(got2.viewContent(), "first finding") // back in the split pane
}

// TestSplitTransientRoundTripHelp is TestSplitTransientRoundTripPrompt's
// counterpart for '?' (help).
func TestSplitTransientRoundTripHelp(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withReview(splitTestReview()))
	m.focus = focusDetail
	m.currentView = viewReview

	got, _ := pressKey(m, '?')
	assert.Equal(viewHelp, got.currentView)
	assert.False(got.splitActive())

	got2, _ := pressSpecial(got, tea.KeyEscape)
	assert.Equal(viewReview, got2.currentView)
	assert.Equal(focusDetail, got2.focus)
	assert.Equal(layoutSplit, got2.layout)
	assert.True(got2.splitActive())
}

// TestResizeBelowBreakpointWhileDetailFocused covers sweep item 3: shrinking
// the terminal below the split breakpoint while focused on the detail pane
// must fall back to the full-screen (stacked) review view for the SAME
// review, and growing back past the breakpoint must restore split with
// focus back on the detail pane, still showing that review.
func TestResizeBelowBreakpointWhileDetailFocused(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withReview(splitTestReview()))
	m.focus = focusDetail
	m.currentView = viewReview

	res, _ := m.handleWindowSizeMsg(tea.WindowSizeMsg{Width: 100, Height: 30})
	got := res.(model)
	assert.Equal(layoutStacked, got.layout)
	assert.Equal(viewReview, got.currentView)
	require.NotNil(t, got.currentReview)
	assert.Equal(int64(2), got.currentReview.JobID)
	assert.False(got.splitActive())

	res2, _ := got.handleWindowSizeMsg(tea.WindowSizeMsg{Width: 150, Height: 40})
	got2 := res2.(model)
	assert.Equal(layoutSplit, got2.layout)
	assert.Equal(focusDetail, got2.focus)
	assert.Equal(viewReview, got2.currentView)
	require.NotNil(t, got2.currentReview)
	assert.Equal(int64(2), got2.currentReview.JobID)
	assert.True(got2.splitActive())
}

// TestWindowResizeRefillsToPaneCapacityInSplit covers sweep item 4:
// handleWindowSizeMsg's pagination refill check must use the split list
// pane's own row capacity (queuePaneRowCapacity), not queueVisibleRows'
// full-screen chrome reservation, or a resize into a taller split pane can
// leave rows unfilled even though more data is available server-side.
func TestWindowResizeRefillsToPaneCapacityInSplit(t *testing.T) {
	assert := assert.New(t)
	m := initTestModel(
		withCurrentView(viewQueue),
		withDimensions(150, 40),
		withTestJobs(testQueueJobs()...),
		withSelection(1, 2),
	)
	m.layout = layoutSplit
	m.preferredLayout = layoutSplit
	m.layoutLocked = true
	m.hasMore = true
	m.loadingJobs = false         // newModel starts in a "loading" state; the refill gate requires it clear
	m.activeBranchFilter = "main" // != branchNone, so canPaginate/refill applies

	paneCapacity := m.queuePaneRowCapacity()
	fullScreenRows := m.queueVisibleRows()
	require.Greater(t, paneCapacity, fullScreenRows,
		"expected the split pane to fit more rows than the full-screen chrome reservation at these dimensions")

	// len(m.jobs)=3 sits strictly between the two thresholds (offset by
	// queuePrefetchBuffer): a refill keyed to the pane capacity must fire,
	// even though a refill keyed to the (smaller) full-screen row count
	// would not need to.
	m.jobs = make([]storage.ReviewJob, fullScreenRows+queuePrefetchBuffer)
	for i := range m.jobs {
		m.jobs[i] = storage.ReviewJob{ID: int64(i + 1), Status: storage.JobStatusDone}
	}
	require.Less(t, len(m.jobs), paneCapacity+queuePrefetchBuffer)
	require.GreaterOrEqual(t, len(m.jobs), fullScreenRows+queuePrefetchBuffer)

	res, cmd := m.handleWindowSizeMsg(tea.WindowSizeMsg{Width: m.width, Height: m.height})
	got := res.(model)
	assert.NotNil(cmd, "expected a refetch to fill the taller split pane")
	assert.True(got.loadingJobs)
}

// TestSplitInfoLineShowsFlash covers sweep item 5: setFlash(..., viewQueue)
// while split is active and list-focused must surface on the split info
// line (splitInfoLine renders m.renderFlash(m.currentView) first).
func TestSplitInfoLineShowsFlash(t *testing.T) {
	assert := assert.New(t)
	m := splitModel()
	m.focus = focusList
	m.currentView = viewQueue
	m.setFlash("No older review", 2*time.Second, viewQueue)

	footerRows := m.splitFooterRows()
	g := splitGeometry(m.width, m.height, len(reflowHelpRows(footerRows, m.width)))
	info := m.splitInfoLine(g)
	assert.Contains(info, "No older review")

	// Rendered into the full split screen too.
	assert.Contains(m.renderSplit(), "No older review")
}

// TestReviewFollowFetchFailureShowsInPane covers sweep item 6 (the
// fetchReview-follow half): a follow fetch failure (arriving as
// reviewFollowErrMsg, tagged with the requested jobID) is recorded in
// m.splitDetailErr, and renderDetailPane's done-branch surfaces it instead
// of leaving the "Loading review..." placeholder stuck forever. A stale
// jobID (selection has since moved on) is dropped.
func TestReviewFollowFetchFailureShowsInPane(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	_, m := mockServerModel(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	m.layout = layoutSplit
	m.jobs = testQueueJobs()
	m.selectedIdx, m.selectedJobID = 1, 2 // job 2: done

	cmd := m.fetchReviewFollow(2, 0)
	msg := cmd()
	followErr, ok := msg.(reviewFollowErrMsg)
	require.True(ok, "expected a reviewFollowErrMsg, got %T", msg)
	assert.Equal(int64(2), followErr.jobID)

	res, _ := m.handleReviewFollowErrMsg(followErr)
	got := res.(model)
	require.Error(got.splitDetailErr)

	lines := strings.Join(got.renderDetailPane(88, 25), "\n")
	assert.Contains(lines, "Failed to load review")
	assert.Contains(lines, "re-select the row to retry")

	// A stale jobID (selection moved on) is dropped, leaving splitDetailErr
	// untouched.
	m2 := m
	m2.selectedJobID = 3
	res2, _ := m2.handleReviewFollowErrMsg(reviewFollowErrMsg{jobID: 2, err: errors.New("boom")})
	assert.NoError(res2.(model).splitDetailErr)
}

// TestScheduleDetailFollowClearsSplitDetailErr covers the other half of
// sweep item 6's contract: a fresh follow (cursor moved to a new job)
// clears any earlier splitDetailErr so a stale error from a DIFFERENT job
// doesn't linger and get misattributed once the new job's content loads.
func TestScheduleDetailFollowClearsSplitDetailErr(t *testing.T) {
	m := splitModel()
	m.splitDetailErr = errors.New("stale error from a previous job")

	got, _ := m.scheduleDetailFollow()
	assert.NoError(t, got.splitDetailErr)
}

// TestPaneLogOutputErrorShowsInPaneAndStopsTick covers sweep item 6 (the
// paneLogOutputMsg half): a real fetch error (not the benign errNoLog
// "job just started" case) in the running-job branch stops the tail
// (paneLogStreaming=false, no further tick scheduled) and records the error
// for renderDetailPane's running-branch to show as one line.
func TestPaneLogOutputErrorShowsInPaneAndStopsTick(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withSelection(0, 3)) // job 3: running
	m.paneLogJobID, m.paneLogSeq, m.paneLogStreaming = 3, 5, true

	res, cmd := m.handlePaneLogOutputMsg(paneLogOutputMsg{jobID: 3, seq: 5, err: errors.New("log fetch failed")})
	got := res.(model)
	assert.Nil(cmd, "tick loop must stop on a real fetch error")
	assert.False(got.paneLogStreaming)
	require.Error(t, got.splitDetailErr)

	lines := strings.Join(got.renderDetailPane(88, 25), "\n")
	assert.Contains(lines, "Failed to load log")
}

// TestMdCacheMaxScrollSingleRendererPerFrame covers the ledger carry-over
// (sweep item 7): viewContent()'s dispatch is a single if/return chain --
// splitActive() (pane renderer, keyed to the detail pane's inner width) is
// checked BEFORE the full-screen viewReview branch (keyed to m.width), and
// the two are mutually exclusive (splitActive() requires layoutSplit;
// leaving split falls through to the full-screen branch). Only one of the
// two renderers can run per View() call, so mdCache.lastReviewMaxScroll
// can't drift between them mid-frame. This regression test locks that in:
// content long/wide enough to wrap differently at the two window widths
// produces two DIFFERENT recorded max-scroll values across two separate
// render passes -- proving each pass computed and wrote its own, not a
// stale value the other renderer left behind.
func TestMdCacheMaxScrollSingleRendererPerFrame(t *testing.T) {
	assert := assert.New(t)
	rev := splitTestReview()
	var sb strings.Builder
	for i := range 80 {
		fmt.Fprintf(&sb, "line %d of the review body\n\n", i)
	}
	rev.Output = sb.String()

	m := splitModel(withReview(rev))
	m.focus = focusDetail
	m.currentView = viewReview

	require.True(t, m.splitActive())
	_ = m.viewContent() // pane renderer: keyed to the detail pane's inner width
	paneMaxScroll := m.mdCache.lastReviewMaxScroll
	assert.Positive(paneMaxScroll)

	m.layout = layoutStacked
	require.False(t, m.splitActive())
	_ = m.viewContent() // full-screen renderer: keyed to m.width
	fullMaxScroll := m.mdCache.lastReviewMaxScroll

	assert.NotEqual(paneMaxScroll, fullMaxScroll)
}

// TestReviewMsgDoesNotClobberTransientView covers sweep item 8: a non-follow
// reviewMsg (from enterReviewCmd, Enter on a done job in the queue) must not
// yank the user into the review view if a transient view (e.g. the comment
// editor) was opened after the fetch was dispatched but before it resolved.
// The msg.jobID != selectedJobID staleness gate does NOT protect against
// this -- the selection hasn't moved, only currentView has. The review data
// is still stored so it's ready once the user returns from the transient
// view.
func TestReviewMsgDoesNotClobberTransientView(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := initTestModel(
		withCurrentView(viewQueue),
		withDimensions(150, 40),
		withTestJobs(testQueueJobs()...),
		withSelection(1, 2), // job 2: done
	)

	// Enter dispatches the async fetch; currentView stays viewQueue until
	// it resolves.
	res, cmd := m.handleEnterKey()
	m = res.(model)
	require.NotNil(cmd)
	assert.Equal(viewQueue, m.currentView)

	// Before the fetch resolves, the user opens the comment editor.
	res, _ = m.handleCommentOpenKey()
	m = res.(model)
	require.Equal(viewKindComment, m.currentView)
	m.commentText = "in progress comment"

	// The fetch now resolves for the same job (selection unchanged).
	res, _ = m.handleReviewMsg(reviewMsg{review: splitTestReview(), jobID: 2, fetchSeq: m.reviewFetchSeq})
	got := res.(model)
	assert.Equal(viewKindComment, got.currentView, "must not be yanked out of the comment editor")
	assert.Equal("in progress comment", got.commentText)
	require.NotNil(got.currentReview)
	assert.Equal(int64(2), got.currentReview.JobID) // loaded and ready for later
}

// ---------------------------------------------------------------------------
// Tasks-view navigation into the review view.
// ---------------------------------------------------------------------------

// TestTasksViewEnterSwitchesToReviewView: handleTasksKey's
// Enter/ctrl+j path on a done fix task (handlers_modal.go) dispatches
// fetchReview while staying on viewTasks until it resolves -- the
// handleReviewMsg view-switch guard added for sweep item 8 must include
// viewTasks in its allowed set, or this legitimate switch silently stops
// happening.
func TestTasksViewEnterSwitchesToReviewView(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := initTestModel(withCurrentView(viewTasks), withDimensions(150, 40))
	m.fixJobs = []storage.ReviewJob{
		{ID: 101, Status: storage.JobStatusDone},
	}
	m.fixSelectedIdx = 0

	got, cmd := pressSpecial(m, tea.KeyEnter)
	require.NotNil(cmd)
	assert.Equal(viewTasks, got.currentView) // fetch still in flight
	assert.Equal(int64(101), got.selectedJobID)
	assert.Equal(viewTasks, got.reviewFromView)

	res, _ := got.handleReviewMsg(reviewMsg{
		review: makeReview(20, &storage.ReviewJob{ID: 101}), jobID: 101,
		dispatchedFrom: viewTasks, // what fetchReview stamps from currentView here
		fetchSeq:       got.reviewFetchSeq,
	})
	final := res.(model)
	assert.Equal(viewReview, final.currentView)
	require.NotNil(final.currentReview)
}

// TestTasksViewParentShortcutSwitchesToReviewView is
// TestTasksViewEnterSwitchesToReviewView's counterpart for 'P' (open parent
// review for a fix task).
func TestTasksViewParentShortcutSwitchesToReviewView(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	parentID := int64(77)
	m := initTestModel(withCurrentView(viewTasks), withDimensions(150, 40))
	m.fixJobs = []storage.ReviewJob{
		{ID: 101, Status: storage.JobStatusDone, ParentJobID: &parentID},
	}
	m.fixSelectedIdx = 0

	got, cmd := pressKey(m, 'P')
	require.NotNil(cmd)
	assert.Equal(viewTasks, got.currentView)
	assert.Equal(parentID, got.selectedJobID)

	res, _ := got.handleReviewMsg(reviewMsg{
		review: makeReview(20, &storage.ReviewJob{ID: parentID}), jobID: parentID,
		dispatchedFrom: viewTasks, fetchSeq: got.reviewFetchSeq,
	})
	final := res.(model)
	assert.Equal(viewReview, final.currentView)
	require.NotNil(final.currentReview)
}

// TestReviewMsgConsumesPendingFixPanelEvenWithTransientViewOpen covers
// Finding B: 'F' from the queue sets reviewFixPanelPending and dispatches a
// fetch while remaining on viewQueue; if the user opens a transient view
// (e.g. the comment editor) before the fetch resolves, the fix panel state
// must still be consumed when it lands -- the view-switch guard (sweep item
// 8 / Finding A) must not also block the pending-fix-panel consumption, or
// it's stranded forever (nothing else ever reopens it).
func TestReviewMsgConsumesPendingFixPanelEvenWithTransientViewOpen(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := initTestModel(
		withCurrentView(viewQueue),
		withDimensions(150, 40),
		withTestJobs(testQueueJobs()...),
		withSelection(1, 2), // job 2: done
	)
	m.tasksEnabled = true

	res, cmd := m.handleFixKey()
	m = res.(model)
	require.NotNil(cmd)
	require.True(m.reviewFixPanelPending)
	assert.Equal(viewQueue, m.currentView)

	// Before the fix-triggered fetch resolves, the user opens the comment
	// editor.
	res, _ = m.handleCommentOpenKey()
	m = res.(model)
	require.Equal(viewKindComment, m.currentView)

	// The fetch resolves.
	res, _ = m.handleReviewMsg(reviewMsg{review: splitTestReview(), jobID: 2, fetchSeq: m.reviewFetchSeq})
	got := res.(model)
	assert.Equal(viewKindComment, got.currentView, "must not be yanked out of the comment editor")
	assert.False(got.reviewFixPanelPending, "pending fix panel must be consumed")
	assert.True(got.reviewFixPanelOpen)
	assert.True(got.reviewFixPanelFocused)
	require.NotNil(got.currentReview)
}

// TestScheduleDetailFollowInvalidatesStalePaneLogTail covers Finding C:
// paneLogOutputMsg/paneLogTickMsg are validated by paneLogSeq alone, with no
// jobID check, so moving the queue selection off a running job whose log is
// being tailed must invalidate that tail (bump paneLogSeq) or an in-flight
// response for the OLD job -- including a failure -- can still land after
// the fact and set splitDetailErr for whatever job is newly selected.
func TestScheduleDetailFollowInvalidatesStalePaneLogTail(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withSelection(0, 3)) // job 3: running, tailed
	m.paneLogJobID, m.paneLogSeq, m.paneLogStreaming = 3, 5, true

	// Moving the selection to job 2 (down from job 3) goes through the
	// real handleKeyMsg -> scheduleDetailFollow path.
	got, cmd := pressSpecial(m, tea.KeyDown)
	assert.Equal(int64(2), got.selectedJobID)
	assert.NotNil(cmd)
	assert.Greater(got.paneLogSeq, uint64(5))
	assert.False(got.paneLogStreaming)

	// A stale response for job 3, correctly tagged jobID: 3 (it genuinely
	// came from that in-flight fetch) but the OLD (now-invalid) seq, must
	// still be dropped on the seq check alone -- no splitDetailErr leaks
	// through for job 2.
	res, cmd2 := got.handlePaneLogOutputMsg(paneLogOutputMsg{jobID: 3, seq: 5, err: errors.New("stale failure from job 3")})
	assert.Nil(cmd2)
	assert.NoError(res.(model).splitDetailErr)
}

// TestScheduleDetailFollowLeavesTailAloneWhenSelectionUnchanged is
// TestScheduleDetailFollowInvalidatesStalePaneLogTail's negative case: a
// follow reschedule that does NOT change the selection (e.g. re-selecting
// the same job) must not disturb an in-progress tail for that same job.
func TestScheduleDetailFollowLeavesTailAloneWhenSelectionUnchanged(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withSelection(0, 3)) // job 3: running, tailed
	m.paneLogJobID, m.paneLogSeq, m.paneLogStreaming = 3, 5, true

	got, _ := m.scheduleDetailFollow()
	assert.Equal(uint64(5), got.paneLogSeq)
	assert.True(got.paneLogStreaming)
}

// TestSplitReconcileDetailRestartsStalledRunningTail covers Finding D: the
// "invalidate, don't restart" resize path (handleWindowSizeMsg, when a
// transient view is covering the split pane) leaves a running job's pane
// tail stopped until something restarts it. splitReconcileDetail, called
// from handleJobsMsg on every jobs refresh (SSE push or the periodic poll),
// must notice a running selected job whose tail isn't active and restart it
// -- recovering automatically within one refresh cycle.
func TestSplitReconcileDetailRestartsStalledRunningTail(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withSelection(0, 3)) // job 3: running, selected
	m.paneLogJobID, m.paneLogSeq, m.paneLogStreaming = 3, 5, false

	res, cmd := m.handleJobsMsg(jobsMsg{jobs: testQueueJobs(), stats: storage.JobStats{}})
	got := res.(model)
	assert.NotNil(cmd)
	assert.True(got.paneLogStreaming)
	assert.Greater(got.paneLogSeq, uint64(5))
	assert.Equal(int64(3), got.paneLogJobID)
}

// TestSplitReconcileDetailLeavesActiveTailAlone is the negative case for
// Finding D: a running job whose tail is already active must not be
// restarted (that would reset paneLogLines/offset and drop buffered log
// content for no reason).
func TestSplitReconcileDetailLeavesActiveTailAlone(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withSelection(0, 3)) // job 3: running, selected
	m.paneLogJobID, m.paneLogSeq, m.paneLogStreaming = 3, 5, true
	m.paneLogLines = []logLine{{text: "already buffered"}}

	got, cmd := m.splitReconcileDetail()
	assert.Nil(cmd)
	assert.Equal(uint64(5), got.paneLogSeq)
	assert.Equal([]logLine{{text: "already buffered"}}, got.paneLogLines)
}

// TestSplitReconcileDetailClearsStaleSplitDetailErr and
// TestPaneLogCompletionClearsStaleSplitDetailErr cover Finding E:
// splitDetailErr must be cleared before splitReconcileDetail's and
// handlePaneLogOutputMsg's running->done handoff's fetchReviewFollow calls,
// or a stale error from an earlier failed attempt can render for one frame
// against the freshly-resolving job before the new fetch lands.
func TestSplitReconcileDetailClearsStaleSplitDetailErr(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withSelection(1, 2)) // job 2: done
	m.splitDetailErr = errors.New("stale error from a previous attempt")

	got, cmd := m.splitReconcileDetail()
	assert.NotNil(cmd)
	assert.NoError(got.splitDetailErr)
}

func TestPaneLogCompletionClearsStaleSplitDetailErr(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withSelection(0, 3)) // job 3: running
	m.paneLogJobID, m.paneLogSeq, m.paneLogStreaming = 3, 5, true
	m.splitDetailErr = errors.New("stale log error")

	res, cmd := m.handlePaneLogOutputMsg(paneLogOutputMsg{jobID: 3, seq: 5, hasMore: false})
	got := res.(model)
	assert.NotNil(cmd)
	assert.NoError(got.splitDetailErr)
}

// TestQueuePaneRowCapacityMatchesRenderInCompactMode covers Finding F:
// queuePaneRowCapacity must equal the number of data rows
// renderQueuePaneBody actually draws, in compact mode too. It previously
// subtracted 0 (instead of 2) for the table header/separator budget in
// compact mode, overestimating by 2 -- renderQueuePaneBody always reserves
// those 2 lines in its call to renderQueueTable regardless of compact mode
// (the header just isn't drawn there; the 2 freed lines come back as blank
// padding, not extra data rows). This renders the real pane, packed with
// far more jobs than fit, and counts the actual non-blank rows rather than
// re-deriving the same formula the fix uses, so a regression that changes
// renderQueuePaneBody's budget without updating this helper would still be
// caught.
func TestQueuePaneRowCapacityMatchesRenderInCompactMode(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withDimensions(150, 14)) // height < 15 -> queueCompact() true
	require.True(t, m.queueCompact())

	jobs := make([]storage.ReviewJob, 100)
	for i := range jobs {
		jobs[i] = storage.ReviewJob{ID: int64(i + 1), Status: storage.JobStatusDone}
	}
	m.jobs = jobs

	footerRows := m.splitFooterRows()
	g := splitGeometry(m.width, m.height, len(reflowHelpRows(footerRows, m.width)))
	lines := m.renderQueuePaneBody(g.listInnerW, g.listInnerH)

	nonBlank := 0
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			nonBlank++
		}
	}
	assert.Equal(nonBlank, m.queuePaneRowCapacity())
	assert.Less(m.queuePaneRowCapacity(), g.listInnerH, "compact mode still reserves the header/separator budget even though it isn't drawn")
}

// ---------------------------------------------------------------------------
// Review responses respect their dispatch origin.
// ---------------------------------------------------------------------------

// TestReviewMsgRespectsDispatchOriginQueueThenTasks: widening
// handleReviewMsg's view-switch
// guard to an allowlist (viewQueue|viewReview|viewTasks) to let the
// tasks-view fetchReview paths resolve correctly. But viewQueue and
// viewTasks are mutually reachable via 'T'/Esc while a fetch from EITHER
// one is still in flight, so the allowlist let a fetch dispatched from the
// queue resolve into viewReview even after the user explicitly switched to
// Tasks in the meantime. The fix replaces the allowlist with dispatch-origin
// tracking via m.reviewFromView (already set at every relevant dispatch
// site): the switch only fires when currentView is still the origin the
// fetch was dispatched from, or already viewReview.
func TestReviewMsgRespectsDispatchOriginQueueThenTasks(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := initTestModel(
		withCurrentView(viewQueue),
		withDimensions(150, 40),
		withTestJobs(testQueueJobs()...),
		withSelection(1, 2), // job 2: done
	)
	m.tasksEnabled = true

	// Enter dispatches the fetch from the queue.
	res, cmd := m.handleEnterKey()
	m = res.(model)
	require.NotNil(cmd)
	assert.Equal(viewQueue, m.reviewFromView)
	assert.Equal(viewQueue, m.currentView)

	// Before it resolves, the user switches to the Tasks view.
	res, _ = m.handleToggleTasksKey()
	m = res.(model)
	require.Equal(viewTasks, m.currentView)

	// The queue-origin fetch resolves for the same job (selection
	// unchanged, so the staleness gate alone doesn't catch this).
	res, _ = m.handleReviewMsg(reviewMsg{review: splitTestReview(), jobID: 2, fetchSeq: m.reviewFetchSeq})
	got := res.(model)
	assert.Equal(viewTasks, got.currentView, "must not be yanked into viewReview while the user is on Tasks")
	require.NotNil(got.currentReview)
}

// TestReviewMsgRespectsDispatchOriginTasksThenQueue is
// TestReviewMsgRespectsDispatchOriginQueueThenTasks's mirror case: a fetch
// dispatched from the Tasks view (Enter on a done fix task) must not
// resolve into viewReview if the user has since backed out to the queue
// (Esc/T from handleTasksKey).
func TestReviewMsgRespectsDispatchOriginTasksThenQueue(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := initTestModel(withCurrentView(viewTasks), withDimensions(150, 40))
	m.tasksEnabled = true
	m.fixJobs = []storage.ReviewJob{
		{ID: 101, Status: storage.JobStatusDone},
	}
	m.fixSelectedIdx = 0

	got, cmd := pressSpecial(m, tea.KeyEnter)
	require.NotNil(cmd)
	assert.Equal(viewTasks, got.reviewFromView)
	assert.Equal(viewTasks, got.currentView)

	// Before it resolves, the user backs out to the queue -- which also
	// repairs the selection off the fix job (exitTasksToQueue), since no
	// queue row can resolve a fix job's ID.
	got2, _ := pressSpecial(got, tea.KeyEscape)
	require.Equal(viewQueue, got2.currentView)
	require.NotEqual(int64(101), got2.selectedJobID,
		"backing out to the queue must move the selection off the fix job")

	res, _ := got2.handleReviewMsg(reviewMsg{
		review: makeReview(20, &storage.ReviewJob{ID: 101}), jobID: 101,
		dispatchedFrom: viewTasks,
		fetchSeq:       got2.reviewFetchSeq,
	})
	final := res.(model)
	assert.Equal(viewQueue, final.currentView, "must not be yanked into viewReview while the user backed out to the queue")
	assert.Nil(final.currentReview,
		"the abandoned tasks fetch must not load content for a fix job the exit de-selected")
}

// ---------------------------------------------------------------------------
// Tasks-origin reviews render full-screen, outside the split composition.
// ---------------------------------------------------------------------------

// TestSplitActiveExcludesTasksOriginReview: opening a
// completed fix task from the Tasks view while split layout is enabled
// switches currentView to viewReview (per the dispatch-origin
// guard), which would make splitActive() true and route rendering through
// renderSplit -- but fix jobs live in m.fixJobs, not m.jobs, so
// renderDetailPane's m.selectedJob() lookup (m.jobs/panelMembers-only)
// can't resolve selectedJobID and would show "No job selected" instead of
// the loaded review. splitActive() now excludes a tasks-origin review
// (reviewFromView == viewTasks) so it renders full-screen via the ordinary
// review renderer instead, same as any other transient view.
func TestSplitActiveExcludesTasksOriginReview(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := initTestModel(withCurrentView(viewTasks), withDimensions(150, 40))
	m.layout = layoutSplit
	m.tasksEnabled = true
	m.fixJobs = []storage.ReviewJob{
		{ID: 101, Status: storage.JobStatusDone},
	}
	m.fixSelectedIdx = 0

	got, cmd := pressSpecial(m, tea.KeyEnter)
	require.NotNil(cmd)
	assert.Equal(viewTasks, got.reviewFromView)

	res, _ := got.handleReviewMsg(reviewMsg{
		review:         makeReview(20, &storage.ReviewJob{ID: 101}, withReviewOutput("fix task review output")),
		jobID:          101,
		dispatchedFrom: viewTasks,
		fetchSeq:       got.reviewFetchSeq,
	})
	final := res.(model)
	assert.Equal(viewReview, final.currentView)
	assert.False(final.splitActive(), "a tasks-origin review must not route through the split pane")

	content := final.viewContent()
	assert.Contains(content, "fix task review output")
	assert.NotContains(content, "No job selected")
}

// TestSplitEscQuitReturnTasksOriginReviewToTasksView covers Finding 1's
// knock-on effect (a): the split esc/q shortcuts (handleEscKey/
// handleQuitKey) that normally jump straight back to viewQueue when
// layout==layoutSplit && currentView==viewReview must NOT fire for a
// tasks-origin review -- they must fall through to the general
// reviewFromView-based return logic instead, landing back on viewTasks.
func TestSplitEscQuitReturnTasksOriginReviewToTasksView(t *testing.T) {
	assert := assert.New(t)
	newTasksOriginReview := func() model {
		m := initTestModel(withCurrentView(viewReview), withDimensions(150, 40))
		m.layout = layoutSplit
		m.reviewFromView = viewTasks
		m.currentReview = makeReview(20, &storage.ReviewJob{ID: 101}, withReviewOutput("fix output"))
		return m.normalizeSplitState()
	}

	esc := newTasksOriginReview()
	res, _ := esc.handleEscKey()
	assert.Equal(viewTasks, res.(model).currentView, "esc must return to viewTasks, not viewQueue")

	quit := newTasksOriginReview()
	res2, cmd := quit.handleQuitKey()
	assert.Equal(viewTasks, res2.(model).currentView, "q must return to viewTasks, not viewQueue")
	assert.Nil(cmd)
}

// TestSplitActiveStillTrueForQueueOriginReview is
// TestSplitActiveExcludesTasksOriginReview's regression guard: an
// ordinary queue-origin review (reviewFromView == viewQueue, the zero
// value) must still render through the split pane, unaffected by the
// tasks-origin exclusion.
func TestSplitActiveStillTrueForQueueOriginReview(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withReview(splitTestReview()))
	assert.True(m.splitActive())
	assert.Contains(m.viewContent(), "first finding") // pane body content
}

// TestSplitNormalizeStateHarmlessForTasksOriginReview covers Finding 1's
// knock-on effect (b): normalizeSplitState unconditionally sets
// focus=focusDetail for currentView==viewReview while layout==layoutSplit,
// including for a tasks-origin review that (per splitActive()'s exclusion)
// renders full-screen rather than via the split pane. Proves the flip is
// harmless: m.focus is only ever consumed by code gated on splitActive()
// (rendering/mouse) or by applyLayout's leaving-split branch (which reads
// it to decide whether to KEEP a loaded review open -- true either way
// here), so it has no rendering or key-handling impact for a tasks-origin
// review.
func TestSplitNormalizeStateHarmlessForTasksOriginReview(t *testing.T) {
	assert := assert.New(t)
	m := initTestModel(withCurrentView(viewReview), withDimensions(150, 40))
	m.layout = layoutSplit
	m.reviewFromView = viewTasks
	m.currentReview = makeReview(20, &storage.ReviewJob{ID: 101}, withReviewOutput("fix output"))
	m.focus = focusList // arbitrary pre-state

	got := m.normalizeSplitState()
	assert.Equal(focusDetail, got.focus, "the flip happens as documented -- that alone is fine")

	// No rendering impact: still full-screen, review still shown.
	assert.False(got.splitActive())
	assert.Contains(got.viewContent(), "fix output")
	assert.Equal(viewReview, got.currentView)

	// No key-handling impact: esc still returns to viewTasks (the guard
	// checks reviewFromView, not focus).
	res, _ := got.handleEscKey()
	assert.Equal(viewTasks, res.(model).currentView)
}

// TestSplitTabKeyStampsQueueOriginAfterStaleTasksReview covers Finding 1's
// knock-on effect (c): handleTabKey's split branch enters viewReview using
// whatever review the split's background follow-fetch already loaded (not
// a fresh dispatch through handleReviewMsg's origin-tracking guard), so if
// m.reviewFromView is left stale from an EARLIER tasks-origin review,
// splitActive()'s exclusion could misfire and force a genuine queue-origin
// review full-screen. handleTabKey now stamps reviewFromView = viewQueue
// on this transition to prevent that.
func TestSplitTabKeyStampsQueueOriginAfterStaleTasksReview(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withReview(splitTestReview())) // queue-origin review already loaded via follow-fetch
	m.focus = focusList
	m.currentView = viewQueue
	m.reviewFromView = viewTasks // stale, left over from an earlier tasks-origin review

	res, _ := m.handleTabKey()
	got := res.(model)
	assert.Equal(viewQueue, got.reviewFromView, "must stamp the real origin, not leave the stale value")
	assert.Equal(viewReview, got.currentView)
	assert.Equal(focusDetail, got.focus)
	assert.True(got.splitActive(), "a genuinely queue-origin review must still render via the split pane")
}

// TestSplitMouseClickIntoDetailStampsQueueOrigin is
// TestSplitTabKeyStampsQueueOriginAfterStaleTasksReview's mouse-click
// counterpart: handleSplitMouse's click-into-the-detail-pane branch has
// the identical staleness risk (same "already-loaded via follow-fetch, not
// through the origin-tracking guard" reasoning) and needed the same fix.
func TestSplitMouseClickIntoDetailStampsQueueOrigin(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withReview(splitTestReview()))
	m.reviewFromView = viewTasks // stale
	g := splitGeometry(150, 40, len(reflowHelpRows(m.splitFooterRows(), 150)))

	res, _ := m.handleSplitMouse(mouseClickAt(g.listOuterW+5, 10))
	got := res.(model)
	assert.Equal(viewQueue, got.reviewFromView)
	assert.Equal(focusDetail, got.focus)
	assert.True(got.splitActive())
}

// TestPaneLogOutputRejectsMismatchedJobID covers Finding 2: pane-log
// invalidation previously happened only in scheduleDetailFollow (bumping
// paneLogSeq on a keyboard/mouse selection change); a selection-mutation
// path that doesn't call it -- e.g. the control socket's select-job
// command -- leaves an in-flight pane-log fetch's response for the OLD job
// still passing the seq-alone gate. paneLogOutputMsg now carries jobID
// (set in fetchPaneLog from the jobID it was dispatched for), and
// handlePaneLogOutputMsg requires it to match m.paneLogJobID in addition
// to seq -- an independent, message-level invariant that doesn't depend on
// every selection-mutation call site remembering to invalidate the tail.
func TestPaneLogOutputRejectsMismatchedJobID(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withSelection(0, 3)) // job 3: running, tailed
	m.paneLogJobID, m.paneLogSeq, m.paneLogStreaming = 3, 5, true

	// paneLogJobID has moved on to job 2, but the seq wasn't bumped in
	// lockstep (simulating any path that reassigns paneLogJobID without
	// going through scheduleDetailFollow's seq-bump invalidation).
	m.paneLogJobID = 2

	// An in-flight error response tagged for the OLD job (3), with the
	// still-current seq, must be dropped: jobID mismatches even though seq
	// matches.
	res, cmd := m.handlePaneLogOutputMsg(paneLogOutputMsg{jobID: 3, seq: 5, err: errors.New("stale failure from job 3")})
	got := res.(model)
	assert.Nil(cmd)
	require.NoError(t, got.splitDetailErr)
	assert.Empty(got.paneLogLines)
	assert.True(got.paneLogStreaming, "a rejected message must not touch any paneLog* state")
}

// TestSplitReconcileDetailClearsSplitDetailErrOnTailRestart and
// TestPaneLogOutputSuccessClearsStaleSplitDetailErr cover the round-4
// finding: splitDetailErr could stick indefinitely when the selection
// landed on a RUNNING job through a path that bypasses
// scheduleDetailFollow (e.g. the control socket's select-job, which
// mutates selectedIdx/selectedJobID directly). splitReconcileDetail
// cleared it only in its done branch, and handlePaneLogOutputMsg only on
// the hasMore==false completion path -- so renderDetailPane's running
// branch, which renders splitDetailErr unconditionally, showed a dead
// "Failed to load log" from a PREVIOUS job over the new job's live tail
// forever.
func TestSplitReconcileDetailClearsSplitDetailErrOnTailRestart(t *testing.T) {
	assert := assert.New(t)
	jobs := []storage.ReviewJob{
		{
			ID: 4, GitRef: "dddd444", Branch: "main", RepoName: "repoA",
			Agent: "codex", Status: storage.JobStatusRunning,
		},
		{
			ID: 3, GitRef: "cccc333", Branch: "main", RepoName: "repoA",
			Agent: "codex", Status: storage.JobStatusRunning,
		},
	}
	m := splitModel(withTestJobs(jobs...), withSelection(1, 3))
	m.paneLogJobID, m.paneLogSeq, m.paneLogStreaming = 3, 5, true

	// Job 3's tail fails: splitDetailErr is recorded for job 3.
	res, _ := m.handlePaneLogOutputMsg(paneLogOutputMsg{
		jobID: 3, seq: 5, err: errors.New("tail failed for job 3"),
	})
	m = res.(model)
	require.Error(t, m.splitDetailErr)

	// Control-socket-style selection mutation onto running job 4: no
	// scheduleDetailFollow, so nothing on this path clears the error.
	m.selectedIdx, m.selectedJobID = 0, 4

	got, cmd := m.splitReconcileDetail()
	assert.NotNil(cmd, "a tail restart command must be issued for the newly selected running job")
	require.NoError(t, got.splitDetailErr, "job 3's error is stale once job 4's tail restarts")
	assert.Equal(int64(4), got.paneLogJobID)
	assert.True(got.paneLogStreaming)
	assert.NotContains(strings.Join(got.renderDetailPane(88, 25), "\n"), "Failed to load log")
}

func TestPaneLogOutputSuccessClearsStaleSplitDetailErr(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withSelection(0, 3)) // job 3: running, tailed
	m.paneLogJobID, m.paneLogSeq, m.paneLogStreaming = 3, 5, true
	m.splitDetailErr = errors.New("stale error from a previous job")

	// A successful still-running fetch (hasMore == true) proves the pane is
	// healthy, so the stale error must go even though the tail continues.
	res, cmd := m.handlePaneLogOutputMsg(paneLogOutputMsg{
		jobID: 3, seq: 5, hasMore: true, lines: []logLine{{text: "live line"}},
	})
	got := res.(model)
	assert.NotNil(cmd, "a still-running fetch re-arms the poll tick")
	require.NoError(t, got.splitDetailErr)
	assert.Equal([]logLine{{text: "live line"}}, got.paneLogLines)
	assert.True(got.paneLogStreaming)
}

// ---------------------------------------------------------------------------
// Control-socket selection changes take the shared detail-follow transition.
// ---------------------------------------------------------------------------

// TestCtrlSelectJobRoutesThroughDetailFollowInSplit: the control
// socket's select-job used to mutate
// selectedIdx/selectedJobID directly, bypassing scheduleDetailFollow. In
// split layout that left the detail pane tailing the PREVIOUS job (and
// showing its splitDetailErr) until the next jobs refresh reconciled it,
// and left the old job's in-flight pane-log fetches able to land, because
// handlePaneLogOutputMsg's seq and jobID gates both still referenced the
// old job. handleCtrlSelectJob now routes a real selection change through
// scheduleDetailFollow when split layout is on.
func TestCtrlSelectJobRoutesThroughDetailFollowInSplit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withSelection(0, 3)) // job 3: running, tailed
	m.paneLogJobID, m.paneLogSeq, m.paneLogStreaming = 3, 5, true
	m.paneLogLines = []logLine{{text: "job 3 tail"}}
	m.splitDetailErr = errors.New("job 3 log error")

	params, err := json.Marshal(map[string]int64{"job_id": 2})
	require.NoError(err)
	got, resp, cmd := m.handleCtrlSelectJob(params)
	require.True(resp.OK, "expected OK, got error: %s", resp.Error)
	assert.Equal(int64(2), got.selectedJobID)
	assert.Equal(1, got.selectedIdx)
	require.NotNil(cmd, "split layout must arm the debounced detail follow")
	assert.Equal(uint64(6), got.paneLogSeq,
		"the tail for the job we navigated away from must be invalidated")
	assert.False(got.paneLogStreaming)
	require.NoError(got.splitDetailErr, "job 3's error must not stick to job 2")

	// The returned cmd is scheduleDetailFollow's debounce tick, tagged with
	// the generation the model now carries.
	tick, ok := cmd().(detailFollowTickMsg)
	require.True(ok, "expected a detailFollowTickMsg")
	assert.Equal(got.detailFollowGen, tick.gen)
}

// TestCtrlSelectJobRejectsInFlightTailResponseFromOldJob: an
// in-flight pane-log response for the job select-job navigated AWAY from
// must fail the seq gate, or a SUCCESSFUL one would clear a
// splitDetailErr that belongs to the newly selected job. Routing select-job
// through scheduleDetailFollow bumps paneLogSeq, so it is now rejected.
func TestCtrlSelectJobRejectsInFlightTailResponseFromOldJob(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withSelection(0, 3)) // job 3: running, tailed
	m.paneLogJobID, m.paneLogSeq, m.paneLogStreaming = 3, 5, true

	params, err := json.Marshal(map[string]int64{"job_id": 2}) // job 2: done
	require.NoError(err)
	got, resp, _ := m.handleCtrlSelectJob(params)
	require.True(resp.OK, "expected OK, got error: %s", resp.Error)

	// Job 2's own review follow fetch fails: that error belongs to the pane.
	// gen matches what a REAL follow fetch dispatched for job 2 (after the
	// reselect above already bumped it via scheduleDetailFollow) would
	// have captured at dispatch time.
	res0, _ := got.handleReviewFollowErrMsg(reviewFollowErrMsg{
		jobID: 2, gen: got.detailFollowGen, err: errors.New("review fetch failed for job 2"),
	})
	got = res0.(model)
	require.Error(got.splitDetailErr)

	// Job 3's successful in-flight tail response lands late, carrying the
	// pre-select-job seq.
	res, cmd := got.handlePaneLogOutputMsg(paneLogOutputMsg{
		jobID: 3, seq: 5, hasMore: true, lines: []logLine{{text: "job 3 line"}},
	})
	final := res.(model)
	assert.Nil(cmd)
	assert.Empty(final.paneLogLines, "a rejected response applies no lines")
	assert.Error(final.splitDetailErr, "job 2's error must survive job 3's stale response")
}

// TestCtrlSelectJobStackedLeavesPaneLogAlone: outside split layout there is
// no detail pane to follow, so select-job behaves exactly as before -- no
// follow cmd, no tail invalidation.
func TestCtrlSelectJobStackedLeavesPaneLogAlone(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withSelection(0, 3))
	m.layout = layoutStacked
	m.paneLogJobID, m.paneLogSeq, m.paneLogStreaming = 3, 5, true

	params, err := json.Marshal(map[string]int64{"job_id": 2})
	require.NoError(err)
	got, resp, cmd := m.handleCtrlSelectJob(params)
	require.True(resp.OK, "expected OK, got error: %s", resp.Error)
	assert.Nil(cmd)
	assert.Equal(int64(2), got.selectedJobID)
	assert.Equal(uint64(5), got.paneLogSeq)
	assert.True(got.paneLogStreaming)
}

// reviewNavJobs returns two done jobs so stepReviewNav has somewhere to
// step to.
func reviewNavJobs() []storage.ReviewJob {
	verdictP := "P"
	return []storage.ReviewJob{
		{
			ID: 6, GitRef: "ffff666", Branch: "main", RepoName: "repoA",
			Agent: "codex", Status: storage.JobStatusDone, Verdict: &verdictP,
		},
		{
			ID: 2, GitRef: "bbbb222", Branch: "feat/x", RepoName: "repoA",
			Agent: "codex", Status: storage.JobStatusDone, Verdict: &verdictP,
		},
	}
}

// TestStepReviewNavFetchDoesNotReopenReviewAfterEsc: handleReviewMsg's
// view-switch guard must not compare currentView against
// m.reviewFromView, which is the Esc RETURN target rather than the fetch's
// dispatch origin. stepReviewNav dispatches from INSIDE viewReview while
// reviewFromView still says viewQueue, so escaping back to the queue before
// the fetch resolved satisfied the guard (queue == queue) and yanked the
// user back into the review view. The guard now compares against
// msg.dispatchedFrom, stamped by fetchReview from currentView at
// command-creation time.
func TestStepReviewNavFetchDoesNotReopenReviewAfterEsc(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := initTestModel(
		withCurrentView(viewReview),
		withDimensions(150, 40),
		withTestJobs(reviewNavJobs()...),
		withSelection(1, 2),
	)
	m.reviewFromView = viewQueue
	m.currentReview = makeReview(20, &storage.ReviewJob{ID: 2})

	res, cmd := m.stepReviewNav(-1)
	got := res.(model)
	require.NotNil(cmd, "stepping onto a done job dispatches a review fetch")
	require.Equal(int64(6), got.selectedJobID)
	assert.Equal(viewReview, got.currentView, "dispatched from inside the review view")
	assert.Equal(viewQueue, got.reviewFromView, "reviewFromView is the RETURN target, not the origin")

	// The user escapes back to the queue before the fetch resolves.
	got2, _ := pressSpecial(got, tea.KeyEscape)
	require.Equal(viewQueue, got2.currentView)

	final, _ := got2.handleReviewMsg(reviewMsg{
		review: makeReview(20, &storage.ReviewJob{ID: 6}), jobID: 6,
		dispatchedFrom: viewReview, fetchSeq: got2.reviewFetchSeq,
	})
	fm := final.(model)
	assert.Equal(viewQueue, fm.currentView,
		"a review-view-dispatched fetch must not re-open the review the user just left")
	require.NotNil(fm.currentReview, "content is still updated, ready for a return to the review")
}

// TestStepReviewNavFetchUpdatesInPlaceWhenStillInReview is the counterpart:
// the user who stays in the review view gets the stepped-to review, exactly
// as before.
func TestStepReviewNavFetchUpdatesInPlaceWhenStillInReview(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := initTestModel(
		withCurrentView(viewReview),
		withDimensions(150, 40),
		withTestJobs(reviewNavJobs()...),
		withSelection(1, 2),
	)
	m.reviewFromView = viewQueue
	m.currentReview = makeReview(20, &storage.ReviewJob{ID: 2})

	res, cmd := m.stepReviewNav(-1)
	got := res.(model)
	require.NotNil(cmd)

	final, _ := got.handleReviewMsg(reviewMsg{
		review: makeReview(21, &storage.ReviewJob{ID: 6}), jobID: 6,
		dispatchedFrom: viewReview, fetchSeq: got.reviewFetchSeq,
	})
	fm := final.(model)
	assert.Equal(viewReview, fm.currentView)
	require.NotNil(fm.currentReview)
	assert.Equal(int64(6), fm.currentReview.JobID)
}

// TestReviewMsgStampsDispatchOriginFromCurrentView pins the other half of
// the fix: fetchReview must capture currentView at command-CREATION time
// (the model is a value snapshot there), not read it when the response is
// built, so the origin reflects where the user actually was when the fetch
// was issued.
func TestReviewMsgStampsDispatchOriginFromCurrentView(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"job_id": 7}`))
		},
	))
	defer ts.Close()

	for _, view := range []viewKind{viewQueue, viewReview, viewTasks} {
		m := newModel(testEndpointFromURL(ts.URL), withExternalIODisabled())
		m.currentView = view
		cmd := m.fetchReview(7, 1)
		// Navigating away after the command was built must not change the
		// stamped origin.
		m.currentView = viewLog
		msg, ok := cmd().(reviewMsg)
		require.True(t, ok, "expected a reviewMsg for view %v", view)
		assert.Equal(t, view, msg.dispatchedFrom)
	}
}

// ---------------------------------------------------------------------------
// Layout transitions invalidate and restore pane state.
// ---------------------------------------------------------------------------

// TestToggleLayoutInvalidatesTailWhenLeavingSplit: leaving
// split (L) left paneLogStreaming true with paneLogSeq unbumped, while the
// pending paneLogTickMsg was silently dropped by handlePaneLogTickMsg's
// layout gate -- the poll chain was dead but the model still claimed an
// active tail. Toggling back with the same running job selected then found
// startPaneLog's "already tailing this job" early return satisfied, so the
// pane sat on frozen stale lines until the job completed (a resize rescued
// it; L never did). applyLayout now invalidates the tail on the way out.
// Mirrors TestSplitReconcileDetailRestartsStalledRunningTail's shape.
func TestToggleLayoutInvalidatesTailWhenLeavingSplit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withSelection(0, 3)) // job 3: running, tailed
	m.paneLogJobID, m.paneLogSeq, m.paneLogStreaming = 3, 5, true
	m.paneLogLines = []logLine{{text: "line from the abandoned tail"}}

	// L: split -> stacked.
	res, _ := m.handleToggleLayoutKey()
	stacked := res.(model)
	require.Equal(layoutStacked, stacked.layout)
	assert.Equal(uint64(6), stacked.paneLogSeq, "the abandoned tail must be invalidated")
	assert.False(stacked.paneLogStreaming, "state must not claim a tail whose poll chain is dead")

	// The tick left over from that tail is now rejected on seq alone.
	_, deadCmd := stacked.handlePaneLogTickMsg(paneLogTickMsg{seq: 5})
	assert.Nil(deadCmd)

	// L: back to split, same running job still selected.
	res2, cmd2 := stacked.handleToggleLayoutKey()
	split := res2.(model)
	require.Equal(layoutSplit, split.layout)
	require.NotNil(cmd2, "returning to split must arm the detail follow")
	tick, ok := cmd2().(detailFollowTickMsg)
	require.True(ok, "expected a detailFollowTickMsg")

	res3, cmd3 := split.handleDetailFollowTick(tick)
	restarted := res3.(model)
	assert.NotNil(cmd3, "the tail must restart rather than no-op on the stale streaming flag")
	assert.Greater(restarted.paneLogSeq, stacked.paneLogSeq)
	assert.True(restarted.paneLogStreaming)
	assert.Equal(int64(3), restarted.paneLogJobID)
	assert.Empty(restarted.paneLogLines, "a restart drops the frozen stale lines")
}

// TestPaneLogPaginateNavFailedReviewCarriesJobID covers Finding 2: the
// failed-review synthesized by handleJobsMsg's pagination auto-navigate was
// the one such site missing the JobID stamp, so the split detail pane's
// dispatcher (which matches currentReview.JobID against the selection)
// could not recognize it as the selected job's review.
func TestPaneLogPaginateNavFailedReviewCarriesJobID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(
		withCurrentView(viewReview),
		withTestJobs(storage.ReviewJob{
			ID: 3, GitRef: "cccc333", Branch: "main", RepoName: "repoA",
			Agent: "codex", Status: storage.JobStatusRunning,
		}),
		withSelection(0, 3),
	)
	m.paginateNav = viewReview
	m.loadingMore = true

	// The appended page's next eligible row is a failed job.
	res, _ := m.handleJobsMsg(jobsMsg{
		append: true,
		jobs: []storage.ReviewJob{
			{
				ID: 1, GitRef: "aaaa111", Branch: "main", RepoName: "repoB",
				Agent: "claude-code", Status: storage.JobStatusFailed, Error: "boom",
			},
		},
	})
	got := res.(model)
	require.NotNil(got.currentReview, "auto-navigate synthesizes the failed review")
	assert.Equal(got.selectedJobID, got.currentReview.JobID,
		"the synthesized review must be attributable to the job it describes")
	assert.Contains(got.currentReview.Output, "boom")
}

// ---------------------------------------------------------------------------
// handleJobsMsg's pagination auto-navigate block and the fix panel:
// closeFixPanelIfJobChanged() runs once, near the top
// of handleJobsMsg (gated on splitActive()), BEFORE this block mutates
// m.selectedIdx/selectedJobID to step to the newly appended job. A fix panel
// still open/pending for the job selected BEFORE that step survives the jump
// bound to the wrong job -- an unfocused panel whose later submission would
// fix the wrong review. The fix calls closeFixPanelIfJobChanged() again
// immediately after each of the two mutating branches (viewReview,
// viewKindPrompt); the viewLog branch already gets this via
// followSelectionChange, which it calls itself.
// ---------------------------------------------------------------------------

// TestPaginationAutoNavClosesStaleFixPanelViewReviewFailedJob covers the
// viewReview arm's Failed-job sub-case, the one the reviewer named
// explicitly: pagination lands on a failed job and installs a synthesized
// review while a fix panel bound to the PREVIOUS job is still open.
func TestPaginationAutoNavClosesStaleFixPanelViewReviewFailedJob(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(
		withCurrentView(viewReview),
		withTestJobs(storage.ReviewJob{ID: 2, Status: storage.JobStatusDone}),
		withSelection(0, 2),
	)
	m.paginateNav = viewReview
	m.loadingMore = true
	// A fix panel is open, bound to job 2 -- the job pagination is about to
	// navigate AWAY from.
	m.reviewFixPanelOpen = true
	m.fixPromptJobID = 2
	m.fixPromptText = "unsubmitted fix prompt"

	res, _ := m.handleJobsMsg(jobsMsg{
		append: true,
		jobs: []storage.ReviewJob{
			{ID: 5, Status: storage.JobStatusFailed, Error: "boom"},
		},
	})
	got := res.(model)

	require.Equal(int64(5), got.selectedJobID, "pagination must have navigated to the new job")
	require.NotNil(got.currentReview)
	assert.Equal(int64(5), got.currentReview.JobID)
	assert.False(got.reviewFixPanelOpen,
		"a fix panel bound to the job pagination navigated AWAY from must not survive the jump")
	assert.Equal(int64(0), got.fixPromptJobID,
		"must not stay bound to the stale job -- a later submission would target the wrong review")
}

// TestPaginationAutoNavClosesStaleFixPanelViewReviewDoneJob covers the
// viewReview arm's Done-job sub-case, which dispatches a fetch and RETURNS
// EARLY -- verifying the fix panel is still closed on that early-return path,
// not only the Failed sub-case that falls through to the function's end.
func TestPaginationAutoNavClosesStaleFixPanelViewReviewDoneJob(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(
		withCurrentView(viewReview),
		withTestJobs(storage.ReviewJob{ID: 2, Status: storage.JobStatusDone}),
		withSelection(0, 2),
	)
	m.paginateNav = viewReview
	m.loadingMore = true
	m.reviewFixPanelOpen = true
	m.fixPromptJobID = 2

	res, cmd := m.handleJobsMsg(jobsMsg{
		append: true,
		jobs:   []storage.ReviewJob{{ID: 5, Status: storage.JobStatusDone}},
	})
	got := res.(model)

	require.NotNil(cmd, "the Done sub-case dispatches a review fetch and returns early")
	require.Equal(int64(5), got.selectedJobID)
	assert.False(got.reviewFixPanelOpen, "the early return must not skip the panel cleanup")
	assert.Equal(int64(0), got.fixPromptJobID)
}

// TestPaginationAutoNavClosesStaleFixPanelPromptView covers the
// viewKindPrompt arm -- the second switch case the reviewer asked to audit
// alongside viewReview.
func TestPaginationAutoNavClosesStaleFixPanelPromptView(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(
		withCurrentView(viewKindPrompt),
		withTestJobs(storage.ReviewJob{ID: 2, Status: storage.JobStatusDone}),
		withSelection(0, 2),
	)
	m.paginateNav = viewKindPrompt
	m.loadingMore = true
	m.reviewFixPanelOpen = true
	m.fixPromptJobID = 2

	res, _ := m.handleJobsMsg(jobsMsg{
		append: true,
		jobs:   []storage.ReviewJob{{ID: 5, Status: storage.JobStatusDone}},
	})
	got := res.(model)

	require.Equal(int64(5), got.selectedJobID)
	assert.False(got.reviewFixPanelOpen,
		"the prompt-view arm must not leave a panel bound to the job pagination navigated away from")
	assert.Equal(int64(0), got.fixPromptJobID)
}

// TestPaginationAutoNavKeepsFixPanelBoundToNewSelection is the negative
// case: a panel already bound to the job pagination is ABOUT TO select must
// be left alone -- closeFixPanelIfJobChanged only acts when the job actually
// changed. Uses the viewKindPrompt arm deliberately: splitActive() excludes
// viewKindPrompt, so the EARLIER splitActive()-gated closeFixPanelIfJobChanged
// call near the top of handleJobsMsg (which compares against the selection
// BEFORE this pagination jump, still job 2 here) does not run and cannot
// interfere -- the panel reaches this block still bound to job 5 so the fix's
// own guard is what's actually under test.
func TestPaginationAutoNavKeepsFixPanelBoundToNewSelection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(
		withCurrentView(viewKindPrompt),
		withTestJobs(storage.ReviewJob{ID: 2, Status: storage.JobStatusDone}),
		withSelection(0, 2),
	)
	m.paginateNav = viewKindPrompt
	m.loadingMore = true
	// Panel already bound to job 5 -- the job pagination is about to select.
	m.reviewFixPanelOpen = true
	m.fixPromptJobID = 5

	res, _ := m.handleJobsMsg(jobsMsg{
		append: true,
		jobs:   []storage.ReviewJob{{ID: 5, Status: storage.JobStatusDone}},
	})
	got := res.(model)

	require.Equal(int64(5), got.selectedJobID)
	assert.True(got.reviewFixPanelOpen, "a panel already bound to the newly selected job must be left alone")
	assert.Equal(int64(5), got.fixPromptJobID)
}

// ---------------------------------------------------------------------------
// Enter is a no-op with the queue focused in split layout -- the
// detail pane already follows the cursor, so focusing the detail pane on
// Enter (the old behavior, same as tab) read as "nothing happened".
// ---------------------------------------------------------------------------

// TestEnterKeyNoOpInSplitListFocus covers the split, list-focus case: Enter
// on a done job must not move focus, change the view, dispatch a fetch, or
// flash anything -- the pane already shows the review for the selected job.
func TestEnterKeyNoOpInSplitListFocus(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withSelection(1, 2)) // job 2: done
	m.focus = focusList
	prevFlash := m.flashMessage

	res, cmd := m.handleEnterKey()
	got := res.(model)

	assert.Nil(cmd, "no fetch should be dispatched")
	assert.Equal(focusList, got.focus)
	assert.Equal(viewQueue, got.currentView)
	assert.Equal(prevFlash, got.flashMessage, "no flash message")
}

// TestEnterKeyNoOpInSplitListFocusRunningJob covers the same no-op for a
// queued/running job -- previously Enter would flash "no review yet" even
// though the split pane already shows the job's live status/log.
func TestEnterKeyNoOpInSplitListFocusRunningJob(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withSelection(0, 3)) // job 3: running
	m.focus = focusList

	res, cmd := m.handleEnterKey()
	got := res.(model)

	assert.Nil(cmd)
	assert.Equal(focusList, got.focus)
	assert.Equal(viewQueue, got.currentView)
	assert.Empty(got.flashMessage, "the now-pointless 'no review yet' flash must be suppressed in split")
}

// TestEnterKeyNoOpInSplitListFocusFailedJob covers the failed-job branch:
// Enter must not synthesize the failed-job error review or stamp focusDetail
// in split -- that branch becomes unreachable from the queue via Enter in
// split layout.
func TestEnterKeyNoOpInSplitListFocusFailedJob(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withSelection(2, 1)) // job 1: failed
	m.focus = focusList

	res, cmd := m.handleEnterKey()
	got := res.(model)

	assert.Nil(cmd)
	assert.Equal(focusList, got.focus)
	assert.Equal(viewQueue, got.currentView)
	assert.Nil(got.currentReview)
}

// TestEnterKeyStillDispatchesFetchInStackedLayout is a companion assertion
// to TestReviewMsgDoesNotClobberTransientView / TestReviewMsgRespectsDispatchOriginQueueThenTasks
// above: those already cover Enter dispatching the review fetch on a done
// job in the (default) stacked layout. This test names that behavior
// directly and pins it against the split no-op added in this task.
func TestEnterKeyStillDispatchesFetchInStackedLayout(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := initTestModel(
		withCurrentView(viewQueue),
		withDimensions(150, 40),
		withTestJobs(testQueueJobs()...),
		withSelection(1, 2), // job 2: done
	)
	require.Equal(layoutStacked, m.layout)

	res, cmd := m.handleEnterKey()
	got := res.(model)

	require.NotNil(cmd, "stacked layout must still dispatch the review fetch")
	assert.Equal(viewQueue, got.currentView, "view flips once the fetch resolves, not synchronously")
}

// TestSplitFooterListFocusOmitsEnterHint covers the footer side of the same
// change: the split, list-focus footer must not advertise "review" on
// enter (it's now a no-op there) but must still show the tab hint used to
// focus the detail pane.
func TestSplitFooterListFocusOmitsEnterHint(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withSelection(1, 2))
	m.focus = focusList

	rows := m.splitFooterRows()

	var sawEnter, sawTab bool
	for _, row := range rows {
		for _, item := range row {
			if item.key == "↵" {
				sawEnter = true
			}
			if item.key == "tab" {
				sawTab = true
				assert.Equal("focus detail", item.desc)
			}
		}
	}
	assert.False(sawEnter, "split list-focus footer must not advertise enter/review")
	assert.True(sawTab, "split list-focus footer must still advertise tab as the focus affordance")
}

// ---------------------------------------------------------------------------
// PR feedback fixes: fix panel visibility, stale-review guards on detail
// focus entry, stale review after rerun, and error text sanitization.
// ---------------------------------------------------------------------------

// TestReviewPaneFixPanelFocusedRendersInline covers Finding 1: the split
// detail pane previously ignored m.reviewFixPanelOpen entirely, so pressing
// F while detail-focused opened the fix prompt (handleKeyMsg routes
// keystrokes to handleReviewFixPanelKey) but the pane kept showing only the
// review body -- the user typed blind. The panel must now render inline,
// below the review body, with the body's window shrunk to make room.
func TestReviewPaneFixPanelFocusedRendersInline(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withReview(splitTestReview()), withFixPanel(true, true))
	m.fixPromptText = "add nil check"

	lines := m.renderDetailPane(88, 25)
	assert.Len(lines, 25, "total pane height must stay exactly innerH")
	joined := strings.Join(lines, "\n")
	assert.Contains(joined, "first finding", "review body must still render above the panel")
	assert.Contains(joined, " > add nil check_", "focused input line must show the prompt text and cursor")
	assert.Contains(joined, "tab: scroll review | enter: submit | esc: cancel")
}

// TestReviewPaneFixPanelUnfocusedRendersDimmed covers the unfocused variant
// of Finding 1: panel open but keyboard focus still on the pane/list, shown
// dimmed with the default-prompt hint.
func TestReviewPaneFixPanelUnfocusedRendersDimmed(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withReview(splitTestReview()), withFixPanel(true, false))

	lines := m.renderDetailPane(88, 25)
	assert.Len(lines, 25)
	joined := strings.Join(lines, "\n")
	assert.Contains(joined, "Fix (Tab to focus)")
	assert.Contains(joined, "(blank = default)")
	assert.Contains(joined, "F: fix | tab: focus fix panel")
}

// TestReviewPaneFixPanelClosedRendersUnchanged is the Finding 1 baseline:
// with the panel closed, rendering must be byte-identical to before this
// fix (no panel markers, no height change).
func TestReviewPaneFixPanelClosedRendersUnchanged(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withReview(splitTestReview()))
	baseline := m.renderDetailPane(88, 25)

	m.reviewFixPanelOpen = false
	m.reviewFixPanelFocused = false
	closed := m.renderDetailPane(88, 25)

	assert.Equal(baseline, closed, "panel closed must render identically to the no-panel baseline")
	joined := strings.Join(closed, "\n")
	assert.NotContains(joined, "Fix (Tab to focus)")
	assert.NotContains(joined, "tab: scroll review")
}

// TestReviewPaneFixPanelReservesHeightFromScrollInfo checks that
// reviewPaneScrollInfo's line math (shared with renderReviewPaneBody via
// reviewPaneHeaderLines/reviewPaneBodyLines) accounts for the panel's
// reserved rows, so the split info line's "[x-y of z lines]" stays honest
// once the panel is showing.
func TestReviewPaneFixPanelReservesHeightFromScrollInfo(t *testing.T) {
	assert := assert.New(t)
	rev := splitTestReview()
	var sb strings.Builder
	for i := range 60 {
		fmt.Fprintf(&sb, "line %d of output\n\n", i)
	}
	rev.Output = sb.String()

	closedM := splitModel(withReview(rev))
	_, closedEnd, closedTotal := closedM.reviewPaneScrollInfo(88, 25)

	openM := splitModel(withReview(rev), withFixPanel(true, true))
	_, openEnd, openTotal := openM.reviewPaneScrollInfo(88, 25)

	assert.Equal(closedTotal, openTotal, "total line count is unaffected by the panel")
	assert.Less(openEnd, closedEnd, "the panel must shrink the visible body window")
}

// TestSplitTabKeyNoOpWhenSelectedReviewStale covers Finding 2: after
// selecting a running/queued job, m.currentReview can still hold a
// PREVIOUSLY selected job's review (the follow-fetch hasn't caught up, or
// never will for a job with no review). tab must not enter detail focus in
// that case -- doing so would hand review actions (close/comment/fix) to
// the wrong job.
func TestSplitTabKeyNoOpWhenSelectedReviewStale(t *testing.T) {
	assert := assert.New(t)
	// currentReview is for job 2; selection is on job 3 (running).
	m := splitModel(withReview(splitTestReview()), withSelection(0, 3))

	res, _ := m.handleTabKey()
	got := res.(model)
	assert.Equal(focusList, got.focus, "must not enter detail focus with a stale review for a different job")
	assert.Equal(viewQueue, got.currentView)

	// Selection back on the matching job: tab works normally again.
	got.selectedIdx, got.selectedJobID = 1, 2
	res2, _ := got.handleTabKey()
	got2 := res2.(model)
	assert.Equal(focusDetail, got2.focus)
	assert.Equal(viewReview, got2.currentView)
}

// TestSplitMouseClickIntoDetailNoOpWhenSelectedReviewStale is the mouse
// counterpart of TestSplitTabKeyNoOpWhenSelectedReviewStale: a click into
// the detail pane must not enter detail focus either while the loaded
// review belongs to a different job than the one currently selected.
func TestSplitMouseClickIntoDetailNoOpWhenSelectedReviewStale(t *testing.T) {
	assert := assert.New(t)
	// currentReview is for job 2; selection is on job 3 (running).
	m := splitModel(withReview(splitTestReview()), withSelection(0, 3))
	g := splitGeometry(150, 40, len(reflowHelpRows(m.splitFooterRows(), 150)))

	res, _ := m.handleSplitMouse(mouseClickAt(g.listOuterW+5, 10))
	got := res.(model)
	assert.Equal(focusList, got.focus, "detail-side click must not enter detail focus with a stale review")
	assert.Equal(viewQueue, got.currentView)
}

// TestRerunResultClearsStaleCurrentReview covers Finding 3: a rerun reuses
// the same job ID, so once the rerun completes `currentReview.JobID ==
// job.ID` would wrongly keep matching the review from the PREVIOUS attempt.
// On a confirmed-successful rerun of the job whose review is currently
// loaded, currentReview (and its dependent state) must be cleared so the
// follow/reconcile machinery refetches once the rerun finishes.
func TestRerunResultClearsStaleCurrentReview(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withReview(splitTestReview()), withSelection(1, 2)) // job 2 done, review loaded
	m.currentResponses = []storage.Response{{ID: 1}}
	m.reviewScroll = 12
	m.splitDetailErr = errors.New("stale")

	res, _ := m.handleRerunResultMsg(rerunResultMsg{jobID: 2})
	got := res.(model)
	assert.Nil(got.currentReview)
	assert.Nil(got.currentResponses)
	assert.Equal(0, got.reviewScroll)
	assert.NoError(got.splitDetailErr)
}

// TestRerunResultMsgErrRetainsCurrentReview covers Finding 3's failure
// path: a failed rerun request must leave the previously loaded review in
// place (only the optimistic job-state fields are rolled back).
func TestRerunResultMsgErrRetainsCurrentReview(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withReview(splitTestReview()), withSelection(1, 2))

	res, _ := m.handleRerunResultMsg(rerunResultMsg{jobID: 2, err: errors.New("boom")})
	got := res.(model)
	assert.NotNil(got.currentReview)
	assert.Equal(int64(2), got.currentReview.JobID)
}

// TestRerunResultMsgLeavesUnrelatedReviewAlone covers Finding 3's JobID
// guard: a rerun confirmation for a DIFFERENT job than the one currently
// loaded (e.g. rerunning from the queue while a different review is open)
// must not clear currentReview.
func TestRerunResultMsgLeavesUnrelatedReviewAlone(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withReview(splitTestReview()), withSelection(1, 2)) // review for job 2

	res, _ := m.handleRerunResultMsg(rerunResultMsg{jobID: 3}) // rerunning job 3, unrelated
	got := res.(model)
	require.NotNil(t, got.currentReview)
	assert.Equal(int64(2), got.currentReview.JobID)
}

// TestRerunResultThenJobDoneTriggersFollowFetch is the reviewer's
// queued-to-done transition scenario for Finding 3: after a rerun
// confirmation clears the stale review, the job later reappearing as done
// in a jobsMsg must make splitReconcileDetail issue a fresh follow fetch
// rather than silently doing nothing (which is what the stale JobID match
// used to cause).
func TestRerunResultThenJobDoneTriggersFollowFetch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withReview(splitTestReview()), withSelection(1, 2)) // job 2 done, review loaded

	res, _ := m.handleRerunResultMsg(rerunResultMsg{jobID: 2})
	m = res.(model)
	require.Nil(m.currentReview)

	// The job later reports done again (rerun completed).
	jobs := testQueueJobs()
	res2, cmd := m.handleJobsMsg(jobsMsg{jobs: jobs, stats: storage.JobStats{}})
	got := res2.(model)
	assert.Equal(int64(2), got.selectedJobID)
	assert.NotNil(cmd, "splitReconcileDetail must issue a follow fetch to reload the review after rerun")
}

// TestDetailPaneFailedJobSanitizesError covers Finding 4: job.Error is
// untrusted process/agent output rendered directly in the failed-job
// branch (before any follow-fetch has synthesized a sanitized pseudo-
// review). It must be run through sanitizeForDisplay so it can't inject
// terminal escapes -- OSC clipboard writes, CSI cursor/screen control, or
// \r/\b overwrite tricks.
func TestDetailPaneFailedJobSanitizesError(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withSelection(2, 1)) // job 1: failed
	m.currentReview = nil
	m.jobs[2].Error = "boom \x1b]52;c;evil\x07 \x1b[2Jmore\rtext\x08here"

	lines := m.renderDetailPane(88, 25)
	joined := strings.Join(lines, "\n")
	assert.Contains(joined, "boom")
	assert.Contains(joined, "moretexthere")
	// The pane's own chrome (titleStyle, statusStyle, ...) legitimately emits
	// SGR color codes, so assert on the malicious payload specifically
	// rather than banning \x1b outright: no OSC/CSI injection sequences,
	// and no raw \r/\x08 survive anywhere in the rendered output.
	assert.NotContains(joined, "\x1b]52")
	assert.NotContains(joined, "\x1b[2J")
	assert.NotContains(joined, "\r")
	assert.NotContains(joined, "\x08")
}

// ---------------------------------------------------------------------------
// Follow-up: normalizeSplitState's viewReview/currentReview repair must
// hold regardless of layout, not just in split. Scoped review finding: a
// rerun-success clear (Finding 3) has no view guard, and a control-socket
// rerun (unlike the interactive rerun key, which requires currentView ==
// viewQueue) can complete while a full-screen review of the SAME job is
// open in STACKED layout. Split self-heals via normalizeSplitState's
// existing viewReview branch; stacked previously had no equivalent, so
// currentView was left dangling on viewReview with a nil currentReview --
// viewContent's nil guard silently falls back to rendering the queue while
// currentView (and therefore key routing) stays on viewReview, so
// arrow/page keys keep manipulating the invisible reviewScroll instead of
// the queue until the user presses esc.
// ---------------------------------------------------------------------------

// TestNormalizeSplitStateRepairsStaleReviewViewStacked is scenario (a) from
// the review: stacked layout, full-screen review of job X open, a
// rerunResultMsg success for X arrives (e.g. via the control socket, which
// unlike the interactive rerun key has no currentView guard). Going through
// the full Update() chokepoint (which calls normalizeSplitState after every
// handler), currentView must land back on viewQueue -- a visible, coherent
// view -- rather than staying on viewReview with nothing loaded, and a
// subsequent down-key must move the queue cursor, not the orphaned
// reviewScroll.
func TestNormalizeSplitStateRepairsStaleReviewViewStacked(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := initTestModel(
		withCurrentView(viewReview),
		withDimensions(80, 24),
		withTestJobs(testQueueJobs()...),
		withSelection(1, 2), // job 2: done, review open
		withReview(splitTestReview()),
	)
	m.layout = layoutStacked

	res, _ := m.Update(rerunResultMsg{jobID: 2})
	got, ok := res.(model)
	require.True(ok)
	assert.Equal(viewQueue, got.currentView, "must repair to a visible, coherent view even in stacked layout")
	assert.Nil(got.currentReview)
	assert.Equal(0, got.reviewScroll)

	// A down-key now moves the queue cursor, not the (now-meaningless)
	// reviewScroll -- proving key routing actually followed the repaired
	// view rather than a redraw-only fix.
	prevSelected := got.selectedJobID
	res2, _ := got.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	got2, ok := res2.(model)
	require.True(ok)
	assert.NotEqual(prevSelected, got2.selectedJobID, "down must move the queue cursor now that the view is repaired")
	assert.Equal(0, got2.reviewScroll, "reviewScroll must not have absorbed the down-key")
}

// TestNormalizeSplitStateRerunStillHealsInSplit is scenario (b): the split
// equivalent of the stacked repair above must keep behaving exactly as it
// did before this fix -- the detail pane falls back to its loading/status
// rendering and focus is normalized back to the list.
func TestNormalizeSplitStateRerunStillHealsInSplit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withReview(splitTestReview()), withSelection(1, 2)) // job 2 done, review loaded
	m.focus = focusDetail
	m.currentView = viewReview

	res, _ := m.Update(rerunResultMsg{jobID: 2})
	got, ok := res.(model)
	require.True(ok)
	assert.Equal(viewQueue, got.currentView)
	assert.Equal(focusList, got.focus, "split focus must be normalized back to the list")
	assert.Nil(got.currentReview)

	// The detail pane renders its fallback (loading/status card) rather
	// than panicking or showing stale content -- job 2 still reports done
	// in m.jobs, so this is the "loading" branch pending a fresh
	// follow-fetch.
	lines := strings.Join(got.renderDetailPane(88, 20), "\n")
	assert.Contains(lines, "Loading")
}

// TestNormalizeSplitStateRerunOfDifferentJobLeavesReviewOpen is scenario
// (c): a stacked full-screen review of job X must stay open, untouched,
// when a DIFFERENT job's rerun is confirmed -- the JobID guard in
// handleRerunResultMsg (Finding 3) must not over-fire.
func TestNormalizeSplitStateRerunOfDifferentJobLeavesReviewOpen(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := initTestModel(
		withCurrentView(viewReview),
		withDimensions(80, 24),
		withTestJobs(testQueueJobs()...),
		withSelection(1, 2), // job 2: done, review open
		withReview(splitTestReview()),
	)
	m.layout = layoutStacked

	res, _ := m.Update(rerunResultMsg{jobID: 3}) // unrelated job
	got, ok := res.(model)
	require.True(ok)
	assert.Equal(viewReview, got.currentView, "an unrelated job's rerun must not touch the open review")
	require.NotNil(got.currentReview)
	assert.Equal(int64(2), got.currentReview.JobID)
}

// ---------------------------------------------------------------------------
// A race in the rerun invalidation above: clearing currentReview on rerun
// success does not by itself invalidate an ALREADY IN-FLIGHT follow fetch
// for the same job:
//
//   1. Follow fetch for job X dispatched (75ms debounce fired), stamped
//      with the gen it was dispatched at.
//   2. Rerun of X confirms -- handleRerunResultMsg's clear nils
//      currentReview.
//   3. The OLD fetch's reviewMsg lands. Its jobID staleness gate
//      (jobID == selectedJobID) still passes -- selection never moved --
//      so the follow path restored the PREVIOUS attempt's review right
//      back into currentReview.
//   4. When the rerun completes, splitReconcileDetail sees
//      currentReview.JobID == X and skips fetching the new result: the
//      stale review is shown indefinitely.
//
// Fixed by tagging follow fetches with m.detailFollowGen at dispatch
// (reviewMsg.gen, stamped in fetchReviewFollow) and rejecting a follow
// response in handleReviewMsg whose gen no longer matches
// m.detailFollowGen -- which handleRerunResultMsg's rerun-success clear now
// also bumps, so step 3's stale response is dropped instead of landing.
// ---------------------------------------------------------------------------

// TestFollowRejectsPreRerunAttemptAfterRerunClear reproduces the exact
// race: a follow fetch's response, stamped at its dispatch time, arrives
// AFTER a rerun-success clear for the same job. It must be rejected
// (currentReview stays nil, not resurrected), and the subsequent done
// transition must still trigger a fresh fetch rather than being skipped as
// "already loaded."
//
// The rejecting mechanism is the per-job attempt stamp, not
// detailFollowGen, which a confirmed rerun does not bump.
func TestFollowRejectsPreRerunAttemptAfterRerunClear(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withSelection(1, 2), withReview(splitTestReview())) // job 2 done, review loaded

	// A follow fetch for job 2 was dispatched earlier and is still in
	// flight -- capture the gen it was stamped with at dispatch time, the
	// same way fetchReviewFollow does (m.detailFollowGen at call time).
	oldGen := m.detailFollowGen

	// The rerun for job 2 is confirmed while that fetch is still in
	// flight: the clear nils currentReview and bumps job 2's attempt
	// counter.
	res, _ := m.handleRerunResultMsg(rerunResultMsg{jobID: 2})
	m = res.(model)
	require.Nil(m.currentReview)
	require.Positive(m.jobAttemptGen[2])

	// The stale fetch's response lands now, still tagged with the
	// pre-rerun gen and (implicitly) the pre-rerun attempt 0. jobID ==
	// selectedJobID still holds (selection never moved), so only an
	// attempt/gen check can catch it.
	res2, _ := m.handleReviewMsg(reviewMsg{
		review: splitTestReview(), jobID: 2, follow: true, gen: oldGen,
	})
	got := res2.(model)
	assert.Nil(got.currentReview, "a follow response dispatched before the rerun-success gen bump must not resurrect the stale review")

	// The job later reports done again (rerun completed): splitReconcileDetail
	// must issue a fresh follow fetch, not skip it thinking the (correctly
	// nil) currentReview is already loaded.
	res3, cmd := got.handleJobsMsg(jobsMsg{jobs: testQueueJobs(), stats: storage.JobStats{}})
	got2 := res3.(model)
	assert.Equal(int64(2), got2.selectedJobID)
	assert.NotNil(cmd, "must fetch the fresh review instead of leaving the stale one cleared forever")
}

// TestFollowAtCurrentGenLandsNormally is the non-regression counterpart:
// a follow fetch dispatched at (and still tagged with) the current
// generation must land exactly as before this fix.
func TestFollowAtCurrentGenLandsNormally(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withSelection(1, 2))
	// A real selection move bumps detailFollowGen; the fetch dispatched
	// for the new selection is stamped with the post-bump value.
	m, _ = m.scheduleDetailFollow()

	res, _ := m.handleReviewMsg(reviewMsg{
		review: splitTestReview(), jobID: 2, follow: true, gen: m.detailFollowGen,
	})
	got := res.(model)
	require.NotNil(t, got.currentReview, "a follow response at the current gen must still land")
	assert.Equal(int64(2), got.currentReview.JobID)
}

// TestFetchReviewFollowStampsCurrentGen is a direct unit check on the
// stamping half of the fix: fetchReviewFollow's dispatched reviewMsg
// carries whatever m.detailFollowGen was at call time.
func TestFetchReviewFollowStampsCurrentGen(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	_, m := mockServerModel(t, mockReviewHandler(*splitTestReview(), nil))
	m.layout = layoutSplit
	m.jobs = testQueueJobs()
	m.selectedIdx, m.selectedJobID = 1, 2
	m.detailFollowGen = 7

	cmd := m.fetchReviewFollow(2, 9)
	msg := cmd()
	rm, ok := msg.(reviewMsg)
	require.True(ok, "expected a reviewMsg, got %T", msg)
	assert.True(rm.follow)
	assert.Equal(uint64(7), rm.gen)
	assert.Equal(uint64(9), rm.fetchSeq, "fetchSeq must be stamped through verbatim")
}

// TestFetchReviewStampsCurrentGen is TestFetchReviewFollowStampsCurrentGen's
// counterpart for every
// fetchReview call, not just the follow wrapper: a regular (non-follow)
// fetch also carries whatever m.detailFollowGen was at call time.
func TestFetchReviewStampsCurrentGen(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	_, m := mockServerModel(t, mockReviewHandler(*splitTestReview(), nil))
	m.jobs = testQueueJobs()
	m.selectedIdx, m.selectedJobID = 1, 2
	m.detailFollowGen = 9

	cmd := m.fetchReview(2, 1)
	msg := cmd()
	rm, ok := msg.(reviewMsg)
	require.True(ok, "expected a reviewMsg, got %T", msg)
	assert.False(rm.follow)
	assert.Equal(uint64(9), rm.gen)
	assert.Equal(uint64(1), rm.fetchSeq, "an ordinary fetch is stamped with the shared epoch too")
}

// TestQueueEnterLandsAtUnchangedGenInStackedMode is the non-regression case
// for widening the gen check to non-follow fetches: a plain queue Enter in
// stacked mode (detailFollowGen is a split-only mechanism, so nothing ever
// bumps it here) must still open the review normally when the response
// lands.
func TestQueueEnterLandsAtUnchangedGenInStackedMode(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	_, m := mockServerModel(t, mockReviewHandler(*splitTestReview(), nil))
	m.currentView = viewQueue
	m.jobs = testQueueJobs()
	m.selectedIdx, m.selectedJobID = 1, 2 // job 2: done
	require.Equal(layoutStacked, m.layout)

	cmd := m.enterReviewCmd(m.jobs[1])
	msg := cmd()
	rm, ok := msg.(reviewMsg)
	require.True(ok, "expected a reviewMsg, got %T", msg)
	assert.Equal(uint64(0), rm.gen, "gen must be unchanged (0) -- nothing bumps detailFollowGen in stacked mode")

	res, _ := m.handleReviewMsg(rm)
	got := res.(model)
	require.NotNil(got.currentReview, "a legitimate stacked-mode fetch must still land")
	assert.Equal(viewReview, got.currentView)
}

// TestNonFollowFetchRejectedAfterInterveningSelectionChange is the stale
// case for the same fix: a non-follow fetch dispatched for job 2, with a
// split-mode selection change (which bumps detailFollowGen) landing before
// the response arrives, must be rejected -- even though the response's
// jobID still equals the (now-restored) selectedJobID, so the pre-existing
// jobID-only check alone would have let it through.
func TestNonFollowFetchRejectedAfterInterveningSelectionChange(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withSelection(1, 2)) // job 2 selected

	// A non-follow fetch for job 2 (e.g. stepReviewNav) was dispatched and
	// is still in flight, stamped with the gen at that time.
	oldGen := m.detailFollowGen

	// The cursor moves away and back to job 2 before the fetch resolves --
	// each move goes through the real handleKeyMsg -> scheduleDetailFollow
	// path (the production mechanism, per handlers.go's post-processing
	// wrapper: a split-mode selection change bumps detailFollowGen), so by
	// the time the cursor lands back on job 2 the gen has moved even though
	// the selected job hasn't.
	m, _ = pressSpecial(m, tea.KeyDown)
	m, _ = pressSpecial(m, tea.KeyUp)
	require.Equal(int64(2), m.selectedJobID)
	require.Greater(m.detailFollowGen, oldGen)

	res, _ := m.handleReviewMsg(reviewMsg{
		review: splitTestReview(), jobID: 2, gen: oldGen,
	})
	got := res.(model)
	assert.Nil(got.currentReview, "a non-follow fetch dispatched before an intervening away-and-back gen bump must be rejected")
}

// TestNonFollowFetchRejectedAfterSameJobRerunConfirms is Finding A's core
// repro: a regular (non-follow) fetchReview for job 2 -- e.g. a queue Enter
// or stepReviewNav re-fetch -- is still in flight when a rerun of that same
// selected job is confirmed. Pre-fix, only fetchReviewFollow stamped gen, so
// this response passed the jobID==selectedJobID check alone (a rerun
// doesn't move the selection) and resurrected the previous attempt's
// review.
func TestNonFollowFetchRejectedAfterSameJobRerunConfirms(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withSelection(1, 2), withReview(splitTestReview())) // job 2, review loaded
	m.currentView = viewQueue

	oldGen := m.detailFollowGen

	// Rerun of job 2 (the currently displayed job) confirms while the
	// stale fetch is still in flight.
	res, _ := m.handleRerunResultMsg(rerunResultMsg{jobID: 2})
	m = res.(model)
	require.Nil(m.currentReview)
	require.Positive(m.jobAttemptGen[2])

	// The stale (pre-rerun) fetch's response lands now, still tagged with
	// oldGen and the pre-rerun attempt 0.
	res2, _ := m.handleReviewMsg(reviewMsg{
		review: splitTestReview(), jobID: 2, gen: oldGen, dispatchedFrom: viewQueue,
	})
	got := res2.(model)
	assert.Nil(got.currentReview, "a non-follow fetch dispatched before the rerun must not resurrect the stale review")
}

// TestSelectedJobRerunInvalidatesFetchEvenWhileReviewLoading covers Finding
// B: the invalidation on a confirmed rerun used to be gated on
// m.currentReview already matching the reran job, so a rerun confirmed
// while the pane was still LOADING (currentReview nil, an earlier follow
// fetch in flight) invalidated nothing at all. The stale in-flight fetch's
// response would then pass every check unchanged, populate currentReview
// with the OLD attempt's content, and block the rerun's real result from
// ever being fetched. (The invalidation is the per-job attempt counter,
// which is what the assertion below names.)
func TestSelectedJobRerunInvalidatesFetchEvenWhileReviewLoading(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withSelection(0, 3)) // job 3: running, selected
	m.currentReview = nil                // pane still loading -- no review yet

	oldGen := m.detailFollowGen

	res, _ := m.handleRerunResultMsg(rerunResultMsg{jobID: 3})
	got := res.(model)
	assert.Positive(got.jobAttemptGen[3],
		"the attempt counter must bump for a confirmed rerun even while currentReview is still nil")

	// The stale in-flight follow response lands, tagged with the pre-rerun
	// gen and attempt.
	res2, _ := got.handleReviewMsg(reviewMsg{
		review: splitTestReview(), jobID: 3, follow: true, gen: oldGen,
	})
	got2 := res2.(model)
	assert.Nil(got2.currentReview, "the stale follow response must be rejected, not populate currentReview with the old attempt")

	// The job later reports done: reconcile must fetch fresh, not skip
	// thinking a nil currentReview is somehow already the answer.
	doneJobs := testQueueJobs()
	doneJobs[0].Status = storage.JobStatusDone // job 3
	res3, cmd := got2.handleJobsMsg(jobsMsg{jobs: doneJobs, stats: storage.JobStats{}})
	got3 := res3.(model)
	require.Equal(int64(3), got3.selectedJobID)
	assert.NotNil(cmd, "must fetch the fresh review instead of leaving currentReview nil forever")
}

// TestRerunClearsFixPanelForRerunJob covers Finding C: the inline fix panel
// is scoped to whatever review is on screen (fixPromptJobID stamped from
// that review's job), so invalidating currentReview on rerun success must
// also close the panel -- otherwise, once the view repairs back to the
// queue and the user opens a DIFFERENT review, the stale panel renders over
// it and submitting would target the reran job instead of the one
// displayed. handleRerunResultMsg is reached identically whether the rerun
// was dispatched via the key handler (handleRerunKey) or the control-socket
// path (handleCtrlRerunJob) -- both just call rerunJob, whose returned cmd
// produces the rerunResultMsg this handler processes -- so exercising the
// handler directly covers both dispatch paths.
func TestRerunClearsFixPanelForRerunJob(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withSelection(1, 2), withReview(splitTestReview())) // job 2 review loaded
	m.reviewFixPanelOpen = true
	m.reviewFixPanelFocused = true
	m.fixPromptText = "some in-progress fix text"
	m.fixPromptJobID = 2

	res, _ := m.handleRerunResultMsg(rerunResultMsg{jobID: 2})
	got := res.(model)

	assert.False(got.reviewFixPanelOpen)
	assert.False(got.reviewFixPanelFocused)
	assert.False(got.reviewFixPanelPending)
	assert.Equal(int64(0), got.fixPromptJobID)
	assert.Empty(got.fixPromptText)
}

// TestFixPanelPendingNotClearedWhenSuperseded: handleReviewMsg's
// early-reject branch clears reviewFixPanelPending
// (and pendingReviewOpenJobID) ONLY on a genuine jobID mismatch, never on
// a gen-mismatch-alone rejection with the selected job UNCHANGED -- a gen
// bump alone does not mean the underlying 'F' request was abandoned (see
// that branch's doc comment for the full derivation: scheduleDetailFollow
// can bump gen via a same-job bootstrap that is explicitly NOT
// abandonment, and handleRerunResultMsg -- the other gen-bumper -- now
// disarms both intents itself at the point it confirms a genuine
// abandonment, rather than leaving it to this reactive path).
//
// The retired test used a real down-then-up cursor move to produce the gen
// bump, believing it isolated "gen mismatch alone" -- it did not: each
// move is ALSO a genuine (if transient) selection change, and
// closeFixPanelIfJobChanged (called from scheduleDetailFollow on the
// intermediate "down" move, before "up" restores the selection) already
// clears the panel via that entirely different, job-comparison-based path
// before handleReviewMsg ever sees the stale response. This test uses a
// direct gen bump
// instead, so it actually isolates handleReviewMsg's OWN rejection logic
// from closeFixPanelIfJobChanged's independent one.
//
// Note on the rescue interaction:
// reviewIntentRescuable serves a gen-stale response for the
// SAME job when it's still the single freshest dispatch (fetchSeq
// matches) AND an intent is armed for it -- so "gen-mismatch alone never
// touches the panel" is not quite true, and would
// be a lie to keep as the test's name. What this pins NOW is narrower but
// still real: a gen-mismatched response that is ALSO NOT the freshest
// dispatch (something newer went out since -- simulated here by advancing
// m.reviewFetchSeq past the message's own fetchSeq) is neither cleared NOR
// served -- it's simply dropped, exactly like a superseded response would
// be, leaving the panel exactly as it was for whatever IS the freshest
// dispatch to eventually resolve. See
// TestFixPanelPendingRescuedOnGenMismatchWhenStillFreshest for the
// sibling case this test no longer covers (rescue).
func TestFixPanelPendingNotClearedWhenSuperseded(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withSelection(1, 2)) // job 2 selected
	m.reviewFixPanelPending = true
	m.fixPromptJobID = 2

	oldGen := m.detailFollowGen
	oldSeq := m.reviewFetchSeq
	// A same-job bootstrap (e.g. maybeBootstrapDetail on a resize/L) or a
	// same-job rerun confirmation -- either way, gen bumps with the
	// selection unchanged. A newer dispatch (e.g. a fresh 'F' or Enter) has
	// ALSO since gone out, superseding this message's own fetchSeq -- so
	// it is not the single freshest one anymore, and must not be rescued.
	m.detailFollowGen++
	m.reviewFetchSeq++

	res, _ := m.handleReviewMsg(reviewMsg{
		review: splitTestReview(), jobID: 2, gen: oldGen, fetchSeq: oldSeq,
	})
	got := res.(model)
	assert.True(got.reviewFixPanelPending,
		"a gen-mismatched, non-freshest response must NOT clear a still-valid pending fix panel")
	assert.Equal(int64(2), got.fixPromptJobID)
	assert.Nil(got.currentReview, "the stale response must still not populate currentReview")
}

// TestFixPanelPendingRescuedOnGenMismatchWhenStillFreshest pins the rescue
// half of the same fix: a gen-mismatched response for the SAME still-
// selected job that IS still the single freshest dispatch (nothing newer
// went out since) must be served -- not silently discarded -- because
// nothing else is guaranteed to ever serve the pending fix panel
// otherwise (a same-job maybeBootstrapDetail bump's own
// debounced follow can be dropped, e.g. if the layout flips back before it
// fires).
func TestFixPanelPendingRescuedOnGenMismatchWhenStillFreshest(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withSelection(1, 2)) // job 2 selected
	m.reviewFixPanelPending = true
	m.fixPromptJobID = 2

	oldGen := m.detailFollowGen
	seq := m.reviewFetchSeq // nothing newer dispatched since
	// A same-job bootstrap bumps gen; nothing else has been dispatched.
	m.detailFollowGen++

	res, _ := m.handleReviewMsg(reviewMsg{
		review: splitTestReview(), jobID: 2, gen: oldGen, fetchSeq: seq, dispatchedFrom: viewQueue,
	})
	got := res.(model)
	assert.True(got.reviewFixPanelOpen,
		"the gen-stale response must be rescued -- nothing else was going to serve this pending fix panel")
	assert.False(got.reviewFixPanelPending)
	assert.NotNil(got.currentReview, "the rescued response's content must be accepted")
}

// ---------------------------------------------------------------------------
// Actions taken while the split LIST is focused mutate queue state and
// must keep currentReview in sync, because currentView stays viewQueue so
// the review-view-only code paths are skipped.
// ---------------------------------------------------------------------------

// TestSplitListCloseFlipsCurrentReviewClosedState covers Finding 1: closing
// a review from the split list (list focus, currentView still viewQueue)
// updated job.Closed/pendingClosed/stats, but never touched
// currentReview.Closed -- so the detail pane's header kept showing the old
// closed state, and reconciliation had no reason to fix it (the job ID
// still matched). A failing closedResultMsg must roll BOTH back together,
// keyed by the same seq.
func TestSplitListCloseFlipsCurrentReviewClosedState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withReview(splitTestReview())) // job 2 selected+loaded, list focus
	m.jobs[1].Closed = new(false)

	res, _ := m.handleCloseKey()
	got := res.(model)
	require.NotNil(got.currentReview)
	assert.True(got.currentReview.Closed, "currentReview.Closed must flip alongside the job when closing from the split list")
	require.NotNil(got.jobs[1].Closed)
	assert.True(*got.jobs[1].Closed)

	lines := strings.Join(got.renderDetailPane(88, 25), "\n")
	assert.Contains(lines, "[CLOSED]", "the pane header must reflect the optimistic close immediately")

	pending, ok := got.pendingClosed[2]
	require.True(ok)
	res2, _ := got.handleClosedResultMsg(closedResultMsg{
		jobID: 2, oldState: false, newState: true, seq: pending.seq,
		err: errors.New("server error"),
	})
	got2 := res2.(model)
	require.NotNil(got2.jobs[1].Closed)
	assert.False(*got2.jobs[1].Closed, "job.Closed must roll back on failure")
	require.NotNil(got2.currentReview)
	assert.False(got2.currentReview.Closed, "currentReview.Closed must roll back alongside the job, not half-apply")
}

// TestSplitListCommentResultRefreshesViaFollow covers Finding 2: a comment
// submitted from the split list (currentView still viewQueue, 'c' works
// from the queue too) never refreshed currentResponses, because
// handleCommentResultMsg's refresh was gated on currentView == viewReview.
// The fix dispatches via fetchReviewFollow (not fetchReview) so the
// response lands through the follow path without switching view or
// stealing focus.
func TestSplitListCommentResultRefreshesViaFollow(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	responses := []storage.Response{{ID: 1, Response: "a new comment"}}
	_, m := mockServerModel(t, mockReviewHandler(*splitTestReview(), responses))
	m.layout = layoutSplit
	m.currentView = viewQueue
	m.focus = focusList
	m.jobs = testQueueJobs()
	m.selectedIdx, m.selectedJobID = 1, 2 // job 2
	m.currentReview = splitTestReview()

	res, cmd := m.handleCommentResultMsg(commentResultMsg{jobID: 2})
	got := res.(model)
	require.NotNil(cmd, "a comment for the selected job in split list focus must trigger a refresh")
	assert.Equal(viewQueue, got.currentView)
	assert.Equal(focusList, got.focus)

	msg := cmd()
	rm, ok := msg.(reviewMsg)
	require.True(ok, "expected a follow reviewMsg, got %T", msg)
	assert.True(rm.follow, "must use fetchReviewFollow (not fetchReview) so landing it doesn't switch view or steal focus")

	res2, _ := got.handleReviewMsg(rm)
	got2 := res2.(model)
	require.Len(got2.currentResponses, 1)
	assert.Equal("a new comment", got2.currentResponses[0].Response)
	assert.Equal(viewQueue, got2.currentView, "landing the follow response must not switch view")
	assert.Equal(focusList, got2.focus, "landing the follow response must not steal focus")
}

// TestFailedJobViaJobsMsgArrivalFocusableThroughReviewPaneBody covers
// A selection landing on a failed job via
// a filter change or initial load -- a jobsMsg-driven arrival, not a
// cursor move -- is handled by the JobStatusFailed case in
// splitReconcileDetail, which runs from handleJobsMsg regardless of how
// the selection got there. This test pins that path explicitly.
func TestFailedJobViaJobsMsgArrivalFocusableThroughReviewPaneBody(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	// Selection already on the failed job (job 1) with no currentReview
	// loaded -- as if just arrived via a filter change or initial load,
	// never having gone through a cursor move (scheduleDetailFollow).
	m := splitModel(withSelection(2, 1))
	m.currentReview = nil

	res, _ := m.handleJobsMsg(jobsMsg{jobs: testQueueJobs(), stats: storage.JobStats{}})
	got := res.(model)
	require.NotNil(got.currentReview, "splitReconcileDetail's Failed branch must synthesize the review on a jobsMsg-driven arrival, not just a cursor move")
	assert.Equal(int64(1), got.currentReview.JobID)

	res2, _ := got.handleTabKey()
	got2 := res2.(model)
	assert.Equal(focusDetail, got2.focus)
	assert.Equal(viewReview, got2.currentView)

	lines := strings.Join(got2.renderDetailPane(88, 25), "\n")
	assert.Contains(lines, "Review #1", "must render through renderReviewPaneBody's scrollable pane-body path, not the card fallback")
	assert.Contains(lines, "boom")
}

// TestSplitPromptEscPreservesCurrentReview and
// TestSplitPromptQuitPreservesCurrentReview cover Finding 4: every
// queue-origin prompt exit (esc, q, and the 'p' toggle) used to nil
// currentReview unconditionally, blanking the split pane to "Loading
// review..." until the next periodic refresh (up to ~15s) even though
// nothing about the review changed.
func TestSplitPromptEscPreservesCurrentReview(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withReview(splitTestReview())) // job 2 selected+loaded
	m.currentView = viewKindPrompt
	m.promptFromQueue = true

	res, _ := m.handleEscKey()
	got := res.(model)
	assert.Equal(viewQueue, got.currentView)
	require.NotNil(got.currentReview, "the review must be retained, not nil'd, so the pane doesn't blank")
	assert.Equal(int64(2), got.currentReview.JobID)

	lines := strings.Join(got.renderDetailPane(88, 25), "\n")
	assert.NotContains(lines, "Loading review")
}

func TestSplitPromptQuitPreservesCurrentReview(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withReview(splitTestReview())) // job 2 selected+loaded
	m.currentView = viewKindPrompt
	m.promptFromQueue = true

	res, _ := m.handleQuitKey()
	got := res.(model)
	assert.Equal(viewQueue, got.currentView)
	require.NotNil(got.currentReview, "the review must be retained, not nil'd, so the pane doesn't blank")
	assert.Equal(int64(2), got.currentReview.JobID)

	lines := strings.Join(got.renderDetailPane(88, 25), "\n")
	assert.NotContains(lines, "Loading review")
}

// TestSplitPromptToggleKeyPreservesCurrentReview covers the third exit site
// (handlePromptKey's own viewKindPrompt branch, reached by pressing 'p'
// again while already in the prompt view) with the same fix.
func TestSplitPromptToggleKeyPreservesCurrentReview(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withReview(splitTestReview())) // job 2 selected+loaded
	m.currentView = viewKindPrompt
	m.promptFromQueue = true

	res, _ := m.handlePromptKey()
	got := res.(model)
	assert.Equal(viewQueue, got.currentView)
	require.NotNil(got.currentReview, "the review must be retained, not nil'd, so the pane doesn't blank")
	assert.Equal(int64(2), got.currentReview.JobID)
}

// TestStackedPromptEscStillClearsCurrentReview is the non-regression
// control for Finding 4: stacked layout has no persistent pane to retain a
// review for, so it must keep the prior nil-and-reload behavior.
func TestStackedPromptEscStillClearsCurrentReview(t *testing.T) {
	assert := assert.New(t)
	m := initTestModel(
		withCurrentView(viewKindPrompt),
		withTestJobs(testQueueJobs()...),
		withSelection(1, 2),
	)
	m.layout = layoutStacked
	m.promptFromQueue = true
	m.currentReview = splitTestReview()

	res, _ := m.handleEscKey()
	got := res.(model)
	assert.Equal(viewQueue, got.currentView)
	assert.Nil(got.currentReview, "stacked layout must keep the prior nil-and-reload behavior")
}

// TestSplitReconcileDetailDetectsSameSecondRerunViaNonTerminalObservation:
// reviewJobCompletionChanged
// compares job.FinishedAt at whole-second (RFC3339) resolution, so a rerun
// that completes within the SAME wall-clock second as the attempt it
// replaced compared equal and the stale review survived. The job must be
// observed queued/running in between (the rerun's non-terminal window) for
// the state-machine signal to catch what the timestamp comparison alone
// cannot.
func TestSplitReconcileDetailDetectsSameSecondRerunViaNonTerminalObservation(t *testing.T) {
	assert := assert.New(t)
	finish := time.Now().Truncate(time.Second) // whole-second, matching storage precision
	m := splitModel(withSelection(1, 2))       // job 2: done
	review := splitTestReview()
	review.Job.FinishedAt = &finish
	m.currentReview = review

	// Observed running again -- the rerun's non-terminal window.
	runningJobs := testQueueJobs()
	runningJobs[1].Status = storage.JobStatusRunning
	m.jobs = runningJobs
	m, _ = m.splitReconcileDetail()

	// Completes within the SAME wall-clock second as the previous attempt.
	doneJobs := testQueueJobs()
	doneJobs[1].FinishedAt = &finish
	m.jobs = doneJobs
	_, cmd := m.splitReconcileDetail()
	assert.NotNil(cmd, "a same-second rerun completion, observed running in between, must still be detected as changed")
}

// TestSplitReconcileDetailReplacesFailedReviewWithChangedMetadataDespiteSameErrorText
// covers Finding 5(b): the Failed branch's idempotency check compared
// rendered error text only, so a rerun that fails with the SAME message but
// different execution metadata (e.g. resolved agent) retained the stale
// synthetic review, including its embedded job snapshot. Output text is
// identical either way (it's built from job.Error alone), so only the
// observed-non-terminal signal can catch this.
func TestSplitReconcileDetailReplacesFailedReviewWithChangedMetadataDespiteSameErrorText(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withSelection(2, 1)) // job 1: failed, "boom", agent claude-code
	m.currentReview = synthesizeFailedReview(&m.jobs[2], nil)
	require.Equal("claude-code", m.currentReview.Job.Agent)

	// Observed running again -- the rerun's non-terminal window.
	runningJobs := testQueueJobs()
	runningJobs[2].Status = storage.JobStatusRunning
	m.jobs = runningJobs
	m, _ = m.splitReconcileDetail()

	// Fails again with the SAME error text but a different resolved agent.
	failedJobs := testQueueJobs()
	failedJobs[2].Agent = "codex"
	m.jobs = failedJobs
	got, cmd := m.splitReconcileDetail()
	assert.NotNil(cmd, "the local rebuild also dispatches the persisted-comments fetch")
	require.NotNil(got.currentReview)
	assert.Equal("codex", got.currentReview.Job.Agent, "must rebuild with the CURRENT job metadata despite identical rendered error text")
}

// ---------------------------------------------------------------------------
// Prompt-view navigation and the split pane's sibling state.
// ---------------------------------------------------------------------------

// TestPromptNavToDifferentJobClearsStaleSiblings covers a hazard of
// preserveOrClearReviewOnQueueReturn:
// split, job X selected with X's review AND X's comments loaded via the
// pane's own follow fetch; 'p' opens the prompt; stepPromptNav (<-/->)
// walks to job Y's prompt, whose fetch (fetchReviewForPrompt) lands in
// handlePromptMsg with currentReview replaced by Y's review while
// currentResponses is still X's -- handlePromptMsg only ever touched
// currentReview. Esc then goes through preserveOrClearReviewOnQueueReturn,
// whose guard only compares currentReview.JobID (now Y) against
// selectedJobID (now Y) and retains it, rendering Y's review with X's
// stale comments underneath. Nothing else catches this: reconcile's
// idempotency checks only ever examine currentReview, never its siblings.
func TestPromptNavToDifferentJobClearsStaleSiblings(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	// Job X (2) selected+loaded with X's review AND X's comments.
	m := splitModel(withReview(splitTestReview())) // JobID: 2
	m.currentResponses = []storage.Response{{ID: 1, Response: "X's comment"}}
	m.currentView = viewKindPrompt
	m.promptFromQueue = true

	// stepPromptNav walked to job Y (1)'s prompt (moveSelectionToJobID
	// already ran); its fetchReviewForPrompt(1) now lands here.
	m.selectedJobID = 1
	yFinishedAt := splitTestFinishedAt
	yReview := &storage.Review{
		ID: 20, JobID: 1, Agent: "codex", Output: "Y's output",
		Job: &storage.ReviewJob{ID: 1, FinishedAt: &yFinishedAt},
	}
	res, _ := m.handlePromptMsg(promptMsg{review: yReview, jobID: 1})
	got := res.(model)
	require.NotNil(got.currentReview)
	assert.Equal(int64(1), got.currentReview.JobID)
	assert.Empty(got.currentResponses, "stale comments from the PREVIOUS job must be cleared when the loaded review's job changes")

	// esc: preserveOrClearReviewOnQueueReturn's guard passes (both sides
	// are job 1 now) and retains the review.
	res2, _ := got.handleEscKey()
	got2 := res2.(model)
	assert.Equal(viewQueue, got2.currentView)
	require.NotNil(got2.currentReview)
	assert.Equal(int64(1), got2.currentReview.JobID)

	lines := strings.Join(got2.renderDetailPane(88, 25), "\n")
	assert.Contains(lines, "Y's output")
	assert.NotContains(lines, "X's comment", "must not render the PREVIOUS job's comments under the new job's review")
}

// TestSplitReconcileDetailRetriesAfterFollowFetchFails:
// paneReviewSeenNonTerminal must clear when the fetch is actually
// ACCEPTED, not at the DECISION to dispatch it. If
// that fetch then fails (handleReviewFollowErrMsg records splitDetailErr
// but leaves currentReview/the flag alone), the fallback signal
// (FinishedAt.After) is blind to a same-second completion and the flag was
// already cleared, so no later reconcile pass would retry -- a stuck stale
// review plus a stuck pane error until manual reselection.
func TestSplitReconcileDetailRetriesAfterFollowFetchFails(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	finish := time.Now().Truncate(time.Second)
	m := splitModel(withSelection(1, 2)) // job 2: done
	review := splitTestReview()
	review.Job.FinishedAt = &finish
	m.currentReview = review

	// Observed running again -- the rerun's non-terminal window.
	runningJobs := testQueueJobs()
	runningJobs[1].Status = storage.JobStatusRunning
	m.jobs = runningJobs
	m, _ = m.splitReconcileDetail()

	// Completes within the SAME wall-clock second: only the
	// observed-non-terminal signal catches this, so a fetch is dispatched.
	doneJobs := testQueueJobs()
	doneJobs[1].FinishedAt = &finish
	m.jobs = doneJobs
	m, cmd := m.splitReconcileDetail()
	require.NotNil(cmd, "the same-second completion, observed running in between, must dispatch a fetch")

	// That fetch FAILS. fetchSeq must match the dispatch's for this error
	// to be accepted rather than dropped as superseded -- m.reviewFetchSeq
	// still holds the value splitReconcileDetail just stamped on the
	// outgoing request above (the suppression slot is released before
	// any staleness check).
	res, _ := m.handleReviewFollowErrMsg(reviewFollowErrMsg{jobID: 2, fetchSeq: m.reviewFetchSeq, err: errors.New("network error")})
	m = res.(model)
	require.Error(m.splitDetailErr)
	require.NotNil(m.currentReview, "a failed fetch must not clear the stale review")

	// A later reconcile pass, nothing new observed, must still retry --
	// the flag must not have been lost when the fetch failed.
	_, cmd2 := m.splitReconcileDetail()
	assert.NotNil(cmd2, "a failed follow fetch must not lose the observed-non-terminal signal -- a later pass must retry")
}

// TestSplitReconcileDetailResetsSignalExactlyOnceOnSuccess is Issue 2's
// non-regression companion: the reset must still happen, exactly once, when
// the follow fetch actually succeeds -- not at every reconcile pass
// thereafter.
func TestSplitReconcileDetailResetsSignalExactlyOnceOnSuccess(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	finish := time.Now().Truncate(time.Second)
	freshReview := *splitTestReview()
	freshReview.Job.FinishedAt = &finish
	_, m := mockServerModel(t, mockReviewHandler(freshReview, nil))
	m.layout = layoutSplit
	m.jobs = testQueueJobs()
	m.selectedIdx, m.selectedJobID = 1, 2
	oldReview := splitTestReview()
	oldReview.Job.FinishedAt = &finish
	m.currentReview = oldReview

	runningJobs := testQueueJobs()
	runningJobs[1].Status = storage.JobStatusRunning
	m.jobs = runningJobs
	m, _ = m.splitReconcileDetail()

	doneJobs := testQueueJobs()
	doneJobs[1].FinishedAt = &finish
	m.jobs = doneJobs
	m, cmd := m.splitReconcileDetail()
	require.NotNil(cmd)

	msg := cmd()
	rm, ok := msg.(reviewMsg)
	require.True(ok, "expected a follow reviewMsg, got %T", msg)

	res, _ := m.handleReviewMsg(rm)
	m = res.(model)
	require.NotNil(m.currentReview)

	_, cmd2 := m.splitReconcileDetail()
	assert.Nil(cmd2, "the reset must happen exactly once, on acceptance -- a subsequent pass with no new observation must not refetch again")
}

// TestStepReviewNavFailedReviewClearsStaleResponses covers Issue 3: of the
// five synthesizeFailedReview call sites, three (stepReviewNav here,
// handleEnterKey, the pagination auto-nav) cleared only currentBranch, not
// currentResponses -- so a failed job's synthetic review (which has no
// comments of its own) could render the PREVIOUS job's stale comments.
// This test covers stepReviewNav as the representative site.
func TestStepReviewNavFailedReviewClearsStaleResponses(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	// Job 2 (done) selected with stale comments loaded; stepReviewNav
	// walks to job 1 (failed, dir=1 is "older" per its doc comment).
	m := splitModel(withReview(splitTestReview()), withSelection(1, 2))
	m.currentView = viewReview
	m.currentResponses = []storage.Response{{ID: 1, Response: "stale comment for job 2"}}

	res, _ := m.stepReviewNav(1)
	got := res.(model)
	require.NotNil(got.currentReview)
	assert.Equal(int64(1), got.currentReview.JobID)
	assert.Empty(got.currentResponses, "a synthetic failed review has no comments of its own -- stale siblings must be cleared")
}

// TestControlSocketCloseFlipsCurrentReviewClosedState covers Issue 5: the
// control-socket close route (handleCtrlCloseReview) sets pendingClosed
// and must also flip currentReview.Closed, like the key path does.
func TestControlSocketCloseFlipsCurrentReviewClosedState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withReview(splitTestReview())) // job 2 selected+loaded
	m.jobs[1].Closed = new(false)

	params, err := json.Marshal(map[string]any{"job_id": int64(2), "closed": true})
	require.NoError(err)
	got, resp, cmd := m.handleCtrlCloseReview(params)
	require.True(resp.OK, "expected OK, got error: %s", resp.Error)
	assert.NotNil(cmd)
	require.NotNil(got.currentReview)
	assert.True(got.currentReview.Closed, "currentReview.Closed must flip via the control-socket close route too")
}

// ---------------------------------------------------------------------------
// Keeping paneReviewSeenNonTerminal set until a response
// lands means every intervening jobs refresh before
// that response arrives re-dispatches ANOTHER fetchReviewFollow for the
// same completion -- concurrent requests sharing the same jobID and
// detailFollowGen, both passing handleReviewMsg's staleness gates, so an
// older response landing after a newer accepted one can overwrite it, and
// a slow daemon accumulates unbounded duplicate requests.
// ---------------------------------------------------------------------------

// TestSplitReconcileDetailSuppressesDuplicateFollowDispatch is the
// duplicate-dispatch repro (test a): a same-second rerun is observed
// running, then completes; two consecutive jobs refreshes arrive before
// any response lands -- only the FIRST issues a fetch cmd.
func TestSplitReconcileDetailSuppressesDuplicateFollowDispatch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	finish := time.Now().Truncate(time.Second)
	m := splitModel(withSelection(1, 2)) // job 2: done
	review := splitTestReview()
	review.Job.FinishedAt = &finish
	m.currentReview = review

	// Observed running again -- the rerun's non-terminal window.
	runningJobs := testQueueJobs()
	runningJobs[1].Status = storage.JobStatusRunning
	m.jobs = runningJobs
	m, _ = m.splitReconcileDetail()

	// Completes within the SAME wall-clock second.
	doneJobs := testQueueJobs()
	doneJobs[1].FinishedAt = &finish
	m.jobs = doneJobs

	m, cmd1 := m.splitReconcileDetail()
	require.NotNil(cmd1, "the first refresh after completion must dispatch a fetch")

	// A second refresh arrives before the first fetch's response lands --
	// nothing about the job changed in between.
	_, cmd2 := m.splitReconcileDetail()
	assert.Nil(cmd2, "a second refresh while the first fetch is still in flight must NOT dispatch another")
}

// TestSplitReconcileDetailRejectsOlderFollowResponseAfterNewerAccepted is
// the out-of-order protection test (test b): even if two follow fetches
// somehow end up in flight for the same job (simulated here by letting the
// first fail so a second goes out, rather than manufacturing a fetchSeq
// literal -- this test never references the tracking fields directly),
// accepting the newer one first must make a later-arriving older response
// get dropped instead of overwriting currentReview.
func TestSplitReconcileDetailRejectsOlderFollowResponseAfterNewerAccepted(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	finish := time.Now().Truncate(time.Second)

	olderReview := *splitTestReview()
	olderReview.Output = "OLDER content"
	olderReview.Job.FinishedAt = &finish
	newerReview := *splitTestReview()
	newerReview.Output = "NEWER content"
	newerReview.Job.FinishedAt = &finish

	callCount := 0
	handler := func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/review":
			callCount++
			if callCount == 1 {
				json.NewEncoder(w).Encode(olderReview)
			} else {
				json.NewEncoder(w).Encode(newerReview)
			}
		case "/api/comments":
			json.NewEncoder(w).Encode(map[string]any{"responses": []storage.Response{}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
	_, m := mockServerModel(t, handler)
	m.layout = layoutSplit
	m.jobs = testQueueJobs()
	m.selectedIdx, m.selectedJobID = 1, 2
	review := splitTestReview()
	review.Job.FinishedAt = &finish
	m.currentReview = review

	// First dispatch -- will fetch "older" when eventually run.
	runningJobs := testQueueJobs()
	runningJobs[1].Status = storage.JobStatusRunning
	m.jobs = runningJobs
	m, _ = m.splitReconcileDetail()

	doneJobs := testQueueJobs()
	doneJobs[1].FinishedAt = &finish
	m.jobs = doneJobs
	m, cmd1 := m.splitReconcileDetail()
	require.NotNil(cmd1)
	msg1 := cmd1() // the "older" response -- held aside, not delivered yet
	_, ok := msg1.(reviewMsg)
	require.True(ok, "expected a reviewMsg from the mock server, got %T", msg1)

	// That first request is abandoned (fails) so a second can go out --
	// fetchSeq must match the dispatch's (m.reviewFetchSeq, still holding
	// what the first splitReconcileDetail call above just stamped) for
	// this error to be recorded rather than dropped as superseded.
	res, _ := m.handleReviewFollowErrMsg(reviewFollowErrMsg{jobID: 2, fetchSeq: m.reviewFetchSeq})
	m = res.(model)

	m, cmd2 := m.splitReconcileDetail()
	require.NotNil(cmd2, "a second dispatch must go out now that the first is no longer tracked as in flight")
	msg2 := cmd2() // the "newer" response

	// Accept the NEWER response first.
	res2, _ := m.handleReviewMsg(msg2.(reviewMsg))
	got := res2.(model)
	require.NotNil(got.currentReview)
	require.Equal("NEWER content", got.currentReview.Output)

	// The OLDER response (from the first dispatch) now lands late.
	res3, _ := got.handleReviewMsg(msg1.(reviewMsg))
	got2 := res3.(model)
	assert.Equal("NEWER content", got2.currentReview.Output, "an older follow response arriving after a newer accepted one must be rejected, not overwrite currentReview")
}

// TestSplitReconcileDetailRetriesAfterFollowFetchFailsAndInFlightClears is
// test (c): a failed follow fetch must clear in-flight tracking (not
// just keep the retry signal set), so a later refresh actually dispatches
// again instead of being suppressed by a stuck in-flight flag.
func TestSplitReconcileDetailRetriesAfterFollowFetchFailsAndInFlightClears(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	finish := time.Now().Truncate(time.Second)
	_, m := mockServerModel(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	m.layout = layoutSplit
	m.jobs = testQueueJobs()
	m.selectedIdx, m.selectedJobID = 1, 2
	review := splitTestReview()
	review.Job.FinishedAt = &finish
	m.currentReview = review

	runningJobs := testQueueJobs()
	runningJobs[1].Status = storage.JobStatusRunning
	m.jobs = runningJobs
	m, _ = m.splitReconcileDetail()

	doneJobs := testQueueJobs()
	doneJobs[1].FinishedAt = &finish
	m.jobs = doneJobs
	m, cmd := m.splitReconcileDetail()
	require.NotNil(cmd)

	_, cmdWhileInFlight := m.splitReconcileDetail()
	require.Nil(cmdWhileInFlight, "a refresh while the first fetch is in flight must not dispatch another")

	msg := cmd()
	errMsgVal, ok := msg.(reviewFollowErrMsg)
	require.True(ok, "expected a reviewFollowErrMsg, got %T", msg)
	res, _ := m.handleReviewFollowErrMsg(errMsgVal)
	m = res.(model)

	_, cmd2 := m.splitReconcileDetail()
	assert.NotNil(cmd2, "a failed follow fetch must clear in-flight tracking so a later pass can retry")
}

// TestSplitReconcileDetailNormalDonePathDispatchesSingleFetch is test (d):
// the ordinary running->done transition (no rerun, no prior review loaded)
// is unaffected by the in-flight suppression -- it still dispatches
// exactly one fetch.
func TestSplitReconcileDetailNormalDonePathDispatchesSingleFetch(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withSelection(0, 3)) // job 3: running, selected
	m.currentReview = nil

	doneJobs := testQueueJobs()
	doneJobs[0].Status = storage.JobStatusDone
	m.jobs = doneJobs

	_, cmd := m.splitReconcileDetail()
	assert.NotNil(cmd, "a normal running->done transition must still dispatch a follow fetch")
}

// ---------------------------------------------------------------------------
// reconcileFetchJobID/Seq must be scoped to the request they track -- a
// single global flag has two "stuck forever" gaps.
// ---------------------------------------------------------------------------

// TestSplitReconcileDetailStalePendingForOldJobDoesNotBlockDifferentJob is
// test (a): the stuck-forever repro. A tracked follow fetch is left
// outstanding for job A (never resolved -- e.g. its response was consumed
// while a different job was selected, or is simply still in flight); the
// selection is mutated to a DIFFERENT job B without going through
// scheduleDetailFollow (exactly what handleCtrlSelectJob does while
// layout != layoutSplit: selectedJobID changes directly, no gen bump, no
// tracking clear -- control_handlers.go). Re-entering split,
// maybeBootstrapDetail's JobID-only bootstrap-skip (layout.go) doesn't
// consult the tracking at all, so it can't help either. Job B then
// genuinely needs a rebuild -- this must not be blocked by job A's stale,
// unresolved tracked-request slot.
func TestSplitReconcileDetailStalePendingForOldJobDoesNotBlockDifferentJob(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t1 := time.Now().Truncate(time.Second)

	jobA := storage.ReviewJob{ID: 2, Status: storage.JobStatusDone, FinishedAt: &t1, Agent: "codex"}
	jobB := storage.ReviewJob{ID: 3, Status: storage.JobStatusDone, FinishedAt: &t1, Agent: "codex"}
	m := splitModel(withTestJobs(jobA, jobB), withSelection(0, 2))
	reviewA := &storage.Review{ID: 20, JobID: 2, Agent: "codex", Output: "A v1", Job: &storage.ReviewJob{ID: 2, FinishedAt: &t1}}
	m.currentReview = reviewA

	// Job A observed running again (a same-second rerun window), then
	// completes -- dispatches a TRACKED follow
	// fetch that this test deliberately leaves unresolved (no response is
	// ever delivered for it).
	runningA := []storage.ReviewJob{{ID: 2, Status: storage.JobStatusRunning}, jobB}
	m.jobs = runningA
	m, _ = m.splitReconcileDetail()

	doneAAgain := []storage.ReviewJob{{ID: 2, Status: storage.JobStatusDone, FinishedAt: &t1, Agent: "codex"}, jobB}
	m.jobs = doneAAgain
	m, cmd := m.splitReconcileDetail()
	require.NotNil(cmd, "tracked fetch dispatched for job A")

	// Selection is mutated to job B WITHOUT going through
	// scheduleDetailFollow, the way handleCtrlSelectJob does while
	// layout != layoutSplit. currentReview is left untouched by that
	// mutation (it's a raw selectedJobID change, not a review load).
	m.selectedIdx, m.selectedJobID = 1, 3

	// Re-entering split, maybeBootstrapDetail's JobID-only match skips --
	// contrived directly here (currentReview already "shows" job B): the
	// guard only compares JobIDs and doesn't care how that came to be.
	reviewB := &storage.Review{ID: 21, JobID: 3, Agent: "codex", Output: "B v1", Job: &storage.ReviewJob{ID: 3, FinishedAt: &t1}}
	m.currentReview = reviewB
	m, bootstrapCmd := m.maybeBootstrapDetail()
	require.Nil(bootstrapCmd, "bootstrap correctly skips -- currentReview already matches the selected job")

	// Job B now genuinely needs a rebuild (observed running, then done
	// again with a newer completion) -- must NOT be blocked by job A's
	// stale, never-resolved tracked-request slot.
	runningB := []storage.ReviewJob{{ID: 2, Status: storage.JobStatusDone, FinishedAt: &t1, Agent: "codex"}, {ID: 3, Status: storage.JobStatusRunning}}
	m.jobs = runningB
	m, _ = m.splitReconcileDetail()

	t2 := t1.Add(time.Hour)
	doneBAgain := []storage.ReviewJob{{ID: 2, Status: storage.JobStatusDone, FinishedAt: &t1, Agent: "codex"}, {ID: 3, Status: storage.JobStatusDone, FinishedAt: &t2, Agent: "codex"}}
	m.jobs = doneBAgain
	_, cmd2 := m.splitReconcileDetail()
	assert.NotNil(cmd2, "job B's genuine rebuild must not be blocked by job A's stale tracked-request slot")
}

// TestUntrackedFollowResponseForDifferentJobDoesNotClearTrackedPending:
// an untracked follow response (e.g.
// from handleDetailFollowTick reacting to a selection change) lands for a
// DIFFERENT job while a TRACKED request is genuinely still outstanding for
// the original job -- accepting it (its fetchSeq is a real, current one
// now, not a 0 sentinel) must not release the tracked slot just because
// SOME response landed, or reconcile would dispatch a duplicate on top of
// the still-outstanding one once the original job needs another rebuild.
func TestUntrackedFollowResponseForDifferentJobDoesNotClearTrackedPending(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t1 := time.Now().Truncate(time.Second)

	jobA := storage.ReviewJob{ID: 2, Status: storage.JobStatusDone, FinishedAt: &t1, Agent: "codex"}
	jobB := storage.ReviewJob{ID: 3, Status: storage.JobStatusDone, FinishedAt: &t1, Agent: "codex"}
	m := splitModel(withTestJobs(jobA, jobB), withSelection(0, 2))
	reviewA := &storage.Review{ID: 20, JobID: 2, Agent: "codex", Output: "A v1", Job: &storage.ReviewJob{ID: 2, FinishedAt: &t1}}
	m.currentReview = reviewA

	// Job A observed running, then completes -- a TRACKED fetch is
	// dispatched for A and left unresolved (never delivered in this
	// test, simulating it still being genuinely in flight).
	runningA := []storage.ReviewJob{{ID: 2, Status: storage.JobStatusRunning}, jobB}
	m.jobs = runningA
	m, _ = m.splitReconcileDetail()
	doneAAgain := []storage.ReviewJob{{ID: 2, Status: storage.JobStatusDone, FinishedAt: &t1, Agent: "codex"}, jobB}
	m.jobs = doneAAgain
	m, cmd := m.splitReconcileDetail()
	require.NotNil(cmd, "tracked fetch dispatched for job A")

	// An UNTRACKED follow response lands for a DIFFERENT job (B) -- as if
	// handleDetailFollowTick reacted to a selection change to B in the
	// meantime. Every fetchReviewFollow dispatch is sequenced,
	// so this simulates the real untracked-dispatch
	// pattern (bump the shared seq, stamp it on the outgoing request)
	// rather than a special 0 sentinel -- there is no more such sentinel.
	m.selectedIdx, m.selectedJobID = 1, 3
	m.reviewFetchSeq++
	untrackedReviewB := &storage.Review{ID: 99, JobID: 3, Agent: "codex", Output: "B content", Job: &storage.ReviewJob{ID: 3, FinishedAt: &t1}}
	res, _ := m.handleReviewMsg(reviewMsg{review: untrackedReviewB, jobID: 3, follow: true, fetchSeq: m.reviewFetchSeq})
	m = res.(model)
	require.NotNil(m.currentReview)
	require.Equal(int64(3), m.currentReview.JobID, "the untracked response for job B lands normally")

	// Selection returns to job A -- its review still shows the OLD
	// content (reviewA), and job A completed AGAIN (a newer FinishedAt)
	// while the pane was on B, so a rebuild is genuinely due.
	m.selectedIdx, m.selectedJobID = 0, 2
	m.currentReview = reviewA
	t2 := t1.Add(time.Hour)
	doneAAgainNewer := []storage.ReviewJob{{ID: 2, Status: storage.JobStatusDone, FinishedAt: &t2, Agent: "codex"}, jobB}
	m.jobs = doneAAgainNewer

	_, cmd2 := m.splitReconcileDetail()
	assert.Nil(cmd2, "the ORIGINAL tracked fetch for job A must still be considered outstanding -- the untracked B landing must not have cleared it, so reconcile must not dispatch a duplicate")
}

// TestReviewFollowErrMsgDiscardsStaleGen: a follow fetch failure matched
// on jobID alone, unlike its success counterpart
// (gen-gated), would let a stale error from a fetch dispatched before the
// user moved away and back to the same job could overwrite splitDetailErr
// for the reselected job. Both a stale-gen error (discarded) and a
// current-gen error (recorded) are covered (test e).
func TestReviewFollowErrMsgDiscardsStaleGen(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withSelection(1, 2)) // job 2 selected

	oldGen := m.detailFollowGen
	// The selection moves away and back (bumping gen via the real
	// handleKeyMsg -> scheduleDetailFollow path) before the stale error
	// arrives.
	m, _ = pressSpecial(m, tea.KeyDown)
	m, _ = pressSpecial(m, tea.KeyUp)
	require.Greater(t, m.detailFollowGen, oldGen)
	require.Equal(t, int64(2), m.selectedJobID)

	res, _ := m.handleReviewFollowErrMsg(reviewFollowErrMsg{
		jobID: 2, gen: oldGen, err: errors.New("stale error from the abandoned fetch"),
	})
	got := res.(model)
	require.NoError(t, got.splitDetailErr, "a stale-gen error must be discarded, not overwrite splitDetailErr for the reselected job")

	// The current-gen counterpart must still be recorded.
	res2, _ := m.handleReviewFollowErrMsg(reviewFollowErrMsg{
		jobID: 2, gen: m.detailFollowGen, err: errors.New("current error"),
	})
	got2 := res2.(model)
	assert.Error(got2.splitDetailErr, "a current-gen error must still be recorded")
}

// ---------------------------------------------------------------------------
// Follow-up scoped review: two more findings, plus doc corrections applied
// as code comments above (not tested directly).
// ---------------------------------------------------------------------------

// TestScheduleDetailFollowClosesFixPanelOnDifferentJobSelection covers
// Finding 1(a): a mouse click selecting a DIFFERENT row while the inline
// fix panel is open must close it (and clear fixPromptJobID), since mouse
// selection changes route through scheduleDetailFollow without ever
// passing through the panel's own close paths.
func TestScheduleDetailFollowClosesFixPanelOnDifferentJobSelection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withReview(splitTestReview())) // job 2 selected+loaded
	m.reviewFixPanelOpen = true
	m.reviewFixPanelFocused = true
	m.fixPromptText = "fix instructions for job 2"
	m.fixPromptJobID = 2

	firstDataY := splitFirstDataRowY(t, m, "cccc333") // job 3's row
	res, _ := m.handleSplitMouse(mouseClickAt(5, firstDataY))
	got := res.(model)
	require.Equal(int64(3), got.selectedJobID, "the click must have selected a different job")
	assert.False(got.reviewFixPanelOpen, "the fix panel must close when the selection moves to a different job")
	assert.False(got.reviewFixPanelFocused)
	assert.Equal(int64(0), got.fixPromptJobID)
	assert.Empty(got.fixPromptText)
}

// TestCtrlSelectJobClosesFixPanelOnDifferentJobSelection covers Finding
// 1(b): the control-socket select-job path goes through the same
// scheduleDetailFollow call and must close the panel identically.
func TestCtrlSelectJobClosesFixPanelOnDifferentJobSelection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withReview(splitTestReview())) // job 2 selected+loaded
	m.reviewFixPanelOpen = true
	m.fixPromptJobID = 2
	m.fixPromptText = "fix instructions"

	params, err := json.Marshal(map[string]int64{"job_id": 3})
	require.NoError(err)
	got, resp, _ := m.handleCtrlSelectJob(params)
	require.True(resp.OK, "expected OK, got error: %s", resp.Error)
	require.Equal(int64(3), got.selectedJobID)
	assert.False(got.reviewFixPanelOpen, "control-socket reselect to a different job must close the fix panel")
	assert.Equal(int64(0), got.fixPromptJobID)
}

// TestScheduleDetailFollowLeavesFixPanelAloneForSameJob covers Finding
// 1(c): a selection "change" that lands on the SAME job the panel is
// already scoped to must not close it.
func TestScheduleDetailFollowLeavesFixPanelAloneForSameJob(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withReview(splitTestReview())) // job 2 selected+loaded
	m.reviewFixPanelOpen = true
	m.reviewFixPanelFocused = true
	m.fixPromptJobID = 2 // matches m.selectedJobID already

	got, _ := m.scheduleDetailFollow()
	assert.True(got.reviewFixPanelOpen, "a panel already scoped to the still-selected job must not be closed")
	assert.True(got.reviewFixPanelFocused)
	assert.Equal(int64(2), got.fixPromptJobID)
}

// TestReconcilePassLeavesFixPanelAloneWhenJobUnchanged covers Finding
// 1(d): an open panel on the still-selected job must survive an unrelated
// reconcile pass (splitReconcileDetail/handleJobsMsg never call
// scheduleDetailFollow, so this should hold trivially -- verified
// explicitly per the request).
func TestReconcilePassLeavesFixPanelAloneWhenJobUnchanged(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withSelection(1, 2)) // job 2: done, selected
	m.currentReview = splitTestReview()
	m.reviewFixPanelOpen = true
	m.reviewFixPanelFocused = true
	m.fixPromptJobID = 2

	res, _ := m.handleJobsMsg(jobsMsg{jobs: testQueueJobs(), stats: storage.JobStats{}})
	got := res.(model)
	assert.True(got.reviewFixPanelOpen, "an open panel on the still-selected job must survive an unrelated reconcile pass")
	assert.Equal(int64(2), got.fixPromptJobID)
}

// TestPaneLogOutputRejectsLateResponseAfterVanishedSelectionReassignment
// covers Finding 2's first named path: handleJobsMsg reassigns the
// selection when the tailed job vanishes from a refresh (e.g. filtered
// out), bypassing scheduleDetailFollow entirely -- paneLogSeq is never
// bumped, so a late response for the OLD job, still matching paneLogJobID
// and seq, used to satisfy the old guard even though the selection (and
// the pane's actual content) had already moved on.
func TestPaneLogOutputRejectsLateResponseAfterVanishedSelectionReassignment(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withSelection(0, 3)) // job 3: running, tailed
	m.paneLogJobID, m.paneLogSeq, m.paneLogStreaming = 3, 5, true

	// Job 3 vanishes from the refreshed list.
	newJobs := []storage.ReviewJob{
		{ID: 2, GitRef: "bbbb222", RepoName: "repoA", Agent: "codex", Status: storage.JobStatusDone},
	}
	res, _ := m.handleJobsMsg(jobsMsg{jobs: newJobs, stats: storage.JobStats{}})
	got := res.(model)
	require.NotEqual(int64(3), got.selectedJobID, "selection must have moved off job 3")
	require.Equal(uint64(5), got.paneLogSeq, "seq must NOT have been bumped by this bypass path -- that's the gap")

	res2, cmd := got.handlePaneLogOutputMsg(paneLogOutputMsg{
		jobID: 3, seq: 5, hasMore: true, lines: []logLine{{text: "job 3 line"}},
	})
	final := res2.(model)
	assert.Nil(cmd)
	assert.Empty(final.paneLogLines, "a rejected response must apply no lines")
	assert.NoError(final.splitDetailErr)
}

// TestPaneLogOutputRejectsLateResponseAfterCloseRollbackRestoreSelection
// covers Finding 2's second named path: closedResultMsg's restoreSelection
// rollback (handleClosedResultMsg) moves the selection back via
// selectJobByID, also bypassing scheduleDetailFollow.
func TestPaneLogOutputRejectsLateResponseAfterCloseRollbackRestoreSelection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	// Selection was on job 3 (tailed, running) -- e.g. after optimistically
	// moving there when job 2 was closed with hideClosed active.
	m := splitModel(withSelection(0, 3), withTestJobs(
		storage.ReviewJob{ID: 3, Status: storage.JobStatusRunning},
		storage.ReviewJob{ID: 2, Status: storage.JobStatusDone, Closed: new(false)},
	))
	m.paneLogJobID, m.paneLogSeq, m.paneLogStreaming = 3, 5, true
	m.pendingClosed[2] = pendingState{newState: true, seq: 1}

	// The close request for job 2 FAILS: the rollback restores selection
	// back to job 2 via selectJobByID, bypassing scheduleDetailFollow.
	res, _ := m.handleClosedResultMsg(closedResultMsg{
		jobID: 2, restoreSelection: true, oldState: false, newState: true, seq: 1,
		err: errors.New("server error"),
	})
	got := res.(model)
	require.Equal(int64(2), got.selectedJobID, "rollback must restore selection to job 2")
	// The rollback now routes through the shared detail-follow transition
	// (followSelectionChange -> scheduleDetailFollow), which invalidates
	// the tail for the job the selection moved off -- so the late response
	// below is rejected by the seq gate rather than only by the jobID
	// comparison. This is the gap this path used to have.
	require.Greater(got.paneLogSeq, uint64(5), "the restored selection must invalidate the old job's tail")
	require.False(got.paneLogStreaming)

	res2, cmd := got.handlePaneLogOutputMsg(paneLogOutputMsg{
		jobID: 3, seq: 5, hasMore: true, lines: []logLine{{text: "job 3 line"}},
	})
	final := res2.(model)
	assert.Nil(cmd)
	assert.Empty(final.paneLogLines, "a rejected response must apply no lines")
	assert.NoError(final.splitDetailErr)
}

// TestPaneLogOutputAcceptsNormalSameJobResponse is the non-regression
// control: a response for the job that's both currently tailed AND
// currently selected must still be accepted normally.
func TestPaneLogOutputAcceptsNormalSameJobResponse(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withSelection(0, 3)) // job 3: running, tailed, selected
	m.paneLogJobID, m.paneLogSeq, m.paneLogStreaming = 3, 5, true

	res, cmd := m.handlePaneLogOutputMsg(paneLogOutputMsg{
		jobID: 3, seq: 5, hasMore: true, lines: []logLine{{text: "job 3 line"}},
	})
	got := res.(model)
	assert.NotNil(cmd, "a normal same-job response must still schedule the next poll")
	require.Len(t, got.paneLogLines, 1)
	assert.Equal("job 3 line", got.paneLogLines[0].text)
}

// ---------------------------------------------------------------------------
// Follow fetches come in two classes -- tracked
// (splitReconcileDetail) and untracked (handleDetailFollowTick, the
// pane-log completion handoff, the split-list comment refresh) -- and
// they must be orderable against each other: an
// untracked response landing must not clear the tracked slot without
// invalidating the still-outstanding tracked request, whose fetchSeq
// still matched m.reviewFetchSeq. An older tracked response (fetched
// BEFORE a comment, say) could then land AFTER a newer untracked one and
// overwrite its fresher content.
// ---------------------------------------------------------------------------

// TestOlderTrackedResponseDoesNotOverwriteNewerUntrackedCommentRefresh is
// test (a): the exact repro. A tracked reconcile fetch is outstanding for
// job X; a comment is submitted for X, dispatching an UNTRACKED refresh
// that lands first with the new comment; the OLDER tracked response
// (dispatched before the comment) then finally lands -- currentResponses
// must still contain the new comment.
func TestOlderTrackedResponseDoesNotOverwriteNewerUntrackedCommentRefresh(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t1 := time.Now().Truncate(time.Second)

	m := splitModel(withSelection(1, 2)) // job 2: done, selected
	review := splitTestReview()
	review.Job.FinishedAt = &t1
	m.currentReview = review
	m.currentResponses = []storage.Response{{ID: 1, Response: "old comment"}}

	// A tracked reconcile fetch for job 2 is dispatched (a same-second
	// rerun window) and left unresolved.
	runningJobs := testQueueJobs()
	runningJobs[1].Status = storage.JobStatusRunning
	m.jobs = runningJobs
	m, _ = m.splitReconcileDetail()
	doneJobs := testQueueJobs()
	doneJobs[1].FinishedAt = &t1
	m.jobs = doneJobs
	m, cmd := m.splitReconcileDetail()
	require.NotNil(cmd, "tracked fetch dispatched for job 2")
	trackedFollowSeq := m.reviewFetchSeq // this dispatch's own seq, captured before anything supersedes it

	// The user submits a comment for job 2: handleCommentResultMsg
	// dispatches its own UNTRACKED follow refresh.
	res, commentCmd := m.handleCommentResultMsg(commentResultMsg{jobID: 2})
	m = res.(model)
	require.NotNil(commentCmd, "the comment refresh must dispatch")

	// That untracked refresh lands FIRST, with the new comment.
	freshWithComment := &storage.Review{ID: 20, JobID: 2, Agent: "codex", Output: review.Output, Job: &storage.ReviewJob{ID: 2, FinishedAt: &t1}}
	res2, _ := m.handleReviewMsg(reviewMsg{
		review: freshWithComment, jobID: 2, follow: true, gen: m.detailFollowGen,
		responses: []storage.Response{{ID: 1, Response: "old comment"}, {ID: 2, Response: "the user's new comment"}},
		fetchSeq:  m.reviewFetchSeq,
	})
	m = res2.(model)
	require.Len(m.currentResponses, 2, "the new comment must be visible after the untracked refresh lands")

	// The OLDER tracked response (dispatched before the comment) now
	// finally lands, carrying pre-comment data.
	staleReview := &storage.Review{ID: 19, JobID: 2, Agent: "codex", Output: review.Output, Job: &storage.ReviewJob{ID: 2, FinishedAt: &t1}}
	res3, _ := m.handleReviewMsg(reviewMsg{
		review: staleReview, jobID: 2, follow: true, gen: m.detailFollowGen,
		responses: []storage.Response{{ID: 1, Response: "old comment"}},
		fetchSeq:  trackedFollowSeq,
	})
	got := res3.(model)
	assert.Len(got.currentResponses, 2, "the user's comment must NOT disappear -- the older tracked response must be rejected as superseded")
}

// TestUntrackedAcceptanceBlocksLaterStaleTrackedResponse is test (b): the
// general ordering property, using a DIFFERENT untracked source (the
// pane-log completion handoff) than test (a)'s comment refresh, to confirm
// this isn't specific to one caller. Once an untracked response has been
// accepted, a stale tracked response landing afterward must not be able
// to overwrite it.
func TestUntrackedAcceptanceBlocksLaterStaleTrackedResponse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t1 := time.Now().Truncate(time.Second)
	m := splitModel(withSelection(1, 2)) // job 2: done, selected
	review := splitTestReview()
	review.Job.FinishedAt = &t1
	m.currentReview = review

	// Tracked reconcile fetch dispatched and left unresolved.
	runningJobs := testQueueJobs()
	runningJobs[1].Status = storage.JobStatusRunning
	m.jobs = runningJobs
	m, _ = m.splitReconcileDetail()
	doneJobs := testQueueJobs()
	doneJobs[1].FinishedAt = &t1
	m.jobs = doneJobs
	m, cmd := m.splitReconcileDetail()
	require.NotNil(cmd)
	trackedFollowSeq := m.reviewFetchSeq

	// A DIFFERENT untracked caller (the pane-log completion handoff)
	// dispatches its own follow fetch afterward.
	m.paneLogJobID, m.paneLogSeq, m.paneLogStreaming = 2, 5, true
	res, tickCmd := m.handlePaneLogOutputMsg(paneLogOutputMsg{jobID: 2, seq: 5, hasMore: false})
	m = res.(model)
	require.NotNil(tickCmd, "the pane-log completion handoff must still dispatch its own follow fetch")

	// That untracked fetch resolves first.
	newerReview := &storage.Review{ID: 30, JobID: 2, Agent: "codex", Output: "NEWER content", Job: &storage.ReviewJob{ID: 2, FinishedAt: &t1}}
	res2, _ := m.handleReviewMsg(reviewMsg{review: newerReview, jobID: 2, follow: true, gen: m.detailFollowGen, fetchSeq: m.reviewFetchSeq})
	m = res2.(model)
	require.Equal("NEWER content", m.currentReview.Output)

	// The OLDER tracked response (dispatched before the handoff) now
	// lands.
	staleReview := &storage.Review{ID: 29, JobID: 2, Agent: "codex", Output: "STALE content", Job: &storage.ReviewJob{ID: 2, FinishedAt: &t1}}
	res3, _ := m.handleReviewMsg(reviewMsg{review: staleReview, jobID: 2, follow: true, gen: m.detailFollowGen, fetchSeq: trackedFollowSeq})
	got := res3.(model)
	assert.Equal("NEWER content", got.currentReview.Output, "a stale tracked response arriving after a newer untracked one must not overwrite it")
}

// TestUntrackedCallersStillDispatchWhileTrackedRequestOutstanding is test
// (c): sequencing every fetchReviewFollow dispatch must not
// make the three untracked call sites start suppressing each other or the
// reconcile path -- they each fire on a genuine, one-shot event and must
// remain free to dispatch regardless of whether a tracked request happens
// to be outstanding. The suppression guard (reconcileFetchJobID) stays
// scoped to splitReconcileDetail's own dispatches only.
func TestUntrackedCallersStillDispatchWhileTrackedRequestOutstanding(t *testing.T) {
	assert := assert.New(t)

	// handleDetailFollowTick.
	mTick := splitModel(withSelection(1, 2))
	mTick.currentReview = nil // pane still loading -- its own guard would dispatch
	mTick.reconcileFetchJobID = 2
	mTick.reviewFetchSeq = 5
	resTick, cmdTick := mTick.handleDetailFollowTick(detailFollowTickMsg{gen: mTick.detailFollowGen})
	gotTick := resTick.(model)
	assert.NotNil(cmdTick, "handleDetailFollowTick must still be able to dispatch while a tracked request is outstanding")
	assert.Equal(int64(2), gotTick.reconcileFetchJobID, "the tracked slot must be untouched by this dispatch")

	// The pane-log completion handoff.
	mLog := splitModel(withSelection(1, 2))
	mLog.paneLogJobID, mLog.paneLogSeq, mLog.paneLogStreaming = 2, 5, true
	mLog.reconcileFetchJobID = 2
	mLog.reviewFetchSeq = 7
	resLog, cmdLog := mLog.handlePaneLogOutputMsg(paneLogOutputMsg{jobID: 2, seq: 5, hasMore: false})
	gotLog := resLog.(model)
	assert.NotNil(cmdLog, "the pane-log completion handoff must still be able to dispatch while a tracked request is outstanding")
	assert.Equal(int64(2), gotLog.reconcileFetchJobID, "the tracked slot must be untouched by this dispatch")

	// The split-list comment refresh.
	mComment := splitModel(withReview(splitTestReview())) // job 2 loaded, list focus
	mComment.reconcileFetchJobID = 2
	mComment.reviewFetchSeq = 9
	resComment, cmdComment := mComment.handleCommentResultMsg(commentResultMsg{jobID: 2})
	gotComment := resComment.(model)
	assert.NotNil(cmdComment, "the comment refresh must still be able to dispatch while a tracked request is outstanding")
	assert.Equal(int64(2), gotComment.reconcileFetchJobID, "the tracked slot must be untouched by this dispatch")
}

// ---------------------------------------------------------------------------
// Pane-log staleness rejections and their consequences for the tail.
// ---------------------------------------------------------------------------

// TestPaneLogOutputInvalidatesLiveTailWhenSelectionMovedElsewhere:
// handlePaneLogOutputMsg's msg.jobID != m.selectedJobID rejection must
// also invalidate the tail, not leave paneLogStreaming true. If the
// selection later returns to that same running job via a path that
// bypasses scheduleDetailFollow, splitReconcileDetail's Running-branch
// restart guard would see the dead tail as still active and never
// restart it -- the live log freezes permanently.
func TestPaneLogOutputInvalidatesLiveTailWhenSelectionMovedElsewhere(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withSelection(0, 3)) // job 3: running, tailed
	m.paneLogJobID, m.paneLogSeq, m.paneLogStreaming = 3, 5, true
	// Selection moved to job 2 via a bypass path (e.g. handleCtrlSelectJob
	// while stacked, or handleJobsMsg's vanished-selection reassignment) --
	// paneLogSeq never bumped.
	m.selectedIdx, m.selectedJobID = 1, 2

	// A late response for job 3 arrives, still matching the tail's own
	// bookkeeping (seq and paneLogJobID).
	res, cmd := m.handlePaneLogOutputMsg(paneLogOutputMsg{
		jobID: 3, seq: 5, hasMore: true, lines: []logLine{{text: "job 3 line"}},
	})
	got := res.(model)
	assert.Nil(cmd)
	assert.Empty(got.paneLogLines, "a rejected response must apply no lines")
	assert.False(got.paneLogStreaming, "the tail must be invalidated, not left claiming to be active")
	assert.Greater(got.paneLogSeq, uint64(5))

	// Reselecting job 3 later: reconcile must restart the tail now that
	// it's correctly marked stopped.
	m2 := got
	m2.selectedIdx, m2.selectedJobID = 0, 3
	_, cmd2 := m2.splitReconcileDetail()
	assert.NotNil(cmd2, "reconcile must restart the tail for job 3 now that it's correctly marked stopped")
}

// TestPaneLogOutputStaleSeqLeavesLiveTailAlone is the negative control:
// a response that's stale by SEQ (not by selection) must be rejected
// without touching a live tail at all -- the same distinction
// handlePaneLogTickMsg's second guard draws.
func TestPaneLogOutputStaleSeqLeavesLiveTailAlone(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withSelection(0, 3)) // job 3: running, tailed, selected
	m.paneLogJobID, m.paneLogSeq, m.paneLogStreaming = 3, 5, true

	res, cmd := m.handlePaneLogOutputMsg(paneLogOutputMsg{
		jobID: 3, seq: 4, hasMore: true, lines: []logLine{{text: "stale"}},
	})
	got := res.(model)
	assert.Nil(cmd)
	assert.Empty(got.paneLogLines)
	assert.True(got.paneLogStreaming, "a stale-seq response must not invalidate a live tail")
	assert.Equal(uint64(5), got.paneLogSeq)
}

// TestCommentResultSkipsDispatchForJobNoLongerSelected: the comment
// refresh is the
// only one of the four fetchReviewFollow/fetchReview dispatchers not tied
// to the selection -- it keyed purely on m.currentReview.JobID, and
// currentReview is not cleared on a selection change. A comment result for
// a job that's no longer selected must not dispatch at all, and must not
// disturb the ordering seq an ALREADY-outstanding legitimate fetch for the
// CURRENTLY selected job depends on.
func TestCommentResultSkipsDispatchForJobNoLongerSelected(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	// Split, job X (2) was showing when the comment was submitted; the
	// cursor has since moved to job Y (3), and a follow fetch is already
	// outstanding for Y.
	m := splitModel(withSelection(1, 2)) // starts on job X (2)
	m.currentReview = splitTestReview()  // JobID: 2
	m.selectedIdx, m.selectedJobID = 0, 3
	m.reviewFetchSeq++
	outstandingSeqForY := m.reviewFetchSeq // stand-in for Y's own already-dispatched request

	res, cmd := m.handleCommentResultMsg(commentResultMsg{jobID: 2})
	got := res.(model)
	assert.Nil(cmd, "no dispatch for job X, which is no longer selected")

	// Y's own (legitimate, already-dispatched) response must still be
	// accepted normally afterward -- confirming the X dispatch never
	// bumped reviewFetchSeq out from under it.
	reviewY := &storage.Review{ID: 40, JobID: 3, Agent: "codex", Output: "Y content", Job: &storage.ReviewJob{ID: 3}}
	res2, _ := got.handleReviewMsg(reviewMsg{review: reviewY, jobID: 3, follow: true, gen: got.detailFollowGen, fetchSeq: outstandingSeqForY})
	got2 := res2.(model)
	require.NotNil(got2.currentReview)
	assert.Equal(int64(3), got2.currentReview.JobID, "Y's legitimate response must still be accepted")
	assert.Equal("Y content", got2.currentReview.Output)
}

// TestCommentResultFromSplitDetailFocusUsesSequencedPath: commenting
// from the split DETAIL pane
// (commentFromView == viewReview, handleCommentKey restores currentView
// synchronously before dispatch) must not fall through to the
// STACKED-mode branch's plain fetchReview instead of the sequenced follow
// path -- reachable because that branch had no layout check. A tracked
// reconcile fetch dispatched before the comment could then land AFTER it
// and wipe currentResponses back to pre-comment content: the same
// "comment disappears" symptom, just reachable from detail focus too.
func TestCommentResultFromSplitDetailFocusUsesSequencedPath(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t1 := time.Now().Truncate(time.Second)

	m := splitModel(withSelection(1, 2)) // job 2: done, selected
	m.focus = focusDetail
	m.currentView = viewReview // detail focus
	review := splitTestReview()
	review.Job.FinishedAt = &t1
	m.currentReview = review

	// A tracked reconcile fetch for job 2 is dispatched and left
	// unresolved.
	runningJobs := testQueueJobs()
	runningJobs[1].Status = storage.JobStatusRunning
	m.jobs = runningJobs
	m, _ = m.splitReconcileDetail()
	doneJobs := testQueueJobs()
	doneJobs[1].FinishedAt = &t1
	m.jobs = doneJobs
	m, cmd := m.splitReconcileDetail()
	require.NotNil(cmd, "tracked fetch dispatched for job 2")
	trackedFollowSeq := m.reviewFetchSeq

	// The user comments from the split DETAIL pane -- this must dispatch
	// via the SEQUENCED follow path (bumping the shared ordering seq),
	// not the stacked-mode branch's plain fetchReview.
	res, commentCmd := m.handleCommentResultMsg(commentResultMsg{jobID: 2})
	m = res.(model)
	require.NotNil(commentCmd, "the comment refresh must dispatch")
	require.Greater(m.reviewFetchSeq, trackedFollowSeq, "the comment refresh from detail focus must bump the shared ordering seq -- the whole point of routing it through the sequenced follow path")

	// The comment's own refresh lands with the new comment.
	m.currentResponses = []storage.Response{{ID: 1, Response: "old comment"}, {ID: 2, Response: "the user's new comment"}}

	// The OLDER tracked response (dispatched before the comment) now
	// finally lands.
	staleReview := &storage.Review{ID: 19, JobID: 2, Agent: "codex", Output: review.Output, Job: &storage.ReviewJob{ID: 2, FinishedAt: &t1}}
	res3, _ := m.handleReviewMsg(reviewMsg{
		review: staleReview, jobID: 2, follow: true, gen: m.detailFollowGen,
		responses: []storage.Response{{ID: 1, Response: "old comment"}},
		fetchSeq:  trackedFollowSeq,
	})
	got := res3.(model)
	assert.Len(got.currentResponses, 2, "the user's comment must NOT disappear -- the older tracked response must be rejected as superseded")
	assert.Equal(viewReview, got.currentView, "must not switch view -- the pane already shows the review")
	assert.Equal(focusDetail, got.focus, "must not steal focus")
}

// TestCommentResultStackedSkipsRefreshOnceTheSelectionMovedOn records
// why the selection gate on the stacked comment refresh is worth its
// cost. isReviewAnchored()
// (helpers.go) only protects handleJobsMsg's vanished-selection
// reassignment, NOT handleCtrlSelectJob, which in stacked mode overwrites
// m.selectedJobID directly and touches nothing else -- currentView/
// currentReview keep showing the previously selected job. So gating the
// stacked comment refresh on msg.jobID == m.selectedJobID dropped the
// refresh for the DISPLAYED review, which is why that gate was reverted.
//
// What changed since: the dispatch that the revert restored could never be
// ACCEPTED in this state anyway -- handleReviewMsg's own msg.jobID !=
// m.selectedJobID gate drops the response on arrival -- so it was merely
// useless. Now that every review fetch advances the SHARED ordering epoch,
// the same useless dispatch is destructive: it supersedes a concurrent,
// legitimate fetch for the job that IS selected (see
// TestStackedCommentRefreshForUnselectedJobDoesNotDestroyConcurrentFetch,
// where the user arrows to another review mid-POST and that review is
// silently skipped). The gate is therefore back, and this test now pins
// the deliberate consequence rather than treating it as a regression: in
// stacked mode a comment submitted for a review the selection has since
// moved off does not refresh -- the same outcome as before, since the
// response was dropped on arrival, just reached without collateral damage.
func TestCommentResultStackedSkipsRefreshOnceTheSelectionMovedOn(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	job2 := storage.ReviewJob{ID: 2, Status: storage.JobStatusDone}
	job3 := storage.ReviewJob{ID: 3, Status: storage.JobStatusDone}
	m := initTestModel(
		withCurrentView(viewReview),
		withTestJobs(job2, job3),
		withSelection(0, 2),
	)
	m.layout = layoutStacked
	m.currentReview = &storage.Review{ID: 20, JobID: 2, Agent: "codex", Output: "job 2 review"}

	// A control-socket select-job to a DIFFERENT job (3) arrives before
	// the comment POST resolves. In stacked mode, handleCtrlSelectJob
	// overwrites m.selectedJobID directly and touches nothing else.
	params, err := json.Marshal(map[string]int64{"job_id": 3})
	require.NoError(err)
	got, resp, cmd := m.handleCtrlSelectJob(params)
	require.True(resp.OK, "expected OK, got error: %s", resp.Error)
	assert.Nil(cmd, "no follow cmd is scheduled in stacked mode")
	require.Equal(int64(3), got.selectedJobID)
	require.Equal(viewReview, got.currentView, "currentView is untouched by the control-socket reselect")
	require.NotNil(got.currentReview)
	require.Equal(int64(2), got.currentReview.JobID, "currentReview still shows job 2, not the newly selected job 3")

	// The comment result for job 2 (the DISPLAYED review, not the
	// now-selected job 3) now arrives.
	_, refreshCmd := got.handleCommentResultMsg(commentResultMsg{jobID: 2})
	assert.Nil(refreshCmd, "a refresh whose response handleReviewMsg would drop on arrival must not be dispatched -- under the shared epoch it would supersede whatever legitimate fetch is outstanding for the selected job")
}

// TestCommentResultTasksOriginReviewUsesNonFollowPathEvenInSplit: the
// split comment-refresh branch covers viewReview (detail focus), but a
// TASKS-ORIGIN review is deliberately rendered FULL-SCREEN even on a
// split-capable terminal (splitActive() returns false for it), so the
// sequenced follow path's failures -- recorded only in m.splitDetailErr,
// which only the split detail pane renders -- would be completely silent
// for it. The refresh must take the plain, non-follow fetchReview path
// instead, whose failures surface through the ordinary full-screen error
// mechanism.
func TestCommentResultTasksOriginReviewUsesNonFollowPathEvenInSplit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withCurrentView(viewReview), withReview(splitTestReview())) // job 2's review loaded, split-capable terminal
	m.reviewFromView = viewTasks                                                // tasks-origin -- rendered full-screen even in split

	res, cmd := m.handleCommentResultMsg(commentResultMsg{jobID: 2})
	got := res.(model)
	require.NotNil(cmd, "the refresh must still dispatch")
	// Every review fetch is ordered now, so the epoch bump no longer
	// distinguishes the two paths -- the follow TAG does. A tasks-origin
	// review must take the non-follow path, whose failures surface through
	// the ordinary full-screen error mechanism rather than splitDetailErr.
	assert.Greater(got.reviewFetchSeq, m.reviewFetchSeq, "every review fetch is stamped from the one shared epoch")
	msg := cmd()
	rm, ok := msg.(reviewMsg)
	if ok {
		assert.False(rm.follow, "a tasks-origin review must take the NON-follow path")
	} else {
		// A failed non-follow fetch
		// surfaces as the typed reviewErrMsg (handled by
		// handleReviewErrMsg), not reviewFollowErrMsg -- see
		// reviewErrMsg's doc comment (types.go).
		_, isErr := msg.(reviewErrMsg)
		assert.True(isErr, "a failed non-follow fetch surfaces as reviewErrMsg, not reviewFollowErrMsg: %T", msg)
	}
}

// ---------------------------------------------------------------------------
// PR review finding (handlers.go:27): log-view navigation changed
// selectedJobID without ever routing through followSelectionChange.
// stepLogNav mutates the selection via moveSelectionToJobID -- a THIRD path
// distinct from stepReviewNav/stepPromptNav -- but unlike those, the log
// view's content lives entirely in logLines/paneLog* fields, never in
// currentReview, so the split detail pane was left pointed at the job the
// user navigated away from until an unrelated jobs refresh reconciled it.
// ---------------------------------------------------------------------------

// TestLogNavInSplitSchedulesDetailFollow is the reviewer's exact repro: split
// mode, log view opened from the queue, arrow-key log navigation to a
// different job. The detail pane must target the NEW job (follow scheduled:
// gen bumped and a fetch cmd issued), not the one the log view was opened
// for -- confirmed both immediately (gen bump, non-nil cmd, a real fetch
// from the resulting tick) and after esc back to the queue.
func TestLogNavInSplitSchedulesDetailFollow(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	// testQueueJobs order: job 3 (running, idx0), job 2 (done, idx1),
	// job 1 (failed, idx2).
	m := splitModel(withSelection(1, 2)) // job 2: done
	m.currentView = viewLog
	m.logFromView = viewQueue
	m.logJobID = 2
	m.logStreaming = false
	prevGen := m.detailFollowGen

	// Right = newer (see TestArrowKeysStepReviewNavInSplitDetailFocus):
	// job 2 -> job 3 (running).
	res, cmd := m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyRight})
	got := res.(model)

	assert.Equal(int64(3), got.selectedJobID, "the queue selection must move to the new job")
	assert.Equal(int64(3), got.logJobID, "the log view opens the new job's log")
	assert.Equal(viewLog, got.currentView)
	assert.Greater(got.detailFollowGen, prevGen,
		"detail follow must be scheduled for the new job (this is what stepLogNav skipped pre-fix)")
	require.NotNil(cmd, "a command (log fetch batched with the detail follow) must be issued")

	// The scheduled follow, once its debounce tick fires, targets the NEW
	// job (3, running) -- not the one the log view was opened for.
	_, followTickCmd := got.handleDetailFollowTick(detailFollowTickMsg{gen: got.detailFollowGen})
	assert.NotNil(followTickCmd,
		"the follow tick must dispatch a fetch for the new job (startPaneLog, since job 3 is running)")

	// esc back to the queue: the pane's follow state already targets job 3,
	// not job 2 (the job the log view was opened for).
	res2, _ := got.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
	final := res2.(model)
	assert.Equal(viewQueue, final.currentView)
	assert.Equal(int64(3), final.selectedJobID,
		"the detail pane's target on return to the queue is the NEW job, not the one the log view was opened for")
}

// TestLogNavFromTasksDoesNotScheduleDetailFollow confirms tasks-origin log
// nav (logFromView == viewTasks) is unaffected by the stepLogNav fix: it
// never reaches stepLogNav at all (handleNextKey/handlePrevKey route it to
// nextFixLog/prevFixLog instead, which walk m.fixJobs via fixSelectedIdx),
// so it must not touch selectedJobID or schedule a detail follow.
func TestLogNavFromTasksDoesNotScheduleDetailFollow(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withSelection(1, 2)) // job 2 selected in the queue
	m.currentView = viewLog
	m.logFromView = viewTasks
	m.logJobID = 20
	m.fixSelectedIdx = 1
	m.fixJobs = []storage.ReviewJob{
		{ID: 10, Status: storage.JobStatusDone},
		{ID: 20, Status: storage.JobStatusRunning},
		{ID: 30, Status: storage.JobStatusFailed},
	}
	prevGen := m.detailFollowGen
	prevSelected := m.selectedJobID

	res, cmd := m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyLeft})
	got := res.(model)

	require.NotNil(cmd, "expected a command from tasks-origin log nav")
	assert.Equal(2, got.fixSelectedIdx, "tasks-origin log nav steps m.fixJobs, unaffected by the stepLogNav fix")
	assert.Equal(prevSelected, got.selectedJobID, "tasks-origin log nav must not touch the queue selection")
	assert.Equal(prevGen, got.detailFollowGen, "tasks-origin log nav must not schedule a detail follow")
}

// TestLogNavInStackedDoesNotScheduleDetailFollow confirms the stepLogNav fix
// is a no-op outside split layout: followSelectionChange gates on
// m.layout != layoutSplit, so stacked-mode log nav still moves
// selectedJobID/opens the new job's log exactly as before, with no follow
// side effects -- there is no persistent detail pane to follow.
func TestLogNavInStackedDoesNotScheduleDetailFollow(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := initTestModel(
		withCurrentView(viewLog),
		withDimensions(150, 40),
		withTestJobs(testQueueJobs()...),
		withSelection(1, 2),
	)
	m.layout = layoutStacked
	m.logFromView = viewQueue
	m.logJobID = 2
	prevGen := m.detailFollowGen

	res, cmd := m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyRight})
	got := res.(model)

	assert.Equal(int64(3), got.selectedJobID, "selection still moves to the new job")
	assert.Equal(int64(3), got.logJobID)
	// A genuine selection change is an abandonment in any layout: the gen
	// bump invalidates the old job's in-flight dispatch. What stays
	// split-only is the follow TICK -- none is scheduled here, the only
	// cmd is the log-open.
	assert.Greater(got.detailFollowGen, prevGen,
		"the selection change abandons the old job's dispatch even in stacked")
	require.NotNil(cmd, "the log-open command is still issued")
}

// TestLogPaginateNavSchedulesDetailFollow covers a SECOND instance of the
// same bug class, found by auditing every path that moves selectedJobID
// during log-view content nav (not just stepLogNav by name): handleJobsMsg's
// paginateNav-triggered auto-navigate, reached when log-view nav hits the
// end of the currently loaded page and resumes after fetchMoreJobs lands.
// This branch used to `return` early with only openLogView's cmd, bypassing
// BOTH followSelectionChange and the splitReconcileDetail call at the
// bottom of handleJobsMsg -- so the split detail pane had no self-healing
// path at all until the next unrelated jobs refresh.
func TestLogPaginateNavSchedulesDetailFollow(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(
		withCurrentView(viewLog),
		withTestJobs(storage.ReviewJob{
			ID: 3, GitRef: "cccc333", Branch: "main", RepoName: "repoA",
			Agent: "codex", Status: storage.JobStatusDone,
		}),
		withSelection(0, 3),
	)
	m.logFromView = viewQueue
	m.logJobID = 3
	m.paginateNav = viewLog
	m.loadingMore = true
	prevGen := m.detailFollowGen

	// The appended page's next eligible row (older) is job 1, running.
	res, cmd := m.handleJobsMsg(jobsMsg{
		append: true,
		jobs: []storage.ReviewJob{
			{
				ID: 1, GitRef: "aaaa111", Branch: "main", RepoName: "repoB",
				Agent: "claude-code", Status: storage.JobStatusRunning,
			},
		},
	})
	got := res.(model)

	require.NotNil(cmd)
	assert.Equal(int64(1), got.selectedJobID, "auto-navigate must move selection to the newly loaded job")
	assert.Equal(int64(1), got.logJobID)
	assert.Greater(got.detailFollowGen, prevGen,
		"detail follow must be scheduled for the newly loaded job (skipped pre-fix by the early return)")

	_, followTickCmd := got.handleDetailFollowTick(detailFollowTickMsg{gen: got.detailFollowGen})
	assert.NotNil(followTickCmd,
		"the follow tick must dispatch a fetch (startPaneLog) for the newly selected running job")
}

// ---------------------------------------------------------------------------
// A pending queue-'F' request can be stranded when a NEWER
// follow fetch that superseded it then FAILS. reviewFixPanelPending is
// deliberately left armed on supersession (handleReviewMsg's fetchSeq
// branch), trusting the newer dispatch to eventually resolve it --
// acceptReview's success-only consume handles that dispatch landing
// successfully, but handleReviewFollowErrMsg didn't handle it FAILING, so a
// follow failure left the flag armed with nothing left in flight to serve
// it. Combined with an older (merely stale) review already loaded for the
// job, splitReconcileDetail's Done-branch idempotency check then skips
// re-fetching entirely, so nothing else would ever retry either: the panel
// is silently, permanently stranded, and the user's deliberate 'F' vanishes.
//
// This is the third variant of the stranded-pending-panel class (after the
// gen-mismatch strand and the wrong-origin strand): the FAILURE-path
// strand. The fix retries the follow once (bounded, so a persistently
// failing fetch can't loop forever) before clearing the pending flag with
// user-visible feedback.
// ---------------------------------------------------------------------------

// TestPendingFixPanelRetriesThenClearsAfterFollowFetchFails is the
// reviewer's exact repro: 'F' armed from the queue, its own ordinary fetch
// superseded by a follow fetch (as a reconcile pass or debounce tick would
// dispatch), which then fails -- with an OLDER review already loaded for
// the job, so splitReconcileDetail alone would never retry. After the
// first failure the pending flag must still be armed, but now with a NEW
// fetch actually in flight to serve it (not stranded). After a SECOND
// failure (the retry's own), the pending flag must be cleared with a
// user-visible flash instead of left armed with nothing in flight.
func TestPendingFixPanelRetriesThenClearsAfterFollowFetchFails(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	// job 2: done, with an OLDER review already loaded -- exactly the
	// condition under which splitReconcileDetail's idempotency check would
	// never retry on its own.
	m := splitModel(withReview(splitTestReview()), withSelection(1, 2), withTasksEnabled(true))

	// 'F' from the queue arms the pending panel and dispatches its own
	// ordinary fetch.
	res, ordinaryCmd := m.handleFixKey()
	got := res.(model)
	require.NotNil(ordinaryCmd, "'F' must dispatch its own ordinary fetch")
	require.True(got.reviewFixPanelPending)
	require.Equal(int64(2), got.fixPromptJobID)
	require.False(got.fixPromptFollowRetried)

	// A reconcile pass (or the debounce tick) dispatches a FOLLOW fetch for
	// the same job, superseding 'F's own fetch -- exactly what
	// splitReconcileDetail/handleDetailFollowTick do on an ordinary jobs
	// refresh while 'F's fetch is still outstanding.
	followCmd := got.dispatchReviewFollow(2)
	require.NotNil(followCmd)

	// That follow fetch FAILS.
	firstFail := reviewFollowErrMsg{
		jobID: 2, gen: got.detailFollowGen, fetchSeq: got.reviewFetchSeq,
		err: errors.New("network blip"),
	}
	res2, retryCmd := got.handleReviewFollowErrMsg(firstFail)
	afterFirstFail := res2.(model)

	require.NotNil(retryCmd,
		"the first follow failure must retry -- the pending panel must not be left armed with nothing in flight")
	assert.True(afterFirstFail.reviewFixPanelPending, "still armed: the retry is now in flight to serve it")
	assert.Equal(int64(2), afterFirstFail.fixPromptJobID)
	assert.True(afterFirstFail.fixPromptFollowRetried, "the retry must be marked so a second failure doesn't loop")
	require.Error(afterFirstFail.splitDetailErr, "the failure is still recorded for the pane, as before")

	// The retry ALSO fails.
	secondFail := reviewFollowErrMsg{
		jobID: 2, gen: afterFirstFail.detailFollowGen, fetchSeq: afterFirstFail.reviewFetchSeq,
		err: errors.New("network blip again"),
	}
	res3, cmd3 := afterFirstFail.handleReviewFollowErrMsg(secondFail)
	final := res3.(model)

	assert.Nil(cmd3, "bounded to one retry -- must not loop forever")
	assert.False(final.reviewFixPanelPending, "the pending flag must be resolved, not left stranded")
	assert.Zero(final.fixPromptJobID)
	assert.False(final.fixPromptFollowRetried)
	assert.NotEmpty(final.flashMessage, "the user must be told 'F' could not be served")
	assert.Equal(final.currentView, final.flashView, "the flash must be visible on the view the user is actually on")
	assert.True(final.flashWarning, "a failure flash should render as a warning")
}

// TestPendingFixPanelUnaffectedByOrdinaryFlowSuccess confirms the
// ordinary (non-superseded, non-retried) 'F' flow is unaffected by the
// retry machinery: the panel still opens the instant ITS OWN fetch's
// response lands.
func TestPendingFixPanelUnaffectedByOrdinaryFlowSuccess(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withSelection(1, 2), withTasksEnabled(true)) // job 2: done, nothing loaded yet
	m.currentReview = nil

	res, cmd := m.handleFixKey()
	got := res.(model)
	require.NotNil(cmd)
	require.True(got.reviewFixPanelPending)
	require.Equal(int64(2), got.fixPromptJobID)

	review := splitTestReview()
	final := applyReviewMsg(t, got, reviewMsg{
		review: review, jobID: 2, fetchSeq: got.reviewFetchSeq, dispatchedFrom: viewQueue,
	})
	assert.True(final.reviewFixPanelOpen, "the ordinary flow still opens the panel on its own response")
	assert.False(final.reviewFixPanelPending)
	assert.False(final.fixPromptFollowRetried)
}

// TestReviewFollowErrMsgNoPendingPanelUnaffected confirms a failed
// follow fetch with NO pending 'F' request just records splitDetailErr
// -- no retry is dispatched and no flash is shown, since there is
// nothing pending to resolve.
func TestReviewFollowErrMsgNoPendingPanelUnaffected(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withSelection(1, 2)) // job 2: done, no pending fix panel
	require.False(m.reviewFixPanelPending)
	prevFlash := m.flashMessage

	res, cmd := m.handleReviewFollowErrMsg(reviewFollowErrMsg{
		jobID: 2, gen: m.detailFollowGen, fetchSeq: m.reviewFetchSeq, err: errors.New("network error"),
	})
	got := res.(model)

	assert.Nil(cmd, "no pending fix panel means nothing to retry")
	require.Error(got.splitDetailErr, "the failure is still recorded exactly as before")
	assert.False(got.reviewFixPanelPending)
	assert.Equal(prevFlash, got.flashMessage, "no new flash when there was nothing pending to resolve")
}

// ---------------------------------------------------------------------------
// Per-job attempt invalidation (m.jobAttemptGen).
//
// A rerun invalidation via a
// detailFollowGen bump would have to be gated on the reran job being SELECTED --
// because detailFollowGen is one global counter, so an unconditional bump
// would have invalidated a legitimate in-flight fetch for whatever job was
// actually selected. The gate's cost: a pre-rerun response for a job that
// was NOT selected at rerun-confirm time stayed fresh on every axis and was
// accepted as CONTENT the moment the user landed back on that job.
// ---------------------------------------------------------------------------

// TestStackedPreRerunResponseRejectedAfterReturnToJob is the exact repro:
// stacked, start loading job X, rerun X, navigate to Y before the rerun
// confirmation lands, return to X, and X's PRE-rerun response finally
// arrives. Nothing on the pre-fix code path invalidated it -- the selection
// never moved in a way that bumps gen (stacked selection changes don't
// touch it), the rerun's own bump was skipped because X wasn't selected
// when the confirmation landed, and no newer dispatch superseded the epoch
// -- so the previous attempt's review was accepted as current content and
// the view switched to it.
func TestStackedPreRerunResponseRejectedAfterReturnToJob(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	_, m := mockServerModel(t, mockReviewHandler(*splitTestReview(), nil))
	m.currentView = viewQueue
	m.jobs = testQueueJobs()
	m.selectedIdx, m.selectedJobID = 1, 2 // X = job 2 (done), selected
	require.Equal(layoutStacked, m.layout)

	// 1. Start loading X. The real dispatcher stamps the request with X's
	//    attempt counter as it stands right now (0 -- no rerun observed).
	cmd := m.dispatchReviewFetch(2)
	require.NotNil(cmd)
	staleMsg, ok := cmd().(reviewMsg)
	require.True(ok, "expected a reviewMsg, got %T", cmd())
	require.Equal(uint64(0), staleMsg.attempt, "sanity: dispatched before any rerun of X")

	// 2/3. Rerun X, then navigate to Y (job 3) before the confirmation
	//      lands. Stacked, so this dispatches nothing and bumps nothing.
	m = m.moveSelectionToJobID(3)
	require.Equal(int64(3), m.selectedJobID)
	require.Equal(staleMsg.fetchSeq, m.reviewFetchSeq, "sanity: nothing superseded the in-flight fetch")

	// 4. The rerun confirmation lands while Y is selected.
	res, _ := m.handleRerunResultMsg(rerunResultMsg{jobID: 2})
	m = res.(model)

	// 5. Return to X. Again nothing dispatches or bumps in stacked mode.
	m = m.moveSelectionToJobID(2)
	require.Equal(staleMsg.gen, m.detailFollowGen, "sanity: gen never moved -- gen alone cannot catch this")
	require.Equal(staleMsg.fetchSeq, m.reviewFetchSeq, "sanity: the epoch never moved either")

	// 6. X's pre-rerun response finally arrives.
	res2, _ := m.handleReviewMsg(staleMsg)
	got := res2.(model)

	assert.Nil(got.currentReview,
		"a response from the attempt the rerun superseded must not become the pane's content")
	assert.Equal(viewQueue, got.currentView,
		"and it must not open the review view for the superseded attempt either")
}

// TestUnrelatedJobRerunDoesNotInvalidateSelectedJobFetch is the property the
// old selection gate existed to protect, now protected by construction: a
// rerun of job X must leave a legitimate in-flight fetch for the SELECTED
// job Y completely alone. This is what made an unconditional detailFollowGen
// bump unacceptable; a per-job counter has no such cross-job cost.
func TestUnrelatedJobRerunDoesNotInvalidateSelectedJobFetch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withSelection(0, 3)) // Y = job 3 selected

	// A legitimate follow fetch for Y is in flight, stamped now.
	inflight := reviewMsg{
		review: &storage.Review{
			ID: 30, JobID: 3, Agent: "codex",
			Job: &storage.ReviewJob{ID: 3, Status: storage.JobStatusDone},
		},
		jobID: 3, follow: true, gen: m.detailFollowGen,
		fetchSeq: m.reviewFetchSeq, attempt: m.jobAttemptGen[3],
	}

	// An unrelated job X (2) is reran -- via the control socket, say.
	res, _ := m.handleRerunResultMsg(rerunResultMsg{jobID: 2})
	m = res.(model)
	require.Positive(m.jobAttemptGen[2], "sanity: X's counter moved")
	require.Equal(uint64(0), m.jobAttemptGen[3], "Y's counter must be untouched by X's rerun")

	res2, _ := m.handleReviewMsg(inflight)
	got := res2.(model)
	require.NotNil(got.currentReview, "Y's in-flight fetch must still land normally")
	assert.Equal(int64(3), got.currentReview.JobID)
}

// TestPostRerunFetchForSameJobAccepted covers the other half: once the rerun
// is confirmed, a FRESH fetch for that job is stamped with the new attempt
// value and must be accepted normally. The counter invalidates the old
// attempt, not the job.
func TestPostRerunFetchForSameJobAccepted(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	_, m := mockServerModel(t, mockReviewHandler(*splitTestReview(), nil))
	m.currentView = viewQueue
	m.jobs = testQueueJobs()
	m.selectedIdx, m.selectedJobID = 1, 2

	res, _ := m.handleRerunResultMsg(rerunResultMsg{jobID: 2})
	m = res.(model)
	require.Equal(uint64(1), m.jobAttemptGen[2])

	cmd := m.dispatchReviewFetch(2)
	require.NotNil(cmd)
	fresh, ok := cmd().(reviewMsg)
	require.True(ok, "expected a reviewMsg, got %T", cmd())
	assert.Equal(uint64(1), fresh.attempt, "a post-rerun dispatch carries the post-rerun attempt")

	res2, _ := m.handleReviewMsg(fresh)
	got := res2.(model)
	require.NotNil(got.currentReview, "the rerun's own fetch must land")
	assert.Equal(int64(2), got.currentReview.JobID)
}

// TestPreRerunFollowErrRejectedForUnselectedRerun is the failure-path twin of
// the repro above: a follow fetch that FAILS for an attempt a rerun has since
// superseded must not reach splitDetailErr. Same shape for the ordinary
// fetch's typed failure, so both error handlers are covered.
func TestPreRerunFollowErrRejectedForUnselectedRerun(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withSelection(1, 2))

	staleErr := reviewFollowErrMsg{
		jobID: 2, err: errors.New("fetch for the superseded attempt failed"),
		gen: m.detailFollowGen, fetchSeq: m.reviewFetchSeq,
		attempt: m.jobAttemptGen[2],
	}
	staleOrdinaryErr := reviewErrMsg{
		jobID: 2, err: errors.New("ordinary fetch for the superseded attempt failed"),
		gen: m.detailFollowGen, fetchSeq: m.reviewFetchSeq,
		attempt: m.jobAttemptGen[2],
	}

	// The selection moves away, the rerun of job 2 confirms, the selection
	// returns -- the pre-fix path that left the response fully "fresh".
	m = m.moveSelectionToJobID(3)
	res, _ := m.handleRerunResultMsg(rerunResultMsg{jobID: 2})
	m = res.(model)
	m = m.moveSelectionToJobID(2)
	require.Greater(m.jobAttemptGen[2], staleErr.attempt)

	res2, _ := m.handleReviewFollowErrMsg(staleErr)
	require.NoError(res2.(model).splitDetailErr,
		"a follow failure belonging to a superseded attempt must not reach the pane")

	res3, _ := m.handleReviewErrMsg(staleOrdinaryErr)
	assert.NoError(res3.(model).err,
		"an ordinary failure belonging to a superseded attempt must not become the global error")
}

// TestFetchReviewStampsJobAttempt is the direct unit check on the stamping
// half (jobAttemptGen contract clause 3): fetchReview stamps the attempt
// counter of the job it is FETCHING -- not of the selected job -- and
// fetchReviewFollow carries it through onto the follow failure it re-tags.
func TestFetchReviewStampsJobAttempt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	_, m := mockServerModel(t, mockReviewHandler(*splitTestReview(), nil))
	m.jobs = testQueueJobs()
	m.selectedIdx, m.selectedJobID = 0, 3
	m.jobAttemptGen[2] = 4
	m.jobAttemptGen[3] = 9

	msg := m.fetchReview(2, 1)()
	rm, ok := msg.(reviewMsg)
	require.True(ok, "expected a reviewMsg, got %T", msg)
	assert.Equal(uint64(4), rm.attempt,
		"the stamp is the FETCHED job's attempt count, not the selected job's")

	// The failure path (a server that 500s) carries the same stamp through
	// fetchReviewFollow's re-tag.
	_, mErr := mockServerModel(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	mErr.jobAttemptGen[2] = 4
	fmsg := mErr.fetchReviewFollow(2, 1)()
	fe, ok := fmsg.(reviewFollowErrMsg)
	require.True(ok, "expected a reviewFollowErrMsg, got %T", fmsg)
	assert.Equal(uint64(4), fe.attempt, "the follow re-tag must carry the attempt stamp")
}

// ---------------------------------------------------------------------------
// Every path that moves selectedJobID must
// make the split detail pane FOLLOW the new job, not merely avoid stranding
// a pending intent. stepPromptNav and handleJobsMsg's pagination
// viewKindPrompt arm both changed the selection without the shared
// transition, so a prompt-view walk between two RUNNING jobs (which
// eligiblePromptRow admits) left the previous job's live-log buffer and
// splitDetailErr rendering beneath the new job's status card, and never
// started the new job's tail until an unrelated jobs refresh reconciled it.
// ---------------------------------------------------------------------------

// promptNavJobs returns two RUNNING jobs that both carry a prompt, so
// eligiblePromptRow admits both and prompt-view nav can step between them.
func promptNavJobs() []storage.ReviewJob {
	return []storage.ReviewJob{
		{
			ID: 5, GitRef: "eeee555", Branch: "main", RepoName: "repoA",
			Agent: "codex", Status: storage.JobStatusRunning, Prompt: "prompt for 5",
		},
		{
			ID: 4, GitRef: "dddd444", Branch: "main", RepoName: "repoA",
			Agent: "codex", Status: storage.JobStatusRunning, Prompt: "prompt for 4",
		},
	}
}

// tailingPromptModel is a split-layout prompt view on running job 4, whose
// live tail is active in the detail pane and has already buffered a line and
// recorded an error.
func tailingPromptModel() model {
	m := splitModel(
		withCurrentView(viewKindPrompt),
		withTestJobs(promptNavJobs()...),
		withSelection(1, 4),
	)
	m.promptFromQueue = true
	m.paneLogJobID = 4
	m.paneLogStreaming = true
	m.paneLogSeq = 3
	m.paneLogLines = []logLine{{text: "job 4 buffered log line"}}
	m.splitDetailErr = errors.New("job 4 tail failure")
	return m
}

func TestPromptNavInSplitFollowsSelection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := tailingPromptModel()
	prevGen := m.detailFollowGen

	// Right = newer: job 4 -> job 5, both running.
	res, cmd := m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyRight})
	got := res.(model)

	require.Equal(int64(5), got.selectedJobID, "the queue selection must move to the new job")
	assert.Greater(got.detailFollowGen, prevGen,
		"the detail follow must be scheduled for the new job (this is what stepPromptNav skipped pre-fix)")
	require.NotNil(cmd, "the scheduled follow must be returned as a command")
	assert.False(got.paneLogStreaming, "the previous job's tail must be stopped")
	assert.Greater(got.paneLogSeq, uint64(3),
		"an in-flight pane-log fetch for the previous job must be invalidated")
	require.NoError(got.splitDetailErr, "the previous job's error must not survive under the new job")
	assert.NotContains(got.renderSplit(), "job 4 tail failure",
		"the previous job's error must not render beneath the new job's status")

	// The new job's tail starts off the scheduled follow, with no jobs
	// refresh needed.
	res2, tickCmd := got.handleDetailFollowTick(detailFollowTickMsg{gen: got.detailFollowGen})
	got2 := res2.(model)
	require.NotNil(tickCmd, "the follow tick must start the newly selected running job's tail")
	assert.Equal(int64(5), got2.paneLogJobID)
	assert.True(got2.paneLogStreaming)
	assert.Empty(got2.paneLogLines, "the previous job's buffered log must be dropped")
	assert.NotContains(got2.renderSplit(), "job 4 buffered log line")
}

// TestPromptPaginateNavInSplitFollowsSelection is the same property for the
// pagination auto-nav arm, reached when prompt-view nav runs past the end of
// the loaded page and resumes once fetchMoreJobs lands. The newly loaded job
// is DONE here, which is the arm's early-return branch: pre-fix it returned
// with only the prompt fetch, bypassing BOTH the follow transition and the
// splitReconcileDetail call at the bottom of handleJobsMsg, so the pane had
// no path back to the new job at all until some later, unrelated refresh --
// meanwhile still tailing (and still showing the error of) the job the user
// navigated away from.
func TestPromptPaginateNavInSplitFollowsSelection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(
		withCurrentView(viewKindPrompt),
		withTestJobs(promptNavJobs()[0]), // only job 5 (running) loaded so far
		withSelection(0, 5),
	)
	m.promptFromQueue = true
	m.paginateNav = viewKindPrompt
	m.loadingMore = true
	m.paneLogJobID = 5
	m.paneLogStreaming = true
	m.paneLogSeq = 3
	m.splitDetailErr = errors.New("job 5 tail failure")
	prevGen := m.detailFollowGen

	res, cmd := m.handleJobsMsg(jobsMsg{append: true, jobs: []storage.ReviewJob{
		{
			ID: 4, GitRef: "dddd444", Branch: "main", RepoName: "repoA",
			Agent: "codex", Status: storage.JobStatusDone, Prompt: "prompt for 4",
		},
	}})
	got := res.(model)

	require.NotNil(cmd)
	require.Equal(int64(4), got.selectedJobID, "auto-navigate must move the selection to the newly loaded job")
	assert.Greater(got.detailFollowGen, prevGen,
		"the detail follow must be scheduled for the newly loaded job")
	assert.False(got.paneLogStreaming, "the previous job's tail must be stopped")
	assert.Greater(got.paneLogSeq, uint64(3),
		"an in-flight pane-log fetch for the previous job must be invalidated")
	require.NoError(got.splitDetailErr, "the previous job's error must not survive under the new job")

	res2, tickCmd := got.handleDetailFollowTick(detailFollowTickMsg{gen: got.detailFollowGen})
	require.NotNil(tickCmd,
		"the follow tick must fetch the newly selected done job's review")
	assert.Equal(int64(4), res2.(model).selectedJobID)
}

// TestPromptNavInStackedUnaffected confirms the fix is inert outside split:
// followSelectionChange gates on m.layout, so stacked prompt nav still just
// moves the selection and installs the new job's prompt.
func TestPromptNavInStackedUnaffected(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := initTestModel(
		withCurrentView(viewKindPrompt),
		withDimensions(150, 40),
		withTestJobs(promptNavJobs()...),
		withSelection(1, 4),
	)
	m.promptFromQueue = true
	m.paneLogJobID = 4
	m.paneLogStreaming = true
	m.paneLogSeq = 3
	prevGen := m.detailFollowGen
	require.Equal(layoutStacked, m.layout)

	res, _ := m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyRight})
	got := res.(model)

	assert.Equal(int64(5), got.selectedJobID, "the selection still moves")
	// Abandonment is layout-independent (gen bump on a genuine selection
	// change); the split-only parts -- the follow tick and the pane-log
	// invalidation -- must still not run in stacked.
	assert.Greater(got.detailFollowGen, prevGen,
		"the selection change abandons the old job's dispatch even in stacked")
	assert.True(got.paneLogStreaming, "stacked mode must not touch pane-log state")
	assert.Equal(uint64(3), got.paneLogSeq)
	require.NotNil(got.currentReview)
	assert.Equal("prompt for 5", got.currentReview.Prompt)
}

// ---------------------------------------------------------------------------
// handleJobsMsg's selection normalization changes
// selectedJobID and must disarm the
// pending-open intent, not only close the fix panel. "Reactive-only" is
// not safe here: a stale intent is only cleared
// reactively when a message for the old job arrives with a mismatched jobID.
// That argument fails when NOTHING is in flight for the old job -- nothing
// arrives, nothing clears, and the intent waits for the selection to come
// back, at which point reconciliation's follow response consumes it and
// opens the review unbidden.
// ---------------------------------------------------------------------------

// TestNormalizationDisarmsPendingOpenForDeselectedJob is the repro: 'F' arms
// an intent for job 2, a jobs refresh drops job 2 (so normalization moves the
// selection off it), a later refresh selects job 2 again, and then a follow
// response for job 2 lands. The review must NOT open and no panel may spring
// open.
func TestNormalizationDisarmsPendingOpenForDeselectedJob(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withSelection(1, 2)) // split, viewQueue, job 2 selected
	m.tasksEnabled = true

	// 'F' on job 2 arms both intents and dispatches an ordinary fetch.
	res, cmd := m.handleFixKey()
	m = res.(model)
	require.NotNil(cmd)
	require.Equal(int64(2), m.pendingReviewOpenJobID)

	// A refresh drops job 2 from the list: normalization moves the
	// selection off it. Nothing is in flight for job 2 that could clear
	// the intent reactively -- its own fetch is the only outstanding
	// request, and it is exactly what the test does NOT deliver.
	res2, _ := m.handleJobsMsg(jobsMsg{
		jobs:  []storage.ReviewJob{testQueueJobs()[0]}, // job 3 only
		stats: storage.JobStats{},
	})
	m = res2.(model)
	require.NotEqual(int64(2), m.selectedJobID, "sanity: normalization moved the selection off job 2")
	assert.Equal(int64(0), m.pendingReviewOpenJobID,
		"normalization moving the selection off the intent's job abandons that intent")

	// A later refresh brings job 2 back as the only row, so normalization
	// reselects it.
	res3, _ := m.handleJobsMsg(jobsMsg{
		jobs:  []storage.ReviewJob{testQueueJobs()[1]}, // job 2 only
		stats: storage.JobStats{},
	})
	m = res3.(model)
	require.Equal(int64(2), m.selectedJobID, "sanity: the selection came back to job 2")

	// Reconciliation's follow response for job 2 lands.
	res4, _ := m.handleReviewMsg(reviewMsg{
		review: splitTestReview(), jobID: 2, follow: true,
		gen: m.detailFollowGen, fetchSeq: m.reviewFetchSeq,
		attempt: m.jobAttemptGen[2], dispatchedFrom: viewQueue,
	})
	got := res4.(model)

	assert.Equal(viewQueue, got.currentView,
		"a follow response must not open the review view for an intent abandoned by normalization")
	assert.Equal(focusList, got.focus, "and must not steal focus")
	assert.False(got.reviewFixPanelOpen, "no panel may spring open")
	assert.False(got.reviewFixPanelPending)
}

// TestNormalizationKeepsIntentWhenSelectionUnchanged is the property
// this must not regress: a refresh that leaves the selection where it is
// abandons nothing, so the armed intent is still served when its own
// response arrives.
func TestNormalizationKeepsIntentWhenSelectionUnchanged(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withSelection(1, 2))
	m.tasksEnabled = true

	res, cmd := m.handleFixKey()
	m = res.(model)
	require.NotNil(cmd)
	require.Equal(int64(2), m.pendingReviewOpenJobID)

	// A perfectly ordinary refresh: job 2 is still there and still selected.
	res2, _ := m.handleJobsMsg(jobsMsg{jobs: testQueueJobs(), stats: storage.JobStats{}})
	m = res2.(model)
	require.Equal(int64(2), m.selectedJobID)
	assert.Equal(int64(2), m.pendingReviewOpenJobID,
		"a refresh that does not move the selection must not disarm the intent")
	assert.True(m.reviewFixPanelPending, "nor close the pending fix panel")

	// The 'F' fetch's own response still serves it.
	res3, _ := m.handleReviewMsg(reviewMsg{
		review: splitTestReview(), jobID: 2,
		gen: m.detailFollowGen, fetchSeq: m.reviewFetchSeq,
		attempt: m.jobAttemptGen[2], dispatchedFrom: viewQueue,
	})
	got := res3.(model)
	assert.Equal(viewReview, got.currentView, "the intent must still be served")
	assert.True(got.reviewFixPanelOpen, "and the panel must still open")
}

// TestNormalizationLeavesForeignIntentArmed guards the keying: the disarm is
// keyed on the job the selection moved OFF, so an intent armed for some other
// job -- tasks 'P' on a parent that the queue selection has never sat on, for
// instance -- is untouched by unrelated normalization.
func TestNormalizationLeavesForeignIntentArmed(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withSelection(1, 2))
	// An intent armed for job 9, which is not the selection.
	m.pendingReviewOpenJobID = 9
	m.pendingReviewOpenOrigin = viewTasks
	m.pendingReviewOpenSeq = m.reviewFetchSeq

	res, _ := m.handleJobsMsg(jobsMsg{
		jobs:  []storage.ReviewJob{testQueueJobs()[0]}, // job 3 only: selection moves off 2
		stats: storage.JobStats{},
	})
	got := res.(model)

	assert.NotEqual(int64(2), got.selectedJobID, "sanity: normalization moved the selection")
	assert.Equal(int64(9), got.pendingReviewOpenJobID,
		"an intent for a job the selection never sat on must survive an unrelated normalization")
}

// TestFilterResetDisarmsPendingOpenForDeselectedJob:
// resetQueueForFilterChange (filter.go) is the
// shared chokepoint for every filter mutation -- the filter modal, the queue
// shortcuts and the control socket all funnel through it -- and it zeroes
// selectedJobID. The reactive clear does not cover that, because the refetch
// it triggers can RE-SELECT the intent's own job before any message for that
// job arrives, at which point the intent looks current again and the next
// response for it opens the review (or springs the pending panel open) with
// a filter change as the only thing the user actually asked for.
func TestFilterResetDisarmsPendingOpenForDeselectedJob(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withSelection(1, 2))
	m.tasksEnabled = true

	res, cmd := m.handleFixKey()
	m = res.(model)
	require.NotNil(cmd)
	require.Equal(int64(2), m.pendingReviewOpenJobID)
	require.True(m.reviewFixPanelPending)

	m.resetQueueForFilterChange()
	assert.Equal(int64(0), m.pendingReviewOpenJobID,
		"zeroing the selection abandons the intent bound to the job it deselected")
	assert.False(m.reviewFixPanelPending, "and the pending fix panel with it")

	// The post-filter refetch lands with job 2 as the only visible row, so
	// normalization reselects exactly the job the intent was armed for.
	res2, _ := m.handleJobsMsg(jobsMsg{
		seq:   m.fetchSeq,
		jobs:  []storage.ReviewJob{testQueueJobs()[1]},
		stats: storage.JobStats{},
	})
	m2 := res2.(model)
	require.Equal(int64(2), m2.selectedJobID, "sanity: the filtered list reselects job 2")

	// Reconciliation's follow response for job 2 lands.
	res3, _ := m2.handleReviewMsg(reviewMsg{
		review: splitTestReview(), jobID: 2, follow: true,
		gen: m2.detailFollowGen, fetchSeq: m2.reviewFetchSeq,
		attempt: m2.jobAttemptGen[2], dispatchedFrom: viewQueue,
	})
	got := res3.(model)

	assert.Equal(viewQueue, got.currentView,
		"a filter change must not end with the review view opening by itself")
	assert.False(got.reviewFixPanelOpen, "nor with the fix panel springing open")
}

// ---------------------------------------------------------------------------
// Tasks Enter and tasks 'P' move the QUEUE selection
// from inside the Tasks view. Tasks keys return from handleKeyMsg's early
// view switch before its followSelectionChange wrapper, so neither site gave
// the split detail pane the shared transition: it kept tailing (and kept
// showing the splitDetailErr of) the job the selection moved off, until
// handlePaneLogTickMsg's own selected-job check killed the tail on a later
// poll and splitReconcileDetail rebuilt on a later refresh.
// ---------------------------------------------------------------------------

// tailingTasksModel is the Tasks view over a split layout whose queue
// selection is running job 3, with the detail pane actively tailing it and
// an error recorded against that tail.
func tailingTasksModel() model {
	m := splitModel(withCurrentView(viewTasks), withSelection(0, 3)) // job 3: running
	m.tasksEnabled = true
	m.paneLogJobID = 3
	m.paneLogStreaming = true
	m.paneLogSeq = 3
	m.paneLogLines = []logLine{{text: "job 3 buffered log line"}}
	m.splitDetailErr = errors.New("job 3 tail failure")
	m.fixSelectedIdx = 0
	return m
}

func TestTasksParentKeyFollowsSelection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	parentID := int64(2)
	m := tailingTasksModel()
	m.fixJobs = []storage.ReviewJob{
		{ID: 101, Status: storage.JobStatusDone, ParentJobID: &parentID},
	}
	prevGen := m.detailFollowGen

	got, cmd := pressKey(m, 'P')

	require.Equal(parentID, got.selectedJobID, "'P' moves the queue selection to the parent job")
	require.NotNil(cmd, "the follow must be batched with the parent's review fetch")
	assert.Greater(got.detailFollowGen, prevGen,
		"the detail follow must be scheduled for the newly selected job")
	assert.False(got.paneLogStreaming, "the previous job's tail must be stopped")
	assert.Greater(got.paneLogSeq, uint64(3),
		"an in-flight pane-log fetch for the previous job must be invalidated")
	require.NoError(got.splitDetailErr, "the previous job's error must not survive under the new job")
	assert.Equal(parentID, got.pendingReviewOpenJobID,
		"the follow's disarm must run BEFORE the dispatch re-arms the intent for the parent")
}

func TestTasksEnterFollowsSelection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := tailingTasksModel()
	m.fixJobs = []storage.ReviewJob{{ID: 101, Status: storage.JobStatusDone}}
	prevGen := m.detailFollowGen

	res, cmd := m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := res.(model)

	require.Equal(int64(101), got.selectedJobID, "Enter moves the queue selection to the fix job")
	require.NotNil(cmd)
	assert.Greater(got.detailFollowGen, prevGen,
		"the detail follow must be scheduled for the newly selected job")
	assert.False(got.paneLogStreaming, "the previous job's tail must be stopped")
	assert.Greater(got.paneLogSeq, uint64(3))
	assert.NoError(got.splitDetailErr)
}

// TestTasksKeysInStackedUnaffected confirms the site-21 fix is inert outside
// split layout, like every other followSelectionChange caller.
func TestTasksKeysInStackedUnaffected(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	parentID := int64(2)
	m := initTestModel(
		withCurrentView(viewTasks),
		withDimensions(150, 40),
		withTestJobs(testQueueJobs()...),
		withSelection(0, 3),
	)
	m.tasksEnabled = true
	m.fixJobs = []storage.ReviewJob{
		{ID: 101, Status: storage.JobStatusDone, ParentJobID: &parentID},
	}
	m.paneLogJobID = 3
	m.paneLogStreaming = true
	m.paneLogSeq = 3
	prevGen := m.detailFollowGen
	require.Equal(layoutStacked, m.layout)

	got, cmd := pressKey(m, 'P')

	require.NotNil(cmd)
	assert.Equal(parentID, got.selectedJobID, "the selection still moves")
	// The selection change is an abandonment in any layout (gen bump);
	// the dispatch that follows immediately re-arms the pending-open
	// intent at the new gen, so the user's 'P' still opens the parent.
	// The split-only parts -- the follow tick and pane-log invalidation
	// -- must still not run in stacked.
	assert.Greater(got.detailFollowGen, prevGen,
		"the selection change abandons the old job's dispatch even in stacked")
	assert.Equal(parentID, got.pendingReviewOpenJobID,
		"the tasks 'P' dispatch re-arms the open intent after the abandonment")
	assert.True(got.paneLogStreaming, "stacked mode must not touch pane-log state")
	assert.Equal(uint64(3), got.paneLogSeq)
}

// TestEligibleReviewRowExcludesLiveJobs pins the constraint documented on
// eligibleReviewRow (nav.go): stepReviewNav and handleJobsMsg's pagination
// viewReview arm move selectedJobID WITHOUT followSelectionChange, and are
// correct only because this predicate can never land them on a job with a
// live pane-log tail. Widening it without routing those two consumers
// through followSelectionChange first would strand the pane on the old
// job, so this test fails deliberately if the predicate changes.
func TestEligibleReviewRowExcludesLiveJobs(t *testing.T) {
	assert := assert.New(t)
	assert.False(eligibleReviewRow(storage.ReviewJob{Status: storage.JobStatusRunning}),
		"running jobs must stay out of review nav -- see eligibleReviewRow's constraint comment (nav.go)")
	assert.False(eligibleReviewRow(storage.ReviewJob{Status: storage.JobStatusQueued}),
		"queued jobs must stay out of review nav -- see eligibleReviewRow's constraint comment (nav.go)")
	assert.True(eligibleReviewRow(storage.ReviewJob{Status: storage.JobStatusDone}))
	assert.True(eligibleReviewRow(storage.ReviewJob{Status: storage.JobStatusFailed}))
}

// TestCommentOnSynthesizedFailedReviewAppendsLocally: a synthesized
// failed-job review has no persisted review row, so the comment refresh's
// /api/review fetch would 404 and the created comment would never appear.
// The handler must append the posted comment directly instead.
func TestCommentOnSynthesizedFailedReviewAppendsLocally(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withSelection(2, 1)) // job 1: failed
	m.currentReview = synthesizeFailedReview(&m.jobs[2], nil)
	require.Zero(m.currentReview.ID, "sanity: synthesized reviews have no persisted row")

	res, cmd := m.handleCommentResultMsg(commentResultMsg{
		jobID: 1, responder: "wes", comment: "known flake, not a regression",
	})
	got := res.(model)
	assert.NotNil(cmd, "the append dispatches a reconciling comments refetch")
	require.Len(got.currentResponses, 1)
	assert.Equal("known flake, not a regression", got.currentResponses[0].Response)
	assert.Equal("wes", got.currentResponses[0].Responder)

	lines := strings.Join(got.renderDetailPane(88, 25), "\n")
	assert.Contains(lines, "known flake, not a regression",
		"the appended comment must render under the failure review")
}

// TestStaleFailedCommentsResponseCannotOverwriteNewerState: the comments
// side channel carries its own request identity. A pre-post fetch's
// response landing after the post-success local append must be dropped
// (the append's own reconciling refetch bumped the seq), and an older
// dispatch's response landing after a newer dispatch's must be dropped
// too -- synthesized reviews all share ID 0, so nothing else tells the
// two requests apart.
func TestStaleFailedCommentsResponseCannotOverwriteNewerState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withSelection(2, 1)) // job 1: failed, selected

	// First display dispatches fetch #1 (pre-post: server has no comments).
	m, cmd := m.splitReconcileDetail()
	require.NotNil(cmd)
	preP0stSeq := m.failedCommentsSeq

	// The user's comment posts successfully: local append + refetch #2.
	res, cmd2 := m.handleCommentResultMsg(commentResultMsg{
		jobID: 1, responder: "wes", comment: "fresh comment",
	})
	m = res.(model)
	require.NotNil(cmd2)
	require.Len(m.currentResponses, 1)

	// Fetch #1's response (empty, served before the POST) lands late:
	// it must not wipe the appended comment.
	res, _ = m.handleFailedCommentsMsg(failedCommentsMsg{jobID: 1, seq: preP0stSeq})
	m = res.(model)
	require.Len(m.currentResponses, 1,
		"a stale pre-post response must not wipe the just-appended comment")
	assert.Equal("fresh comment", m.currentResponses[0].Response)

	// Refetch #2's response (server truth, includes the comment) lands.
	res, _ = m.handleFailedCommentsMsg(failedCommentsMsg{
		jobID: 1, seq: m.failedCommentsSeq,
		responses: []storage.Response{{Responder: "wes", Response: "fresh comment"}},
	})
	m = res.(model)
	assert.Len(m.currentResponses, 1, "server truth replaces the optimistic copy exactly once")
}

// TestFailedReviewCommentsSurviveNavigationRoundTrip: every synthesized
// acceptance dispatches a persisted-comments fetch, so comments on a
// failed job's review survive navigating away and back (the rebuild
// clears the in-memory copy; the fetch restores server state).
func TestFailedReviewCommentsSurviveNavigationRoundTrip(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withSelection(2, 1)) // job 1: failed, selected

	// First display: reconcile synthesizes and dispatches the fetch.
	m, cmd := m.splitReconcileDetail()
	require.NotNil(cmd)
	res, _ := m.handleFailedCommentsMsg(failedCommentsMsg{
		jobID: 1, seq: m.failedCommentsSeq,
		responses: []storage.Response{{Responder: "wes", Response: "known flake"}},
	})
	m = res.(model)
	require.Len(m.currentResponses, 1)

	// Navigate away: a late comments response for job 1 is stale now.
	m = m.moveSelectionToJobID(3)
	m, _ = m.followSelectionChange(1)
	res, _ = m.handleFailedCommentsMsg(failedCommentsMsg{
		jobID: 1, seq: m.failedCommentsSeq,
		responses: []storage.Response{{Responder: "eve", Response: "should not land"}},
	})
	m = res.(model)
	assert.NotContains(strings.Join(m.renderDetailPane(88, 25), "\n"), "should not land")

	// Back to job 1: the tick re-synthesizes and re-dispatches the fetch.
	m = m.moveSelectionToJobID(1)
	m, _ = m.followSelectionChange(3)
	res, tickCmd := m.handleDetailFollowTick(detailFollowTickMsg{gen: m.detailFollowGen})
	m = res.(model)
	require.NotNil(m.currentReview)
	require.NotNil(tickCmd, "the re-synthesis must re-dispatch the comments fetch")
	require.Empty(m.currentResponses, "sanity: the rebuild cleared the in-memory comments")

	// The fetch response restores them and they render.
	res, _ = m.handleFailedCommentsMsg(failedCommentsMsg{
		jobID: 1, seq: m.failedCommentsSeq,
		responses: []storage.Response{{Responder: "wes", Response: "known flake"}},
	})
	m = res.(model)
	assert.Contains(strings.Join(m.renderDetailPane(88, 25), "\n"), "known flake",
		"comments must survive the navigation round-trip")
}

// TestClosingLastVisibleJobClearsSelection: with hide-closed on, closing
// the only visible job must clear the selection (like the cancel twin) --
// left pointing at the now-hidden job, the list shows "No jobs" while the
// detail pane stays actionable for the invisible review. The rollback
// restores by msg.jobID, so a server rejection still re-selects it.
func TestClosingLastVisibleJobClearsSelection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	closed := false
	finishedAt := splitTestFinishedAt
	only := storage.ReviewJob{
		ID: 2, GitRef: "bbbb222", RepoName: "repoA", Agent: "codex",
		Status: storage.JobStatusDone, Closed: &closed, FinishedAt: &finishedAt,
	}
	m := splitModel(withTestJobs(only), withSelection(0, 2))
	m.hideClosed = true

	res, cmd := m.handleCloseKey()
	got := res.(model)
	require.NotNil(cmd)
	assert.Equal(int64(0), got.selectedJobID, "no visible replacement: the selection must clear")
	assert.Equal(-1, got.selectedIdx)

	// A server rejection rolls the selection back onto the job.
	res, _ = got.handleClosedResultMsg(closedResultMsg{
		jobID: 2, restoreSelection: true, oldState: false, newState: true,
		seq: got.closedSeq, err: errors.New("daemon unreachable"),
	})
	assert.Equal(int64(2), res.(model).selectedJobID,
		"the rollback must restore the selection from the cleared state")
}

// TestNormalizeSelectionClearsWhenNothingVisible: returning to the queue
// with the selection on a hidden job and no visible job anywhere must
// clear the selection, matching normalizeSelectionIfHidden's own
// out-of-bounds branch.
func TestNormalizeSelectionClearsWhenNothingVisible(t *testing.T) {
	assert := assert.New(t)
	closed := true
	finishedAt := splitTestFinishedAt
	only := storage.ReviewJob{
		ID: 2, GitRef: "bbbb222", RepoName: "repoA", Agent: "codex",
		Status: storage.JobStatusDone, Closed: &closed, FinishedAt: &finishedAt,
	}
	m := splitModel(withTestJobs(only), withSelection(0, 2))
	m.hideClosed = true

	m.normalizeSelectionIfHidden()
	assert.Equal(int64(0), m.selectedJobID,
		"a hidden selection with no visible replacement must clear")
	assert.Equal(-1, m.selectedIdx)
}

// TestRunningTaskPromptNotOverwrittenByReconcile: opening a running task's
// prompt moves the selection to the fix job like the completed-task path.
// Left on the previous queue job, split reconciliation would keep
// following THAT job on every jobs refresh and its follow fetch would
// replace currentReview -- swapping the displayed prompt's backing review
// out from under the user.
func TestRunningTaskPromptNotOverwrittenByReconcile(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withReview(splitTestReview())) // queue selection: job 2
	m.tasksEnabled = true
	m.currentView = viewTasks
	m.fixJobs = []storage.ReviewJob{{
		ID: 101, Status: storage.JobStatusRunning,
		Agent: "codex", Prompt: "fix the null deref",
	}}
	m.fixSelectedIdx = 0

	res, _ := m.handleTasksKey(keySpecialMsg(tea.KeyEnter))
	m = res.(model)
	require.Equal(viewKindPrompt, m.currentView)
	require.Equal("fix the null deref", m.currentReview.Prompt)
	assert.Equal(int64(101), m.selectedJobID,
		"the selection must follow the task job so reconcile has nothing stale to follow")

	// A jobs refresh lands while the prompt is displayed.
	res, _ = m.handleJobsMsg(jobsMsg{jobs: testQueueJobs(), stats: storage.JobStats{}})
	got := res.(model)
	assert.Equal(viewKindPrompt, got.currentView)
	require.NotNil(got.currentReview)
	assert.Equal("fix the null deref", got.currentReview.Prompt,
		"the displayed prompt's backing review must survive the refresh")
}

// TestAnchoredClosedReviewSurvivesHideClosedPruning: with hide-closed on,
// closing the displayed review removes its row from the next refresh while
// handleJobsMsg preserves the review-anchored selection -- the pane must
// keep rendering the loaded review (and its actions must stay unblocked,
// e.g. 'a' to unclose) instead of vanishing into "No job selected".
func TestAnchoredClosedReviewSurvivesHideClosedPruning(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withReview(splitTestReview())) // job 2's review loaded
	m.currentView = viewReview
	m.focus = focusDetail
	m.hideClosed = true

	// The refresh omits the just-closed job 2 entirely.
	pruned := []storage.ReviewJob{testQueueJobs()[0], testQueueJobs()[2]}
	res, _ := m.handleJobsMsg(jobsMsg{jobs: pruned, stats: storage.JobStats{}})
	got := res.(model)
	require.Equal(int64(2), got.selectedJobID, "sanity: the anchored selection is preserved")
	_, ok := got.selectedJob()
	require.False(ok, "sanity: the job row is gone")

	assert.True(got.selectedReviewLoaded(),
		"the anchored review is the only truth for the pruned job and must count as loaded")
	lines := strings.Join(got.renderDetailPane(88, 25), "\n")
	assert.Contains(lines, "first finding", "the review must keep rendering")
	assert.NotContains(lines, "No job selected")

	_, blocked := got.guardStaleSplitDetailAction(keyPressMsg('a'))
	assert.False(blocked, "review actions (unclose) must not be blocked")
}

// TestLeaveSplitClosesPanelWithDiscardedReview: applyLayout's leave-split
// discard branch nils currentReview -- an open fix panel bound to that
// review must close with it, or the next review rendered in stacked (e.g.
// Enter on a failed job, whose synchronous synthesis assigns
// currentReview directly without acceptReview's wrong-job panel close)
// shows the stale panel and submitting targets the previous job.
func TestLeaveSplitClosesPanelWithDiscardedReview(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withReview(splitTestReview())) // job 2's review loaded
	m.focus = focusList                            // list focus: the leave discards the review
	m.reviewFixPanelOpen = true
	m.fixPromptJobID = 2
	m.fixPromptText = "typed for job 2"

	m.applyLayout(layoutStacked)
	require.Nil(m.currentReview, "sanity: the leave discarded the review")
	assert.False(m.reviewFixPanelOpen, "the panel must close with the review it was bound to")
	assert.Equal(int64(0), m.fixPromptJobID)

	// Selecting a failed job in stacked renders its synthesized review
	// with no stale panel over it.
	m.selectedIdx, m.selectedJobID = 2, 1
	m.currentReview = synthesizeFailedReview(&m.jobs[2], nil)
	m.currentView = viewReview
	assert.False(m.reviewFixPanelOpen,
		"the failed job's review must not inherit the previous job's panel")
}

// TestSupersededResponseDoesNotReopenAfterEsc: the superseded-branch
// fallback switch fires only while the matching pending-open intent is
// still armed. Once a newer response consumed the intent (opening the
// view) and the user pressed esc back to the queue, a late older response
// owes nothing -- an unconditional content-present switch would yank the
// user back into the review with no request outstanding.
func TestSupersededResponseDoesNotReopenAfterEsc(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel() // job 2 (done) selected, viewQueue

	require.NotNil(m.dispatchReviewFetch(2)) // ordinary fetch, arms the intent
	staleMsg := reviewMsg{
		review: splitTestReview(), jobID: 2,
		gen: m.detailFollowGen, fetchSeq: m.reviewFetchSeq,
		dispatchedFrom: viewQueue,
	}

	// A newer follow lands first: consumes the intent, opens the view.
	require.NotNil(m.dispatchReviewFollow(2))
	res, _ := m.handleReviewMsg(reviewMsg{
		review: splitTestReview(), jobID: 2, follow: true,
		gen: m.detailFollowGen, fetchSeq: m.reviewFetchSeq,
	})
	m = res.(model)
	require.Equal(viewReview, m.currentView, "sanity: the intent was served")
	require.Equal(int64(0), m.pendingReviewOpenJobID)

	// The user leaves the review.
	res, _ = pressSpecial(m, tea.KeyEscape)
	m = res.(model)
	require.Equal(viewQueue, m.currentView)

	// The old ordinary response finally lands: superseded, no armed
	// intent -- it must not reopen the review.
	res, _ = m.handleReviewMsg(staleMsg)
	assert.Equal(viewQueue, res.(model).currentView,
		"a late superseded response must not reopen a review the user already left")
}

// TestRerunDuringPromptViewReturnsToQueue: a control-socket rerun of the
// prompted job clears currentReview while the prompt view is open;
// viewContent then silently renders the queue while keys still route as
// prompt input. normalizeSplitState must repair the view.
func TestRerunDuringPromptViewReturnsToQueue(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withReview(splitTestReview())) // job 2's review loaded
	m.currentView = viewKindPrompt
	m.promptFromQueue = true

	res, _ := m.handleRerunResultMsg(rerunResultMsg{jobID: 2})
	got := res.(model)
	require.Nil(got.currentReview, "sanity: the rerun cleared the loaded review")

	got = got.normalizeSplitState() // Update applies this before rendering
	assert.Equal(viewQueue, got.currentView,
		"a nil-backed prompt view must return to the queue, not linger over a queue render")
}

// TestFailedReviewStaleAfterMissedRerunWindow: a rerun whose whole
// queued/running window fell between refreshes and then failed with
// IDENTICAL error text leaves the loaded earlier failure matching on
// JobID and Output -- the completion-identity comparison (FinishedAt) is
// what marks it stale, in both selectedReviewLoaded and the reconcile
// fast path.
func TestFailedReviewStaleAfterMissedRerunWindow(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withSelection(2, 1)) // job 1: failed, error "boom"
	m.currentReview = synthesizeFailedReview(&m.jobs[2], nil)
	require.True(m.selectedReviewLoaded(), "sanity: the synthesized failure is current")

	// A missed-window rerun fails again at a later time with the SAME text.
	later := splitTestFinishedAt.Add(time.Minute)
	m.jobs[2].FinishedAt = &later
	assert.False(m.selectedReviewLoaded(),
		"a newer completion with identical text must still read as stale")

	// Reconcile rebuilds rather than treating the old attempt as current.
	got, _ := m.splitReconcileDetail()
	require.NotNil(got.currentReview)
	require.NotNil(got.currentReview.Job.FinishedAt)
	assert.True(got.currentReview.Job.FinishedAt.Equal(later),
		"the rebuild must embed the NEW attempt's completion identity")
}

// TestPreRerunResponseCannotOverwriteSynthesizedFailure: the synchronous
// failed-job rebuild is routed through the shared ordered acceptance
// (acceptSynthesizedFailure bumps the fetch epoch). Without the bump, a
// reviewMsg dispatched BEFORE an external rerun -- which bumps no
// jobAttemptGen and moves no selection, so every other gate still passes
// -- would land after the rebuild and replace the synthesized failure
// with the previous attempt's review until a later refresh corrected it.
func TestPreRerunResponseCannotOverwriteSynthesizedFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel() // job 2 (done) selected, no review loaded

	// An ordinary fetch for job 2 goes out pre-rerun.
	require.NotNil(m.dispatchReviewFetch(2))
	staleMsg := reviewMsg{
		review: splitTestReview(), jobID: 2,
		gen: m.detailFollowGen, fetchSeq: m.reviewFetchSeq,
		dispatchedFrom: viewQueue,
	}

	// The job is externally rerun and FAILS before that response lands.
	m.jobs[1].Status = storage.JobStatusFailed
	m.jobs[1].Error = "rerun exploded"
	m, _ = m.splitReconcileDetail()
	require.NotNil(m.currentReview)
	require.Contains(m.currentReview.Output, "rerun exploded")

	// The pre-rerun response lands afterward: superseded, content dropped.
	res, _ := m.handleReviewMsg(staleMsg)
	got := res.(model)
	require.NotNil(got.currentReview)
	assert.Contains(got.currentReview.Output, "rerun exploded",
		"the previous attempt's review must not overwrite the synthesized failure")
	assert.NotContains(got.currentReview.Output, "first finding")
}

// TestDetailFollowTickRefetchesStaleAttemptReview: the follow tick's Done
// fast path treats only a CURRENT review as "already loaded". A
// stale-but-matching review (the job was rerun and completed since it was
// loaded) renders as "Loading review..." under renderDetailPane's
// freshness gate, so skipping the fetch would stall that placeholder until
// the next fallback poll.
func TestDetailFollowTickRefetchesStaleAttemptReview(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withReview(splitTestReview())) // job 2 (done) selected
	later := splitTestFinishedAt.Add(time.Minute)
	m.jobs[1].FinishedAt = &later // a rerun completed since the review was loaded

	m, _ = m.scheduleDetailFollow()
	res, cmd := m.handleDetailFollowTick(detailFollowTickMsg{gen: m.detailFollowGen})
	assert.NotNil(cmd, "a stale-but-matching review must dispatch an immediate refetch")
	_ = res

	// Control: a fresh review skips the fetch.
	m2 := splitModel(withReview(splitTestReview()))
	m2, _ = m2.scheduleDetailFollow()
	_, cmd2 := m2.handleDetailFollowTick(detailFollowTickMsg{gen: m2.detailFollowGen})
	assert.Nil(cmd2, "a current review needs no refetch")
}

// TestBootstrapDetailRefetchesStaleAttemptReview: maybeBootstrapDetail's
// twin of the follow-tick fast path -- entering split with a
// stale-but-matching review must schedule the follow rather than leave the
// pane's loading placeholder stalled.
func TestBootstrapDetailRefetchesStaleAttemptReview(t *testing.T) {
	assert := assert.New(t)
	m := splitModel(withReview(splitTestReview()))
	later := splitTestFinishedAt.Add(time.Minute)
	m.jobs[1].FinishedAt = &later

	_, cmd := m.maybeBootstrapDetail()
	assert.NotNil(cmd, "a stale-but-matching review must schedule the follow on split engage")

	// Control: a fresh review schedules nothing.
	m2 := splitModel(withReview(splitTestReview()))
	_, cmd2 := m2.maybeBootstrapDetail()
	assert.Nil(cmd2)
}

// TestFilterResetAbandonsPromptAndReconcileSlot: resetQueueForFilterChange
// is an abandonment event like user navigation -- it must doom an
// in-flight prompt response (a refetch can re-select the same job before
// it lands, re-satisfying every other handlePromptMsg gate) and release
// the reconcile suppression slot (left armed, it suppresses the
// re-selected job's replacement fetch until another refresh).
func TestFilterResetAbandonsPromptAndReconcileSlot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	// Prompt half.
	m := splitModel() // job 2 (done) selected
	require.NotNil(m.dispatchPromptFetch(2))
	staleSeq := m.promptFetchSeq
	m.resetQueueForFilterChange()
	m.jobs = testQueueJobs() // the refetch lands and re-selects job 2
	m.selectedIdx, m.selectedJobID = 1, 2
	res, _ := m.handlePromptMsg(promptMsg{
		review: splitTestReview(), jobID: 2,
		promptSeq: staleSeq, dispatchedFrom: viewQueue,
	})
	got := res.(model)
	assert.Equal(viewQueue, got.currentView,
		"an abandoned prompt response must not open the prompt view after a filter reset")
	assert.Nil(got.currentReview)

	// Reconcile-slot half.
	m = splitModel()
	m, cmd := m.splitReconcileDetail() // arms the slot for job 2
	require.NotNil(cmd)
	require.Equal(int64(2), m.reconcileFetchJobID)
	m.resetQueueForFilterChange()
	assert.Zero(m.reconcileFetchJobID, "the abandoned era's suppression slot must be released")
	m.jobs = testQueueJobs()
	m.selectedIdx, m.selectedJobID = 1, 2
	_, cmd2 := m.splitReconcileDetail()
	assert.NotNil(cmd2, "the re-selected job's replacement fetch must not be suppressed")
}

// TestJobsNormalizationAbandonsPromptAndReconcileSlot: handleJobsMsg's
// selection normalization (the selected job vanished from the refresh) is
// the same abandonment event -- same request-scoped state to doom.
func TestJobsNormalizationAbandonsPromptAndReconcileSlot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel() // job 2 (done) selected
	require.NotNil(m.dispatchPromptFetch(2))
	stalePromptSeq := m.promptFetchSeq
	m, cmd := m.splitReconcileDetail() // arms the slot for job 2
	require.NotNil(cmd)
	require.Equal(int64(2), m.reconcileFetchJobID)

	// A refresh without job 2 normalizes the selection off it.
	remaining := []storage.ReviewJob{testQueueJobs()[0], testQueueJobs()[2]}
	res, _ := m.handleJobsMsg(jobsMsg{jobs: remaining, stats: storage.JobStats{}})
	got := res.(model)
	require.NotEqual(int64(2), got.selectedJobID, "sanity: normalization moved the selection")

	assert.NotEqual(stalePromptSeq, got.promptFetchSeq,
		"normalization must doom the abandoned prompt request")
	assert.Zero(got.reconcileFetchJobID,
		"normalization must release the abandoned era's suppression slot")
}

// TestLeaveSplitDropsStaleAttemptReview: leaving split with detail focus
// keeps the full-screen review only when it is the selected job's CURRENT
// attempt. A stale attempt's review (external rerun observed) must not
// carry into stacked, where reconcile no longer runs to replace it and
// review actions would target the obsolete attempt indefinitely.
func TestLeaveSplitDropsStaleAttemptReview(t *testing.T) {
	assert := assert.New(t)

	m := splitModel(withReview(splitTestReview())) // job 2 (done) selected
	m.currentView = viewReview
	m.focus = focusDetail
	m.paneReviewSeenNonTerminalJob = 2 // job 2's external rerun was observed

	m.applyLayout(layoutStacked)
	assert.Equal(viewQueue, m.currentView, "a stale review must not carry into stacked full-screen")
	assert.Nil(m.currentReview)

	// Control: a fresh review survives the leave.
	m2 := splitModel(withReview(splitTestReview()))
	m2.currentView = viewReview
	m2.focus = focusDetail
	m2.applyLayout(layoutStacked)
	assert.Equal(viewReview, m2.currentView)
	assert.NotNil(m2.currentReview)

	// Control: a tasks-origin review (fix job, never resolvable in m.jobs)
	// is preserved unconditionally -- the user is reading it.
	m3 := splitModel()
	m3.currentView = viewReview
	m3.reviewFromView = viewTasks
	m3.focus = focusDetail
	m3.currentReview = &storage.Review{JobID: 99, Output: "fix review"}
	m3.selectedJobID = 99
	m3.applyLayout(layoutStacked)
	assert.Equal(viewReview, m3.currentView)
	assert.NotNil(m3.currentReview)
}

// TestDistractionFreeForcesStackedAtSplitDims: distraction-free is a
// title-plus-list-only contract; at split-capable dimensions it must
// disable the split composition (detail pane, borders, info line, footer)
// entirely, not render split chrome around a compact list. L cannot
// re-engage split while it is active; toggling D off restores split.
func TestDistractionFreeForcesStackedAtSplitDims(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withReview(splitTestReview())) // 150x40: split-capable
	require.True(m.splitActive())

	got, _ := pressKey(m, 'D')
	assert.True(got.distractionFree)
	assert.Equal(layoutStacked, got.layout)
	assert.False(got.splitActive())
	out := got.viewContent()
	assert.NotContains(out, "│", "no split pane borders in distraction-free")
	assert.NotContains(out, "focus detail", "no split help footer in distraction-free")

	// L must not re-engage the split composition while active.
	got2, _ := pressKey(got, 'L')
	assert.Equal(layoutStacked, got2.layout)
	assertFlashMessage(t, got2, viewQueue,
		"Split layout is unavailable in distraction-free mode (press D to exit)")

	// Toggling D off restores split at these dimensions.
	got3, _ := pressKey(got2, 'D')
	assert.False(got3.distractionFree)
	assert.Equal(layoutSplit, got3.layout)
}

// TestRunningPromptSurvivesJobFailure: the prompt view over a running job
// renders a synthetic review built from the row's Prompt, and the daemon
// strips terminal jobs' prompts from listings (stripJobPrompts) -- so when
// the job fails, that synthetic review is the LAST copy of the prompt the
// TUI holds. splitReconcileDetail's failure synthesis must carry it over,
// or the open prompt view blanks unrecoverably.
func TestRunningPromptSurvivesJobFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withSelection(0, 3)) // job 3: running
	m.jobs[0].Prompt = "review the diff carefully"

	res, _ := m.handlePromptKey()
	m = res.(model)
	require.Equal(viewKindPrompt, m.currentView)
	require.Equal("review the diff carefully", m.currentReview.Prompt)

	// The job fails; the refreshed row arrives with its prompt stripped.
	m.jobs[0].Status = storage.JobStatusFailed
	m.jobs[0].Error = "agent exploded"
	m.jobs[0].Prompt = ""
	m, _ = m.splitReconcileDetail()

	assert.Equal(viewKindPrompt, m.currentView, "the prompt view stays put")
	require.NotNil(m.currentReview)
	assert.Equal("review the diff carefully", m.currentReview.Prompt,
		"the displayed prompt must survive the failure synthesis")
	assert.Contains(m.currentReview.Output, "agent exploded",
		"the synthesized failure content still replaces the review body")
}

// TestPromptMsgDoesNotYankTransientView: the queue stays interactive while
// a 'p' fetch is in flight, so its response can land after the user opened
// a filter/tasks/help view -- the dispatch-origin guard must drop it
// rather than replace the view the user is now in.
func TestPromptMsgDoesNotYankTransientView(t *testing.T) {
	assert := assert.New(t)
	m := splitModel() // job 2 (done) selected, viewQueue
	cmd := m.dispatchPromptFetch(2)
	require.NotNil(t, cmd)
	msg := promptMsg{
		review: splitTestReview(), jobID: 2,
		promptSeq: m.promptFetchSeq, dispatchedFrom: viewQueue,
	}

	// The user opens the filter view before the response lands.
	m.currentView = viewFilter
	res, _ := m.handlePromptMsg(msg)
	got := res.(model)
	assert.Equal(viewFilter, got.currentView,
		"a slow prompt response must not replace the view the user opened")
	assert.Nil(got.currentReview, "dropped entirely, not loaded in the background")
}

// TestAbandonedPromptRequestDroppedOnReturn: navigating away and back to
// the same job re-satisfies the jobID gate, so without its own request
// identity an abandoned 'p' fetch would pop the prompt view open with no
// fresh keypress. followSelectionChange bumps promptFetchSeq on every
// genuine selection change, dooming the in-flight request.
func TestAbandonedPromptRequestDroppedOnReturn(t *testing.T) {
	assert := assert.New(t)
	m := splitModel() // job 2 (done) selected, viewQueue
	cmd := m.dispatchPromptFetch(2)
	require.NotNil(t, cmd)
	staleSeq := m.promptFetchSeq

	// Navigate to job 3 and back to job 2 before the response lands.
	m = m.moveSelectionToJobID(3)
	m, _ = m.followSelectionChange(2)
	m = m.moveSelectionToJobID(2)
	m, _ = m.followSelectionChange(3)

	res, _ := m.handlePromptMsg(promptMsg{
		review: splitTestReview(), jobID: 2,
		promptSeq: staleSeq, dispatchedFrom: viewQueue,
	})
	got := res.(model)
	assert.Equal(viewQueue, got.currentView,
		"an abandoned prompt request must not open the prompt view on return")
	assert.Nil(got.currentReview)
}

// TestPromptFetchRejectedAfterRerunSupersedesAttempt is the prompt
// path's copy of the attempt-staleness repro, and the reason
// fetchReviewForPrompt stamps the
// attempt counter: handlePromptMsg writes the SAME currentReview field the
// split pane renders and reconcile's idempotency check reads, so a response
// from a superseded attempt overwrites the nil handleRerunResultMsg just
// wrote and then blocks the rerun's real result from ever being fetched.
func TestPromptFetchRejectedAfterRerunSupersedesAttempt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	_, m := mockServerModel(t, mockReviewHandler(*splitTestReview(), nil))
	m.currentView = viewQueue
	m.jobs = testQueueJobs()
	m.selectedIdx, m.selectedJobID = 1, 2 // job 2: done, selected

	// 'p' on job 2 dispatches the prompt fetch, stamped with job 2's
	// attempt counter as it stands now.
	staleMsg, ok := m.dispatchPromptFetch(2)().(promptMsg)
	require.True(ok, "expected a promptMsg")
	require.Equal(uint64(0), staleMsg.attempt, "sanity: dispatched before any rerun")

	// A rerun of job 2 confirms while that fetch is in flight.
	res, _ := m.handleRerunResultMsg(rerunResultMsg{jobID: 2})
	m = res.(model)
	require.Nil(m.currentReview, "sanity: the rerun cleared the loaded review")

	// The pre-rerun prompt response lands. The selection never moved, so
	// the jobID gate alone cannot catch it.
	res2, _ := m.handlePromptMsg(staleMsg)
	got := res2.(model)

	assert.Nil(got.currentReview,
		"a prompt response from the attempt the rerun superseded must not overwrite the cleared review")
	assert.Equal(viewQueue, got.currentView,
		"and it must not switch into the prompt view for the superseded attempt")
}

// TestPromptFetchAcceptedAtCurrentAttempt is the non-regression counterpart:
// the ordinary path (no rerun in between) still loads the prompt and opens
// the view, and a post-rerun fetch is stamped with the new value and lands
// normally.
func TestPromptFetchAcceptedAtCurrentAttempt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	_, m := mockServerModel(t, mockReviewHandler(*splitTestReview(), nil))
	m.currentView = viewQueue
	m.jobs = testQueueJobs()
	m.selectedIdx, m.selectedJobID = 1, 2

	msg, ok := m.dispatchPromptFetch(2)().(promptMsg)
	require.True(ok)
	res, _ := m.handlePromptMsg(msg)
	got := res.(model)
	require.NotNil(got.currentReview, "an ordinary prompt fetch must still land")
	assert.Equal(viewKindPrompt, got.currentView)

	// After a rerun, a FRESH prompt fetch carries the new attempt value.
	res2, _ := got.handleRerunResultMsg(rerunResultMsg{jobID: 2})
	m2 := res2.(model)
	fresh, ok := m2.dispatchPromptFetch(2)().(promptMsg)
	require.True(ok)
	assert.Equal(uint64(1), fresh.attempt, "a post-rerun dispatch carries the post-rerun attempt")

	res3, _ := m2.handlePromptMsg(fresh)
	require.NotNil(res3.(model).currentReview, "the post-rerun prompt fetch must be accepted")
}

// ---------------------------------------------------------------------------
// Rerunning a panel SYNTHESIS parent is not an abandonment of
// that job's attempt. internal/daemon/server.go routes those to
// rerunPanelRun, which clones the members and the synthesis row into a
// brand-new run under a fresh panel_run_uuid with NEW job IDs and leaves the
// original run -- this job's row and its review -- intact as history. 'r'
// reaches that case: handleRerunKey blocks only PanelRoleMember.
//
// Bumping jobAttemptGen for it would falsify the counter's clause 2 ("a bump
// is always abandonment"), which clause 4 ("nothing needs disarming on the
// rejection path") is derived from -- so the exception is enforced in code,
// decided at dispatch where the job is provably in hand.
// ---------------------------------------------------------------------------

func panelSynthesisJobs() []storage.ReviewJob {
	jobs := testQueueJobs()
	jobs[1].JobType = storage.JobTypeSynthesis // job 2
	jobs[1].PanelRunUUID = testUUIDPtr("panel-run-1")
	return jobs
}

func TestPanelSynthesisRerunDoesNotSupersedeAttempt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withTestJobs(panelSynthesisJobs()...), withSelection(1, 2),
		withReview(splitTestReview()))
	m.tasksEnabled = true
	require.True(m.jobs[1].IsSynthesisJob(), "sanity: job 2 is a panel synthesis parent")

	// An 'F' request for job 2 is armed and its fetch is in flight.
	res, cmd := m.handleFixKey()
	m = res.(model)
	require.NotNil(cmd)
	require.Equal(int64(2), m.pendingReviewOpenJobID)
	inflight := reviewMsg{
		review: splitTestReview(), jobID: 2, dispatchedFrom: viewQueue,
		gen: m.detailFollowGen, fetchSeq: m.reviewFetchSeq,
		attempt: m.jobAttemptGen[2],
	}
	prevFlash := m.flashMessage

	// The rerun confirms. The daemon enqueued a whole new panel run; this
	// job's row, review and in-flight fetch are all still current.
	res2, _ := m.handleRerunResultMsg(rerunResultMsg{jobID: 2, spawnsNewRun: true})
	got := res2.(model)

	assert.Equal(uint64(0), got.jobAttemptGen[2],
		"a panel rerun spawns new job IDs, so THIS job's attempt was not superseded")
	assert.NotNil(got.currentReview,
		"the synthesis parent's review is still the current one and must not be cleared")
	assert.Equal(int64(2), got.pendingReviewOpenJobID,
		"the pending open intent must not be cancelled")
	assert.True(got.reviewFixPanelPending, "nor the pending fix panel")
	assert.NotContains(got.flashMessage, "open request canceled",
		"no misleading 'job is rerunning -- open request canceled' flash: nothing was cancelled")
	assert.Contains(got.flashMessage, "Panel rerun queued",
		"the accepted rerun is confirmed instead, since this job's own row does not visibly change")
	_ = prevFlash

	// The in-flight fetch is still correct and must land.
	res3, _ := got.handleReviewMsg(inflight)
	final := res3.(model)
	require.NotNil(final.currentReview)
	assert.Equal(int64(2), final.currentReview.JobID)
	assert.True(final.reviewFixPanelOpen, "the still-valid request is served normally")
}

// TestOrdinaryRerunStillSupersedesAttempt is the counterpart: the exception
// is scoped to panel synthesis parents and must not weaken the ordinary case.
func TestOrdinaryRerunStillSupersedesAttempt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withSelection(1, 2), withReview(splitTestReview()))

	res, _ := m.handleRerunResultMsg(rerunResultMsg{jobID: 2})
	got := res.(model)

	assert.Equal(uint64(1), got.jobAttemptGen[2], "an ordinary rerun still bumps")
	require.Nil(got.currentReview, "and still clears the superseded review")
}

// TestRerunDispatchRecordsPanelRunShape pins the half of the fix that decides
// the flag at DISPATCH -- where the job is provably in hand, unlike when the
// result lands (by then the job may have left m.jobs).
func TestRerunDispatchRecordsPanelRunShape(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	rerunOK := func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"success": true})
	}

	// Synthesis parent: 'r' must mark the rerun as spawning a new run.
	_, m := mockServerModel(t, rerunOK)
	m.currentView = viewQueue
	m.jobs = panelSynthesisJobs()
	m.selectedIdx, m.selectedJobID = 1, 2
	res, cmd := m.handleRerunKey()
	require.NotNil(cmd, "'r' is allowed on a synthesis parent -- only members are blocked")
	_ = res
	rm, ok := cmd().(rerunResultMsg)
	require.True(ok, "expected a rerunResultMsg, got %T", cmd())
	require.NoError(rm.err)
	assert.True(rm.spawnsNewRun, "a synthesis parent's rerun spawns a new panel run")

	// Ordinary job: the flag stays false.
	_, m2 := mockServerModel(t, rerunOK)
	m2.currentView = viewQueue
	m2.jobs = testQueueJobs()
	m2.selectedIdx, m2.selectedJobID = 1, 2
	_, cmd2 := m2.handleRerunKey()
	require.NotNil(cmd2)
	rm2, ok := cmd2().(rerunResultMsg)
	require.True(ok)
	require.NoError(rm2.err)
	assert.False(rm2.spawnsNewRun, "an ordinary rerun re-runs the job in place")
}

// ---------------------------------------------------------------------------
// stepReviewNav and the pagination viewReview arm
// replace currentReview for the new job and must not leave splitDetailErr pointing at
// the old one. renderDetailPane's done branch renders that error whenever
// currentReview does not yet match the selected job -- exactly the window
// both sites create while their fetch is in flight.
// ---------------------------------------------------------------------------

func TestReviewNavClearsPreviousJobsDetailError(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	// testQueueJobs: job 3 running (idx0), job 2 done (idx1), job 1 failed (idx2).
	m := splitModel(withCurrentView(viewReview), withSelection(1, 2),
		withReview(splitTestReview()))
	m.reviewFromView = viewQueue
	// A reconcile follow failed while job 2's review was displayed.
	m.splitDetailErr = errors.New("job 2 follow failure")
	prevGen := m.detailFollowGen

	// Left = older: job 2 -> job 1 (failed, also review-eligible).
	res, cmd := m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyLeft})
	got := res.(model)

	require.Equal(int64(1), got.selectedJobID, "the selection must move to the older review")
	require.NoError(got.splitDetailErr,
		"the previous job's failure must not render under the newly selected job")
	assert.Greater(got.detailFollowGen, prevGen,
		"review nav takes the shared detail-follow transition like every other selection move")
	assert.NotContains(got.renderSplit(), "job 2 follow failure")
	_ = cmd
}

func TestReviewPaginateNavClearsPreviousJobsDetailError(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(
		withCurrentView(viewReview),
		withTestJobs(storage.ReviewJob{
			ID: 5, GitRef: "eeee555", Branch: "main", RepoName: "repoA",
			Agent: "codex", Status: storage.JobStatusDone,
		}),
		withSelection(0, 5),
	)
	m.reviewFromView = viewQueue
	m.paginateNav = viewReview
	m.loadingMore = true
	m.splitDetailErr = errors.New("job 5 follow failure")
	prevGen := m.detailFollowGen

	res, cmd := m.handleJobsMsg(jobsMsg{append: true, jobs: []storage.ReviewJob{
		{
			ID: 4, GitRef: "dddd444", Branch: "main", RepoName: "repoA",
			Agent: "codex", Status: storage.JobStatusDone,
		},
	}})
	got := res.(model)

	require.NotNil(cmd)
	require.Equal(int64(4), got.selectedJobID, "auto-navigate must move to the newly loaded review")
	require.NoError(got.splitDetailErr,
		"the previous job's failure must not survive the pagination jump (this arm's early return skips reconcile, so nothing else would clear it)")
	assert.Greater(got.detailFollowGen, prevGen,
		"the pagination review arm takes the shared transition too")
}

// ---------------------------------------------------------------------------
// The same fact that makes a panel synthesis parent's ATTEMPT
// survive its "rerun" -- the daemon enqueues a separate run and leaves this
// row and its review untouched -- also means the row's own state must not be
// optimistically re-queued. Both dispatchers wrote status=queued and wiped
// the timestamps, error, closed state and verdict before the request went
// out, so the parent displayed and behaved as a queued job, with its verdict
// and closed state gone, while its review was still current.
// ---------------------------------------------------------------------------

// rerunOKHandler answers /api/job/rerun with a success body.
func rerunOKHandler(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]any{"success": true})
}

// assertSynthesisRowIntact checks every field the optimistic re-queue used to
// clobber, plus the behaviour that a fake queued status breaks.
func assertSynthesisRowIntact(t *testing.T, m model, jobID int64) {
	t.Helper()
	assert := assert.New(t)
	var job *storage.ReviewJob
	for i := range m.jobs {
		if m.jobs[i].ID == jobID {
			job = &m.jobs[i]
		}
	}
	require.NotNil(t, job, "the synthesis parent must still be in the list")
	assert.Equal(storage.JobStatusDone, job.Status,
		"the parent is not re-queued: the daemon enqueues a separate run")
	require.NotNil(t, job.Verdict, "its verdict must not be wiped")
	assert.Equal("P", *job.Verdict)
	require.NotNil(t, job.Closed, "its closed state must not be wiped")
	assert.True(*job.Closed)
	assert.NotNil(job.FinishedAt, "its completion timestamp must survive")
	assert.Empty(job.Error)
}

func synthesisParentJobs() []storage.ReviewJob {
	jobs := panelSynthesisJobs()
	finished := time.Now().Add(-time.Hour)
	closed := true
	jobs[1].FinishedAt = &finished
	jobs[1].Closed = &closed
	jobs[1].PanelSummary = &storage.PanelSummary{MembersTerminal: 3, MembersTotal: 3}
	return jobs
}

func TestRerunKeyLeavesSynthesisParentRowIntact(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	_, m := mockServerModel(t, rerunOKHandler)
	m.currentView = viewQueue
	m.jobs = synthesisParentJobs()
	m.selectedIdx, m.selectedJobID = 1, 2
	m.currentReview = splitTestReview()

	res, cmd := m.handleRerunKey()
	got := res.(model)
	require.NotNil(cmd, "'r' still dispatches the panel rerun")

	assertSynthesisRowIntact(t, got, 2)
	require.NotNil(got.currentReview, "and the parent's review is still current")

	// The behaviour a fake queued status broke: Enter must still open the
	// parent's review instead of flashing "Panel still synthesizing".
	_, handled := got.panelInProgressFlash(got.jobs[1])
	assert.False(handled,
		"a completed panel parent must not be treated as still synthesizing after its rerun is dispatched")

	// The confirmation still reports the panel shape, so handleRerunResultMsg
	// takes the spawnsNewRun branch.
	rm, ok := cmd().(rerunResultMsg)
	require.True(ok)
	require.NoError(rm.err)
	assert.True(rm.spawnsNewRun)

	res2, _ := got.handleRerunResultMsg(rm)
	final := res2.(model)
	assertSynthesisRowIntact(t, final, 2)
	assert.Contains(final.flashMessage, "Panel rerun queued",
		"the accepted rerun is confirmed by a flash, since the row itself does not change")
}

func TestCtrlRerunLeavesSynthesisParentRowIntact(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	_, m := mockServerModel(t, rerunOKHandler)
	m.currentView = viewQueue
	m.jobs = synthesisParentJobs()
	m.selectedIdx, m.selectedJobID = 1, 2

	got, resp, cmd := m.handleCtrlRerunJob(json.RawMessage(`{"job_id":2}`))
	require.Empty(resp.Error, "the control socket still accepts the rerun")
	require.NotNil(cmd)

	assertSynthesisRowIntact(t, got, 2)

	rm, ok := cmd().(rerunResultMsg)
	require.True(ok)
	require.NoError(rm.err)
	assert.True(rm.spawnsNewRun, "the control path records the panel shape too")
}

// TestRerunKeyStillShowsOptimisticQueueForOrdinaryJobs is the counterpart:
// for a job the daemon really does re-run in place, the optimistic re-queue
// is correct and must be unchanged.
func TestRerunKeyStillShowsOptimisticQueueForOrdinaryJobs(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	_, m := mockServerModel(t, rerunOKHandler)
	m.currentView = viewQueue
	jobs := testQueueJobs()
	finished := time.Now().Add(-time.Hour)
	closed := true
	jobs[1].FinishedAt = &finished
	jobs[1].Closed = &closed
	m.jobs = jobs
	m.selectedIdx, m.selectedJobID = 1, 2

	res, cmd := m.handleRerunKey()
	got := res.(model)
	require.NotNil(cmd)

	assert.Equal(storage.JobStatusQueued, got.jobs[1].Status,
		"an ordinary rerun still shows the optimistic queued state immediately")
	assert.Nil(got.jobs[1].Verdict, "and still clears the stale verdict")
	assert.Nil(got.jobs[1].Closed)
	assert.Nil(got.jobs[1].FinishedAt)
}

func registerRerunPickerAgent(t *testing.T, name string) {
	t.Helper()
	agent.Register(&agent.FakeAgent{NameStr: name})
	t.Cleanup(func() { agent.Unregister(name) })
}

type rerunPickerSchemaAgent struct {
	*agent.FakeAgent
}

func (a *rerunPickerSchemaAgent) ClassifyWithSchema(
	context.Context, string, string, string, json.RawMessage, io.Writer,
) (json.RawMessage, error) {
	return nil, nil
}

func registerRerunPickerSchemaAgent(t *testing.T, name string) {
	t.Helper()
	agent.Register(&rerunPickerSchemaAgent{
		FakeAgent: &agent.FakeAgent{NameStr: name},
	})
	t.Cleanup(func() { agent.Unregister(name) })
}

func rerunPickerModel(t *testing.T, handler http.HandlerFunc) model {
	t.Helper()
	_, m := mockServerModel(t, handler)
	m.currentView = viewQueue
	m.globalCfg = config.DefaultConfig()
	m.jobs = []storage.ReviewJob{{
		ID: 42, Agent: "picker-current", Status: storage.JobStatusDone,
		RepoPath: t.TempDir(),
	}}
	m.selectedIdx, m.selectedJobID = 0, 42
	return m
}

func TestRerunAgentPickerTransitionsAndOptions(t *testing.T) {
	registerRerunPickerAgent(t, "picker-current")
	registerRerunPickerAgent(t, "picker-alpha")
	registerRerunPickerAgent(t, "picker-zeta")
	m := rerunPickerModel(t, rerunOKHandler)
	m.globalCfg.ACP = config.ACPAgentConfigs{
		"picker-unavailable": {Command: "roborev-command-that-does-not-exist"},
	}

	res, cmd := m.handleKeyMsg(keyPressMsg('R'))
	got := res.(model)
	require.Nil(t, cmd)
	require.Equal(t, viewRerunAgent, got.currentView)
	assert.Equal(t, int64(42), got.rerunAgentJobID)
	assert.Contains(t, got.rerunAgentOptions, "picker-alpha")
	assert.Contains(t, got.rerunAgentOptions, "picker-zeta")
	assert.NotContains(t, got.rerunAgentOptions, "picker-current")
	assert.NotContains(t, got.rerunAgentOptions, "test")
	assert.NotContains(t, got.rerunAgentOptions, "acp.picker-unavailable")

	start := got.rerunAgentSelected
	res, _ = got.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyDown})
	got = res.(model)
	assert.Equal(t, start+1, got.rerunAgentSelected)
	res, _ = got.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
	got = res.(model)
	assert.Equal(t, viewQueue, got.currentView)
	assert.Empty(t, got.rerunAgentOptions)
}

func TestRerunAgentPickerFiltersNonSchemaClassifierAgents(t *testing.T) {
	registerRerunPickerAgent(t, "picker-current")
	registerRerunPickerAgent(t, "picker-non-schema")
	registerRerunPickerSchemaAgent(t, "picker-schema")
	m := rerunPickerModel(t, rerunOKHandler)
	m.jobs[0].JobType = storage.JobTypeClassify
	m.jobs[0].ReviewType = "design"

	res, cmd := m.handleRerunAgentKey()
	got := res.(model)
	assert.Nil(t, cmd)
	assert.Equal(t, viewRerunAgent, got.currentView)
	assert.Contains(t, got.rerunAgentOptions, "picker-schema")
	assert.NotContains(t, got.rerunAgentOptions, "picker-non-schema")
}

func TestRerunAgentPickerEligibility(t *testing.T) {
	registerRerunPickerAgent(t, "picker-alternate")
	for _, tt := range []struct {
		name     string
		job      storage.ReviewJob
		wantView viewKind
	}{
		{name: "done", job: storage.ReviewJob{Status: storage.JobStatusDone}, wantView: viewRerunAgent},
		{name: "failed", job: storage.ReviewJob{Status: storage.JobStatusFailed}, wantView: viewRerunAgent},
		{name: "skipped", job: storage.ReviewJob{Status: storage.JobStatusSkipped}, wantView: viewRerunAgent},
		{name: "canceled", job: storage.ReviewJob{Status: storage.JobStatusCanceled}, wantView: viewRerunAgent},
		{name: "member", job: storage.ReviewJob{Status: storage.JobStatusDone, PanelRole: storage.PanelRoleMember}},
		{name: "synthesis", job: storage.ReviewJob{Status: storage.JobStatusDone, PanelRole: storage.PanelRoleSynthesis}},
		{name: "running", job: storage.ReviewJob{Status: storage.JobStatusRunning}},
		{name: "stopping", job: storage.ReviewJob{Status: storage.JobStatusCanceled, WorkerID: "worker"}},
		{name: "experiment", job: storage.ReviewJob{Status: storage.JobStatusDone, Experiments: []storage.ExperimentAssignment{{ID: "experiment"}}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := rerunPickerModel(t, rerunOKHandler)
			tt.job.ID, tt.job.RepoPath = m.jobs[0].ID, m.jobs[0].RepoPath
			m.jobs[0] = tt.job
			res, cmd := m.handleRerunAgentKey()
			assert.Nil(t, cmd)
			assert.Equal(t, tt.wantView, res.(model).currentView)
		})
	}
}

func TestRerunAgentPickerRechecksJobBeforeEnter(t *testing.T) {
	registerRerunPickerAgent(t, "picker-current")
	registerRerunPickerAgent(t, "picker-recheck")
	m := rerunPickerModel(t, rerunOKHandler)
	res, _ := m.handleRerunAgentKey()
	m = res.(model)
	require.Equal(t, viewRerunAgent, m.currentView)
	m.jobs[0].Status = storage.JobStatusRunning

	res, cmd := m.handleRerunAgentPickerKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := res.(model)
	assert.Nil(t, cmd)
	assert.Equal(t, viewQueue, got.currentView)
	assert.Contains(t, got.flashMessage, "no longer eligible")
	assert.Equal(t, "picker-current", got.jobs[0].Agent)
}

func TestRerunAgentPickerSubmission(t *testing.T) {
	registerRerunPickerAgent(t, "picker-current")
	registerRerunPickerAgent(t, "picker-selected")
	for _, tt := range []struct {
		status     int
		wantAgent  string
		wantStatus storage.JobStatus
	}{
		{http.StatusOK, "picker-selected", storage.JobStatusQueued},
		{http.StatusBadRequest, "picker-current", storage.JobStatusDone},
	} {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			var request struct {
				JobID int64  `json:"job_id"`
				Agent string `json:"agent"`
			}
			m := rerunPickerModel(t, func(w http.ResponseWriter, r *http.Request) {
				assert.NoError(t, json.NewDecoder(r.Body).Decode(&request))
				if tt.status == http.StatusBadRequest {
					http.Error(w, "agent unavailable", tt.status)
					return
				}
				rerunOKHandler(w, r)
			})
			res, _ := m.handleRerunAgentKey()
			m = res.(model)
			m.rerunAgentSelected = slices.Index(m.rerunAgentOptions, "picker-selected")
			require.GreaterOrEqual(t, m.rerunAgentSelected, 0)
			res, cmd := m.handleRerunAgentPickerKey(tea.KeyPressMsg{Code: tea.KeyEnter})
			m = res.(model)
			require.NotNil(t, cmd)
			assert.Equal(t, viewQueue, m.currentView)
			assert.Equal(t, storage.JobStatusQueued, m.jobs[0].Status)
			assert.Equal(t, "picker-selected", m.jobs[0].Agent)

			res, _ = m.Update(cmd())
			m = res.(model)
			assert.Equal(t, int64(42), request.JobID)
			assert.Equal(t, "picker-selected", request.Agent)
			assert.Equal(t, tt.wantAgent, m.jobs[0].Agent)
			assert.Equal(t, tt.wantStatus, m.jobs[0].Status)
		})
	}
}

func TestDefaultAndControlRerunsOmitAgent(t *testing.T) {
	var requests []map[string]any
	handler := func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		requests = append(requests, body)
		rerunOKHandler(w, r)
	}

	m := rerunPickerModel(t, handler)
	_, cmd := m.handleRerunKey()
	require.NotNil(t, cmd)
	require.IsType(t, rerunResultMsg{}, cmd())

	m = rerunPickerModel(t, handler)
	_, response, cmd := m.handleCtrlRerunJob(json.RawMessage(`{"job_id":42}`))
	require.True(t, response.OK)
	require.NotNil(t, cmd)
	require.IsType(t, rerunResultMsg{}, cmd())

	require.Len(t, requests, 2)
	for _, request := range requests {
		assert.NotContains(t, request, "agent")
	}
}

// ---------------------------------------------------------------------------
// Two consequences of not mutating a synthesis parent's row.
//
// (1) The row stays terminal while the rerun is in flight, so the status
//     check that suppresses a second 'r' for an ordinary job no longer
//     suppresses anything here -- and the daemon accepts both requests,
//     because the status IT checks is this same unchanged row.
// (2) The error path restored the pre-rerun snapshot unconditionally. For a
//     job that was never mutated there is nothing to undo, and doing it
//     anyway overwrites whatever DID change the row while the request was
//     in flight.
// ---------------------------------------------------------------------------

func TestPanelRerunSuppressesDuplicateDispatch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	_, m := mockServerModel(t, rerunOKHandler)
	m.currentView = viewQueue
	m.jobs = synthesisParentJobs()
	m.selectedIdx, m.selectedJobID = 1, 2

	res, cmd := m.handleRerunKey()
	m = res.(model)
	require.NotNil(cmd, "the first 'r' dispatches")
	require.True(m.panelRerunInFlight[2], "the dispatch must be recorded as in flight")

	// The row is still terminal (deliberately unmutated), so only the new
	// set can stop the second press.
	require.Equal(storage.JobStatusDone, m.jobs[1].Status)
	res2, cmd2 := m.handleRerunKey()
	m2 := res2.(model)
	assert.Nil(cmd2, "a second 'r' must not spawn a second panel run")
	assert.Contains(m2.flashMessage, "Panel rerun already in progress",
		"and must say so rather than doing nothing silently")

	// The result releases the slot, so a deliberate later rerun still works.
	rm, ok := cmd().(rerunResultMsg)
	require.True(ok)
	res3, _ := m2.handleRerunResultMsg(rm)
	m3 := res3.(model)
	assert.False(m3.panelRerunInFlight[2], "the result must release the slot")
	_, cmd4 := m3.handleRerunKey()
	assert.NotNil(cmd4, "a rerun requested after the previous one resolved is allowed")
}

// TestPanelRerunSlotReleasedOnFailure is the no-leak half: a failed request
// must not leave the job blocked for the rest of the session.
func TestPanelRerunSlotReleasedOnFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	_, m := mockServerModel(t, rerunOKHandler)
	m.currentView = viewQueue
	m.jobs = synthesisParentJobs()
	m.selectedIdx, m.selectedJobID = 1, 2

	res, _ := m.handleRerunKey()
	m = res.(model)
	require.True(m.panelRerunInFlight[2])

	res2, _ := m.handleRerunResultMsg(rerunResultMsg{
		jobID: 2, spawnsNewRun: true, err: errors.New("daemon unreachable"),
	})
	m2 := res2.(model)
	assert.False(m2.panelRerunInFlight[2], "a FAILED rerun must release the slot too")
	require.Error(m2.err, "and still surface the failure")

	_, cmd := m2.handleRerunKey()
	assert.NotNil(cmd, "the job must not be permanently blocked by its failed rerun")
}

func TestCtrlPanelRerunSuppressesDuplicateDispatch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	_, m := mockServerModel(t, rerunOKHandler)
	m.currentView = viewQueue
	m.jobs = synthesisParentJobs()
	m.selectedIdx, m.selectedJobID = 1, 2

	got, resp, cmd := m.handleCtrlRerunJob(json.RawMessage(`{"job_id":2}`))
	require.Empty(resp.Error)
	require.NotNil(cmd)
	require.True(got.panelRerunInFlight[2])

	_, resp2, cmd2 := got.handleCtrlRerunJob(json.RawMessage(`{"job_id":2}`))
	assert.Nil(cmd2, "a second control-socket rerun must not spawn a second panel run")
	assert.Contains(resp2.Error, "already in flight",
		"and must report an explicit error so a scripted caller can tell")
}

// TestOrdinaryRerunUnaffectedBySuppression: an ordinary job's own optimistic
// re-queue is what suppresses its second press, exactly as before, and it
// never enters the panel set.
func TestOrdinaryRerunUnaffectedBySuppression(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	_, m := mockServerModel(t, rerunOKHandler)
	m.currentView = viewQueue
	m.jobs = testQueueJobs()
	m.selectedIdx, m.selectedJobID = 1, 2

	res, cmd := m.handleRerunKey()
	m = res.(model)
	require.NotNil(cmd)
	assert.Empty(m.panelRerunInFlight, "an ordinary rerun uses no suppression slot")
	assert.Equal(storage.JobStatusQueued, m.jobs[1].Status)

	_, cmd2 := m.handleRerunKey()
	assert.Nil(cmd2, "the optimistic queued status still suppresses the second press")
}

// TestFailedPanelRerunKeepsInterleavedRowState is FINDING 2's repro: the row
// changed while the request was in flight, and the failure must surface
// without dragging the row back to its pre-rerun snapshot.
func TestFailedPanelRerunKeepsInterleavedRowState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	_, m := mockServerModel(t, rerunOKHandler)
	m.currentView = viewQueue
	m.jobs = synthesisParentJobs()
	m.selectedIdx, m.selectedJobID = 1, 2
	oldClosed := false
	oldVerdict := "P"
	m.jobs[1].Closed = &oldClosed
	m.jobs[1].Verdict = &oldVerdict

	res, _ := m.handleRerunKey()
	m = res.(model)

	// While the request is in flight, a jobs refresh lands fresh server
	// state for the same row: now closed, with a different verdict.
	newClosed := true
	newVerdict := "F"
	refreshed := synthesisParentJobs()
	refreshed[1].Closed = &newClosed
	refreshed[1].Verdict = &newVerdict
	res2, _ := m.handleJobsMsg(jobsMsg{jobs: refreshed, stats: storage.JobStats{}})
	m = res2.(model)
	require.Equal("F", *m.jobs[1].Verdict, "sanity: the refresh landed")

	// The rerun request then fails, carrying the PRE-rerun snapshot.
	res3, _ := m.handleRerunResultMsg(rerunResultMsg{
		jobID: 2, spawnsNewRun: true, err: errors.New("daemon unreachable"),
		oldState: storage.JobStatusDone, oldClosed: &oldClosed, oldVerdict: &oldVerdict,
	})
	got := res3.(model)

	require.NotNil(got.jobs[1].Verdict)
	assert.Equal("F", *got.jobs[1].Verdict,
		"a failure for a job that was never mutated must not overwrite state that changed since")
	require.NotNil(got.jobs[1].Closed)
	assert.True(*got.jobs[1].Closed,
		"the interleaved closed state must survive the failed rerun")
	assert.Error(got.err, "the failure itself is still surfaced")
}

// TestStackedNavigateAwayAndBackAbandonsPendingOpen covers the stacked half
// of the abandonment rule: followSelectionChange used to early-return
// outside split layout, so navigating X -> Y -> X in stacked mode left the
// pending-open intent armed and the original dispatch gen-fresh -- when the
// abandoned response finally landed, it was accepted and opened X's review
// with no fresh keypress. The rule is layout-independent: the selection
// leaving a job abandons its pending requests, whoever moves it.
func TestStackedNavigateAwayAndBackAbandonsPendingOpen(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := initTestModel(withCurrentView(viewQueue), withDimensions(150, 40),
		withTestJobs(testQueueJobs()...), withSelection(1, 2))
	m.layout = layoutStacked

	// Enter on job 2 (done): dispatches the ordinary fetch, arms the
	// pending-open intent.
	res, cmd := m.handleEnterKey()
	got := res.(model)
	require.NotNil(cmd)
	require.Equal(int64(2), got.pendingReviewOpenJobID)
	seqA := got.reviewFetchSeq
	genA := got.detailFollowGen

	// Away to job 3 and back to job 2, both before the response lands.
	away, _ := pressSpecial(got, tea.KeyUp)
	require.Equal(int64(3), away.selectedJobID, "sanity: selection moved away")
	assert.Equal(int64(0), away.pendingReviewOpenJobID,
		"navigating away abandons the pending open, in stacked too")
	back, _ := pressSpecial(away, tea.KeyDown)
	require.Equal(int64(2), back.selectedJobID, "sanity: selection returned")

	// The abandoned response lands: jobID matches again, but the two
	// selection changes each bumped the gen, and with the intent disarmed
	// the response is un-rescuable -- it must not open the review.
	res2, _ := back.handleReviewMsg(reviewMsg{
		review: splitTestReview(), jobID: 2,
		fetchSeq: seqA, gen: genA, dispatchedFrom: viewQueue,
	})
	final := res2.(model)
	assert.Equal(viewQueue, final.currentView,
		"an abandoned request's response must not open the review after navigating back")
}

// TestStackedNavigateAwayAndBackAbandonsPendingFixPanel is the fix-panel
// twin: F on X, navigate away and back, X's response arrives -- the panel
// must not spring open for a request the user walked away from.
func TestStackedNavigateAwayAndBackAbandonsPendingFixPanel(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := initTestModel(withCurrentView(viewQueue), withDimensions(150, 40),
		withTestJobs(testQueueJobs()...), withSelection(1, 2))
	m.layout = layoutStacked
	m.tasksEnabled = true

	res, cmd := m.handleFixKey()
	got := res.(model)
	require.NotNil(cmd)
	require.True(got.reviewFixPanelPending)
	require.Equal(int64(2), got.fixPromptJobID)
	seqA := got.reviewFetchSeq
	genA := got.detailFollowGen

	away, _ := pressSpecial(got, tea.KeyUp)
	require.Equal(int64(3), away.selectedJobID, "sanity: selection moved away")
	assert.False(away.reviewFixPanelPending,
		"navigating away abandons the pending fix panel, in stacked too")
	assert.Equal(int64(0), away.fixPromptJobID)
	back, _ := pressSpecial(away, tea.KeyDown)
	require.Equal(int64(2), back.selectedJobID)

	res2, _ := back.handleReviewMsg(reviewMsg{
		review: splitTestReview(), jobID: 2,
		fetchSeq: seqA, gen: genA, dispatchedFrom: viewQueue,
	})
	final := res2.(model)
	assert.False(final.reviewFixPanelOpen,
		"an abandoned F's response must not spring the fix panel open after navigating back")
	assert.Equal(viewQueue, final.currentView)
}

// TestSplitEscNormalizesHiddenSelectionAndRefills: the split-specific esc
// shortcut back to the list used to bypass normalizeSelectionIfHidden, so
// after closing a review with hideClosed enabled the selection stayed on
// the now-invisible job -- later actions targeted a job with no highlighted
// row. It must normalize exactly as the stacked return path does, follow
// the resulting selection change, and refill the hideClosed-pruned queue.
func TestSplitEscNormalizesHiddenSelectionAndRefills(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	jobs := testQueueJobs()
	closed := true
	jobs[1].Closed = &closed // job 2: done and closed -> hidden under hideClosed
	m := splitModel(withTestJobs(jobs...), withSelection(1, 2), withReview(splitTestReview()))
	m.currentView = viewReview
	m.focus = focusDetail
	m.hideClosed = true
	prevGen := m.detailFollowGen

	got, cmd := pressSpecial(m, tea.KeyEsc)
	assert.Equal(viewQueue, got.currentView)
	assert.Equal(focusList, got.focus)
	assert.NotEqual(int64(2), got.selectedJobID,
		"the hidden job must not stay selected after returning to the list")
	assert.Equal(prevGen+1, got.detailFollowGen,
		"the normalized selection change must be followed exactly once: "+
			"handleKeyMsg's wrapper owns the transition, the esc branch "+
			"must not double-bump")
	assert.True(got.loadingJobs, "the hideClosed refill must be dispatched")
	require.NotNil(cmd)
}

// TestSplitQuitNormalizesHiddenSelectionAndRefills is the q-key twin of the
// esc test above -- both split shortcuts share the same body and had the
// same gap.
func TestSplitQuitNormalizesHiddenSelectionAndRefills(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	jobs := testQueueJobs()
	closed := true
	jobs[1].Closed = &closed
	m := splitModel(withTestJobs(jobs...), withSelection(1, 2), withReview(splitTestReview()))
	m.currentView = viewReview
	m.focus = focusDetail
	m.hideClosed = true
	prevGen := m.detailFollowGen

	got, cmd := pressKey(m, 'q')
	assert.Equal(viewQueue, got.currentView)
	assert.Equal(focusList, got.focus)
	assert.NotEqual(int64(2), got.selectedJobID,
		"the hidden job must not stay selected after returning to the list")
	assert.Equal(prevGen+1, got.detailFollowGen,
		"the normalized selection change must be followed exactly once: "+
			"handleKeyMsg's wrapper owns the transition, the q branch "+
			"must not double-bump")
	assert.True(got.loadingJobs, "the hideClosed refill must be dispatched")
	require.NotNil(cmd)
}

// TestStackedMouseWheelAwayAbandonsPendingOpen is the mouse twin of
// TestStackedNavigateAwayAndBackAbandonsPendingOpen: mouse events never pass
// through handleKeyMsg's viewQueue wrapper, so the stacked wheel paths used
// to move the selection while skipping the abandonment chokepoint -- the
// pending-open intent stayed armed for a job the cursor already left.
func TestStackedMouseWheelAwayAbandonsPendingOpen(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := initTestModel(withCurrentView(viewQueue), withDimensions(150, 40),
		withTestJobs(testQueueJobs()...), withSelection(1, 2))
	m.layout = layoutStacked

	res, cmd := m.handleEnterKey()
	got := res.(model)
	require.NotNil(cmd)
	require.Equal(int64(2), got.pendingReviewOpenJobID)
	seqA := got.reviewFetchSeq
	genA := got.detailFollowGen

	away, _ := updateModel(t, got, mouseWheelAt(0, 0, tea.MouseWheelUp))
	require.Equal(int64(3), away.selectedJobID, "sanity: wheel moved the selection away")
	assert.Equal(int64(0), away.pendingReviewOpenJobID,
		"wheel navigation away abandons the pending open, like the arrow keys")
	back, _ := updateModel(t, away, mouseWheelAt(0, 0, tea.MouseWheelDown))
	require.Equal(int64(2), back.selectedJobID, "sanity: selection returned")

	res2, _ := back.handleReviewMsg(reviewMsg{
		review: splitTestReview(), jobID: 2,
		fetchSeq: seqA, gen: genA, dispatchedFrom: viewQueue,
	})
	final := res2.(model)
	assert.Equal(viewQueue, final.currentView,
		"an abandoned request's response must not open the review after wheeling back")
}

// TestStackedMouseClickAwayAbandonsPendingFixPanel is the click/fix-panel
// twin: F on X, click Y's row, click back to X -- clicks bypassed the
// chokepoint the same way the wheel did, so X's late response sprang the
// fix panel open for a request the user walked away from.
func TestStackedMouseClickAwayAbandonsPendingFixPanel(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := initTestModel(withCurrentView(viewQueue), withDimensions(150, 40),
		withTestJobs(testQueueJobs()...), withSelection(1, 2))
	m.layout = layoutStacked
	m.tasksEnabled = true

	res, cmd := m.handleFixKey()
	got := res.(model)
	require.NotNil(cmd)
	require.True(got.reviewFixPanelPending)
	require.Equal(int64(2), got.fixPromptJobID)
	seqA := got.reviewFetchSeq
	genA := got.detailFollowGen

	// Row y math: 5 chrome rows (title, status, update, header, separator),
	// then data rows -- job 3 at y=5 (idx 0), job 2 at y=6 (idx 1).
	away, _ := updateModel(t, got, mouseClickAt(4, 5))
	require.Equal(int64(3), away.selectedJobID, "sanity: click moved the selection away")
	assert.False(away.reviewFixPanelPending,
		"click navigation away abandons the pending fix panel, like the arrow keys")
	assert.Equal(int64(0), away.fixPromptJobID)
	back, _ := updateModel(t, away, mouseClickAt(4, 6))
	require.Equal(int64(2), back.selectedJobID, "sanity: selection returned")

	res2, _ := back.handleReviewMsg(reviewMsg{
		review: splitTestReview(), jobID: 2,
		fetchSeq: seqA, gen: genA, dispatchedFrom: viewQueue,
	})
	final := res2.(model)
	assert.False(final.reviewFixPanelOpen,
		"an abandoned F's response must not spring the fix panel open after clicking back")
	assert.Equal(viewQueue, final.currentView)
}

// TestStackedNormalizationDisarmsPendingFixPanel is the stacked twin of
// the normalization repro for the FIX intent: handleJobsMsg's refresh
// normalization closed a job-mismatched fix panel only when splitActive(),
// so in stacked a pending F for job X survived the selection moving off X
// and sprang the panel open when a later refresh reselected X and the old
// response finally landed.
func TestStackedNormalizationDisarmsPendingFixPanel(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := initTestModel(withCurrentView(viewQueue), withDimensions(150, 40),
		withTestJobs(testQueueJobs()...), withSelection(1, 2))
	m.layout = layoutStacked
	m.tasksEnabled = true

	res, cmd := m.handleFixKey()
	m = res.(model)
	require.NotNil(cmd)
	require.True(m.reviewFixPanelPending)
	require.Equal(int64(2), m.fixPromptJobID)
	seqA := m.reviewFetchSeq
	genA := m.detailFollowGen

	// A refresh drops job 2: normalization moves the selection off it, and
	// nothing is in flight for job 2 that could clear the intent reactively.
	res2, _ := m.handleJobsMsg(jobsMsg{
		jobs:  []storage.ReviewJob{testQueueJobs()[0]}, // job 3 only
		stats: storage.JobStats{},
	})
	m = res2.(model)
	require.NotEqual(int64(2), m.selectedJobID,
		"sanity: normalization moved the selection off job 2")
	assert.False(m.reviewFixPanelPending,
		"normalization moving the selection off the intent's job abandons the pending fix panel, in stacked too")
	assert.Equal(int64(0), m.fixPromptJobID)

	// A later refresh brings job 2 back as the only row and reselects it.
	res3, _ := m.handleJobsMsg(jobsMsg{
		jobs:  []storage.ReviewJob{testQueueJobs()[1]}, // job 2 only
		stats: storage.JobStats{},
	})
	m = res3.(model)
	require.Equal(int64(2), m.selectedJobID,
		"sanity: the selection came back to job 2")

	// The armed-era response finally lands: nothing may spring open.
	res4, _ := m.handleReviewMsg(reviewMsg{
		review: splitTestReview(), jobID: 2,
		fetchSeq: seqA, gen: genA, dispatchedFrom: viewQueue,
	})
	got := res4.(model)
	assert.False(got.reviewFixPanelOpen,
		"an abandoned F's response must not spring the fix panel open after a refresh reselects its job")
	assert.Equal(viewQueue, got.currentView)
}

// TestStackedOpenFixPanelSurvivesRefreshNormalization guards what the
// stacked disarm above must not erode: with the review displayed
// full-screen, isReviewAnchored keeps the selection pinned to the displayed
// job even when a refresh drops it from the list, so normalization is a
// same-selection no-op -- the open panel stays, nothing is disarmed, and
// the abandonment gen bump must not fire (it is gated on the selection
// actually moving).
func TestStackedOpenFixPanelSurvivesRefreshNormalization(t *testing.T) {
	assert := assert.New(t)
	m := initTestModel(withCurrentView(viewReview), withDimensions(150, 40),
		withTestJobs(testQueueJobs()...), withSelection(1, 2),
		withReview(splitTestReview()))
	m.layout = layoutStacked
	m.tasksEnabled = true
	m.reviewFixPanelOpen = true
	m.fixPromptJobID = 2
	prevGen := m.detailFollowGen

	res, _ := m.handleJobsMsg(jobsMsg{
		jobs:  []storage.ReviewJob{testQueueJobs()[0]}, // job 3 only
		stats: storage.JobStats{},
	})
	got := res.(model)
	assert.Equal(int64(2), got.selectedJobID,
		"review-anchored: the selection stays pinned to the displayed job")
	assert.True(got.reviewFixPanelOpen,
		"an open panel outside split is review-bound; normalization must not close it")
	assert.Equal(int64(2), got.fixPromptJobID)
	assert.Equal(prevGen, got.detailFollowGen,
		"a same-selection refresh is not an abandonment; the gen must not bump")
}

// TestFilterResetDoomsArmedEraDispatch: resetQueueForFilterChange
// disarmed the deselected job's intents but never bumped
// detailFollowGen, so
// an armed-era ORDINARY dispatch stayed gen-fresh across the reset -- when
// the refetch re-selected the same job, the stale response passed every
// gate and opened the review off a filter change.
func TestFilterResetDoomsArmedEraDispatch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := initTestModel(withCurrentView(viewQueue), withDimensions(150, 40),
		withTestJobs(testQueueJobs()...), withSelection(1, 2))
	m.layout = layoutStacked

	res, cmd := m.handleEnterKey()
	m = res.(model)
	require.NotNil(cmd)
	seqA := m.reviewFetchSeq
	genA := m.detailFollowGen

	m.resetQueueForFilterChange()
	assert.Greater(m.detailFollowGen, genA,
		"zeroing the selection is an abandonment; the armed-era dispatch must be doomed at the gen gate")

	// The refetch lands with job 2 as the only row (stamped with the
	// reset's own fetch seq), so normalization re-selects the very job
	// the doomed dispatch was for.
	res2, _ := m.handleJobsMsg(jobsMsg{
		jobs:  []storage.ReviewJob{testQueueJobs()[1]}, // job 2 only
		seq:   m.fetchSeq,
		stats: storage.JobStats{},
	})
	m = res2.(model)
	require.Equal(int64(2), m.selectedJobID, "sanity: the refetch re-selected job 2")

	res3, _ := m.handleReviewMsg(reviewMsg{
		review: splitTestReview(), jobID: 2,
		fetchSeq: seqA, gen: genA, dispatchedFrom: viewQueue,
	})
	got := res3.(model)
	assert.Equal(viewQueue, got.currentView,
		"a pre-reset response must not open the review after the filter refetch re-selects its job")
}

// TestSplitBootstrapClosesJobMismatchedPanel: an open panel legitimately
// survives the selection moving under it in stacked (review-bound), but
// split re-binds the panel to the selection-following pane -- so engaging
// split with a panel bound to a job other than the selection must close it,
// or a focused submit would target a job the pane does not show. A panel
// on the selected job survives the engage untouched (the same-selection
// no-disarm rule).
func TestSplitBootstrapClosesJobMismatchedPanel(t *testing.T) {
	assert := assert.New(t)
	m := initTestModel(withCurrentView(viewQueue), withDimensions(150, 40),
		withTestJobs(testQueueJobs()...), withSelection(0, 3),
		withReview(splitTestReview())) // review + panel bound to job 2, selection on 3
	m.reviewFixPanelOpen = true
	m.reviewFixPanelFocused = true
	m.fixPromptJobID = 2

	m.layout = layoutSplit
	got, _ := m.maybeBootstrapDetail()
	assert.False(got.reviewFixPanelOpen,
		"engaging split with a panel bound to another job must close it")
	assert.Zero(got.fixPromptJobID)

	// Same-job case: the panel tracks the selection, so it survives.
	m2 := initTestModel(withCurrentView(viewQueue), withDimensions(150, 40),
		withTestJobs(testQueueJobs()...), withSelection(1, 2),
		withReview(splitTestReview()))
	m2.reviewFixPanelOpen = true
	m2.fixPromptJobID = 2
	m2.layout = layoutSplit
	got2, _ := m2.maybeBootstrapDetail()
	assert.True(got2.reviewFixPanelOpen,
		"a panel bound to the selected job survives the engage")
	assert.Equal(int64(2), got2.fixPromptJobID)
}

// TestResizeDuringTransientViewResumesPaneLogOnReturn: a resize that lands
// while a transient view covers the split panes invalidates the live-log
// tail (restarting it would poll invisibly at the wrong width). Returning
// to the split view must resume the tail immediately -- previously nothing
// did until the next jobs refresh, freezing the log for up to ~15s.
func TestResizeDuringTransientViewResumesPaneLogOnReturn(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := splitModel(withSelection(0, 3)) // job 3: running
	res, cmd := m.startPaneLog(m.jobs[0])
	m = res.(model)
	require.NotNil(cmd)
	require.True(m.paneLogStreaming)

	// A transient view covers the panes; a resize lands while it is open.
	m.currentView = viewHelp
	m2, _ := updateModel(t, m, tea.WindowSizeMsg{Width: 150, Height: 40})
	require.False(m2.paneLogStreaming, "sanity: the resize invalidates the covered tail")
	require.True(m2.paneLogPaused)

	// Esc back to the split view: the tail must resume now, not on the
	// next jobs refresh.
	got, cmd2 := pressSpecial(m2, tea.KeyEsc)
	require.Equal(viewQueue, got.currentView, "sanity: back on the split view")
	assert.True(got.paneLogStreaming, "the paused tail must resume on return")
	assert.False(got.paneLogPaused)
	assert.NotNil(cmd2, "the resumed tail's first fetch must be dispatched")
}
