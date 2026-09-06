package tui

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

// --- Unit tests for control types ---

func TestViewKindString(t *testing.T) {
	tests := []struct {
		v    viewKind
		want string
	}{
		{viewQueue, "queue"},
		{viewReview, "review"},
		{viewTasks, "tasks"},
		{viewLog, "log"},
		{viewFilter, "filter"},
		{viewPatch, "patch"},
		{viewRerunAgent, "rerun-agent"},
		{viewKind(999), "unknown"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, tt.v.String(),
			"viewKind(%d).String()", tt.v)
	}
}

func TestParseViewKind(t *testing.T) {
	tests := []struct {
		s    string
		want viewKind
	}{
		{"queue", viewQueue},
		{"tasks", viewTasks},
		{"invalid", viewKind(-1)},
		{"review", viewKind(-1)}, // only queue and tasks are settable
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, parseViewKind(tt.s),
			"parseViewKind(%q)", tt.s)
	}
}

func TestIsControlCommand(t *testing.T) {
	tests := []struct {
		cmd        string
		wantQuery  bool
		wantMutate bool
	}{
		{"get-state", true, false},
		{"get-filter", true, false},
		{"set-filter", false, true},
		{"cancel-job", false, true},
		{"quit", false, true},
		{"bogus", false, false},
	}
	for _, tt := range tests {
		isQ, isM := isControlCommand(tt.cmd)
		assert.Equal(t, tt.wantQuery, isQ,
			"isControlCommand(%q) query", tt.cmd)
		assert.Equal(t, tt.wantMutate, isM,
			"isControlCommand(%q) mutate", tt.cmd)
	}
}

// --- Unit tests for query handlers ---

func TestBuildStateResponse(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	m.jobs = []storage.ReviewJob{
		makeJob(1, withRepoPath("/a")),
		makeJob(2, withRepoPath("/b")),
	}
	m.selectedJobID = 1
	m.hideClosed = true

	resp := m.buildStateResponse()
	require.True(t, resp.OK, "expected OK, got error: %s", resp.Error)

	data, ok := resp.Data.(stateSnapshot)
	require.True(t, ok, "expected stateSnapshot, got %T", resp.Data)
	assert.Equal(t, "queue", data.View)
	assert.Equal(t, 2, data.JobCount)
	assert.True(t, data.HideClosed)
}

func TestBuildFilterResponse(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	m.activeRepoFilter = []string{"/repo"}
	m.activeBranchFilter = "main"
	m.lockedRepoFilter = true
	m.filterStack = []string{"repo", "branch"}

	resp := m.buildFilterResponse()
	require.True(t, resp.OK, "expected OK, got error: %s", resp.Error)
}

func TestBuildJobsResponse(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	m.jobs = []storage.ReviewJob{
		makeJob(1, withAgent("claude-code"), withRepoPath("/r")),
		makeJob(2, withAgent("codex"), withRepoPath("/r")),
	}

	resp := m.buildJobsResponse()
	require.True(t, resp.OK, "expected OK, got error: %s", resp.Error)

	jobs, ok := resp.Data.([]jobSnapshot)
	require.True(t, ok, "expected []jobSnapshot, got %T", resp.Data)
	require.Len(t, jobs, 2)
	assert.Equal(t, "claude-code", jobs[0].Agent)
}

func TestBuildSelectedResponse_NoSelection(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	m.selectedIdx = -1

	resp := m.buildSelectedResponse()
	require.True(t, resp.OK, "expected OK, got error: %s", resp.Error)
	data := resp.Data.(selectedSnapshot)
	assert.Nil(t, data.Job)
}

func TestBuildSelectedResponse_WithSelection(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	m.jobs = []storage.ReviewJob{
		makeJob(42, withAgent("codex"), withClosed(new(false))),
	}
	m.selectedIdx = 0
	m.selectedJobID = 42

	resp := m.buildSelectedResponse()
	data := resp.Data.(selectedSnapshot)
	require.NotNil(t, data.Job)
	assert.EqualValues(t, 42, data.Job.ID)
	assert.True(t, data.HasReview,
		"expected has_review=true for done job with closed field")
}

// --- Unit tests for mutation handlers ---

func TestHandleCtrlSetFilter(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	repo := "/test/repo"
	branch := "feature"

	params, _ := json.Marshal(map[string]string{
		"repo":   repo,
		"branch": branch,
	})
	updated, resp, _ := m.handleCtrlSetFilter(params)
	require.True(t, resp.OK, "expected OK, got error: %s", resp.Error)
	assert.Equal(t, []string{repo}, updated.activeRepoFilter)
	assert.Equal(t, branch, updated.activeBranchFilter)
	assert.Equal(t, -1, updated.selectedIdx)
}

func TestHandleCtrlSetFilter_DisplayName(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	m.repoNames = map[string][]string{
		"msgvault": {"/home/user/projects/msgvault"},
		"roborev":  {"/home/user/projects/roborev"},
	}

	params, _ := json.Marshal(map[string]string{
		"repo": "msgvault",
	})
	updated, resp, _ := m.handleCtrlSetFilter(params)
	require.True(t, resp.OK, "expected OK, got error: %s", resp.Error)
	assert.Equal(t, []string{"/home/user/projects/msgvault"},
		updated.activeRepoFilter,
		"display name should resolve to root path")
}

func TestHandleCtrlSetFilter_DisplayNameMultiplePaths(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	m.repoNames = map[string][]string{
		"backend": {
			"/home/user/work/backend",
			"/home/user/oss/backend",
		},
		"roborev": {"/home/user/projects/roborev"},
	}

	params, _ := json.Marshal(map[string]string{
		"repo": "backend",
	})
	updated, resp, _ := m.handleCtrlSetFilter(params)
	require.True(t, resp.OK, "expected OK, got error: %s", resp.Error)
	assert.Len(t, updated.activeRepoFilter, 2,
		"display name matching multiple repos should return all paths")
	assert.ElementsMatch(t,
		[]string{"/home/user/work/backend", "/home/user/oss/backend"},
		updated.activeRepoFilter)
}

func TestHandleCtrlSetFilter_DisplayNameNotInJobs(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	// Simulate: jobs are loaded for "roborev" but "msgvault" has no
	// visible jobs. repoNames (from /api/repos) knows about both.
	m.jobs = []storage.ReviewJob{
		makeJob(1, withRepoPath("/home/user/projects/roborev")),
	}
	m.repoNames = map[string][]string{
		"msgvault": {"/home/user/projects/msgvault"},
		"roborev":  {"/home/user/projects/roborev"},
	}

	params, _ := json.Marshal(map[string]string{
		"repo": "msgvault",
	})
	updated, resp, _ := m.handleCtrlSetFilter(params)
	require.True(t, resp.OK, "expected OK, got error: %s", resp.Error)
	assert.Equal(t, []string{"/home/user/projects/msgvault"},
		updated.activeRepoFilter,
		"should resolve repos not in current job page")
}

func TestRepoNamesNotClobberedByBranchFilteredModal(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	// Simulate init fetch: repoNames knows both repos.
	m.repoNames = map[string][]string{
		"msgvault": {"/home/user/projects/msgvault"},
		"roborev":  {"/home/user/projects/roborev"},
	}

	// Simulate opening filter modal with branch filter active:
	// the branch-filtered /api/repos only returns "roborev".
	branchFilteredRepos := reposMsg{
		repos: []repoFilterItem{
			{name: "roborev", rootPaths: []string{"/home/user/projects/roborev"}, count: 5},
		},
		branchFiltered: true,
	}
	result, _ := m.handleReposMsg(branchFilteredRepos)
	updated := result.(model)

	// repoNames should still contain both repos (not overwritten
	// by the branch-scoped modal data).
	assert.Len(t, updated.repoNames, 2,
		"repoNames should not be clobbered by branch-filtered modal")
	assert.Contains(t, updated.repoNames, "msgvault",
		"msgvault should survive branch-filtered modal refresh")

	// Verify set-filter still resolves the repo excluded from
	// the branch-filtered list.
	params, _ := json.Marshal(map[string]string{"repo": "msgvault"})
	updated2, resp, _ := updated.handleCtrlSetFilter(params)
	require.True(t, resp.OK, "expected OK, got error: %s", resp.Error)
	assert.Equal(t, []string{"/home/user/projects/msgvault"},
		updated2.activeRepoFilter)
}

func TestFetchRepos_BranchNoneIsUnfiltered(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			// Verify branch key is absent (not just empty).
			_, hasBranch := r.URL.Query()["branch"]
			assert.False(t, hasBranch,
				"branchNone should not send branch param")
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"repos": []map[string]any{
					{"name": "myrepo", "root_path": "/r", "count": 1},
				},
			})
		},
	))
	defer ts.Close()

	m := newModel(testEndpointFromURL(ts.URL), withExternalIODisabled())
	m.activeBranchFilter = branchNone

	cmd := m.fetchRepos()
	msg := cmd()
	rMsg, ok := msg.(reposMsg)
	require.True(t, ok, "expected reposMsg, got %T", msg)
	assert.False(t, rMsg.branchFiltered,
		"branchNone should produce branchFiltered=false")
}

func TestRepoNamesRefreshedByUnfilteredModal(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	m.repoNames = map[string][]string{
		"roborev": {"/home/user/projects/roborev"},
	}

	// Simulate opening filter modal with no branch filter:
	// the unfiltered response includes a newly registered repo.
	unfilteredRepos := reposMsg{
		repos: []repoFilterItem{
			{name: "roborev", rootPaths: []string{"/home/user/projects/roborev"}, count: 5},
			{name: "newrepo", rootPaths: []string{"/home/user/projects/newrepo"}, count: 1},
		},
	}
	result, _ := m.handleReposMsg(unfilteredRepos)
	updated := result.(model)

	assert.Len(t, updated.repoNames, 2,
		"unfiltered modal should refresh repoNames")
	assert.Contains(t, updated.repoNames, "newrepo",
		"newly registered repo should appear in repoNames")
}

func TestSetFilterFallbackWhenRepoNamesEmpty(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	// Simulate startup fetch failure: repoNames is nil.
	m.repoNames = nil

	params, _ := json.Marshal(map[string]string{"repo": "msgvault"})
	updated, resp, _ := m.handleCtrlSetFilter(params)
	require.True(t, resp.OK, "expected OK, got error: %s", resp.Error)
	// Falls through to literal — not ideal, but doesn't crash.
	assert.Equal(t, []string{"msgvault"}, updated.activeRepoFilter,
		"should accept name as-is when repoNames is empty")
}

func TestHandleCtrlSetFilter_LockedRepo(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	m.lockedRepoFilter = true

	params, _ := json.Marshal(map[string]string{"repo": "/test/repo"})
	_, resp, _ := m.handleCtrlSetFilter(params)
	require.False(t, resp.OK, "expected error for locked repo filter")
	assert.NotEmpty(t, resp.Error)
}

func TestHandleCtrlSetFilter_LockedBranch(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	m.lockedBranchFilter = true

	params, _ := json.Marshal(map[string]string{"branch": "main"})
	_, resp, _ := m.handleCtrlSetFilter(params)
	require.False(t, resp.OK, "expected error for locked branch filter")
}

func TestHandleCtrlSetFilter_LockedBranchNoRepoMutation(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	m.lockedBranchFilter = true
	m.activeRepoFilter = []string{"/original"}

	// Request both repo + branch; branch is locked so the entire
	// request should fail without mutating repo.
	params, _ := json.Marshal(map[string]string{
		"repo": "/new", "branch": "main",
	})
	updated, resp, _ := m.handleCtrlSetFilter(params)
	require.False(t, resp.OK, "expected error for locked branch")
	assert.Equal(t, []string{"/original"}, updated.activeRepoFilter,
		"repo filter should not be mutated on error")
}

func TestHandleCtrlSetFilter_ClearRepo(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	m.activeRepoFilter = []string{"/old"}
	m.filterStack = []string{"repo"}

	empty := ""
	params, _ := json.Marshal(map[string]*string{"repo": &empty})
	updated, resp, _ := m.handleCtrlSetFilter(params)
	require.True(t, resp.OK, "expected OK, got error: %s", resp.Error)
	assert.Nil(t, updated.activeRepoFilter)
}

func TestHandleCtrlClearFilter(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	m.activeRepoFilter = []string{"/repo"}
	m.activeBranchFilter = "main"
	m.filterStack = []string{"repo", "branch"}

	params, _ := json.Marshal(map[string]bool{
		"repo": true, "branch": true,
	})
	updated, resp, _ := m.handleCtrlClearFilter(params)
	require.True(t, resp.OK, "expected OK, got error: %s", resp.Error)
	assert.Nil(t, updated.activeRepoFilter)
	assert.Empty(t, updated.activeBranchFilter)
}

func TestHandleCtrlClearFilter_LockedBranchNoRepoMutation(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	m.lockedBranchFilter = true
	m.activeRepoFilter = []string{"/repo"}
	m.activeBranchFilter = "main"
	m.filterStack = []string{"repo", "branch"}

	params, _ := json.Marshal(map[string]bool{
		"repo": true, "branch": true,
	})
	updated, resp, cmd := m.handleCtrlClearFilter(params)
	require.False(t, resp.OK, "expected error for locked branch")
	assert.Equal(t, []string{"/repo"}, updated.activeRepoFilter,
		"repo filter should not be mutated on error")
	assert.Equal(t, "main", updated.activeBranchFilter,
		"branch filter should not be mutated on error")
	assert.Equal(t, []string{"repo", "branch"}, updated.filterStack,
		"filter stack should not be mutated on error")
	assert.Nil(t, cmd, "no cmd should be returned on error")
}

func TestHandleCtrlSetHideClosed(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	m.hideClosed = false

	params, _ := json.Marshal(map[string]bool{"hide_closed": true})
	updated, resp, _ := m.handleCtrlSetHideClosed(params)
	require.True(t, resp.OK, "expected OK, got error: %s", resp.Error)
	assert.True(t, updated.hideClosed)
}

func TestHandleCtrlSelectJob(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	m.jobs = []storage.ReviewJob{
		makeJob(10), makeJob(20), makeJob(30),
	}

	params, _ := json.Marshal(map[string]int64{"job_id": 20})
	updated, resp, _ := m.handleCtrlSelectJob(params)
	require.True(t, resp.OK, "expected OK, got error: %s", resp.Error)
	assert.Equal(t, 1, updated.selectedIdx)
	assert.EqualValues(t, 20, updated.selectedJobID)
}

func TestHandleCtrlSelectJob_NotFound(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	m.jobs = []storage.ReviewJob{makeJob(1)}

	params, _ := json.Marshal(map[string]int64{"job_id": 999})
	_, resp, _ := m.handleCtrlSelectJob(params)
	require.False(t, resp.OK, "expected error for missing job")
}

func TestHandleCtrlSelectJob_HiddenByFilter(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	m.jobs = []storage.ReviewJob{
		makeJob(10, withRepoPath("/visible")),
		makeJob(20, withRepoPath("/hidden")),
	}
	m.activeRepoFilter = []string{"/visible"}

	params, _ := json.Marshal(map[string]int64{"job_id": 20})
	_, resp, _ := m.handleCtrlSelectJob(params)
	require.False(t, resp.OK, "expected error for hidden job")
}

func TestHandleCtrlSelectJob_HiddenByClosed(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	m.hideClosed = true
	m.jobs = []storage.ReviewJob{
		makeJob(10, withClosed(new(false))),
		makeJob(20, withClosed(new(true))),
	}

	params, _ := json.Marshal(map[string]int64{"job_id": 20})
	_, resp, _ := m.handleCtrlSelectJob(params)
	require.False(t, resp.OK, "expected error for closed-hidden job")
}

func TestHandleCtrlSetView(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	m.currentView = viewQueue

	params, _ := json.Marshal(map[string]string{"view": "queue"})
	updated, resp, _ := m.handleCtrlSetView(params)
	require.True(t, resp.OK, "expected OK, got error: %s", resp.Error)
	assert.Equal(t, viewQueue, updated.currentView)
}

func TestHandleCtrlSetView_Invalid(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())

	params, _ := json.Marshal(map[string]string{"view": "review"})
	_, resp, _ := m.handleCtrlSetView(params)
	require.False(t, resp.OK, "expected error for unsettable view")
}

func TestHandleCtrlSetView_TasksDisabled(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	m.tasksEnabled = false

	params, _ := json.Marshal(map[string]string{"view": "tasks"})
	_, resp, _ := m.handleCtrlSetView(params)
	require.False(t, resp.OK,
		"expected error when tasks workflow disabled")
}

func TestHandleCtrlCloseReview(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	closed := false
	m.jobs = []storage.ReviewJob{
		makeJob(5, withClosed(&closed)),
	}

	closedTrue := true
	params, _ := json.Marshal(map[string]any{
		"job_id": int64(5),
		"closed": closedTrue,
	})
	_, resp, cmd := m.handleCtrlCloseReview(params)
	require.True(t, resp.OK, "expected OK, got error: %s", resp.Error)
	assert.NotNil(t, cmd, "expected non-nil cmd for close operation")
}

func TestHandleCtrlCloseReview_NonSelectedNoReflow(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	m.hideClosed = true
	closed := false
	m.jobs = []storage.ReviewJob{
		makeJob(1, withClosed(new(false))),
		makeJob(2, withClosed(&closed)),
		makeJob(3, withClosed(new(false))),
	}
	m.selectedIdx = 0
	m.selectedJobID = 1

	// Close job 2, which is not the selected job
	params, _ := json.Marshal(map[string]any{
		"job_id": int64(2),
		"closed": true,
	})
	updated, resp, _ := m.handleCtrlCloseReview(params)
	require.True(t, resp.OK, "expected OK, got error: %s", resp.Error)
	assert.EqualValues(t, 1, updated.selectedJobID,
		"selection should remain on job 1")
	assert.Equal(t, 0, updated.selectedIdx,
		"selectedIdx should remain 0")
}

func TestHandleCtrlCloseReview_NoReview(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	m.jobs = []storage.ReviewJob{
		makeJob(5, withStatus(storage.JobStatusRunning)),
	}

	params, _ := json.Marshal(map[string]any{"job_id": int64(5)})
	_, resp, _ := m.handleCtrlCloseReview(params)
	require.False(t, resp.OK, "expected error for job without review")
}

func TestHandleCtrlCloseReview_ClearsSelectionWhenNoneVisible(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	m.hideClosed = true
	// Only one visible job — closing it leaves no visible jobs.
	m.jobs = []storage.ReviewJob{
		makeJob(1, withClosed(new(false))),
	}
	m.selectedIdx = 0
	m.selectedJobID = 1

	params, _ := json.Marshal(map[string]any{
		"job_id": int64(1),
		"closed": true,
	})
	updated, resp, _ := m.handleCtrlCloseReview(params)
	require.True(t, resp.OK, "expected OK, got error: %s", resp.Error)
	assert.Equal(t, -1, updated.selectedIdx,
		"selectedIdx should be cleared when no visible jobs remain")
	assert.EqualValues(t, 0, updated.selectedJobID,
		"selectedJobID should be cleared when no visible jobs remain")
}

func TestHandleCtrlCloseReview_RollbackRestoresSelection(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	m.hideClosed = true
	m.jobs = []storage.ReviewJob{
		makeJob(1, withClosed(new(false))),
	}
	m.selectedIdx = 0
	m.selectedJobID = 1

	params, _ := json.Marshal(map[string]any{
		"job_id": int64(1),
		"closed": true,
	})
	updated, resp, cmd := m.handleCtrlCloseReview(params)
	require.True(t, resp.OK, "expected OK, got error: %s", resp.Error)
	// Selection was cleared (no visible jobs remain).
	require.Equal(t, -1, updated.selectedIdx)

	// Simulate server failure — the result handler should
	// restore job state and reselect the original job.
	msg := cmd()
	result, _ := updated.handleClosedResultMsg(msg.(closedResultMsg))
	restored := result.(model)
	assert.EqualValues(t, 1, restored.selectedJobID,
		"selection should be restored to original job on rollback")
	assert.Equal(t, 0, restored.selectedIdx,
		"selectedIdx should point to the restored job")
}

func TestHandleCtrlCancelJob(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	m.jobs = []storage.ReviewJob{
		makeJob(7, withStatus(storage.JobStatusRunning)),
	}

	params, _ := json.Marshal(map[string]int64{"job_id": 7})
	updated, resp, cmd := m.handleCtrlCancelJob(params)
	require.True(t, resp.OK, "expected OK, got error: %s", resp.Error)
	assert.NotNil(t, cmd, "expected non-nil cmd for cancel")
	assert.Equal(t, storage.JobStatusCanceled, updated.jobs[0].Status)
}

func TestHandleCtrlCancelJob_NonSelectedNoReflow(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	m.hideClosed = true
	m.jobs = []storage.ReviewJob{
		makeJob(1, withClosed(new(false))),
		makeJob(2, withStatus(storage.JobStatusRunning)),
		makeJob(3, withClosed(new(false))),
	}
	m.selectedIdx = 0
	m.selectedJobID = 1

	// Cancel job 2, which is not the selected job
	params, _ := json.Marshal(map[string]int64{"job_id": 2})
	updated, resp, _ := m.handleCtrlCancelJob(params)
	require.True(t, resp.OK, "expected OK, got error: %s", resp.Error)
	assert.EqualValues(t, 1, updated.selectedJobID,
		"selection should remain on job 1")
	assert.Equal(t, 0, updated.selectedIdx,
		"selectedIdx should remain 0")
}

func TestHandleCtrlCancelJob_ClearsSelectionWhenNoneVisible(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	m.hideClosed = true
	// Only one visible job — canceling it hides it under hideClosed.
	m.jobs = []storage.ReviewJob{
		makeJob(1, withStatus(storage.JobStatusRunning)),
	}
	m.selectedIdx = 0
	m.selectedJobID = 1

	params, _ := json.Marshal(map[string]int64{"job_id": 1})
	updated, resp, _ := m.handleCtrlCancelJob(params)
	require.True(t, resp.OK, "expected OK, got error: %s", resp.Error)
	assert.Equal(t, -1, updated.selectedIdx,
		"selectedIdx should be cleared when no visible jobs remain")
	assert.EqualValues(t, 0, updated.selectedJobID,
		"selectedJobID should be cleared when no visible jobs remain")
}

func TestHandleCtrlCancelJob_RollbackRestoresSelection(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	m.hideClosed = true
	m.jobs = []storage.ReviewJob{
		makeJob(1, withStatus(storage.JobStatusRunning)),
	}
	m.selectedIdx = 0
	m.selectedJobID = 1

	params, _ := json.Marshal(map[string]int64{"job_id": 1})
	updated, resp, cmd := m.handleCtrlCancelJob(params)
	require.True(t, resp.OK, "expected OK, got error: %s", resp.Error)
	require.Equal(t, -1, updated.selectedIdx)

	// Simulate server failure — the result handler should
	// restore status and reselect the original job.
	msg := cmd()
	result, _ := updated.handleCancelResultMsg(msg.(cancelResultMsg))
	restored := result.(model)
	assert.Equal(t, storage.JobStatusRunning, restored.jobs[0].Status,
		"status should be rolled back")
	assert.EqualValues(t, 1, restored.selectedJobID,
		"selection should be restored on rollback")
	assert.Equal(t, 0, restored.selectedIdx,
		"selectedIdx should point to the restored job")
}

func TestHandleCtrlCancelJob_WrongStatus(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	m.jobs = []storage.ReviewJob{
		makeJob(7, withStatus(storage.JobStatusDone)),
	}

	params, _ := json.Marshal(map[string]int64{"job_id": 7})
	_, resp, _ := m.handleCtrlCancelJob(params)
	require.False(t, resp.OK, "expected error for non-cancellable job")
}

func TestHandleCancelKey_ClearsSelectionWhenNoneVisible(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	m.currentView = viewQueue
	m.hideClosed = true
	m.jobs = []storage.ReviewJob{
		makeJob(1, withStatus(storage.JobStatusRunning)),
	}
	m.selectedIdx = 0
	m.selectedJobID = 1

	result, cmd := m.handleCancelKey()
	updated := result.(model)
	assert.Equal(t, storage.JobStatusCanceled, updated.jobs[0].Status)
	assert.Equal(t, -1, updated.selectedIdx,
		"selectedIdx should be cleared when no visible jobs remain")
	assert.EqualValues(t, 0, updated.selectedJobID,
		"selectedJobID should be cleared")
	assert.NotNil(t, cmd, "expected non-nil cmd for cancel")
}

func TestHandleCancelKey_RollbackRestoresSelection(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	m.currentView = viewQueue
	m.hideClosed = true
	m.jobs = []storage.ReviewJob{
		makeJob(1, withStatus(storage.JobStatusRunning)),
	}
	m.selectedIdx = 0
	m.selectedJobID = 1

	result, cmd := m.handleCancelKey()
	updated := result.(model)
	require.Equal(t, -1, updated.selectedIdx)

	// Simulate server failure.
	msg := cmd()
	rollback, _ := updated.handleCancelResultMsg(msg.(cancelResultMsg))
	restored := rollback.(model)
	assert.Equal(t, storage.JobStatusRunning, restored.jobs[0].Status,
		"status should be rolled back")
	assert.EqualValues(t, 1, restored.selectedJobID,
		"selection should be restored on rollback")
	assert.Equal(t, 0, restored.selectedIdx,
		"selectedIdx should point to the restored job")
}

func TestHandleCtrlRerunJob(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	m.jobs = []storage.ReviewJob{
		makeJob(8, withStatus(storage.JobStatusFailed)),
	}

	params, _ := json.Marshal(map[string]int64{"job_id": 8})
	updated, resp, cmd := m.handleCtrlRerunJob(params)
	require.True(t, resp.OK, "expected OK, got error: %s", resp.Error)
	assert.NotNil(t, cmd, "expected non-nil cmd for rerun")
	assert.Equal(t, storage.JobStatusQueued, updated.jobs[0].Status)
}

func TestHandleCtrlRerunJob_ClearsClosedAndVerdict(t *testing.T) {
	verdict := "FAIL"
	m := newModel(testEndpoint, withExternalIODisabled())
	m.hideClosed = true
	m.jobs = []storage.ReviewJob{
		makeJob(8,
			withStatus(storage.JobStatusDone),
			withClosed(new(true)),
			func(j *storage.ReviewJob) { j.Verdict = &verdict },
		),
	}
	m.selectedIdx = 0
	m.selectedJobID = 8

	params, _ := json.Marshal(map[string]int64{"job_id": 8})
	updated, resp, _ := m.handleCtrlRerunJob(params)
	require.True(t, resp.OK, "expected OK, got error: %s", resp.Error)
	assert.Equal(t, storage.JobStatusQueued, updated.jobs[0].Status)
	assert.Nil(t, updated.jobs[0].Closed,
		"Closed should be cleared on rerun")
	assert.Nil(t, updated.jobs[0].Verdict,
		"Verdict should be cleared on rerun")
	// Job should now be visible under hideClosed
	assert.True(t, updated.isJobVisible(updated.jobs[0]),
		"rerun job should be visible with hideClosed")
}

func TestHandleCtrlRerunJob_WrongStatus(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	m.jobs = []storage.ReviewJob{
		makeJob(8, withStatus(storage.JobStatusRunning)),
	}

	params, _ := json.Marshal(map[string]int64{"job_id": 8})
	_, resp, _ := m.handleCtrlRerunJob(params)
	require.False(t, resp.OK, "expected error for non-rerunnable job")
}

func TestHandleRerunKey_ClearsClosedAndVerdict(t *testing.T) {
	verdict := "FAIL"
	m := newModel(testEndpoint, withExternalIODisabled())
	m.currentView = viewQueue
	m.hideClosed = true
	m.jobs = []storage.ReviewJob{
		makeJob(10,
			withStatus(storage.JobStatusDone),
			withClosed(new(true)),
			func(j *storage.ReviewJob) { j.Verdict = &verdict },
		),
	}
	m.selectedIdx = 0
	m.selectedJobID = 10

	result, _ := m.handleRerunKey()
	updated := result.(model)
	assert.Equal(t, storage.JobStatusQueued, updated.jobs[0].Status)
	assert.Nil(t, updated.jobs[0].Closed,
		"Closed should be cleared on rerun")
	assert.Nil(t, updated.jobs[0].Verdict,
		"Verdict should be cleared on rerun")
	assert.True(t, updated.isJobVisible(updated.jobs[0]),
		"rerun job should be visible with hideClosed")
}

func TestRerunResultMsg_RestoresClosedOnFailure(t *testing.T) {
	verdict := "FAIL"
	closed := true
	m := newModel(testEndpoint, withExternalIODisabled())
	m.jobs = []storage.ReviewJob{
		makeJob(10, withStatus(storage.JobStatusQueued)),
	}
	// Simulate the optimistic update already applied — job is
	// queued with nil Closed/Verdict. The error path should
	// restore the original values.
	msg := rerunResultMsg{
		jobID:      10,
		oldState:   storage.JobStatusDone,
		oldClosed:  &closed,
		oldVerdict: &verdict,
		err:        fmt.Errorf("server error"),
	}
	result, _ := m.handleRerunResultMsg(msg)
	updated := result.(model)
	assert.Equal(t, storage.JobStatusDone, updated.jobs[0].Status)
	require.NotNil(t, updated.jobs[0].Closed)
	assert.True(t, *updated.jobs[0].Closed,
		"Closed should be restored on failure")
	require.NotNil(t, updated.jobs[0].Verdict)
	assert.Equal(t, "FAIL", *updated.jobs[0].Verdict,
		"Verdict should be restored on failure")
}

func TestHandleCtrlQuit(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())

	updated, resp, cmd := m.handleCtrlQuit()
	assert.True(t, resp.OK, "expected OK response")
	assert.NotNil(t, cmd, "expected non-nil cmd (tea.Quit)")
	_ = updated
}

func TestNoQuit_QKeyInQueueView(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled(), withNoQuit())
	m.currentView = viewQueue

	result, cmd := m.Update(keyPressMsg('q'))
	updated := result.(model)
	assert.Equal(t, viewQueue, updated.currentView,
		"q should be suppressed in queue view with noQuit")
	assert.Nil(t, cmd, "no cmd expected when q is suppressed")
}

func TestNoQuit_CtrlCStillQuits(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled(), withNoQuit())
	m.currentView = viewQueue

	_, cmd := m.Update(ctrlKeyMsg('c'))
	assert.NotNil(t, cmd,
		"ctrl+c should still produce tea.Quit with noQuit")
}

func TestNoQuit_QStillClosesModal(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled(), withNoQuit())
	m.currentView = viewReview
	m.currentReview = &storage.Review{Agent: "test", Output: "test"}

	result, _ := m.Update(keyPressMsg('q'))
	updated := result.(model)
	assert.Equal(t, viewQueue, updated.currentView,
		"q in review view should close modal even with noQuit")
}

func TestNoQuit_QueueHelpOmitsQuit(t *testing.T) {
	normal := newModel(testEndpoint, withExternalIODisabled())
	noQuit := newModel(testEndpoint, withExternalIODisabled(), withNoQuit())

	normalRows := normal.queueHelpRows()
	noQuitRows := noQuit.queueHelpRows()

	hasQuit := func(rows [][]helpItem) bool {
		for _, row := range rows {
			for _, item := range row {
				if item.key == "q" {
					return true
				}
			}
		}
		return false
	}

	assert.True(t, hasQuit(normalRows),
		"normal mode should show q in queue help")
	assert.False(t, hasQuit(noQuitRows),
		"noQuit mode should omit q from queue help")
}

func TestNoQuit_HelpViewOmitsQuit(t *testing.T) {
	normal := helpLines(true, false)
	noQuit := helpLines(true, true)

	contains := func(lines []string, substr string) bool {
		for _, line := range lines {
			if strings.Contains(line, substr) {
				return true
			}
		}
		return false
	}

	assert.True(t, contains(normal, "Quit"),
		"normal help should mention Quit")
	assert.False(t, contains(noQuit, "Quit"),
		"noQuit help should omit Quit")
}

func TestNoQuit_TasksEmptyHelpOmitsQuit(t *testing.T) {
	normal := newModel(testEndpoint, withExternalIODisabled())
	normal.currentView = viewTasks
	normal.tasksEnabled = true
	normal.fixJobs = nil // empty tasks view

	noQuitM := newModel(testEndpoint, withExternalIODisabled(), withNoQuit())
	noQuitM.currentView = viewTasks
	noQuitM.tasksEnabled = true
	noQuitM.fixJobs = nil

	normalOut := stripTestANSI(normal.renderTasksView())
	noQuitOut := stripTestANSI(noQuitM.renderTasksView())

	assert.Contains(t, normalOut, "quit",
		"normal tasks empty view should show quit")
	assert.NotContains(t, noQuitOut, "quit",
		"noQuit tasks empty view should omit quit")
}

// --- Control message routing through Update() ---

func TestUpdateRoutesControlQuery(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	m.jobs = []storage.ReviewJob{makeJob(1)}

	respCh := make(chan controlResponse, 1)
	msg := controlQueryMsg{
		req:    controlRequest{Command: "get-state"},
		respCh: respCh,
	}

	updated, _ := m.Update(msg)
	_, ok := updated.(model)
	require.True(t, ok, "Update returned %T, want model", updated)

	select {
	case resp := <-respCh:
		assert.True(t, resp.OK, "expected OK, got error: %s", resp.Error)
	default:
		require.Fail(t, "no response received on channel")
	}
}

func TestUpdateRoutesControlMutation(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	m.jobs = []storage.ReviewJob{makeJob(1), makeJob(2)}

	params, _ := json.Marshal(map[string]int64{"job_id": 2})
	respCh := make(chan controlResponse, 1)
	msg := controlMutationMsg{
		req: controlRequest{
			Command: "select-job", Params: params,
		},
		respCh: respCh,
	}

	updated, _ := m.Update(msg)
	um := updated.(model)
	assert.EqualValues(t, 2, um.selectedJobID)

	select {
	case resp := <-respCh:
		assert.True(t, resp.OK, "expected OK, got error: %s", resp.Error)
	default:
		require.Fail(t, "no response received on channel")
	}
}

// --- Integration test with real Unix socket ---

func TestControlSocketRoundtrip(t *testing.T) {
	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "test.sock")

	ts := controlTestServer(t)

	m := newModel(testEndpointFromURL(ts.URL), withExternalIODisabled())

	// Provide a pipe for stdin so the program doesn't block on TTY.
	r, w, _ := os.Pipe()
	p := tea.NewProgram(m,
		tea.WithoutRenderer(),
		tea.WithInput(r),
	)
	runDone := make(chan struct{})
	go func() { _, _ = p.Run(); close(runDone) }()
	t.Cleanup(func() {
		// Close the write end first so bubbletea's readLoop sees EOF,
		// then Kill and wait for Run to return.
		w.Close()
		p.Kill()
		<-runDone
		// Deliberately do not close the read end. Bubbletea's input
		// read goroutine (via cancelreader) may still call os.File.Fd()
		// on r after Run returns, and on macOS the kqueue cancelreader
		// races with os.File.Close(). The OS reclaims the fd at process
		// exit; leaking it for this short-lived test is harmless.
	})

	// Give the program time to complete Init() and reach its
	// steady-state event loop.
	time.Sleep(500 * time.Millisecond)

	cleanup, err := startControlListener(socketPath, p)
	require.NoError(t, err, "startControlListener")
	t.Cleanup(cleanup)

	// Test get-state
	resp := sendControlCommand(t, socketPath,
		`{"command":"get-state"}`)
	require.True(t, resp.OK, "get-state failed: %s", resp.Error)

	// Test get-jobs
	resp = sendControlCommand(t, socketPath,
		`{"command":"get-jobs"}`)
	require.True(t, resp.OK, "get-jobs failed: %s", resp.Error)

	// Test set-hide-closed mutation
	resp = sendControlCommand(t, socketPath,
		`{"command":"set-hide-closed","params":{"hide_closed":true}}`)
	require.True(t, resp.OK,
		"set-hide-closed failed: %s", resp.Error)

	// Verify state via get-state
	resp = sendControlCommand(t, socketPath,
		`{"command":"get-state"}`)
	require.True(t, resp.OK,
		"get-state after mutation: %s", resp.Error)

	// Test unknown command
	resp = sendControlCommand(t, socketPath,
		`{"command":"nope"}`)
	assert.False(t, resp.OK, "expected error for unknown command")
}

// controlTestServer returns an httptest.Server that handles all
// TUI Init() API requests with valid empty responses.
func controlTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/jobs":
				json.NewEncoder(w).Encode(map[string]any{
					"jobs":     []any{},
					"has_more": false,
					"stats":    storage.JobStats{},
				})
			case "/api/status":
				json.NewEncoder(w).Encode(
					storage.DaemonStatus{Version: "test"},
				)
			default:
				json.NewEncoder(w).Encode(map[string]any{
					"ok": true,
				})
			}
		},
	))
	t.Cleanup(ts.Close)
	return ts
}

func TestControlSocketInvalidJSON(t *testing.T) {
	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "test.sock")

	// Invalid JSON is rejected at the socket handler level before
	// Program.Send(), so no real HTTP server needed.
	cleanup, err := startControlListener(
		socketPath, newTestProgram(t),
	)
	require.NoError(t, err, "startControlListener")
	t.Cleanup(cleanup)

	resp := sendControlCommand(t, socketPath, `{not json}`)
	assert.False(t, resp.OK, "expected error for invalid JSON")
}

// newTestProgram creates a tea.Program backed by a mock HTTP server
// so that Init() commands complete quickly.
func newTestProgram(t *testing.T) *tea.Program {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{})
		},
	))
	t.Cleanup(ts.Close)
	m := newModel(testEndpointFromURL(ts.URL), withExternalIODisabled())
	p := tea.NewProgram(m, tea.WithoutRenderer())
	go func() { _, _ = p.Run() }()
	t.Cleanup(func() { p.Kill() })
	time.Sleep(100 * time.Millisecond)
	return p
}

// --- Stale socket safety tests ---

func TestRemoveStaleSocket_NonexistentPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nosuch.sock")
	assert.NoError(t, removeStaleSocket(path))
}

func TestRemoveStaleSocket_RegularFileRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file.txt")
	require.NoError(t, os.WriteFile(path, []byte("data"), 0o600))

	err := removeStaleSocket(path)
	require.Error(t, err, "expected error for regular file")
	assert.FileExists(t, path, "regular file should not be deleted")
}

func TestRemoveStaleSocket_StaleSocketRemoved(t *testing.T) {
	// Use a short path to stay within the Unix socket length limit.
	path := shortSocketPath(t, "stale")
	ln, err := net.Listen("unix", path)
	require.NoError(t, err)
	ln.Close()

	require.NoError(t, removeStaleSocket(path),
		"expected stale socket to be removed")
	assert.NoFileExists(t, path, "stale socket should be removed")
}

func TestRemoveStaleSocket_LiveSocketRefused(t *testing.T) {
	path := shortSocketPath(t, "live")
	ln, err := net.Listen("unix", path)
	require.NoError(t, err)
	defer ln.Close()

	err = removeStaleSocket(path)
	require.Error(t, err, "expected error for live socket")
	assert.FileExists(t, path, "live socket should not be deleted")
}

func TestEnsureSocketDir_CreatesParentDir(t *testing.T) {
	base := shortSocketPath(t, "dir")
	// Remove the file shortSocketPath created, use it as a subdir.
	os.Remove(base)
	socketDir := filepath.Join(base, "sub")
	t.Cleanup(func() { os.RemoveAll(base) })

	require.NoError(t, ensureSocketDir(socketDir),
		"expected ensureSocketDir to create dirs")

	_, err := os.Stat(socketDir)
	require.NoError(t, err, "stat created dir")
	// Permission assertions are Unix-only; see
	// TestEnsureSocketDirTightensExistingDir in
	// control_unix_test.go.
}

func TestStartControlListener_CreatesCustomParentDir(t *testing.T) {
	base := shortSocketPath(t, "cust")
	os.Remove(base)
	socketPath := filepath.Join(base, "sub", "t.sock")
	t.Cleanup(func() { os.RemoveAll(base) })

	cleanup, err := startControlListener(
		socketPath, newTestProgram(t),
	)
	require.NoError(t, err,
		"custom socket path with missing parent should work")
	cleanup()
}

// shortSocketPath returns a temporary socket path short enough for
// the Unix socket 104-byte name limit on macOS.
func shortSocketPath(t *testing.T, prefix string) string {
	t.Helper()
	f, err := os.CreateTemp("", prefix)
	require.NoError(t, err)
	path := f.Name()
	f.Close()
	os.Remove(path)
	t.Cleanup(func() { os.Remove(path) })
	return path
}

// --- Runtime metadata tests ---

func TestTUIRuntimeWriteAndRead(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	tmpDir := setupTuiTestEnv(t)

	info := TUIRuntimeInfo{
		PID:        12345,
		SocketPath: filepath.Join(tmpDir, "tui.12345.sock"),
		ServerAddr: "http://127.0.0.1:7373",
	}

	require.NoError(WriteTUIRuntime(info))

	runtimes, err := ListAllTUIRuntimes()
	require.NoError(err)
	require.Len(runtimes, 1)
	assert.Equal(12345, runtimes[0].PID)
	assert.Equal(info.SocketPath, runtimes[0].SocketPath)
}

func TestCleanupStaleTUIRuntimes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	setupTuiTestEnv(t)

	// Create a real stale socket (listener closed, PID dead).
	sockPath := shortSocketPath(t, "stale")
	ln, err := net.Listen("unix", sockPath)
	require.NoError(err)
	ln.Close() // leave stale socket file behind

	info := TUIRuntimeInfo{
		PID:        999999999,
		SocketPath: sockPath,
		ServerAddr: "http://127.0.0.1:7373",
	}
	require.NoError(WriteTUIRuntime(info))

	cleaned := CleanupStaleTUIRuntimes()
	assert.Equal(1, cleaned)

	runtimes, _ := ListAllTUIRuntimes()
	assert.Empty(runtimes)
	assert.NoFileExists(sockPath,
		"stale socket file should be removed")
}

func TestCleanupStaleTUIRuntimes_NonSocketPreserved(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	tmpDir := setupTuiTestEnv(t)

	// Write metadata pointing to a regular file (not a socket).
	// Cleanup should remove the metadata but not the file.
	filePath := filepath.Join(tmpDir, "tui.999999999.sock")
	require.NoError(os.WriteFile(filePath, []byte{}, 0o600))

	info := TUIRuntimeInfo{
		PID:        999999999,
		SocketPath: filePath,
		ServerAddr: "http://127.0.0.1:7373",
	}
	require.NoError(WriteTUIRuntime(info))

	cleaned := CleanupStaleTUIRuntimes()
	assert.Equal(1, cleaned)

	// Metadata should be gone.
	runtimes, _ := ListAllTUIRuntimes()
	assert.Empty(runtimes)
	// The regular file must NOT have been deleted.
	assert.FileExists(filePath,
		"regular file should not be removed")
}

// --- Helper ---

func sendControlCommand(
	t *testing.T, socketPath, command string,
) controlResponse {
	t.Helper()

	conn, err := net.DialTimeout(
		"unix", socketPath, 2*time.Second,
	)
	require.NoError(t, err, "dial socket")
	defer conn.Close()

	require.NoError(t,
		conn.SetDeadline(time.Now().Add(5*time.Second)),
		"set deadline")

	_, err = fmt.Fprintf(conn, "%s\n", command)
	require.NoError(t, err, "write command")

	buf := make([]byte, 64*1024)
	n, err := conn.Read(buf)
	require.NoError(t, err, "read response")

	var resp controlResponse
	require.NoError(t,
		json.Unmarshal(buf[:n], &resp),
		"unmarshal response %q", string(buf[:n]))
	return resp
}
