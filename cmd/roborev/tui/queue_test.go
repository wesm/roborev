package tui

import (
	"encoding/json"
	"errors"
	"fmt"
	"image/color"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
)

func mouseLeftClick(x, y int) tea.MouseMsg {
	return tea.MouseClickMsg(tea.Mouse{
		X:      x,
		Y:      y,
		Button: tea.MouseLeft,
	})
}

func mouseWheelDown() tea.MouseMsg {
	return tea.MouseWheelMsg(tea.Mouse{Button: tea.MouseWheelDown})
}

func mouseWheelUp() tea.MouseMsg {
	return tea.MouseWheelMsg(tea.Mouse{Button: tea.MouseWheelUp})
}

func newTuiModel(serverAddr string) model {
	return newModel(testEndpointFromURL(serverAddr), withExternalIODisabled())
}

const (
	tuiViewQueue  = viewQueue
	tuiViewTasks  = viewTasks
	tuiViewReview = viewReview
)

func TestTUIQueueNavigation(t *testing.T) {
	threeJobs := []storage.ReviewJob{
		makeJob(1),
		makeJob(2, withStatus(storage.JobStatusQueued)),
		makeJob(3),
	}

	tests := []struct {
		name         string
		jobs         []storage.ReviewJob
		activeFilter []string
		startIdx     int
		key          any
		wantIdx      int
		wantJobID    int64
	}{
		{
			name:      "j moves down",
			jobs:      threeJobs,
			startIdx:  1,
			key:       'j',
			wantIdx:   2,
			wantJobID: 3,
		},
		{
			name:      "k moves up",
			jobs:      threeJobs,
			startIdx:  1,
			key:       'k',
			wantIdx:   0,
			wantJobID: 1,
		},
		{
			name:      "down arrow moves down",
			jobs:      threeJobs,
			startIdx:  1,
			key:       tea.KeyDown,
			wantIdx:   2,
			wantJobID: 3,
		},
		{
			name:      "up arrow moves up",
			jobs:      threeJobs,
			startIdx:  1,
			key:       tea.KeyUp,
			wantIdx:   0,
			wantJobID: 1,
		},
		{
			name:      "left arrow moves down",
			jobs:      threeJobs,
			startIdx:  1,
			key:       tea.KeyLeft,
			wantIdx:   2,
			wantJobID: 3,
		},
		{
			name:      "right arrow moves up",
			jobs:      threeJobs,
			startIdx:  1,
			key:       tea.KeyRight,
			wantIdx:   0,
			wantJobID: 1,
		},
		{
			name:      "g jumps to top (unfiltered)",
			jobs:      threeJobs,
			startIdx:  2,
			key:       'g',
			wantIdx:   0,
			wantJobID: 1,
		},
		{
			name: "g jumps to top (filtered)",
			jobs: []storage.ReviewJob{
				makeJob(1, withRepoPath("/repo/alpha")),
				makeJob(2, withRepoPath("/repo/beta")),
				makeJob(3, withRepoPath("/repo/beta")),
			},
			activeFilter: []string{"/repo/beta"},
			startIdx:     2,
			key:          'g',
			wantIdx:      1,
			wantJobID:    2,
		},
		{
			name:      "g jumps to top (empty)",
			jobs:      []storage.ReviewJob{},
			startIdx:  0,
			key:       'g',
			wantIdx:   0,
			wantJobID: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := setupQueue(tt.jobs, tt.startIdx)
			if tt.activeFilter != nil {
				m.activeRepoFilter = tt.activeFilter
			}

			k, ok := tt.key.(rune)
			require.True(t, ok)
			m2, _ := pressSpecial(m, k)

			assert.Equal(t, tt.wantIdx, m2.selectedIdx)
			assert.Equal(t, tt.wantJobID, m2.selectedJobID)
		})
	}
}

func TestTUIQueueMouseClickSelectsVisibleRow(t *testing.T) {
	m := newTuiModel("http://localhost")
	m.currentView = tuiViewQueue
	m.width = 120
	m.height = 20
	m.jobs = []storage.ReviewJob{
		makeJob(1),
		makeJob(2),
		makeJob(3),
		makeJob(4),
		makeJob(5),
	}
	m.selectedIdx = 0
	m.selectedJobID = 1

	m2, _ := updateModel(t, m, mouseLeftClick(4, 6))

	assert.EqualValues(t, 2, m2.selectedJobID)
	assert.Equal(t, 1, m2.selectedIdx)
}

func TestTUIQueueMouseHeaderClickDoesNotSort(t *testing.T) {
	m := newTuiModel("http://localhost")
	m.currentView = tuiViewQueue
	m.width = 120
	m.height = 20
	m.jobs = []storage.ReviewJob{
		makeJob(3),
		makeJob(1),
		makeJob(2),
	}
	m.selectedIdx = 1
	m.selectedJobID = 1

	m2, _ := updateModel(t, m, mouseLeftClick(2, 3))
	if got := []int64{m2.jobs[0].ID, m2.jobs[1].ID, m2.jobs[2].ID}; !slices.Equal(got, []int64{3, 1, 2}) {
		assert.True(t, slices.Equal(got, []int64{3, 1, 2}), "expected header click not to reorder rows, got %v, got")
	}
	assert.False(t, m2.selectedJobID != 1 || m2.selectedIdx != 1)
}

func TestTUIQueueMouseIgnoredOutsideQueueView(t *testing.T) {
	m := newTuiModel("http://localhost")
	m.currentView = tuiViewReview
	m.jobs = []storage.ReviewJob{
		makeJob(2),
		makeJob(1),
	}
	m.selectedIdx = 0
	m.selectedJobID = 2

	m2, _ := updateModel(t, m, mouseLeftClick(2, 3))
	assert.False(t, m2.selectedIdx != 0 || m2.selectedJobID != 2)
	assert.True(t, slices.Equal([]int64{m2.jobs[0].ID, m2.jobs[1].ID}, []int64{2, 1}), "expected job order unchanged outside queue view")
}

func TestTUIQueueCtrlJFetchesReview(t *testing.T) {
	m := newTuiModel("http://localhost")
	m.currentView = tuiViewQueue
	m.jobs = []storage.ReviewJob{
		makeJob(1, withStatus(storage.JobStatusDone)),
	}
	m.selectedIdx = 0
	m.selectedJobID = 1

	m2, cmd := pressCtrl(m, 'j')

	assert.Equal(t, tuiViewQueue, m2.reviewFromView)
	assert.NotNil(t, cmd, "expected fetchReview command for ctrl+j activation")
}

func TestTUIQueueMouseWheelScrollsSelection(t *testing.T) {
	m := newTuiModel("http://localhost")
	m.currentView = tuiViewQueue
	m.jobs = []storage.ReviewJob{
		makeJob(1),
		makeJob(2),
		makeJob(3),
	}
	m.selectedIdx = 0
	m.selectedJobID = 1

	m2, _ := updateModel(t, m, mouseWheelDown())
	assert.False(t, m2.selectedIdx != 1 || m2.selectedJobID != 2)

	m3, _ := updateModel(t, m2, mouseWheelUp())
	assert.False(t, m3.selectedIdx != 0 || m3.selectedJobID != 1)
}

func TestTUIQueueMouseClickScrolledWindow(t *testing.T) {
	m := newTuiModel("http://localhost")
	m.currentView = tuiViewQueue
	m.width = 120
	m.height = 16

	for i := range 20 {
		m.jobs = append(m.jobs, makeJob(int64(i+1)))
	}

	visibleRows := m.queueVisibleRows()
	if visibleRows >= 20 {
		t.Skipf(
			"terminal too tall for scroll test: %d visible rows",
			visibleRows,
		)
	}

	m.selectedIdx = 15
	m.selectedJobID = 16

	start := max(15-visibleRows/2, 0)
	end := start + visibleRows
	if end > 20 {
		end = 20
		start = max(end-visibleRows, 0)
	}
	wantJobID := m.jobs[start].ID
	wantIdx := start

	m2, _ := updateModel(t, m, mouseLeftClick(4, 5))

	assert.Equal(t, wantJobID, m2.selectedJobID)
	assert.Equal(t, wantIdx, m2.selectedIdx)
}

func TestTUIQueueCompactMode(t *testing.T) {
	m := newTuiModel("http://localhost")
	m.currentView = tuiViewQueue
	m.width = 80
	m.height = 10

	for i := range 5 {
		m.jobs = append(m.jobs, makeJob(int64(i+1)))
	}
	m.selectedIdx = 0
	m.selectedJobID = 1

	output := m.View().Content

	assert.Contains(t, output, "roborev")

	assert.NotContains(t, output, "JobID")

	assert.NotContains(t, output, "nav")

	assert.NotContains(t, output, "Daemon:")

	m.selectedIdx = 2
	m.selectedJobID = 3
	m2, _ := updateModel(t, m, mouseLeftClick(4, 1))
	assert.EqualValues(t, 1, m2.selectedJobID)
}

func TestTUIQueueDistractionFreeToggle(t *testing.T) {
	m := newTuiModel("http://localhost")
	m.currentView = tuiViewQueue
	m.width = 120
	m.height = 30

	for i := range 5 {
		m.jobs = append(m.jobs, makeJob(int64(i+1)))
	}
	m.selectedIdx = 0
	m.selectedJobID = 1

	output := m.View().Content
	assert.Contains(t, output, "JobID")
	assert.Contains(t, output, "nav")

	m2, _ := updateModel(t, m, keyPressMsg('D'))
	assert.True(t, m2.distractionFree, "D should toggle distraction-free on")
	output = m2.View().Content
	assert.NotContains(t, output, "JobID")
	assert.NotContains(t, output, "nav")
	assert.NotContains(t, output, "Daemon:")

	m3, _ := updateModel(t, m2, keyPressMsg('D'))
	assert.False(t, m3.distractionFree, "D should toggle distraction-free off")
	output = m3.View().Content
	assert.Contains(t, output, "JobID")
}

// Regression test for #586: distraction-free mode lost the last queue entry.
// The lipgloss table drops the last data row when Headers() is not called,
// so compact mode must still supply (empty) headers and strip the resulting
// blank line.
func TestTUIQueueDistractionFreePreservesLastJob(t *testing.T) {
	assert := assert.New(t)
	for _, h := range []int{20, 24, 30} {
		for _, n := range []int{10, 25, 40} {
			t.Run(fmt.Sprintf("h%d_jobs%d", h, n), func(t *testing.T) {
				m := newTuiModel("http://localhost")
				m.currentView = tuiViewQueue
				m.width = 120
				m.height = h
				for i := range n {
					m.jobs = append(m.jobs, makeJob(int64(9300+i)))
				}
				lastJobID := fmt.Sprintf("%d", 9300+n-1)
				// Select near the end so the last job is in the scroll window.
				m.selectedIdx = n - 2
				m.selectedJobID = int64(9300 + n - 2)

				normalOutput := m.View().Content
				normalHasLast := strings.Contains(normalOutput, lastJobID)

				m2, _ := updateModel(t, m, keyPressMsg('D'))
				compactOutput := m2.View().Content
				compactHasLast := strings.Contains(compactOutput, lastJobID)

				if normalHasLast {
					assert.True(compactHasLast,
						"h=%d n=%d: job %s visible before D but missing after", h, n, lastJobID)
				}
				compactLines := strings.Count(compactOutput, "\n") + 1
				assert.LessOrEqual(compactLines, h,
					"h=%d n=%d: output %d lines exceeds terminal height", h, n, compactLines)
			})
		}
	}
}

func TestTUITasksMouseClickSelectsRow(t *testing.T) {
	m := newTuiModel("http://localhost")
	m.currentView = tuiViewTasks
	m.width = 140
	m.height = 24
	m.fixJobs = []storage.ReviewJob{
		{ID: 101, Status: storage.JobStatusQueued},
		{ID: 102, Status: storage.JobStatusRunning},
		{ID: 103, Status: storage.JobStatusDone},
	}
	m.fixSelectedIdx = 0

	m2, _ := updateModel(t, m, mouseLeftClick(2, 4))
	assert.Equal(t, 1, m2.fixSelectedIdx)
}

func TestTUITasksMouseWheelScrollsSelection(t *testing.T) {
	m := newTuiModel("http://localhost")
	m.currentView = tuiViewTasks
	m.fixJobs = []storage.ReviewJob{
		{ID: 101, Status: storage.JobStatusQueued},
		{ID: 102, Status: storage.JobStatusRunning},
		{ID: 103, Status: storage.JobStatusDone},
	}
	m.fixSelectedIdx = 0

	m2, _ := updateModel(t, m, mouseWheelDown())
	assert.Equal(t, 1, m2.fixSelectedIdx)

	m3, _ := updateModel(t, m2, mouseWheelUp())
	assert.Equal(t, 0, m3.fixSelectedIdx)
}

func TestTUITasksParentShortcutOpensParentReview(t *testing.T) {
	parentID := int64(77)
	m := newTuiModel("http://localhost")
	m.currentView = tuiViewTasks
	m.fixJobs = []storage.ReviewJob{
		{ID: 101, Status: storage.JobStatusDone, ParentJobID: &parentID},
	}
	m.fixSelectedIdx = 0

	m2, cmd := pressKey(m, 'P')
	assert.NotNil(t, cmd, "expected fetchReview command for parent shortcut")
	assert.Equal(t, m2.selectedJobID, parentID)
	assert.Equal(t, tuiViewTasks, m2.reviewFromView)
}

func TestTUITasksParentShortcutWithoutParentShowsFlash(t *testing.T) {
	m := newTuiModel("http://localhost")
	m.currentView = tuiViewTasks
	m.fixJobs = []storage.ReviewJob{
		{ID: 101, Status: storage.JobStatusDone},
	}
	m.fixSelectedIdx = 0

	m2, cmd := pressKey(m, 'P')
	assert.Nil(t, cmd, "expected no command when task has no parent")
	assert.Equal(t, "No parent review for this task", m2.flashMessage)
	assert.Equal(t, tuiViewTasks, m2.flashView)
}

func TestTUITasksCtrlJFetchesReview(t *testing.T) {
	m := newTuiModel("http://localhost")
	m.currentView = tuiViewTasks
	m.fixJobs = []storage.ReviewJob{
		{ID: 101, Status: storage.JobStatusDone},
	}
	m.fixSelectedIdx = 0

	m2, cmd := pressCtrl(m, 'j')

	assert.EqualValues(t, 101, m2.selectedJobID)
	assert.Equal(t, tuiViewTasks, m2.reviewFromView)
	assert.NotNil(t, cmd, "expected fetchReview command for ctrl+j activation")
}

func TestTUITasksViewShowsQueuedColumn(t *testing.T) {
	enqueued := time.Date(2026, time.February, 25, 16, 42, 0, 0, time.Local)
	started := enqueued.Add(30 * time.Second)
	finished := started.Add(1 * time.Minute)
	parentID := int64(42)

	m := newTuiModel("http://localhost")
	m.width = 140
	m.height = 24
	m.fixJobs = []storage.ReviewJob{
		{
			ID:            1001,
			Status:        storage.JobStatusDone,
			ParentJobID:   &parentID,
			RepoName:      "repo-alpha",
			Branch:        "feature/tasks-view",
			GitRef:        "abc1234",
			CommitSubject: "Fix flaky task tests",
			EnqueuedAt:    enqueued,
			StartedAt:     &started,
			FinishedAt:    &finished,
		},
	}

	out := stripANSI(m.renderTasksView())
	assert.Contains(t, out, "Queued")
	assert.Contains(t, out, "Elapsed")
	assert.False(t, !strings.Contains(out, "Branch") || !strings.Contains(out, "Repo"))
	assert.Contains(t, out, "Ref/Subject")
	assert.Contains(t, out, enqueued.Format("Jan 02 15:04"))
	assert.Contains(t, out, "1m0s")
	assert.False(t, !strings.Contains(out, "feature/task") || !strings.Contains(out, "repo-alpha"))
	assert.Contains(t, out, "abc1234 Fix flaky task tests")
}

func TestTUIQueueNavigationBoundaries(t *testing.T) {
	m := newQueueTestModel(
		withQueueTestJobs(makeJob(1), makeJob(2), makeJob(3)),
		withQueueTestSelection(0),
		withQueueTestFlags(false, false, false),
	)

	m2, _ := pressSpecial(m, tea.KeyUp)

	assert.Equal(t, 0, m2.selectedIdx)
	assertFlashMessage(t, m2, viewQueue, "No newer review")

	m.selectedIdx = 2
	m.selectedJobID = 3
	m.flashMessage = ""

	m3, _ := pressSpecial(m, tea.KeyDown)

	assert.Equal(t, 2, m3.selectedIdx)
	assertFlashMessage(t, m3, viewQueue, "No older review")
}

func TestTUIQueueNavigationBoundariesWithMultiRepoFilter(t *testing.T) {
	m := newQueueTestModel(
		withQueueTestJobs(makeJob(1, withRepoPath("/repo1")), makeJob(2, withRepoPath("/repo2"))),
		withQueueTestSelection(1),
		withQueueTestFlags(true, false, false),
	)
	m.activeRepoFilter = []string{"/repo1", "/repo2"}

	m2, cmd := pressSpecial(m, tea.KeyDown)

	assert.True(t, m2.loadingMore)
	assert.NotNil(t, cmd)
}

func TestTUINavigateDownTriggersLoadMore(t *testing.T) {
	m := newQueueTestModel(
		withQueueTestJobs(makeJob(1)),
		withQueueTestSelection(0),
		withQueueTestFlags(true, false, false),
	)

	m2, cmd := pressSpecial(m, tea.KeyDown)

	assert.True(t, m2.loadingMore)
	assert.NotNil(t, cmd)
}

func TestTUINavigateDownLoadsMoreWhenMultiRepoFiltered(t *testing.T) {
	m := newQueueTestModel(
		withQueueTestJobs(makeJob(1, withRepoPath("/path/to/repo"))),
		withQueueTestSelection(0),
		withQueueTestFlags(true, false, false),
	)
	m.activeRepoFilter = []string{"/path/to/repo", "/path/to/repo2"}

	m2, cmd := pressSpecial(m, tea.KeyDown)

	assert.True(t, m2.loadingMore)
	assert.NotNil(t, cmd)
}

func TestTUIJobCellsContent(t *testing.T) {
	m := model{width: 200}

	t.Run("basic cell values", func(t *testing.T) {
		job := makeJob(1,
			withRef("abc1234"),
			withRepoName("myrepo"),
			withAgent("test"),
			withEnqueuedAt(time.Now()),
		)
		cells := m.jobCells(job)

		assert.Contains(t, cells[0], "abc1234")
		assert.Equal(t, "myrepo", cells[colRepo-colRef])
		assert.Equal(t, "test", cells[colAgent-colRef])
		assert.Equal(t, "default", cells[colReviewType-colRef])
		assert.Equal(t, "Done", cells[colStatus-colRef])
		assert.Equal(t, "-", cells[colPF-colRef])
	})

	t.Run("claude-code normalizes to claude", func(t *testing.T) {
		job := makeJob(1, withAgent("claude-code"))
		cells := m.jobCells(job)
		assert.Equal(t, "claude", cells[colAgent-colRef])
	})

	t.Run("reasoning has its own column", func(t *testing.T) {
		job := makeJob(1)
		job.RequestedModel = "gpt-5.5"
		job.Reasoning = "xhigh"
		cells := m.jobCells(job)
		assert.Equal(t, "gpt-5.5", cells[colRequestedModel-colRef])

		assert.Equal(t, "xhigh", cells[colReasoning-colRef])

		job.Reasoning = ""
		cells = m.jobCells(job)
		assert.Equal(t, "gpt-5.5", cells[colRequestedModel-colRef])
		assert.Equal(t, "—", cells[colReasoning-colRef])
	})

	t.Run("verdict and handled values", func(t *testing.T) {
		pass := "P"
		handled := true
		job := makeJob(1)
		job.Verdict = &pass
		job.Closed = &handled

		cells := m.jobCells(job)
		assert.Equal(t, "Done", cells[colStatus-colRef])
		assert.Equal(t, "P", cells[colPF-colRef])
		assert.Equal(t, "yes", cells[colHandled-colRef])
	})

	t.Run("panel member handled value is blank", func(t *testing.T) {
		member := makeJob(11, withPanelMember("R", "security", 1), withClosed(new(false)))
		cells := m.jobCells(member)
		assert.Empty(t, cells[colHandled-colRef])
	})

	t.Run("panel parent elapsed uses panel wall clock when members are cached", func(t *testing.T) {
		elapsedIdx := colElapsed - colRef
		firstMemberStarted := time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)
		synthesisStarted := firstMemberStarted.Add(9 * time.Minute)
		synthesisFinished := firstMemberStarted.Add(11 * time.Minute)
		parent := makeJob(10,
			withSynthesis("R", storage.PanelSummary{MembersTotal: 2, MembersTerminal: 2}),
			withStartedAt(synthesisStarted),
			withFinishedAt(&synthesisFinished),
		)
		memberA := makeJob(11,
			withPanelMember("R", "default", 0),
			withStartedAt(firstMemberStarted),
		)
		memberB := makeJob(12,
			withPanelMember("R", "security", 1),
			withStartedAt(firstMemberStarted.Add(2*time.Minute)),
		)
		m.panelMembers = map[uuid.UUID][]storage.ReviewJob{testUUID("R"): {memberA, memberB}}

		cells := m.jobCells(parent)

		assert.Equal(t, "11m0s", cells[elapsedIdx])
	})

	t.Run("panel parent elapsed uses panel summary before members are cached", func(t *testing.T) {
		elapsedIdx := colElapsed - colRef
		firstMemberStarted := time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)
		synthesisStarted := firstMemberStarted.Add(9 * time.Minute)
		synthesisFinished := firstMemberStarted.Add(11 * time.Minute)
		parent := makeJob(10,
			withSynthesis("R", storage.PanelSummary{
				MembersTotal:    2,
				MembersTerminal: 2,
				FirstStartedAt:  &firstMemberStarted,
			}),
			withStartedAt(synthesisStarted),
			withFinishedAt(&synthesisFinished),
		)
		m.panelMembers = nil

		cells := m.jobCells(parent)

		assert.Equal(t, "11m0s", cells[elapsedIdx])
	})
}

func TestTUIJobCellsReviewTypeColumn(t *testing.T) {
	m := model{width: 80}

	tests := []struct {
		reviewType string
		wantRef    string
		want       string
	}{
		{"", "abc1234", "default"},
		{"default", "abc1234", "default"},
		{"general", "abc1234", "default"},
		{"review", "abc1234", "default"},
		{"security", "abc1234 [security]", "security"},
		{"design", "abc1234 [design]", "design"},
		{"project-conventions", "abc1234 [project-conventions]", "project-conventions"},
	}

	for _, tc := range tests {
		t.Run(tc.reviewType, func(t *testing.T) {
			job := makeJob(1, withRef("abc1234"), withReviewType(tc.reviewType))
			cells := m.jobCells(job)
			assert.Equal(t, tc.wantRef, cells[0])
			assert.Equal(t, tc.want, cells[colReviewType-colRef])
		})
	}

	panel := makeJob(2, withSynthesis("run-2", storage.PanelSummary{MembersTotal: 2}))
	assert.Equal(t, "panel", m.jobCells(panel)[colReviewType-colRef])
}

func TestTUIJobCellsCost(t *testing.T) {
	m := model{width: 200}
	// cells[k] maps to logical column colRef+k (see jobCells copy),
	// so the cost cell is at colCost-colRef.
	costIdx := colCost - colRef

	t.Run("priced cost renders", func(t *testing.T) {
		job := makeJob(1)
		job.TokenUsage = `{"total_output_tokens":28800,` +
			`"peak_context_tokens":118000,"cost_usd":0.42,"has_cost":true}`
		cells := m.jobCells(job)
		assert.Equal(t, "~$0.42", cells[costIdx])
	})

	t.Run("no usage blank", func(t *testing.T) {
		cells := m.jobCells(makeJob(1))
		assert.Empty(t, cells[costIdx])
	})

	t.Run("unpriced tokens blank", func(t *testing.T) {
		job := makeJob(1)
		job.TokenUsage = `{"total_output_tokens":28800,` +
			`"peak_context_tokens":118000,"has_cost":false}`
		cells := m.jobCells(job)
		assert.Empty(t, cells[costIdx])
	})

	t.Run("panel parent sums priced member costs", func(t *testing.T) {
		parent := makeJob(10, withSynthesis("R", storage.PanelSummary{
			MembersTotal: 2, MembersTerminal: 2, MembersSucceeded: 2,
		}))
		memberA := makeJob(11, withPanelMember("R", "default", 0))
		memberA.TokenUsage = `{"cost_usd":0.10,"has_cost":true}`
		memberB := makeJob(12, withPanelMember("R", "security", 1))
		memberB.TokenUsage = `{"cost_usd":0.25,"has_cost":true}`
		m.panelMembers = map[uuid.UUID][]storage.ReviewJob{testUUID("R"): {memberA, memberB}}

		cells := m.jobCells(parent)
		assert.Equal(t, "~$0.35", cells[costIdx])
	})
}

func TestTUIQueueShowsCostColumnByDefault(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.width = 200
	m.height = 30
	job := makeJob(1, withRef("abc1234"),
		withRepoName("repo"), withAgent("test"))
	job.TokenUsage = `{"total_output_tokens":28800,` +
		`"peak_context_tokens":118000,"cost_usd":0.42,"has_cost":true}`
	m.jobs = []storage.ReviewJob{job}
	m.selectedIdx = 0
	m.selectedJobID = 1

	out := stripTestANSI(m.renderQueueView())
	assert.Contains(t, out, "Cost",
		"Cost header should be visible by default")
	assert.Contains(t, out, "~$0.42",
		"cost value should render in the row")
}

func TestTUIQueueShowsReviewTypeColumnByDefault(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.width = 200
	m.height = 30
	m.jobs = []storage.ReviewJob{
		makeJob(2, withRef("def5678"), withReviewType("project-conventions")),
		makeJob(1, withRef("abc1234")),
	}
	m.selectedIdx = 0
	m.selectedJobID = 2

	out := stripTestANSI(m.renderQueueView())

	assert.Contains(t, out, "Review Type")
	assert.Contains(t, out, "project-conventions")
	assert.Contains(t, out, "default")
	assert.Contains(t, out, "def5678 [project-conventions]")
}

func TestTUIQueueKeepsIdentifyingColumnsAt80Characters(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.width = 80
	m.height = 30
	m.jobs = []storage.ReviewJob{
		makeJob(1,
			withRef("abc1234"),
			withBranch("feature"),
			withRepoName("myrepo"),
			withReviewType("security"),
		),
	}
	m.selectedIdx = 0
	m.selectedJobID = 1

	out := stripTestANSI(m.renderQueueView())

	assert.Contains(t, out, "Review Type")
	assert.Contains(t, out, "abc1234")
	assert.Contains(t, out, "feature")
	assert.Contains(t, out, "myrepo")
	assert.Contains(t, out, "security")
}

func TestTUIQueuePanelParentRendersPanelElapsedTime(t *testing.T) {
	firstMemberStarted := time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)
	synthesisStarted := firstMemberStarted.Add(9 * time.Minute)
	synthesisFinished := firstMemberStarted.Add(11 * time.Minute)
	parent := makeJob(10,
		withRef("syn1234"),
		withRepoName("repo"),
		withAgent("test"),
		withSynthesis("R", storage.PanelSummary{
			MembersTotal:    2,
			MembersTerminal: 2,
			FirstStartedAt:  &firstMemberStarted,
		}),
		withStartedAt(synthesisStarted),
		withFinishedAt(&synthesisFinished),
	)

	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.width = 200
	m.height = 30
	m.jobs = []storage.ReviewJob{parent}
	m.selectedIdx = 0
	m.selectedJobID = parent.ID

	out := stripTestANSI(m.renderQueueView())
	assert.Contains(t, out, "11m0s")
	assert.NotContains(t, out, "2m0s")
}

func TestTUIQueueHeaderShowsPausedQueueState(t *testing.T) {
	assert := assert.New(t)
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.width = 200
	m.height = 30
	m.status = storage.DaemonStatus{Version: "test", ActiveWorkers: 0, MaxWorkers: 4, QueuePaused: true}

	out := stripTestANSI(m.renderQueueView())
	assert.Contains(out, "[PAUSED]", "paused queue should show a badge on the daemon status line")
	assert.Contains(out, "Workers: 0/4", "worker counts should still render alongside the badge")
}

func TestTUIQueueHeaderShowsPausedBadgeWhenFiltered(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.width = 200
	m.height = 30
	m.activeBranchFilter = "main"
	m.status = storage.DaemonStatus{Version: "test", QueuePaused: true}

	out := stripTestANSI(m.renderQueueView())
	assert.Contains(t, out, "[PAUSED]",
		"pause is a global daemon state and must show even under an active filter")
}

func TestTUIQueueCompactShowsPausedBadge(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.width = 200
	m.height = 10 // height < 15 triggers compact mode
	m.status = storage.DaemonStatus{Version: "test", QueuePaused: true}

	out := stripTestANSI(m.renderQueueView())
	assert.Contains(t, out, "[PAUSED]",
		"compact mode hides the status area but must still surface a paused queue")
}

func TestTUITogglePauseKeyFlipsBadgeOptimistically(t *testing.T) {
	assert := assert.New(t)
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewQueue
	require.False(t, m.status.QueuePaused)

	updated, cmd := m.handleTogglePauseKey()
	m = updated.(model)
	assert.True(m.status.QueuePaused, "pressing P should optimistically pause the queue")
	assert.NotNil(cmd, "pressing P should issue a pause request")

	updated, cmd = m.handleTogglePauseKey()
	m = updated.(model)
	assert.False(m.status.QueuePaused, "pressing P again should resume the queue")
	assert.NotNil(cmd, "pressing P again should issue a resume request")
}

func TestTUIPauseResultRollsBackOnError(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewQueue
	m.status.QueuePaused = true // optimistic pause already applied

	updated, _ := m.handlePauseResultMsg(pauseResultMsg{paused: true, err: errors.New("boom")})
	m = updated.(model)
	assert.False(t, m.status.QueuePaused,
		"a failed pause request should roll the badge back to running")
}

func TestTUIQueueHelpShowsPauseToggleLabel(t *testing.T) {
	assert := assert.New(t)
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.width = 200
	m.height = 30

	assert.Contains(stripTestANSI(m.renderQueueView()), "pause",
		"running queue should offer a pause action in the help footer")

	m.status.QueuePaused = true
	assert.Contains(stripTestANSI(m.renderQueueView()), "resume",
		"paused queue should offer a resume action in the help footer")
}

func TestTUIQueueCollapsedPanelShowsAggregatedMemberCost(t *testing.T) {
	parent := makeJob(10, withRef("syn"), withStatus(storage.JobStatusDone),
		withSynthesis("R", storage.PanelSummary{MembersTotal: 2, MembersTerminal: 2, MembersSucceeded: 2}))
	memberA := makeJob(11, withPanelMember("R", "default", 0), withStatus(storage.JobStatusDone))
	memberA.TokenUsage = `{"cost_usd":0.10,"has_cost":true}`
	memberB := makeJob(12, withPanelMember("R", "security", 1), withStatus(storage.JobStatusDone))
	memberB.TokenUsage = `{"cost_usd":0.25,"has_cost":true}`

	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.width = 200
	m.height = 30
	m.jobs = []storage.ReviewJob{parent}
	m.panelMembers = map[uuid.UUID][]storage.ReviewJob{testUUID("R"): {memberA, memberB}}
	m.selectedIdx = 0
	m.selectedJobID = parent.ID

	out := stripTestANSI(m.renderQueueView())
	assert.False(t, m.expandedPanels[testUUID("R")], "panel remains collapsed")
	assert.Contains(t, out, "~$0.35", "collapsed parent row shows summed member costs")
}

func TestTUIQueueCostCellDistinguishesMissingAmountFromZero(t *testing.T) {
	m := model{}

	drifted := storage.ReviewJob{
		TokenUsage: `{"total_output_tokens":200,"has_cost":true}`,
	}
	assert.Empty(t, m.jobCostCell(drifted),
		"a cost flag without an amount must not render as a free run")

	free := storage.ReviewJob{
		TokenUsage: `{"cost_usd":0,"has_cost":true}`,
	}
	assert.Equal(t, "~$0.00", m.jobCostCell(free),
		"an explicit zero remains a priced free run")
}

func TestTUIQueueCollapsedPanelShowsSummaryCostBeforeExpansion(t *testing.T) {
	parent := makeJob(10, withRef("syn"), withStatus(storage.JobStatusDone),
		withSynthesis("R", storage.PanelSummary{
			MembersTotal:        2,
			MembersTerminal:     2,
			MembersSucceeded:    2,
			MembersCostUSD:      0.35,
			MembersCostComplete: true,
		}))
	parent.TokenUsage = `{"cost_usd":0.05,"has_cost":true}`

	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.width = 200
	m.height = 30
	m.jobs = []storage.ReviewJob{parent}
	m.panelMembers = map[uuid.UUID][]storage.ReviewJob{}
	m.selectedIdx = 0
	m.selectedJobID = parent.ID

	out := stripTestANSI(m.renderQueueView())
	assert.False(t, m.expandedPanels[testUUID("R")], "panel remains collapsed")
	assert.Contains(t, out, "~$0.40", "collapsed parent row uses panel_summary cost before member fetch")
}

func TestTUIQueueCollapsedPanelShowsPartialSummaryCostBeforeExpansion(t *testing.T) {
	parent := makeJob(10, withRef("syn"), withStatus(storage.JobStatusDone),
		withSynthesis("R", storage.PanelSummary{
			MembersTotal:        2,
			MembersTerminal:     2,
			MembersSucceeded:    2,
			MembersWithCost:     1,
			MembersCostUSD:      0.35,
			MembersCostComplete: false,
		}))

	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.width = 200
	m.height = 30
	m.jobs = []storage.ReviewJob{parent}
	m.panelMembers = map[uuid.UUID][]storage.ReviewJob{}
	m.selectedIdx = 0
	m.selectedJobID = parent.ID

	out := stripTestANSI(m.renderQueueView())
	assert.False(t, m.expandedPanels[testUUID("R")], "panel remains collapsed")
	assert.Contains(t, out, "~$0.35", "collapsed parent row shows known member costs even when partial")
}

func TestTUIQueueCollapsedPanelUsesSummaryCostWhenMemberCacheStale(t *testing.T) {
	parent := makeJob(10, withRef("syn"), withStatus(storage.JobStatusDone),
		withSynthesis("R", storage.PanelSummary{
			MembersTotal:        2,
			MembersTerminal:     2,
			MembersSucceeded:    2,
			MembersCostUSD:      0.35,
			MembersCostComplete: true,
		}))
	parent.TokenUsage = `{"cost_usd":0.05,"has_cost":true}`
	memberA := makeJob(11, withPanelMember("R", "default", 0), withStatus(storage.JobStatusDone))
	memberB := makeJob(12, withPanelMember("R", "security", 1), withStatus(storage.JobStatusDone))

	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.width = 200
	m.height = 30
	m.jobs = []storage.ReviewJob{parent}
	m.panelMembers = map[uuid.UUID][]storage.ReviewJob{testUUID("R"): {memberA, memberB}}
	m.selectedIdx = 0
	m.selectedJobID = parent.ID

	out := stripTestANSI(m.renderQueueView())
	assert.False(t, m.expandedPanels[testUUID("R")], "panel remains collapsed")
	assert.Contains(t, out, "~$0.40", "complete panel_summary cost wins over stale cached members")
}

func TestTUIQueueCollapsedPanelShowsPartialCachedMemberCost(t *testing.T) {
	parent := makeJob(10, withRef("syn"), withStatus(storage.JobStatusDone),
		withSynthesis("R", storage.PanelSummary{MembersTotal: 2, MembersTerminal: 2, MembersSucceeded: 2}))
	memberA := makeJob(11, withPanelMember("R", "default", 0), withStatus(storage.JobStatusDone))
	memberA.TokenUsage = `{"cost_usd":0.10,"has_cost":true}`
	memberB := makeJob(12, withPanelMember("R", "security", 1), withStatus(storage.JobStatusDone))
	memberB.TokenUsage = `{"has_cost":false}`

	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.width = 200
	m.height = 30
	m.jobs = []storage.ReviewJob{parent}
	m.panelMembers = map[uuid.UUID][]storage.ReviewJob{testUUID("R"): {memberA, memberB}}
	m.selectedIdx = 0
	m.selectedJobID = parent.ID

	out := stripTestANSI(m.renderQueueView())
	assert.False(t, m.expandedPanels[testUUID("R")], "panel remains collapsed")
	assert.Contains(t, out, "~$0.10", "collapsed parent row shows known cached member costs even when partial")
}

func TestTUIQueueTableRendersWithinWidth(t *testing.T) {
	widths := []int{80, 100, 120, 200}
	for _, w := range widths {
		t.Run(fmt.Sprintf("width=%d", w), func(t *testing.T) {
			m := newModel(localhostEndpoint, withExternalIODisabled())
			m.width = w
			m.height = 30
			m.jobs = []storage.ReviewJob{
				makeJob(1, withRef("abc1234"), withRepoName("myrepo"), withAgent("test")),
				makeJob(2, withRef("def5678"), withRepoName("other-repo"), withAgent("claude-code")),
			}
			m.selectedIdx = 0
			m.selectedJobID = 1

			output := m.renderQueueView()
			lines := strings.Split(output, "\n")

			tableEnd := min(len(lines), 4+1+1+len(m.jobs))
			for i := 0; i < tableEnd && i < len(lines); i++ {
				line := strings.ReplaceAll(lines[i], "\x1b[K", "")
				line = strings.ReplaceAll(line, "\x1b[J", "")
				visW := lipgloss.Width(line)
				if visW > w+5 {
					assert.LessOrEqual(t, visW, w+5, "line %d exceeds width %d: visW=%d %q", i, w, visW, stripTestANSI(line))
				}
			}
		})
	}
}

func TestStatusColumnAutoWidth(t *testing.T) {
	tests := []struct {
		name      string
		statuses  []storage.JobStatus
		wantWidth int
	}{
		{"done only", []storage.JobStatus{storage.JobStatusDone}, 6},
		{"queued only", []storage.JobStatus{storage.JobStatusQueued}, 6},
		{"running", []storage.JobStatus{storage.JobStatusRunning}, 7},
		{"canceled", []storage.JobStatus{storage.JobStatusCanceled}, 8},
		{"mixed done and error", []storage.JobStatus{storage.JobStatusDone, storage.JobStatusFailed}, 6},
		{"mixed done and canceled", []storage.JobStatus{storage.JobStatusDone, storage.JobStatusCanceled}, 8},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newModel(localhostEndpoint, withExternalIODisabled())
			m.width = 200
			m.height = 30

			jobs := make([]storage.ReviewJob, len(tt.statuses))
			for i, s := range tt.statuses {
				jobs[i] = makeJob(int64(i+1), withStatus(s), withRef("abc1234"), withRepoName("repo"), withAgent("test"))
			}
			m.jobs = jobs
			m.selectedIdx = 0
			m.selectedJobID = 1

			output := m.renderQueueView()
			lines := strings.Split(output, "\n")

			var headerLine string
			for _, line := range lines {
				stripped := stripTestANSI(line)
				if strings.Contains(stripped, "Status") && strings.Contains(stripped, "P/F") {
					headerLine = stripped
					break
				}
			}
			assert.NotEmpty(t, headerLine, "could not find header line with Status and P/F")

			statusIdx := strings.Index(headerLine, "Status")
			pfIdx := strings.Index(headerLine, "P/F")
			assert.False(t, statusIdx < 0 || pfIdx < 0 || pfIdx <= statusIdx)

			gap := pfIdx - statusIdx
			gotWidth := gap - 1
			assert.Equal(t, tt.wantWidth, gotWidth)
		})
	}
}

func TestTUIPaginationAppendMode(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())

	initialJobs := make([]storage.ReviewJob, 50)
	for i := range 50 {
		initialJobs[i] = makeJob(int64(50 - i))
	}
	m.jobs = initialJobs
	m.selectedIdx = 0
	m.selectedJobID = 50
	m.hasMore = true

	moreJobs := make([]storage.ReviewJob, 25)
	for i := range 25 {
		moreJobs[i] = makeJob(int64(i + 1))
	}
	appendMsg := jobsMsg{jobs: moreJobs, hasMore: false, append: true}

	m2, _ := updateModel(t, m, appendMsg)

	assert.Len(t, m2.jobs, 75)

	assert.False(t, m2.hasMore, "hasMore should be false after append with hasMore=false")

	assert.False(t, m2.loadingMore)

	assert.EqualValues(t, 50, m2.selectedJobID)
}

func TestTUIPaginationRefreshMaintainsView(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())

	jobs := make([]storage.ReviewJob, 100)
	for i := range 100 {
		jobs[i] = makeJob(int64(100 - i))
	}
	m.jobs = jobs
	m.selectedIdx = 50
	m.selectedJobID = 50

	refreshedJobs := make([]storage.ReviewJob, 100)
	for i := range 100 {
		refreshedJobs[i] = makeJob(int64(101 - i))
	}
	refreshMsg := jobsMsg{jobs: refreshedJobs, hasMore: true, append: false}

	m2, _ := updateModel(t, m, refreshMsg)

	assert.Len(t, m2.jobs, 100)

	assert.EqualValues(t, 50, m2.selectedJobID)
	assert.Equal(t, 51, m2.selectedIdx)
}

func TestTUILoadingMoreClearedOnPaginationError(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.loadingMore = true

	errMsg := paginationErrMsg{err: fmt.Errorf("network error")}
	m2, _ := updateModel(t, m, errMsg)

	assert.False(t, m2.loadingMore)

	require.Error(t, m2.err)
}

func TestTUILoadingMoreNotClearedOnGenericError(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.loadingMore = true

	errMsg := errMsg(fmt.Errorf("some other error"))
	m2, _ := updateModel(t, m, errMsg)

	assert.True(t, m2.loadingMore)

	require.Error(t, m2.err)
}

func TestTUIPaginationBlockedWhileLoadingJobs(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewQueue
	m.loadingJobs = true
	m.hasMore = true
	m.loadingMore = false

	m.jobs = []storage.ReviewJob{makeJob(1)}
	m.selectedIdx = 0
	m.selectedJobID = 1

	m2, cmd := pressKey(m, 'j')

	assert.False(t, m2.loadingMore)
	assert.Nil(t, cmd)
}

func TestTUIPaginationAllowedWhenNotLoadingJobs(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewQueue
	m.loadingJobs = false
	m.hasMore = true
	m.loadingMore = false

	m.jobs = []storage.ReviewJob{makeJob(1)}
	m.selectedIdx = 0
	m.selectedJobID = 1

	m2, cmd := pressKey(m, 'j')

	assert.True(t, m2.loadingMore)
	assert.NotNil(t, cmd)
}

func TestTUIPaginationAllowedForMultiRepoFilter(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewQueue
	m.loadingJobs = false
	m.hasMore = true
	m.loadingMore = false
	m.activeRepoFilter = []string{"/repo/a", "/repo/b"}

	m.jobs = []storage.ReviewJob{
		makeJob(1, withRepoPath("/repo/a")),
	}
	m.selectedIdx = 0
	m.selectedJobID = 1

	m2, cmd := pressKey(m, 'j')

	assert.True(t, m2.loadingMore)
	assert.NotNil(t, cmd)
}

func TestTUIPaginationBlockedForNoneBranchFilter(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewQueue
	m.loadingJobs = false
	m.hasMore = true
	m.loadingMore = false
	m.activeBranchFilter = branchNone

	m.jobs = []storage.ReviewJob{makeJob(1)}
	m.selectedIdx = 0
	m.selectedJobID = 1

	m2, cmd := pressKey(m, 'j')

	assert.False(t, m2.loadingMore)
	assert.Nil(t, cmd)
}

func TestTUIPageDownBlockedWhileLoadingJobs(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewQueue
	m.loadingJobs = true
	m.hasMore = true
	m.loadingMore = false
	m.height = 30

	m.jobs = []storage.ReviewJob{makeJob(1)}
	m.selectedIdx = 0
	m.selectedJobID = 1

	m2, cmd := pressSpecial(m, tea.KeyPgDown)

	assert.False(t, m2.loadingMore)
	assert.Nil(t, cmd)
}

func TestTUIPageUpDownMovesSelection(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewQueue
	m.hideClosed = true
	m.height = 15

	m.jobs = []storage.ReviewJob{
		makeJob(1),
		makeJob(2),
		makeJob(3),
		makeJob(4),
		makeJob(5),
		makeJob(6, withStatus(storage.JobStatusCanceled)),
		makeJob(7),
		makeJob(8),
		makeJob(9),
		makeJob(10),
		makeJob(11),
	}
	m.selectedIdx = 0
	m.selectedJobID = 1

	m2, _ := pressSpecial(m, tea.KeyPgDown)
	assert.Equal(t, 6, m2.selectedIdx,

		"pgdown: expected selectedIdx=6 (skipped hidden idx 5), got %d",
		m2.selectedIdx)

	assert.EqualValues(t, 7, m2.selectedJobID)

	m3, _ := pressSpecial(m2, tea.KeyPgUp)
	assert.Equal(t, 0, m3.selectedIdx,

		"pgup: expected selectedIdx=0 (back to newest), got %d",
		m3.selectedIdx)

	assert.EqualValues(t, 1, m3.selectedJobID)
}

func TestTUIResizeBehavior(t *testing.T) {
	tests := []struct {
		name                      string
		initialHeight             int
		jobsCount                 int
		loadingJobs               bool
		loadingMore               bool
		activeFilters             []string
		msg                       tea.WindowSizeMsg
		wantCmd                   bool
		wantLoading               bool
		checkRefetchOnLaterResize bool
	}{
		{
			name:          "During Pagination No Refetch",
			initialHeight: 20,
			jobsCount:     3,
			loadingJobs:   false,
			loadingMore:   true,
			msg:           tea.WindowSizeMsg{Height: 40},
			wantCmd:       false,
			wantLoading:   false,
		},
		{
			name:          "Triggers Refetch When Needed",
			initialHeight: 20,
			jobsCount:     3,
			loadingJobs:   false,
			loadingMore:   false,
			msg:           tea.WindowSizeMsg{Height: 40},
			wantCmd:       true,
			wantLoading:   true,
		},
		{
			name:          "No Refetch When Enough Jobs",
			initialHeight: 20,
			jobsCount:     100,
			loadingJobs:   false,
			loadingMore:   false,
			msg:           tea.WindowSizeMsg{Height: 40},
			wantCmd:       false,
			wantLoading:   false,
		},
		{
			name:                      "Refetch On Later Resize",
			initialHeight:             20,
			jobsCount:                 25,
			loadingJobs:               false,
			loadingMore:               false,
			msg:                       tea.WindowSizeMsg{Height: 20},
			wantCmd:                   false,
			wantLoading:               false,
			checkRefetchOnLaterResize: true,
		},
		{
			name:          "No Refetch While Loading Jobs",
			initialHeight: 20,
			jobsCount:     3,
			loadingJobs:   true,
			loadingMore:   false,
			msg:           tea.WindowSizeMsg{Height: 40},
			wantCmd:       false,
			wantLoading:   true,
		},
		{
			name:          "Refetch Multi-Repo Filter Active",
			initialHeight: 20,
			jobsCount:     3,
			loadingJobs:   false,
			loadingMore:   false,
			activeFilters: []string{"/repo1", "/repo2"},
			msg:           tea.WindowSizeMsg{Height: 40},
			wantCmd:       true,
			wantLoading:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			jobs := make([]storage.ReviewJob, tt.jobsCount)
			for i := 0; i < tt.jobsCount; i++ {
				jobs[i] = makeJob(int64(i + 1))
			}

			m := newQueueTestModel(
				withQueueTestJobs(jobs...),
				withQueueTestFlags(true, tt.loadingMore, tt.loadingJobs),
			)
			m.activeRepoFilter = tt.activeFilters
			m.height = tt.initialHeight
			m.heightDetected = false

			var cmd tea.Cmd

			if tt.checkRefetchOnLaterResize {
				m, cmd = updateModel(t, m, tt.msg)

				assert.Nil(t, cmd, "Expected no fetch command on first resize, got one")
				assert.Equal(t, m.height, tt.msg.Height)
				assert.True(t, m.heightDetected)

				m, cmd = updateModel(t, m, tea.WindowSizeMsg{Height: 40})

				assert.NotNil(t, cmd, "Expected fetch command on second resize, got nil")
				assert.Equal(t, 40, m.height)
				assert.True(t, m.loadingJobs)
				return
			} else {
				m, cmd = updateModel(t, m, tt.msg)
			}

			assert.Equal(t, tt.wantCmd, cmd != nil, "fetch command mismatch")
			assert.Equal(t, m.height, tt.msg.Height)
			assert.True(t, m.heightDetected)
			assert.Equal(t, tt.wantLoading, m.loadingJobs)
		})
	}
}

func TestTUIJobsMsgHideClosedUnderfilledViewportAutoPaginates(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewQueue
	m.hideClosed = true
	m.height = 29
	m.loadingJobs = true

	jobs := make([]storage.ReviewJob, 0, 25)
	var id int64 = 200
	for range 13 {
		jobs = append(jobs, makeJob(id, withStatus(storage.JobStatusDone), withClosed(new(false))))
		id--
	}
	for range 12 {
		jobs = append(jobs, makeJob(id, withStatus(storage.JobStatusFailed)))
		id--
	}

	m2, cmd := updateModel(t, m, jobsMsg{
		jobs:    jobs,
		hasMore: true,
		append:  false,
	})

	assert.Len(t, m2.getVisibleJobs(), 13)
	assert.True(t, m2.loadingMore)
	assert.NotNil(t, cmd)
}

func TestTUIJobsMsgHideClosedFilledViewportDoesNotAutoPaginate(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewQueue
	m.hideClosed = true
	m.height = 29
	m.loadingJobs = true

	jobs := make([]storage.ReviewJob, 0, 26)
	var id int64 = 300
	for range 21 {
		jobs = append(jobs, makeJob(id, withStatus(storage.JobStatusDone), withClosed(new(false))))
		id--
	}
	for range 5 {
		jobs = append(jobs, makeJob(id, withStatus(storage.JobStatusFailed)))
		id--
	}

	m2, cmd := updateModel(t, m, jobsMsg{
		jobs:    jobs,
		hasMore: true,
		append:  false,
	})

	assert.GreaterOrEqual(t, len(m2.getVisibleJobs()), 21)
	assert.False(t, m2.loadingMore)
	assert.Nil(t, cmd)
}

func TestTUIEmptyQueueRendersPaddedHeight(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.width = 100
	m.height = 20
	m.jobs = []storage.ReviewJob{}
	m.loadingJobs = false

	output := m.View().Content

	lines := strings.Split(output, "\n")

	assert.GreaterOrEqual(t, len(lines), m.height-3)

	assert.Contains(t, output, "No jobs in queue", "Expected 'No jobs in queue' message in output")
}

func TestTUIEmptyQueueWithFilterRendersPaddedHeight(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.width = 100
	m.height = 20
	m.jobs = []storage.ReviewJob{}
	m.activeRepoFilter = []string{"/some/repo"}
	m.loadingJobs = false

	output := m.View().Content

	lines := strings.Split(output, "\n")
	assert.GreaterOrEqual(t, len(lines), m.height-3)

	assert.Contains(t, output, "No jobs matching filters", "Expected 'No jobs matching filters' message in output")
}

func TestTUILoadingJobsShowsLoadingMessage(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.width = 100
	m.height = 20
	m.jobs = []storage.ReviewJob{}
	m.loadingJobs = true

	output := m.View().Content

	assert.Contains(t, output, "Loading...")
	assert.NotContains(t, output, "No jobs in queue")
	assert.NotContains(t, output, "No jobs matching filters")
}

func TestTUILoadingShowsForLoadingMore(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.width = 100
	m.height = 20
	m.jobs = []storage.ReviewJob{}
	m.loadingJobs = false
	m.loadingMore = true

	output := m.View().Content

	assert.Contains(t, output, "Loading...", "Expected 'Loading...' message when loadingMore is true")
}

func TestTUIQueueNoScrollIndicatorPads(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.width = 100
	m.height = 30

	m.jobs = []storage.ReviewJob{
		makeJob(1, withRef("abc123"), withAgent("test")),
		makeJob(2, withRef("def456"), withAgent("test")),
	}

	output := m.View().Content

	lines := strings.Split(output, "\n")

	assert.GreaterOrEqual(t, len(lines), m.height-5)
}

func setupQueue(jobs []storage.ReviewJob, selectedIdx int) model {
	m := newQueueTestModel(
		withQueueTestJobs(jobs...),
		withQueueTestSelection(selectedIdx),
	)
	return m
}

func TestTUIJobClosedTransitions(t *testing.T) {
	tests := []struct {
		name             string
		initialJobs      []storage.ReviewJob
		initialPending   map[int64]pendingState
		msg              tea.Msg
		wantPending      bool
		wantPendingState *bool
		wantClosed       *bool
		wantError        bool
	}{
		{
			name:           "Late error ignored (same state, diff seq)",
			initialJobs:    []storage.ReviewJob{makeJob(1, withClosed(new(false)))},
			initialPending: map[int64]pendingState{1: {newState: true, seq: 3}},
			msg: closedResultMsg{
				jobID: 1, oldState: false, newState: true, seq: 1,
				err: fmt.Errorf("late error"),
			},
			wantPending:      true,
			wantPendingState: new(true),
			wantClosed:       new(true),
			wantError:        false,
		},
		{
			name:           "Stale error response ignored",
			initialJobs:    []storage.ReviewJob{makeJob(1, withClosed(new(true)))},
			initialPending: map[int64]pendingState{1: {newState: true, seq: 1}},
			msg: closedResultMsg{
				jobID: 1, oldState: true, newState: false, seq: 0,
				err: fmt.Errorf("network error"),
			},
			wantPending:      true,
			wantPendingState: new(true),
			wantClosed:       new(true),
			wantError:        false,
		},
		{
			name:           "Cleared when server nil matches pending false",
			initialJobs:    []storage.ReviewJob{makeJob(1)},
			initialPending: map[int64]pendingState{1: {newState: false, seq: 1}},
			msg:            jobsMsg{jobs: []storage.ReviewJob{makeJob(1)}},
			wantPending:    false,
		},
		{
			name:           "Not cleared when server nil mismatches pending true",
			initialJobs:    []storage.ReviewJob{makeJob(1)},
			initialPending: map[int64]pendingState{1: {newState: true, seq: 1}},
			msg:            jobsMsg{jobs: []storage.ReviewJob{makeJob(1)}},
			wantPending:    true,
			wantClosed:     new(true),
		},
		{
			name:           "Not cleared by stale response (mismatched newState)",
			initialJobs:    []storage.ReviewJob{makeJob(1, withClosed(new(false)))},
			initialPending: map[int64]pendingState{1: {newState: true, seq: 1}},
			msg: closedResultMsg{
				jobID: 1, oldState: true, newState: false, seq: 0,
			},
			wantPending:      true,
			wantPendingState: new(true),
		},
		{
			name:           "Not cleared on success (waits for refresh)",
			initialJobs:    []storage.ReviewJob{makeJob(1, withClosed(new(false)))},
			initialPending: map[int64]pendingState{1: {newState: true, seq: 1}},
			msg: closedResultMsg{
				jobID: 1, oldState: false, newState: true, seq: 1,
			},
			wantPending: true,
		},
		{
			name:           "Cleared by jobs refresh",
			initialJobs:    []storage.ReviewJob{makeJob(1, withClosed(new(false)))},
			initialPending: map[int64]pendingState{1: {newState: true, seq: 1}},
			msg:            jobsMsg{jobs: []storage.ReviewJob{makeJob(1, withClosed(new(true)))}},
			wantPending:    false,
			wantClosed:     new(true),
		},
		{
			name:           "Not cleared by stale jobs refresh",
			initialJobs:    []storage.ReviewJob{makeJob(1, withClosed(new(false)))},
			initialPending: map[int64]pendingState{1: {newState: true, seq: 1}},
			msg:            jobsMsg{jobs: []storage.ReviewJob{makeJob(1, withClosed(new(false)))}},
			wantPending:    true,
			wantClosed:     new(true),
		},
		{
			name:           "Cleared on current error",
			initialJobs:    []storage.ReviewJob{makeJob(1, withClosed(new(true)))},
			initialPending: map[int64]pendingState{1: {newState: false, seq: 1}},
			msg: closedResultMsg{
				jobID: 1, oldState: true, newState: false, seq: 1,
				err: fmt.Errorf("server error"),
			},
			wantPending: false,
			wantClosed:  new(true),
			wantError:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := setupQueue(tt.initialJobs, 0)
			if tt.initialPending != nil {
				m.pendingClosed = make(map[int64]pendingState, len(tt.initialPending))
				maps.Copy(m.pendingClosed, tt.initialPending)
				for id, p := range tt.initialPending {
					for i := range m.jobs {
						if m.jobs[i].ID == id {
							val := p.newState
							m.jobs[i].Closed = &val
						}
					}
				}
			}

			m2, _ := updateModel(t, m, tt.msg)

			for id, p := range tt.initialPending {
				val, exists := m2.pendingClosed[id]
				assert.False(t, tt.wantPending && !exists)
				assert.False(t, !tt.wantPending && exists)
				if exists && tt.wantPendingState != nil {
					assert.Equal(t, *tt.wantPendingState, val.newState)
				}
				assert.False(t, exists && val.seq != p.seq)
			}

			if tt.wantClosed != nil && len(m2.jobs) > 0 {
				assert.NotNil(t, m2.jobs[0].Closed, "expected closed state to be set")
				assert.Equal(t, *tt.wantClosed, *m2.jobs[0].Closed)
			}

			if tt.wantError {
				require.Error(t, m2.err, "expected error, got nil")
			} else {
				require.NoError(t, m2.err, "unexpected error")
			}
		})
	}
}

func TestTUIReviewClosedTransitions(t *testing.T) {
	tests := []struct {
		name                 string
		initialReviewPending map[int64]pendingState
		msg                  tea.Msg
		wantPending          bool
	}{
		{
			name:                 "Pending review-only cleared on success",
			initialReviewPending: map[int64]pendingState{42: {newState: true, seq: 1}},
			msg: closedResultMsg{
				jobID: 0, reviewID: 42, reviewView: true, oldState: false, newState: true, seq: 1,
			},
			wantPending: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := setupQueue(nil, 0)
			if tt.initialReviewPending != nil {
				m.pendingReviewClosed = make(map[int64]pendingState, len(tt.initialReviewPending))
				maps.Copy(m.pendingReviewClosed, tt.initialReviewPending)
			}

			m2, _ := updateModel(t, m, tt.msg)

			for id := range tt.initialReviewPending {
				_, exists := m2.pendingReviewClosed[id]
				assert.False(t, tt.wantPending && !exists)
				assert.False(t, !tt.wantPending && exists)
			}
		})
	}
}

func TestTUIClosedHideClosedStats(t *testing.T) {
	tests := []struct {
		name           string
		initialJobs    []storage.ReviewJob
		initialPending map[int64]pendingState
		initialStats   storage.JobStats
		msg            tea.Msg
		wantPending    bool
		wantStats      *storage.JobStats
	}{
		{
			name:           "HideClosed stats not double counted",
			initialJobs:    []storage.ReviewJob{makeJob(1, withClosed(new(false)))},
			initialStats:   storage.JobStats{Done: 10, Closed: 6, Open: 4},
			initialPending: map[int64]pendingState{1: {newState: true, seq: 1}},
			msg: jobsMsg{
				jobs:  []storage.ReviewJob{},
				stats: storage.JobStats{Done: 10, Closed: 6, Open: 4},
			},
			wantPending: false,
			wantStats:   &storage.JobStats{Closed: 6, Open: 4},
		},
		{
			name:           "HideClosed pending not cleared when server lags",
			initialJobs:    []storage.ReviewJob{makeJob(1, withClosed(new(false)))},
			initialStats:   storage.JobStats{Done: 10, Closed: 6, Open: 4},
			initialPending: map[int64]pendingState{1: {newState: true, seq: 1}},
			msg: jobsMsg{
				jobs:  []storage.ReviewJob{makeJob(1, withClosed(new(false)))},
				stats: storage.JobStats{Done: 10, Closed: 5, Open: 5},
			},
			wantPending: true,
			wantStats:   &storage.JobStats{Closed: 6, Open: 4},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := setupQueue(tt.initialJobs, 0)
			m.hideClosed = true
			if tt.initialPending != nil {
				m.pendingClosed = make(map[int64]pendingState, len(tt.initialPending))
				maps.Copy(m.pendingClosed, tt.initialPending)
				for id, p := range tt.initialPending {
					for i := range m.jobs {
						if m.jobs[i].ID == id {
							val := p.newState
							m.jobs[i].Closed = &val
						}
					}
				}
			}
			m.jobStats = tt.initialStats

			m2, _ := updateModel(t, m, tt.msg)

			for id := range tt.initialPending {
				_, exists := m2.pendingClosed[id]
				assert.False(t, tt.wantPending && !exists)
				assert.False(t, !tt.wantPending && exists)
			}

			if tt.wantStats != nil {
				assert.Equal(t, m2.jobStats.Closed, tt.wantStats.Closed)
				assert.Equal(t, m2.jobStats.Open, tt.wantStats.Open)
			}
		})
	}
}

func TestTUIQueueNavigationSequences(t *testing.T) {
	threeJobs := []storage.ReviewJob{
		makeJob(1),
		makeJob(2),
		makeJob(3),
	}

	m := setupQueue(threeJobs, 0)

	m, _ = pressKey(m, 'j')
	assert.Equal(t, 1, m.selectedIdx)

	m, _ = pressKey(m, 'j')
	assert.Equal(t, 2, m.selectedIdx)

	m, _ = pressKey(m, 'k')
	assert.Equal(t, 1, m.selectedIdx)

	m, _ = pressKey(m, 'j')
	assert.Equal(t, 2, m.selectedIdx)

	m, _ = pressKey(m, 'g')
	assert.Equal(t, 0, m.selectedIdx)

	m, _ = pressSpecial(m, tea.KeyLeft)
	assert.Equal(t, 1, m.selectedIdx)

	m, _ = pressSpecial(m, tea.KeyRight)
	assert.Equal(t, 0, m.selectedIdx)
}

type queueTestModelOption func(*model)

func withQueueTestJobs(jobs ...storage.ReviewJob) queueTestModelOption {
	return func(m *model) {
		m.jobs = jobs
	}
}

func withQueueTestSelection(idx int) queueTestModelOption {
	return func(m *model) {
		m.selectedIdx = idx
		if len(m.jobs) > 0 && idx >= 0 && idx < len(m.jobs) {
			m.selectedJobID = m.jobs[idx].ID
		}
	}
}

func withQueueTestFlags(hasMore, loadingMore, loadingJobs bool) queueTestModelOption {
	return func(m *model) {
		m.hasMore = hasMore
		m.loadingMore = loadingMore
		m.loadingJobs = loadingJobs
	}
}

func newQueueTestModel(opts ...queueTestModelOption) model {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewQueue
	for _, opt := range opts {
		opt(&m)
	}
	return m
}

func assertFlashMessage(t *testing.T, m model, view viewKind, msg string) {
	t.Helper()
	assert.Equal(t, m.flashMessage, msg)
	assert.Equal(t, m.flashView, view)
	assert.False(t, m.flashExpiresAt.IsZero() || m.flashExpiresAt.Before(time.Now()))
}

func TestTUIQueueNarrowWidthFlexAllocation(t *testing.T) {
	for _, w := range []int{20, 30, 40} {
		t.Run(fmt.Sprintf("width=%d", w), func(t *testing.T) {
			m := newModel(localhostEndpoint, withExternalIODisabled())
			m.width = w
			m.height = 20
			m.jobs = []storage.ReviewJob{
				makeJob(1, withRef("abc"), withRepoName("r"), withAgent("test")),
			}
			m.selectedIdx = 0
			m.selectedJobID = 1

			_ = m.renderQueueView()
		})
	}
}

func TestTUIQueueLongCellContent(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.width = 80
	m.height = 20
	m.jobs = []storage.ReviewJob{
		makeJob(1,
			withRef(strings.Repeat("a", 60)),
			withRepoName(strings.Repeat("b", 60)),
			withBranch(strings.Repeat("c", 60)),
			withAgent("test"),
		),
	}
	m.selectedIdx = 0
	m.selectedJobID = 1

	output := m.renderQueueView()
	lines := strings.Split(output, "\n")
	tableEnd := min(len(lines), 7+len(m.jobs))
	for i := 0; i < tableEnd && i < len(lines); i++ {
		line := strings.ReplaceAll(lines[i], "\x1b[K", "")
		line = strings.ReplaceAll(line, "\x1b[J", "")
		visW := lipgloss.Width(line)
		assert.LessOrEqual(t, visW, m.width+1)
	}
}

func TestTUIQueueLongAgentName(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.width = 100
	m.height = 20
	m.jobs = []storage.ReviewJob{
		makeJob(1,
			withRef("abc1234"),
			withRepoName("myrepo"),
			withAgent(strings.Repeat("x", 40)),
		),
	}
	m.selectedIdx = 0
	m.selectedJobID = 1

	output := m.renderQueueView()
	lines := strings.Split(output, "\n")
	tableEnd := min(len(lines), 7+len(m.jobs))
	for i := 0; i < tableEnd && i < len(lines); i++ {
		line := strings.ReplaceAll(lines[i], "\x1b[K", "")
		line = strings.ReplaceAll(line, "\x1b[J", "")
		visW := lipgloss.Width(line)
		assert.LessOrEqual(t, visW, m.width+1)
	}
}

func TestTUIQueueWideCharacterWidth(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.width = 100
	m.height = 20
	m.jobs = []storage.ReviewJob{
		makeJob(1,
			withRef("abc1234"),
			withRepoName("日本語リポ"),
			withBranch("功能分支"),
			withAgent("test"),
		),
	}
	m.selectedIdx = 0
	m.selectedJobID = 1

	output := m.renderQueueView()
	lines := strings.Split(output, "\n")
	tableEnd := min(len(lines), 7+len(m.jobs))
	for i := 0; i < tableEnd && i < len(lines); i++ {
		line := strings.ReplaceAll(lines[i], "\x1b[K", "")
		line = strings.ReplaceAll(line, "\x1b[J", "")
		visW := lipgloss.Width(line)
		assert.LessOrEqual(t, visW, m.width+1)
	}
}

func TestTUIQueueAgentColumnCapped(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.width = 120
	m.height = 20
	longAgent := strings.Repeat("x", 30)
	m.jobs = []storage.ReviewJob{
		makeJob(1, withRef("abc1234"), withRepoName("repo"), withAgent(longAgent)),
	}
	m.selectedIdx = 0
	m.selectedJobID = 1

	output := stripANSI(m.renderQueueView())
	assert.Contains(t, output, "Agent", "expected Agent header in output")

	if strings.Contains(output, longAgent) {
		assert.NotContains(t, output, longAgent, "expected agent name to be truncated, but full name found in output")
	}

	maxRun := 0
	run := 0
	for _, r := range output {
		if r == 'x' {
			run++
			if run > maxRun {
				maxRun = run
			}
		} else {
			run = 0
		}
	}
	assert.LessOrEqual(t, maxRun, 12)
}

func TestTUITasksFlexOvershootHandled(t *testing.T) {
	m := newTuiModel("http://localhost")
	m.currentView = tuiViewTasks
	m.width = 50
	m.height = 20
	m.fixJobs = []storage.ReviewJob{
		{
			ID:            1,
			Status:        storage.JobStatusDone,
			Branch:        strings.Repeat("b", 40),
			RepoName:      "",
			CommitSubject: "",
		},
	}
	m.fixSelectedIdx = 0

	output := m.renderTasksView()
	assert.Contains(t, output, "roborev tasks")

	for line := range strings.SplitSeq(output, "\n") {
		clean := strings.ReplaceAll(line, "\x1b[K", "")
		clean = strings.ReplaceAll(clean, "\x1b[J", "")
		assert.LessOrEqual(t, lipgloss.Width(clean), m.width+1)
	}
}

func TestTUIQueueFlexOvershootHandled(t *testing.T) {
	tests := []struct {
		name   string
		width  int
		ref    string
		repo   string
		branch string
	}{
		{"skewed/w=50", 50, strings.Repeat("r", 40), "", ""},
		{"tight/w=60", 60, "abc", "repo", "main"},
		{"tight/w=61", 61, "abc", "repo", "main"},
		{"tight/w=62", 62, "abc", "repo", "main"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newModel(localhostEndpoint, withExternalIODisabled())
			m.width = tt.width
			m.height = 20
			m.jobs = []storage.ReviewJob{
				makeJob(1,
					withRef(tt.ref),
					withRepoName(tt.repo),
					withBranch(tt.branch),
					withAgent("test"),
				),
			}
			m.selectedIdx = 0
			m.selectedJobID = 1

			output := m.renderQueueView()
			lines := strings.Split(output, "\n")

			for i := 3; i < len(lines); i++ {
				clean := strings.ReplaceAll(lines[i], "\x1b[K", "")
				clean = strings.ReplaceAll(clean, "\x1b[J", "")
				assert.LessOrEqual(t, lipgloss.Width(clean), m.width+1)
			}
		})
	}
}

func TestTUIQueueFlexColumnsGetContentWidth(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	// Leave room for the default-visible Reasoning column.
	m.width = 130
	m.height = 20
	m.jobs = []storage.ReviewJob{
		makeJob(1,
			withRef("abc1234"),
			withRepoName("my-project-repo"),
			withBranch("feature/very-long-branch-name-that-takes-space"),
			withAgent("test"),
		),
	}
	m.selectedIdx = 0
	m.selectedJobID = 1

	output := m.renderQueueView()

	assert.Contains(t, stripTestANSI(output), "my-project-repo")
}

func TestTUITasksStaleSelectionNoPanic(t *testing.T) {
	m := newTuiModel("http://localhost")
	m.currentView = tuiViewTasks
	m.width = 120
	m.height = 20
	m.fixJobs = []storage.ReviewJob{
		{ID: 101, Status: storage.JobStatusDone},
	}

	m.fixSelectedIdx = 5

	output := m.renderTasksView()
	assert.Contains(t, output, "roborev tasks")
}

func TestTUITasksNarrowWidthFlexAllocation(t *testing.T) {
	for _, w := range []int{20, 30, 40} {
		t.Run(fmt.Sprintf("width=%d", w), func(t *testing.T) {
			m := newTuiModel("http://localhost")
			m.currentView = tuiViewTasks
			m.width = w
			m.height = 20
			m.fixJobs = []storage.ReviewJob{
				{ID: 1, Status: storage.JobStatusDone, Branch: "main", RepoName: "r"},
			}
			m.fixSelectedIdx = 0

			_ = m.renderTasksView()
		})
	}
}

func TestColumnOptionsModalOpenClose(t *testing.T) {
	m := newTuiModel("localhost:7373")
	m.jobs = []storage.ReviewJob{makeJob(1)}
	m.currentView = viewQueue
	m.hiddenColumns = map[int]bool{}

	m2, _ := updateModel(t, m, keyPressMsg('o'))
	assert.Equal(t, viewColumnOptions, m2.currentView)
	assert.NotEmpty(t, m2.colOptionsList, "expected non-empty colOptionsList")

	assert.GreaterOrEqual(t, len(m2.colOptionsList), 3)
	borders := m2.colOptionsList[len(m2.colOptionsList)-3]
	assert.False(t, borders.id != colOptionBorders || borders.name != "Column borders")
	mouse := m2.colOptionsList[len(m2.colOptionsList)-2]
	assert.False(t, mouse.id != colOptionMouse || mouse.name != "Mouse interactions")
	tasks := m2.colOptionsList[len(m2.colOptionsList)-1]
	assert.False(t, tasks.id != colOptionTasksWorkflow || tasks.name != "Tasks workflow")

	m3, _ := updateModel(t, m2, keySpecialMsg(tea.KeyEscape))
	assert.Equal(t, viewQueue, m3.currentView)
}

func TestColumnOptionsToggle(t *testing.T) {
	m := newTuiModel("localhost:7373")
	m.jobs = []storage.ReviewJob{makeJob(1)}
	m.currentView = viewQueue
	m.hiddenColumns = map[int]bool{}

	m, _ = updateModel(t, m, keyPressMsg('o'))

	assert.Equal(t, colRef, m.colOptionsList[0].id)
	assert.True(t, m.colOptionsList[0].enabled, "expected Ref to be enabled initially")

	m, _ = updateModel(t, m, keyPressMsg(' '))
	assert.False(t, m.colOptionsList[0].enabled, "expected Ref to be disabled after toggle")
	assert.True(t, m.hiddenColumns[colRef], "expected colRef in hiddenColumns")

	m, _ = updateModel(t, m, keyPressMsg(' '))
	assert.True(t, m.colOptionsList[0].enabled, "expected Ref to be enabled after second toggle")
	assert.False(t, m.hiddenColumns[colRef], "expected colRef removed from hiddenColumns")
}

func TestColumnOptionsMouseClick(t *testing.T) {
	m := newTuiModel("localhost:7373")
	m.jobs = []storage.ReviewJob{makeJob(1)}
	m.currentView = viewQueue
	m.hiddenColumns = map[int]bool{}
	m.mouseEnabled = true

	// Open column options modal.
	m, _ = updateModel(t, m, keyPressMsg('o'))
	require.Equal(t, viewColumnOptions, m.currentView)

	// First option is at row 2 (title=0, blank=1, first option=2).
	firstOpt := m.colOptionsList[0]
	assert.True(t, firstOpt.enabled, "expected first column enabled initially")

	// Click on the first option row to toggle it off.
	m, _ = updateModel(t, m, mouseLeftClick(5, 2))
	assert.False(t, m.colOptionsList[0].enabled, "expected first column disabled after click")
	assert.Equal(t, 0, m.colOptionsIdx, "expected cursor on clicked row")

	// Click again to toggle it back on.
	m, _ = updateModel(t, m, mouseLeftClick(5, 2))
	assert.True(t, m.colOptionsList[0].enabled, "expected first column re-enabled after second click")

	// Click on the second option.
	m, _ = updateModel(t, m, mouseLeftClick(5, 3))
	assert.Equal(t, 1, m.colOptionsIdx, "expected cursor moved to second row")
}

func TestColumnOptionsMouseClickSentinel(t *testing.T) {
	m := newTuiModel("localhost:7373")
	m.jobs = []storage.ReviewJob{makeJob(1)}
	m.currentView = viewQueue
	m.hiddenColumns = map[int]bool{}
	m.mouseEnabled = true

	m, _ = updateModel(t, m, keyPressMsg('o'))
	require.Equal(t, viewColumnOptions, m.currentView)

	// Find the borders option (first sentinel, has a separator line before it).
	bordersIdx := -1
	for i, opt := range m.colOptionsList {
		if opt.id == colOptionBorders {
			bordersIdx = i
			break
		}
	}
	require.NotEqual(t, -1, bordersIdx)

	// Borders is at row = 2 + bordersIdx + 1 (separator line).
	bordersRow := 2 + bordersIdx + 1
	initialBorders := m.colBordersOn

	// Click the separator line (row just before borders) — should be a no-op.
	separatorRow := 2 + bordersIdx
	prevIdx := m.colOptionsIdx
	prevLastEnabled := m.colOptionsList[bordersIdx-1].enabled
	m, _ = updateModel(t, m, mouseLeftClick(5, separatorRow))
	assert.Equal(t, prevIdx, m.colOptionsIdx, "separator click should not move cursor")
	assert.Equal(t, prevLastEnabled, m.colOptionsList[bordersIdx-1].enabled,
		"separator click should not toggle adjacent option")

	// Click the actual borders row.
	m, _ = updateModel(t, m, mouseLeftClick(5, bordersRow))
	assert.Equal(t, bordersIdx, m.colOptionsIdx)
	assert.NotEqual(t, initialBorders, m.colBordersOn, "expected borders toggled")
}

func TestColumnOptionsMouseWheel(t *testing.T) {
	m := newTuiModel("localhost:7373")
	m.jobs = []storage.ReviewJob{makeJob(1)}
	m.currentView = viewQueue
	m.hiddenColumns = map[int]bool{}
	m.mouseEnabled = true

	m, _ = updateModel(t, m, keyPressMsg('o'))
	require.Equal(t, viewColumnOptions, m.currentView)
	assert.Equal(t, 0, m.colOptionsIdx)

	m, _ = updateModel(t, m, mouseWheelDown())
	assert.Equal(t, 1, m.colOptionsIdx)

	m, _ = updateModel(t, m, mouseWheelUp())
	assert.Equal(t, 0, m.colOptionsIdx)

	// Wheel up at top should stay at 0.
	m, _ = updateModel(t, m, mouseWheelUp())
	assert.Equal(t, 0, m.colOptionsIdx)
}

func TestMouseDisabledIgnoresQueueMouseInput(t *testing.T) {
	m := newTuiModel("http://localhost")
	m.currentView = tuiViewQueue
	m.mouseEnabled = false
	m.width = 120
	m.height = 20
	m.jobs = []storage.ReviewJob{
		makeJob(1),
		makeJob(2),
		makeJob(3),
	}
	m.selectedIdx = 0
	m.selectedJobID = 1

	m2, _ := updateModel(t, m, mouseLeftClick(4, 6))
	assert.False(t, m2.selectedIdx != 0 || m2.selectedJobID != 1)

	m3, _ := updateModel(t, m2, mouseWheelDown())
	assert.False(t, m3.selectedIdx != 0 || m3.selectedJobID != 1)
}

func TestHiddenColumnNotRendered(t *testing.T) {
	m := newTuiModel("localhost:7373")
	m.jobs = []storage.ReviewJob{
		makeJob(1, withBranch("main"), withAgent("codex")),
	}
	m.currentView = viewQueue
	m.hiddenColumns = map[int]bool{colAgent: true}
	m.width = 120
	m.height = 30

	output := m.renderQueueView()

	assert.NotContains(t, output, "Agent")

	assert.Contains(t, output, "Branch")
}

func TestColumnBordersRendered(t *testing.T) {
	m := newTuiModel("localhost:7373")
	m.jobs = []storage.ReviewJob{
		makeJob(1, withBranch("main"), withAgent("codex")),
	}
	m.currentView = viewQueue
	m.hiddenColumns = map[int]bool{}
	m.colBordersOn = true
	m.width = 120
	m.height = 30

	output := m.renderQueueView()

	bordersOnCount := strings.Count(output, "▕")

	m.colBordersOn = false
	output2 := m.renderQueueView()
	bordersOffCount := strings.Count(output2, "▕")

	assert.Greater(t, bordersOnCount, bordersOffCount)
}

func TestQueueColWidthCacheColdStart(t *testing.T) {
	m := newTuiModel("http://localhost")
	m.width = 120
	m.height = 24
	m.jobs = []storage.ReviewJob{
		makeJob(1, withRef("abc1234"), withRepoName("myrepo"), withAgent("test")),
	}
	m.selectedIdx = 0
	m.selectedJobID = 1

	output := stripANSI(m.renderQueueView())
	assert.Contains(t, output, "abc1234")

	assert.NotNil(t, m.queueColCache.contentWidths, "cache contentWidths should be populated after first render")
}

func TestQueueColWidthCacheInvalidation(t *testing.T) {
	m := newTuiModel("http://localhost")
	m.width = 120
	m.height = 24
	m.jobs = []storage.ReviewJob{
		makeJob(1, withRef("short"), withRepoName("r"), withAgent("t")),
	}
	m.selectedIdx = 0
	m.selectedJobID = 1

	m.renderQueueView()
	origGen := m.queueColCache.gen
	origWidths := maps.Clone(m.queueColCache.contentWidths)

	m.jobs = []storage.ReviewJob{
		makeJob(1, withRef("a-much-longer-reference"), withRepoName("longer-repo-name"), withAgent("claude-code")),
	}
	m.queueColGen++

	m.renderQueueView()
	assert.NotEqual(t, origGen, m.queueColCache.gen, "cache gen should have advanced after invalidation")

	changed := false
	for k, v := range m.queueColCache.contentWidths {
		if ov, ok := origWidths[k]; ok && ov != v {
			changed = true
			break
		}
	}
	assert.True(t, changed, "content widths should differ after job data change")
}

func TestQueueColWidthCacheReuse(t *testing.T) {
	m := newTuiModel("http://localhost")
	m.width = 120
	m.height = 24
	m.jobs = []storage.ReviewJob{
		makeJob(1, withRef("abc1234"), withRepoName("myrepo"), withAgent("test")),
	}
	m.selectedIdx = 0
	m.selectedJobID = 1

	m.renderQueueView()
	cachedWidthsPtr := fmt.Sprintf("%p", m.queueColCache.contentWidths)
	cachedGen := m.queueColCache.gen

	m.renderQueueView()
	assert.Equal(t, cachedGen, m.queueColCache.gen, "cache gen should not change on re-render without invalidation")
	assert.Equal(t, cachedWidthsPtr, fmt.Sprintf("%p", m.queueColCache.contentWidths))
}

func TestTaskColWidthCacheColdStart(t *testing.T) {
	parentID := int64(42)
	m := newTuiModel("http://localhost")
	m.width = 120
	m.height = 24
	m.fixJobs = []storage.ReviewJob{
		{
			ID:          101,
			Status:      storage.JobStatusDone,
			ParentJobID: &parentID,
			RepoName:    "myrepo",
			Branch:      "main",
			GitRef:      "def5678",
		},
	}

	output := stripANSI(m.renderTasksView())
	assert.Contains(t, output, "def5678")
	assert.NotNil(t, m.taskColCache.contentWidths, "task cache contentWidths should be populated after first render")
}

func TestTaskColWidthCacheInvalidation(t *testing.T) {
	parentID := int64(42)
	m := newTuiModel("http://localhost")
	m.width = 120
	m.height = 24
	m.fixJobs = []storage.ReviewJob{
		{ID: 101, Status: storage.JobStatusQueued, ParentJobID: &parentID, RepoName: "r"},
	}

	m.renderTasksView()
	origGen := m.taskColCache.gen

	m.fixJobs = []storage.ReviewJob{
		{ID: 101, Status: storage.JobStatusDone, ParentJobID: &parentID, RepoName: "a-longer-repo-name"},
	}
	m.taskColGen++

	m.renderTasksView()
	assert.NotEqual(t, origGen, m.taskColCache.gen, "task cache gen should have advanced after invalidation")
}

func TestTaskColWidthCacheReuse(t *testing.T) {
	parentID := int64(42)
	m := newTuiModel("http://localhost")
	m.width = 120
	m.height = 24
	m.fixJobs = []storage.ReviewJob{
		{ID: 101, Status: storage.JobStatusDone, ParentJobID: &parentID, RepoName: "myrepo", Branch: "main", GitRef: "def5678"},
	}

	m.renderTasksView()
	cachedWidthsPtr := fmt.Sprintf("%p", m.taskColCache.contentWidths)
	cachedGen := m.taskColCache.gen

	m.renderTasksView()
	assert.Equal(t, cachedGen, m.taskColCache.gen, "task cache gen should not change on re-render without invalidation")
	assert.Equal(t, cachedWidthsPtr, fmt.Sprintf("%p", m.taskColCache.contentWidths), "task cache content widths should remain stable on re-render")
}

func TestStatusLabel(t *testing.T) {
	tests := []struct {
		name string
		job  storage.ReviewJob
		want string
	}{
		{"queued", storage.ReviewJob{Status: storage.JobStatusQueued}, "Queued"},
		{"running", storage.ReviewJob{Status: storage.JobStatusRunning}, "Running"},
		{"failed", storage.ReviewJob{Status: storage.JobStatusFailed}, "Error"},
		{"canceled", storage.ReviewJob{Status: storage.JobStatusCanceled}, "Canceled"},
		{"done", storage.ReviewJob{Status: storage.JobStatusDone}, "Done"},
		{"applied", storage.ReviewJob{Status: storage.JobStatusApplied}, "Done"},
		{"rebased", storage.ReviewJob{Status: storage.JobStatusRebased}, "Done"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := statusLabel(tt.job)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestStatusColor(t *testing.T) {
	tests := []struct {
		name   string
		status storage.JobStatus
		want   color.Color
	}{
		{"queued", storage.JobStatusQueued, queuedStyle.GetForeground()},
		{"running", storage.JobStatusRunning, runningStyle.GetForeground()},
		{"done", storage.JobStatusDone, doneStyle.GetForeground()},
		{"applied", storage.JobStatusApplied, doneStyle.GetForeground()},
		{"rebased", storage.JobStatusRebased, doneStyle.GetForeground()},
		{"failed", storage.JobStatusFailed, failedStyle.GetForeground()},
		{"canceled", storage.JobStatusCanceled, canceledStyle.GetForeground()},
		{"unknown", storage.JobStatus("unknown"), nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := statusColor(tt.status)
			assert.Equal(t, tt.want, got)
		})
	}

	assert.NotEqual(t, failedStyle.GetForeground(), failStyle.GetForeground())
}

func TestVerdictColor(t *testing.T) {
	strPtr := func(s string) *string { return &s }

	tests := []struct {
		name    string
		verdict *string
		want    color.Color
	}{
		{"pass", strPtr("P"), passStyle.GetForeground()},
		{"fail", strPtr("F"), failStyle.GetForeground()},
		{"unexpected", strPtr("X"), failStyle.GetForeground()},
		{"nil", nil, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := verdictColor(tt.verdict)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestClosedKeyShortcut(t *testing.T) {
	newTestModel := func() model {
		return setupTestModel([]storage.ReviewJob{
			makeJob(1, withStatus(storage.JobStatusDone), withClosed(new(false))),
		}, func(m *model) {
			m.currentView = viewQueue
			m.selectedIdx = 0
			m.selectedJobID = 1
			m.pendingClosed = make(map[int64]pendingState)
		})
	}

	m := newTestModel()
	m2, cmd := pressKey(m, 'a')
	assert.NotNil(t, cmd, "Expected command from 'a' key press")
	pending, ok := m2.pendingClosed[1]
	assert.True(t, ok, "Expected pending closed state for job 1 after 'a'")

	if ok {
		assert.True(t, pending.newState, "Expected pending newState=true (toggled from false)")
	}

	m3 := newTestModel()
	m4, cmd2 := pressKey(m3, 'd')
	assert.Nil(t, cmd2, "'d' key should not trigger any command (shortcut removed)")
	assert.Empty(t, m4.pendingClosed, "'d' should not modify pendingClosed state")
}

func TestMigrateColumnConfig(t *testing.T) {
	tests := []struct {
		name         string
		columnOrder  []string
		hiddenCols   []string
		version      int
		wantDirty    bool
		wantColOrder []string
		wantHidden   []string
		wantVersion  int
	}{
		{
			name:         "nil config unchanged",
			wantDirty:    false,
			wantColOrder: nil,
			wantHidden:   nil,
		},
		{
			name:         "addressed in column_order resets",
			columnOrder:  []string{"ref", "branch", "repo", "agent", "status", "queued", "elapsed", "addressed"},
			wantDirty:    true,
			wantColOrder: nil,
		},
		{
			name:       "addressed in hidden_columns resets",
			hiddenCols: []string{"addressed", "branch"},
			wantDirty:  true,
			wantHidden: nil,
		},
		{
			name:         "old default order resets",
			columnOrder:  []string{"ref", "branch", "repo", "agent", "status", "queued", "elapsed", "closed"},
			wantDirty:    true,
			wantColOrder: nil,
		},
		{
			name:         "combined status default order resets",
			columnOrder:  []string{"ref", "branch", "repo", "agent", "queued", "elapsed", "status", "closed"},
			wantDirty:    true,
			wantColOrder: nil,
		},
		{
			name:         "custom order preserved",
			columnOrder:  []string{"repo", "ref", "agent", "status", "pf", "queued", "elapsed", "branch", "closed"},
			wantDirty:    false,
			wantColOrder: []string{"repo", "ref", "agent", "status", "pf", "queued", "elapsed", "branch", "closed"},
		},
		{
			name:         "current default order preserved",
			columnOrder:  []string{"ref", "branch", "repo", "agent", "queued", "elapsed", "status", "pf", "closed"},
			wantDirty:    false,
			wantColOrder: []string{"ref", "branch", "repo", "agent", "queued", "elapsed", "status", "pf", "closed"},
		},
		{
			name:        "stale hidden_columns backfills only new columns",
			hiddenCols:  []string{"branch"},
			wantDirty:   true,
			wantHidden:  []string{"branch", "requested_model", "requested_provider"},
			wantVersion: 1,
		},
		{
			name:        "visible session_id preserved during backfill",
			hiddenCols:  []string{"branch", "session_id"},
			wantDirty:   true,
			wantHidden:  []string{"branch", "session_id", "requested_model", "requested_provider"},
			wantVersion: 1,
		},
		{
			name:        "hidden_columns already has new defaults",
			hiddenCols:  []string{"session_id", "requested_model", "requested_provider"},
			wantDirty:   true,
			wantHidden:  []string{"session_id", "requested_model", "requested_provider"},
			wantVersion: 1,
		},
		{
			name:       "sentinel preserves show-all intent",
			hiddenCols: []string{"_"},
			wantDirty:  false,
			wantHidden: []string{"_"},
		},
		{
			name:        "version 1 config not re-migrated",
			hiddenCols:  []string{"branch"},
			version:     1,
			wantDirty:   false,
			wantHidden:  []string{"branch"},
			wantVersion: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				ColumnOrder:         slices.Clone(tt.columnOrder),
				HiddenColumns:       slices.Clone(tt.hiddenCols),
				ColumnConfigVersion: tt.version,
			}
			dirty := migrateColumnConfig(cfg)
			assert.Equal(t, tt.wantDirty, dirty)
			assert.True(t, slices.Equal(cfg.ColumnOrder, tt.wantColOrder))
			assert.True(t, slices.Equal(cfg.HiddenColumns, tt.wantHidden))
			assert.Equal(t, tt.wantVersion, cfg.ColumnConfigVersion)
		})
	}
}

func TestParseColumnOrderAppendsMissing(t *testing.T) {
	oldCustom := []string{"repo", "ref", "agent", "status", "queued", "elapsed", "branch", "closed"}
	got := parseColumnOrder(oldCustom)

	wantPrefix := []int{colRepo, colRef, colAgent, colStatus, colQueued, colElapsed, colBranch, colHandled}
	assert.True(t, slices.Equal(got[:len(wantPrefix)], wantPrefix))

	pfCount := 0
	requestedModelCount := 0
	requestedProviderCount := 0
	for _, c := range got {
		if c == colPF {
			pfCount++
		}
		if c == colRequestedModel {
			requestedModelCount++
		}
		if c == colRequestedProvider {
			requestedProviderCount++
		}
	}
	assert.Equal(t, 1, pfCount)
	assert.Equal(t, 1, requestedModelCount)
	assert.Equal(t, 1, requestedProviderCount)
}

func TestDefaultColumnOrderDetection(t *testing.T) {
	defaultOrder := make([]int, len(toggleableColumns))
	copy(defaultOrder, toggleableColumns)

	assert.True(t, slices.Equal(defaultOrder, toggleableColumns), "copy of toggleableColumns should equal toggleableColumns")

	customOrder := make([]int, len(toggleableColumns))
	copy(customOrder, toggleableColumns)
	customOrder[0], customOrder[1] = customOrder[1], customOrder[0]

	assert.False(t, slices.Equal(customOrder, toggleableColumns))
}

func TestDefaultHiddenColumnsIncludeRequestedFields(t *testing.T) {
	hidden := parseHiddenColumns(nil)
	assert.True(t, hidden[colSessionID])
	assert.True(t, hidden[colRequestedModel])
	assert.True(t, hidden[colRequestedProvider])
}

// --- Column smoke tests ---
//
// These invariant tests prevent a class of bug where new columns
// are added to the column enum but one or more metadata structures
// (allHeaders, columnNames, columnConfigNames, fixedWidth,
// defaultHiddenColumns, migrateColumnConfig) are not updated.

// TestColumnMetadataComplete verifies every toggleable column has
// entries in all required metadata maps.
func TestColumnMetadataComplete(t *testing.T) {
	assert := assert.New(t)

	// toggleableColumns should cover all columns except colSel
	// and colJobID (which are always visible).
	assert.Len(toggleableColumns, colCount-2,
		"toggleableColumns count must equal colCount-2; "+
			"did you add a column to the enum without adding "+
			"it to toggleableColumns?")

	for _, col := range toggleableColumns {
		assert.NotEmpty(columnNames[col],
			"column %d missing from columnNames", col)
		assert.NotEmpty(columnConfigNames[col],
			"column %d missing from columnConfigNames", col)
	}
}

// TestAllColumnsVisibleHeadersPresent renders the queue with every
// column visible and checks that each column header appears in the
// rendered output. Catches missing entries in the allHeaders array
// inside renderQueueView.
func TestAllColumnsVisibleHeadersPresent(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.width = 300 // wide enough for all columns
	m.height = 20
	m.hiddenColumns = map[int]bool{} // show all
	m.jobs = []storage.ReviewJob{
		makeJob(1, withRef("abc1234"),
			withRepoName("repo"), withAgent("test")),
	}
	m.selectedIdx = 0
	m.selectedJobID = 1

	output := m.renderQueueView()

	// Find the header line (contains "Status" and "P/F")
	var headerLine string
	for line := range strings.SplitSeq(output, "\n") {
		stripped := stripTestANSI(line)
		if strings.Contains(stripped, "Status") &&
			strings.Contains(stripped, "P/F") {
			headerLine = stripped
			break
		}
	}
	require.NotEmpty(t, headerLine, "could not find header line")

	for _, col := range toggleableColumns {
		name := columnNames[col]
		assert.Contains(t, headerLine, name,
			"header missing column %q (col=%d)", name, col)
	}
}

// TestQueueRenderWithStaleHiddenConfig simulates an existing user
// whose hidden_columns config predates newly added columns. After
// migration, new default-hidden columns must be hidden and flex
// columns (Ref, Branch, Repo) must retain usable widths.
func TestQueueRenderWithStaleHiddenConfig(t *testing.T) {
	// Pre-upgrade config (version 0): only session_id hidden
	staleCfg := &config.Config{
		HiddenColumns: []string{"session_id"},
	}
	migrateColumnConfig(staleCfg)
	migratedHidden := parseHiddenColumns(staleCfg.HiddenColumns)

	// New columns must be hidden after migration
	assert.True(t, migratedHidden[colRequestedModel],
		"requested_model should be hidden after migration")
	assert.True(t, migratedHidden[colRequestedProvider],
		"requested_provider should be hidden after migration")

	// Render with migrated config — flex columns must not be
	// starved by phantom visible columns
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.width = 120
	m.height = 20
	m.hiddenColumns = migratedHidden
	m.jobs = []storage.ReviewJob{
		makeJob(1,
			withRef("abc1234"),
			withRepoName("my-project"),
			withBranch("feature/branch"),
			withAgent("test"),
		),
	}
	m.selectedIdx = 0
	m.selectedJobID = 1

	output := m.renderQueueView()
	found := false
	for line := range strings.SplitSeq(output, "\n") {
		if strings.Contains(stripTestANSI(line), "my-project") {
			found = true
			break
		}
	}
	assert.True(t, found,
		"repo name should not be truncated with migrated config "+
			"at width=120")
}

// TestQueueRenderPostMigrationUserChoice verifies that after the
// version-1 migration, a user who explicitly unhides new columns
// keeps their choice on subsequent startups.
func TestQueueRenderPostMigrationUserChoice(t *testing.T) {
	// Post-migration config: user unhid all default-hidden columns
	postCfg := &config.Config{
		HiddenColumns:       []string{"branch"},
		ColumnConfigVersion: 1,
	}
	dirty := migrateColumnConfig(postCfg)
	assert.False(t, dirty, "version-1 config should not be re-migrated")
	assert.Equal(t, []string{"branch"}, postCfg.HiddenColumns,
		"user's explicit column choices must be preserved")
}

// TestSaveColumnOptionStampsVersion simulates the save-then-reload
// cycle: a user starts from a version-0 config (e.g., sentinel or
// nil hidden_columns), customizes column visibility through the TUI
// options modal, and saves. On next startup, the saved config must
// not be re-migrated — saveColumnOptions stamps ColumnConfigVersion.
func TestSaveColumnOptionStampsVersion(t *testing.T) {
	// Simulate what saveColumnOptions writes: the user chose to hide
	// only "branch", showing all default-hidden columns.
	// saveColumnOptions now stamps ColumnConfigVersion = 1.
	savedCfg := &config.Config{
		HiddenColumns:       []string{"branch"},
		ColumnConfigVersion: 1, // stamped by saveColumnOptions
	}

	// Next startup: migrateColumnConfig must not backfill
	dirty := migrateColumnConfig(savedCfg)
	assert.False(t, dirty)
	assert.Equal(t, []string{"branch"}, savedCfg.HiddenColumns,
		"saved column preferences must survive reload")
	assert.Equal(t, 1, savedCfg.ColumnConfigVersion)

	// Contrast: without the version stamp, migration would backfill
	unstampedCfg := &config.Config{
		HiddenColumns:       []string{"branch"},
		ColumnConfigVersion: 0, // missing stamp (the bug)
	}
	dirty = migrateColumnConfig(unstampedCfg)
	assert.True(t, dirty,
		"version-0 config with explicit hidden_columns must "+
			"trigger migration")
	assert.Equal(t, 1, unstampedCfg.ColumnConfigVersion)
}

// TestSaveColumnOptionsWritesVersion exercises the real
// saveColumnOptions → config.LoadGlobal/SaveGlobal path and
// verifies ColumnConfigVersion is persisted.
func TestSaveColumnOptionsWritesVersion(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", tmpDir)

	// Seed a version-0 config with sentinel hidden_columns
	seedCfg := &config.Config{
		HiddenColumns: []string{config.HiddenColumnsNoneSentinel},
	}
	require.NoError(t, config.SaveGlobal(seedCfg))

	// Build a model that will save column preferences
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.hiddenColumns = map[int]bool{colBranch: true}

	// Execute the save command
	cmd := m.saveColumnOptions()
	msg := cmd()
	assert.Nil(t, msg, "saveColumnOptions should succeed")

	// Reload and verify both version and hidden columns were persisted
	loaded, err := config.LoadGlobal()
	require.NoError(t, err)
	assert.Equal(t, 1, loaded.ColumnConfigVersion,
		"ColumnConfigVersion must be persisted by saveColumnOptions")
	assert.Equal(t, []string{"branch"}, loaded.HiddenColumns,
		"HiddenColumns must match the user's selection")

	// Verify the saved config survives migration without changes
	dirty := migrateColumnConfig(loaded)
	assert.False(t, dirty,
		"freshly saved config must not trigger migration")
}

// TestMigratePreV1ConfigWithVisibleNewColumns documents the
// migration behavior for the edge case where a user was briefly on
// the buggy version (d2d671f6) and explicitly saved preferences
// showing the new columns before ColumnConfigVersion existed.
//
// The migration cannot distinguish this from a stale config, so it
// backfills. This is the correct tradeoff: the buggy window was
// brief and the columns being visible caused broken layouts.
func TestMigratePreV1ConfigWithVisibleNewColumns(t *testing.T) {
	// User saved ["branch"] during buggy window, intending to show
	// requested_model and requested_provider. No version stamp.
	cfg := &config.Config{
		HiddenColumns: []string{"branch"},
	}

	dirty := migrateColumnConfig(cfg)
	assert.True(t, dirty)
	assert.Equal(t, 1, cfg.ColumnConfigVersion)

	// Migration backfills — user must re-unhide via options modal
	hidden := parseHiddenColumns(cfg.HiddenColumns)
	assert.True(t, hidden[colRequestedModel])
	assert.True(t, hidden[colRequestedProvider])

	// Second run: version stamp prevents re-migration
	dirty = migrateColumnConfig(cfg)
	assert.False(t, dirty)
}

func TestJobCells_Skipped(t *testing.T) {
	m := newQueueTestModel()
	j := storage.ReviewJob{
		ID:         42,
		ReviewType: "design",
		Status:     storage.JobStatusSkipped,
		SkipReason: "trivial diff",
		GitRef:     "abc123",
	}
	cells := m.jobCells(j)
	joined := strings.Join(cells, "|")
	assert := assert.New(t)
	assert.Contains(joined, "skipped")
	assert.Contains(joined, "design")
	assert.Contains(joined, "trivial")
}

func TestQueueRenderUnchangedWithoutPanels(t *testing.T) {
	jobs := []storage.ReviewJob{
		makeJob(3, withRef("aaa1111"), withStatus(storage.JobStatusDone)),
		makeJob(2, withRef("bbb2222"), withStatus(storage.JobStatusRunning)),
		makeJob(1, withRef("ccc3333"), withStatus(storage.JobStatusFailed)),
	}
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.width, m.height = 120, 30
	m.jobs = jobs
	m.selectedIdx, m.selectedJobID = 0, 3

	rows := m.visibleQueueRows()
	assert.Len(t, rows, 3)
	for i, r := range rows {
		assert.Equal(t, jobs[i].ID, r.job.ID)
		assert.Equal(t, 0, r.depth)
		assert.False(t, r.hasChildren)
	}
	out := stripTestANSI(m.renderQueueView())
	for _, ref := range []string{"aaa1111", "bbb2222", "ccc3333"} {
		assert.Contains(t, out, ref)
	}
}

func seededPanelModel(t *testing.T) model {
	t.Helper()
	parent := makeJob(10, withRef("syn"), withStatus(storage.JobStatusDone),
		withSynthesis("R", storage.PanelSummary{MembersTotal: 2, MembersTerminal: 2, MembersSucceeded: 2}))
	top := makeJob(20, withRef("top"), withStatus(storage.JobStatusDone))
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.width, m.height = 120, 30
	m.jobs = []storage.ReviewJob{top, parent} // newest-first: 20 then 10
	m.panelMembers = map[uuid.UUID][]storage.ReviewJob{testUUID("R"): {
		makeJob(11, withRef("m0"), withPanelMember("R", "default", 0), withStatus(storage.JobStatusDone)),
		makeJob(12, withRef("m1"), withPanelMember("R", "security", 1), withStatus(storage.JobStatusDone)),
	}}
	m.selectedJobID, m.selectedIdx = 20, 0
	return m
}

func TestSelectedJobResolvesMember(t *testing.T) {
	m := seededPanelModel(t)
	m.expandedPanels[testUUID("R")] = true
	m.selectedJobID = 12 // a member, not in m.jobs
	job, ok := m.selectedJob()
	assert.True(t, ok, "selectedJob resolves a member by id")
	assert.Equal(t, int64(12), job.ID)
}

func TestNavSkipsCollapsedMembers(t *testing.T) {
	m := seededPanelModel(t)            // collapsed: visible rows = [20, 10]
	m, _ = pressSpecial(m, tea.KeyDown) // 20 -> 10
	assert.Equal(t, int64(10), m.selectedJobID)
	m, _ = pressSpecial(m, tea.KeyDown) // no visible row below; members collapsed
	assert.Equal(t, int64(10), m.selectedJobID, "collapsed members are not navigable")
}

func TestNavWalksExpandedMembers(t *testing.T) {
	assert := assert.New(t)
	m := seededPanelModel(t)
	m.expandedPanels[testUUID("R")] = true // visible rows = [20, 10, 11, 12]
	m.selectedJobID, m.selectedIdx = 10, 1
	m, _ = pressSpecial(m, tea.KeyDown)
	assert.Equal(int64(11), m.selectedJobID, "down steps into first member")
	m, _ = pressSpecial(m, tea.KeyDown)
	assert.Equal(int64(12), m.selectedJobID)
	m, _ = pressSpecial(m, tea.KeyUp)
	assert.Equal(int64(11), m.selectedJobID, "up walks back over members")
}

func TestPrevNextKeysWalkFlattenedRows(t *testing.T) {
	m := seededPanelModel(t)
	m.expandedPanels[testUUID("R")] = true
	m.selectedJobID, m.selectedIdx = 10, 1
	m, _ = pressKey(m, 'j') // prev == down
	assert.Equal(t, int64(11), m.selectedJobID)
	m, _ = pressKey(m, 'k') // next == up
	assert.Equal(t, int64(10), m.selectedJobID)
}

func TestSelectionRestoredByIDAfterRefresh(t *testing.T) {
	m := seededPanelModel(t)
	m.expandedPanels[testUUID("R")] = true
	m.selectedJobID = 12 // a member is selected
	reordered := []storage.ReviewJob{m.jobs[1], m.jobs[0]}
	m2, _ := m.Update(jobsMsg{jobs: reordered, append: false, seq: m.fetchSeq})
	assert.Equal(t, int64(12), m2.(model).selectedJobID, "selection survives refresh by id")
}

func TestMutatingActionsBlockedOnMember(t *testing.T) {
	for _, key := range []rune{'r', 'a', 'x'} {
		m := seededPanelModel(t)
		m.expandedPanels[testUUID("R")] = true
		m.selectedJobID = 11 // a member
		m2, cmd := pressKey(m, key)
		assert.Nil(t, cmd, "key %q must not act on a member", string(key))
		assert.NotEmpty(t, m2.flashMessage, "key %q flashes a parent hint on a member", string(key))
	}
}

func TestMemberCloseDoesNotTouchParentStatsOrSelection(t *testing.T) {
	assert := assert.New(t)
	m := seededPanelModel(t)
	m.expandedPanels[testUUID("R")] = true
	m.hideClosed = true
	m.jobStats = storage.JobStats{Open: 5, Closed: 2}
	closed := false
	m.panelMembers[testUUID("R")][0].Closed = &closed // member 11 is closable
	m.selectedJobID = 11
	before := m.jobStats
	m2, _ := pressKey(m, 'a') // close
	assert.Equal(before, m2.jobStats, "member close must not change parent-only stats (server excludes members)")
	assert.Equal(int64(11), m2.selectedJobID, "member close must not move selection under hideClosed")
	assert.NotEmpty(m2.flashMessage)
}

func TestBoundaryFlashWithExpandedPanel(t *testing.T) {
	assert := assert.New(t)
	// rows when expanded: [20 (top), 10 (parent), 11, 12 (last member)]
	m := seededPanelModel(t)
	m.expandedPanels[testUUID("R")] = true

	// Up at the very top (row 20) is a no-op that re-flashes the top boundary.
	m.selectedJobID, m.selectedIdx = 20, 0
	m, _ = pressSpecial(m, tea.KeyUp)
	assert.Equal(int64(20), m.selectedJobID, "Up at the top stays put")
	assert.NotEmpty(m.flashMessage, "Up at the top boundary flashes")

	// Down on the last member (row 12) is a no-op at the bottom boundary.
	m.flashMessage = ""
	m.selectedJobID = 12
	m, _ = pressSpecial(m, tea.KeyDown)
	assert.Equal(int64(12), m.selectedJobID, "Down on the last member stays put")
	assert.NotEmpty(m.flashMessage, "Down at the bottom boundary flashes")
}

func TestSpaceTogglesPanelParent(t *testing.T) {
	m := seededPanelModel(t)
	m.selectedJobID, m.selectedIdx = 10, 1 // the synthesis parent
	m, _ = pressKey(m, ' ')
	assert.True(t, m.expandedPanels[testUUID("R")], "space expands a panel parent")
	m, _ = pressKey(m, ' ')
	assert.False(t, m.expandedPanels[testUUID("R")], "space again collapses")
}

func TestRightArrowExpandsPanelParent(t *testing.T) {
	m := seededPanelModel(t)
	m.selectedJobID, m.selectedIdx = 10, 1 // the synthesis parent

	m, _ = pressSpecial(m, tea.KeyRight)

	assert.True(t, m.expandedPanels[testUUID("R")], "right arrow expands a collapsed panel parent")
	assert.Equal(t, int64(10), m.selectedJobID, "expanding keeps the parent selected")
}

func TestLeftArrowCollapsesExpandedPanelParent(t *testing.T) {
	m := seededPanelModel(t)
	m.expandedPanels[testUUID("R")] = true
	m.selectedJobID, m.selectedIdx = 10, 1 // the synthesis parent

	m, _ = pressSpecial(m, tea.KeyLeft)

	assert.False(t, m.expandedPanels[testUUID("R")], "left arrow collapses an expanded panel parent")
	assert.Equal(t, int64(10), m.selectedJobID, "collapsing keeps the parent selected")
}

func TestLeftArrowOnPanelMemberCollapsesParent(t *testing.T) {
	m := seededPanelModel(t)
	m.expandedPanels[testUUID("R")] = true
	m.selectedJobID, m.selectedIdx = 11, -1 // first member

	m, _ = pressSpecial(m, tea.KeyLeft)

	assert.False(t, m.expandedPanels[testUUID("R")], "left arrow on a member collapses its panel")
	assert.Equal(t, int64(10), m.selectedJobID, "left arrow on a member selects the parent")
}

func TestPanelArrowKeysFallBackToNavigationOnPlainRows(t *testing.T) {
	m := seededPanelModel(t)
	m.selectedJobID, m.selectedIdx = 20, 0 // standalone job above the panel

	m, _ = pressSpecial(m, tea.KeyLeft)
	assert.Equal(t, int64(10), m.selectedJobID, "left arrow still moves down on plain rows")

	plain := newModel(localhostEndpoint, withExternalIODisabled())
	plain.jobs = []storage.ReviewJob{
		makeJob(30, withRef("newer")),
		makeJob(20, withRef("middle")),
		makeJob(10, withRef("older")),
	}
	plain.selectedJobID, plain.selectedIdx = 20, 1

	plain, _ = pressSpecial(plain, tea.KeyRight)
	assert.Equal(t, int64(30), plain.selectedJobID, "right arrow still moves up on plain rows")
}

func TestSpaceNoOpOnNonParent(t *testing.T) {
	m := seededPanelModel(t)
	m.selectedJobID, m.selectedIdx = 20, 0 // a standalone job
	before := len(m.expandedPanels)
	m, cmd := pressKey(m, ' ')
	assert.Len(t, m.expandedPanels, before, "space does nothing on a non-panel row")
	assert.Nil(t, cmd)
}

func TestExpandFetchesMembersWhenUncached(t *testing.T) {
	m := seededPanelModel(t)
	delete(m.panelMembers, testUUID("R")) // not yet fetched
	m.selectedJobID, m.selectedIdx = 10, 1
	m, cmd := pressKey(m, ' ')
	assert.True(t, m.expandedPanels[testUUID("R")])
	assert.NotNil(t, cmd, "expanding an unfetched panel dispatches a member fetch")
}

func TestExpandUsesCacheWhenPresent(t *testing.T) {
	m := seededPanelModel(t) // members for R already cached
	m.selectedJobID, m.selectedIdx = 10, 1
	_, cmd := pressKey(m, ' ')
	assert.Nil(t, cmd, "no refetch when members are already cached")
}

func TestPanelMembersMsgSuccessCaches(t *testing.T) {
	m := seededPanelModel(t)
	delete(m.panelMembers, testUUID("R"))
	fetched := []storage.ReviewJob{
		makeJob(11, withPanelMember("R", "default", 0)),
		makeJob(12, withPanelMember("R", "security", 1)),
	}
	updated, _ := m.Update(panelMembersMsg{runUUID: testUUID("R"), members: fetched})
	m2 := updated.(model)
	assert.Len(t, m2.panelMembers[testUUID("R")], 2)
}

func TestPanelMembersMsgErrorDoesNotCache(t *testing.T) {
	m := seededPanelModel(t)
	delete(m.panelMembers, testUUID("R"))
	updated, _ := m.Update(panelMembersMsg{runUUID: testUUID("R"), err: assert.AnError})
	m2 := updated.(model)
	_, cached := m2.panelMembers[testUUID("R")]
	assert.False(t, cached, "a failed fetch is not cached, so a later expand refetches")
	assert.NotEmpty(t, m2.flashMessage, "fetch failure is surfaced via flash")
}

func TestCollapseKeepsParentSelected(t *testing.T) {
	m := seededPanelModel(t)
	m.expandedPanels[testUUID("R")] = true // members 11,12 visible
	m.selectedJobID, m.selectedIdx = 10, 1 // the parent is selected
	m, _ = pressKey(m, ' ')                // collapse
	assert.False(t, m.expandedPanels[testUUID("R")])
	assert.Equal(t, int64(10), m.selectedJobID, "collapsing keeps the parent row selected")
}

func TestFetchPanelMembersFiltersAndSorts(t *testing.T) {
	assert := assert.New(t)
	// The server returns the full run: a synthesis row plus two members given
	// out of order (index 1 before index 0). The cmd must drop the synthesis
	// row and sort the survivors by member index.
	_, m := mockServerModel(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/jobs", r.URL.Path)
		assert.Equal(testUUID("R").String(), r.URL.Query().Get("panel_run")) //nolint:forbidigo // HTTP query text boundary.
		jobs := []storage.ReviewJob{
			makeJob(10, withSynthesis("R", storage.PanelSummary{MembersTotal: 2})),
			makeJob(12, withPanelMember("R", "security", 1)),
			makeJob(11, withPanelMember("R", "default", 0)),
		}
		assert.NoError(json.NewEncoder(w).Encode(map[string]any{"jobs": jobs}))
	})

	msg, ok := m.fetchPanelMembers(testUUID("R"))().(panelMembersMsg)
	require.True(t, ok)
	require.NoError(t, msg.err)
	assert.Equal(testUUID("R"), msg.runUUID)
	require.Len(t, msg.members, 2) // synthesis row filtered out; gates the indexing below
	assert.Equal(0, msg.members[0].PanelMemberIndex)
	assert.Equal(1, msg.members[1].PanelMemberIndex)
	assert.Equal("default", msg.members[0].PanelMemberName)  // index 0 first
	assert.Equal("security", msg.members[1].PanelMemberName) // index 1 second
	assert.Equal(int64(11), msg.members[0].ID)
	assert.Equal(int64(12), msg.members[1].ID)
}

func TestFetchPanelMembersServerError(t *testing.T) {
	_, m := mockServerModel(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	msg, ok := m.fetchPanelMembers(testUUID("R"))().(panelMembersMsg)
	require.True(t, ok)
	require.ErrorContains(t, msg.err, "list panel members:")
	assert.Empty(t, msg.members)
}

func TestFetchPanelMembersEmptyRun(t *testing.T) {
	// A run with only a synthesis row (no members yet) yields no members
	// and no error.
	_, m := mockServerModel(t, func(w http.ResponseWriter, r *http.Request) {
		jobs := []storage.ReviewJob{
			makeJob(10, withSynthesis("R", storage.PanelSummary{MembersTotal: 0})),
		}
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"jobs": jobs}))
	})

	msg, ok := m.fetchPanelMembers(testUUID("R"))().(panelMembersMsg)
	require.True(t, ok)
	require.NoError(t, msg.err)
	assert.Empty(t, msg.members)
}

// withTestColor forces the queue color predicate on for the duration of a test,
// so the unicode disclosure/connector glyphs are emitted by the render path.
func withTestColor(t *testing.T) {
	t.Helper()
	t.Setenv("NO_COLOR", "")
	t.Setenv("CLICOLOR", "")
	t.Setenv("ROBOREV_COLOR_MODE", "dark")
}

func TestRenderShowsDisclosureAndConnectorsWhenExpanded(t *testing.T) {
	withTestColor(t)
	m := seededPanelModel(t)
	m.expandedPanels[testUUID("R")] = true
	out := stripTestANSI(m.renderQueueView())
	assert.Contains(t, out, "▾", "expanded parent shows the open disclosure")
	assert.Contains(t, out, "└─", "last member shows the └─ connector")
}

func TestRenderShowsTerminalOutcomeSplit(t *testing.T) {
	parent := makeJob(10, withRef("syn"), withStatus(storage.JobStatusDone),
		withSynthesis("R", storage.PanelSummary{MembersTotal: 3, MembersTerminal: 3, MembersSucceeded: 2, MembersFailed: 1}))
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.width, m.height = 120, 30
	m.jobs = []storage.ReviewJob{parent}
	m.selectedJobID, m.selectedIdx = 10, 0
	out := stripTestANSI(m.renderQueueView())
	assert.Contains(t, out, "2 ok")
	assert.Contains(t, out, "1 failed")
}

func TestRenderNoDisclosureColumnWhenNoPanels(t *testing.T) {
	jobs := []storage.ReviewJob{makeJob(1, withRef("plain"), withStatus(storage.JobStatusDone))}
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.width, m.height = 120, 30
	m.jobs = jobs
	m.selectedJobID, m.selectedIdx = 1, 0
	out := stripTestANSI(m.renderQueueView())
	assert.NotContains(t, out, "▸")
	assert.NotContains(t, out, "▾")
	assert.NotContains(t, out, "space", "no expand hint when the page has no panels")
}

func TestExpandHintOnlyWhenParentSelected(t *testing.T) {
	m := seededPanelModel(t)               // jobs = [20 standalone, 10 parent]
	m.selectedJobID, m.selectedIdx = 20, 0 // standalone selected
	assert.NotContains(t, stripTestANSI(m.renderQueueView()), "space",
		"no expand hint while a non-parent row is selected")
	m.selectedJobID, m.selectedIdx = 10, 1 // parent selected
	assert.Contains(t, stripTestANSI(m.renderQueueView()), "space",
		"expand hint appears only when the selected row is a panel parent")
}

// TestQueueHelpLinesAccountForExpandHint proves the help-height reservation
// (queueHelpLines) counts the same rows renderQueueView draws: the base rows
// when a non-parent is selected, and the rows plus the "space — expand" hint
// when a panel parent is selected. Cover widths with and without an extra
// reflow line so adding a shortcut does not invalidate the test's fixture.
func TestQueueHelpLinesAccountForExpandHint(t *testing.T) {
	var extraLineWidths, sameHeightWidths int
	for w := 80; w <= 160; w++ {
		t.Run(fmt.Sprintf("width=%d", w), func(t *testing.T) {
			m := seededPanelModel(t) // jobs = [20 standalone, 10 parent]
			m.width = w

			base := len(reflowHelpRows(m.queueHelpRows(), w))
			hinted := len(reflowHelpRows(withExpandHint(m.queueHelpRows()), w))
			if hinted > base {
				extraLineWidths++
			} else if hinted == base {
				sameHeightWidths++
			}

			// Non-parent selected: reservation must equal the base help height.
			m.selectedJobID, m.selectedIdx = 20, 0
			assert.Equal(t, base, m.queueHelpLines(),
				"non-parent selection must reserve the base help height")

			// Panel parent selected: reservation must equal the hinted height,
			// matching the withExpandHint row renderQueueView appends.
			m.selectedJobID, m.selectedIdx = 10, 1
			assert.Equal(t, hinted, m.queueHelpLines(),
				"panel-parent selection must reserve height for the expand hint")
		})
	}

	assert.Positive(t, extraLineWidths, "exercise widths where the expand hint adds a line")
	assert.Positive(t, sameHeightWidths, "exercise widths where the expand hint fits")
}

func TestEnterOnInProgressParentFlashesProgress(t *testing.T) {
	parent := makeJob(10, withStatus(storage.JobStatusRunning),
		withSynthesis("R", storage.PanelSummary{MembersTotal: 3, MembersTerminal: 2}))
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.jobs = []storage.ReviewJob{parent}
	m.selectedJobID, m.selectedIdx = 10, 0
	m2, cmd := pressSpecial(m, tea.KeyEnter)
	assert.Nil(t, cmd, "no review fetch while the panel is still synthesizing")
	assert.Contains(t, m2.flashMessage, "2/3", "flash reports reviewer progress")
}

func TestEnterOnFailedParentOpensError(t *testing.T) {
	assert := assert.New(t)
	parent := makeJob(10, withStatus(storage.JobStatusFailed),
		withSynthesis("R", storage.PanelSummary{MembersTotal: 3, MembersTerminal: 3, MembersFailed: 3}))
	parent.Error = "synthesis boom"
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.jobs = []storage.ReviewJob{parent}
	m.selectedJobID, m.selectedIdx = 10, 0
	m2, _ := pressSpecial(m, tea.KeyEnter)
	assert.Equal(viewReview, m2.currentView, "failed synthesis parent opens its error detail")
	assert.NotNil(m2.currentReview)
	assert.Contains(m2.currentReview.Output, "synthesis boom", "shows the failure, not a synthesizing flash")
	assert.NotContains(m2.flashMessage, "synthesizing", "must not flash still-synthesizing for a failed parent")
}

func TestPanelMembersNeedFetch(t *testing.T) {
	assert := assert.New(t)
	m := newModel(localhostEndpoint, withExternalIODisabled())
	assert.True(m.panelMembersNeedFetch(testUUID("R")), "uncached run needs a fetch")
	m.panelMembers = map[uuid.UUID][]storage.ReviewJob{testUUID("R"): {
		makeJob(11, withPanelMember("R", "default", 0), withStatus(storage.JobStatusDone)),
		makeJob(12, withPanelMember("R", "security", 1), withStatus(storage.JobStatusDone)),
	}}
	assert.False(m.panelMembersNeedFetch(testUUID("R")), "all-terminal cache is fresh, no refetch")
	m.panelMembers[testUUID("R")][1].Status = storage.JobStatusRunning
	assert.True(m.panelMembersNeedFetch(testUUID("R")), "non-terminal cached member triggers a refetch")
}

func TestEnterOnDoneParentFetchesSynthesisReview(t *testing.T) {
	parent := makeJob(10, withStatus(storage.JobStatusDone),
		withSynthesis("R", storage.PanelSummary{MembersTotal: 3, MembersTerminal: 3, MembersSucceeded: 3}))
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.jobs = []storage.ReviewJob{parent}
	m.selectedJobID, m.selectedIdx = 10, 0
	_, cmd := pressSpecial(m, tea.KeyEnter)
	assert.NotNil(t, cmd, "done parent opens the synthesized review")
}

func TestEnterOnMemberFetchesMemberReview(t *testing.T) {
	m := seededPanelModel(t) // members 11,12 done
	m.expandedPanels[testUUID("R")] = true
	m.selectedJobID = 11 // a member row
	_, cmd := pressSpecial(m, tea.KeyEnter)
	assert.NotNil(t, cmd, "enter on a done member opens that member's review")
}

func TestStaleExpandedPanelRunsTriggersOnNonTerminal(t *testing.T) {
	m := seededPanelModel(t)
	m.expandedPanels[testUUID("R")] = true
	m.panelMembers[testUUID("R")][0].Status = storage.JobStatusRunning // one member still running
	runs := m.staleExpandedPanelRuns()
	assert.Equal(t, []uuid.UUID{testUUID("R")}, runs, "expanded panel with a running member is stale")
}

func TestStaleExpandedPanelRunsSkipsAllTerminal(t *testing.T) {
	m := seededPanelModel(t) // members 11,12 both Done
	m.expandedPanels[testUUID("R")] = true
	assert.Empty(t, m.staleExpandedPanelRuns(), "all-terminal panel does not refetch")
}

func TestStaleExpandedPanelRunsSkipsCollapsed(t *testing.T) {
	m := seededPanelModel(t)
	m.panelMembers[testUUID("R")][0].Status = storage.JobStatusRunning
	// R is NOT expanded
	assert.Empty(t, m.staleExpandedPanelRuns(), "collapsed panel does not refetch")
}

func TestJobsRefreshRefetchesStalePanel(t *testing.T) {
	m := seededPanelModel(t)
	m.expandedPanels[testUUID("R")] = true
	m.panelMembers[testUUID("R")][0].Status = storage.JobStatusRunning
	updated, cmd := m.Update(jobsMsg{jobs: m.jobs, append: false, seq: m.fetchSeq})
	_ = updated
	assert.NotNil(t, cmd, "refresh dispatches a member refetch for the stale expanded panel")
}

func TestRefreshedMembersKeepSelection(t *testing.T) {
	m := seededPanelModel(t)
	m.expandedPanels[testUUID("R")] = true
	m.selectedJobID = 12 // a member selected
	fresh := []storage.ReviewJob{
		makeJob(11, withPanelMember("R", "default", 0), withStatus(storage.JobStatusDone)),
		makeJob(12, withPanelMember("R", "security", 1), withStatus(storage.JobStatusDone)),
	}
	updated, _ := m.Update(panelMembersMsg{runUUID: testUUID("R"), members: fresh})
	assert.Equal(t, int64(12), updated.(model).selectedJobID, "refreshed members keep the selected id")
}

func TestStaleExpandedPanelRunsSortsMultipleRuns(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	summary := storage.PanelSummary{MembersTotal: 1}
	// Two synthesis parents listed Z-before-A so the result order proves the
	// sort rather than echoing insertion or map-iteration order.
	m.jobs = []storage.ReviewJob{
		makeJob(20, withRef("synZ"), withStatus(storage.JobStatusRunning), withSynthesis("Z", summary)),
		makeJob(10, withRef("synA"), withStatus(storage.JobStatusRunning), withSynthesis("A", summary)),
	}
	m.panelMembers = map[uuid.UUID][]storage.ReviewJob{
		testUUID("A"): {makeJob(11, withPanelMember("A", "default", 0), withStatus(storage.JobStatusRunning))},
		testUUID("Z"): {makeJob(21, withPanelMember("Z", "default", 0), withStatus(storage.JobStatusRunning))},
	}
	m.expandedPanels[testUUID("Z")] = true
	m.expandedPanels[testUUID("A")] = true
	assert.Equal(t, []uuid.UUID{testUUID("A"), testUUID("Z")}, m.staleExpandedPanelRuns(),
		"multiple stale runs are returned in sorted order")
}

func TestContentNavWalksFlattenedRowsFromMember(t *testing.T) {
	assert := assert.New(t)
	m := seededPanelModel(t)                // jobs [20,10]; members 11,12 (all Done)
	m.expandedPanels[testUUID("R")] = true  // flattened: [20, 10, 11, 12]
	m.selectedJobID, m.selectedIdx = 11, -1 // viewing member 11's review
	m.currentView = viewReview
	m, _ = pressKey(m, 'j') // prev/older → steps to 12 (next member)
	assert.Equal(int64(12), m.selectedJobID, "older steps to the next member")
	m, _ = pressKey(m, 'k') // next/newer → back to 11
	assert.Equal(int64(11), m.selectedJobID, "newer steps back over members")
	m, _ = pressKey(m, 'k') // newer → parent 10
	assert.Equal(int64(10), m.selectedJobID, "newer steps from first member to its parent")
}

// TestMemberAtBoundaryDoesNotPaginate guards the selectedIdx >= 0 clause in
// contentNavBoundary: a member (selectedIdx == -1) at the bottom boundary must
// flash rather than resume pagination, since the pagination-resume path in
// handlers_msg.go is parent-only. canPaginate() is otherwise satisfied here
// (hasMore, not loading, no repo/branch filter), so the gate is the decisive
// factor — the guard assertion below keeps the test from going vacuous.
func TestMemberAtBoundaryDoesNotPaginate(t *testing.T) {
	assert := assert.New(t)
	m := seededPanelModel(t)
	m.expandedPanels[testUUID("R")] = true  // flattened: [20, 10, 11, 12]
	m.hasMore = true                        // canPaginate() would be satisfiable for a parent
	m.loadingJobs = false                   // seeded model marks the initial fetch in-flight
	m.selectedJobID, m.selectedIdx = 12, -1 // last member, bottom row (no eligible row below)
	m.currentView = viewReview
	assert.True(m.canPaginate(), "the selectedIdx gate must be the only thing blocking pagination")
	m, _ = pressKey(m, 'j') // older
	assert.Zero(m.paginateNav, "a member at the boundary must not trigger pagination")
	assert.False(m.loadingMore, "no pagination load for a member at the boundary")
	assert.NotEmpty(m.flashMessage, "flashes the boundary message instead")
}

func TestContentNavFromMemberSkipsIneligible(t *testing.T) {
	// In log view a queued member is ineligible (log predicate = not queued).
	// Log view binds j/k to scrolling; ←/→ are its prev/next (handlers_modal.go).
	m := seededPanelModel(t)
	m.panelMembers[testUUID("R")][1].Status = storage.JobStatusQueued // member 12 queued
	m.expandedPanels[testUUID("R")] = true
	m.selectedJobID, m.selectedIdx = 11, -1
	m.currentView = viewLog
	m.logFromView = viewQueue
	m, _ = pressSpecial(m, tea.KeyLeft) // older from 11 → 12 is queued (skip) → none below
	assert.Equal(t, int64(11), m.selectedJobID, "queued member is skipped in log nav")
	assert.NotEmpty(t, m.flashMessage)
}

func TestContentNavSelectionGoneFlashesStable(t *testing.T) {
	// Selection id not present in the flattened rows (panel collapsed under us):
	// flash and keep the current view stable; do not jump to a parent index.
	m := seededPanelModel(t)
	m.selectedJobID, m.selectedIdx = 11, -1 // member, but R is NOT expanded → 11 not in rows
	m.currentView = viewReview
	member := m.panelMembers[testUUID("R")][0] // job 11
	m.currentReview = &storage.Review{JobID: member.ID, Job: &member}
	before := m.currentReview
	m, _ = pressKey(m, 'k')
	assert.Equal(t, int64(11), m.selectedJobID, "selection unchanged when gone from rows")
	assert.Equal(t, viewReview, m.currentView, "view stays stable")
	assert.Equal(t, before, m.currentReview, "no review swap")
	assert.NotEmpty(t, m.flashMessage)
}

func TestContentNavParentOnlyUnchanged(t *testing.T) {
	// With no panels, content nav must behave exactly as before (parent list).
	jobs := []storage.ReviewJob{
		makeJob(3, withStatus(storage.JobStatusDone)),
		makeJob(2, withStatus(storage.JobStatusDone)),
		makeJob(1, withStatus(storage.JobStatusDone)),
	}
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.jobs = jobs
	m.selectedJobID, m.selectedIdx = 2, 1
	m.currentView = viewReview
	m, _ = pressKey(m, 'j') // older → job 1
	assert.Equal(t, int64(1), m.selectedJobID)
	m, _ = pressKey(m, 'k') // newer → job 2
	m, _ = pressKey(m, 'k') // newer → job 3
	assert.Equal(t, int64(3), m.selectedJobID)
}

func TestPageKeysStillScrollInReview(t *testing.T) {
	m := seededPanelModel(t)
	m.expandedPanels[testUUID("R")] = true
	m.selectedJobID, m.selectedIdx = 11, -1
	m.currentView = viewReview
	m.reviewScroll = 5
	m, _ = pressSpecial(m, tea.KeyPgUp)
	assert.Equal(t, int64(11), m.selectedJobID, "PgUp does not step reviews")
	assert.Less(t, m.reviewScroll, 5, "PgUp still scrolls the review body")
}

// TestContentNavParentHiddenByHideClosedRecovers covers the T7 regression: a
// normal/parent review hidden from the flattened rows by hide-closed (the
// review was closed while open) must fall back to positional nav and open the
// adjacent visible review, not strand the user with a "no longer visible" flash.
// Both directions recover from the hidden anchor over m.jobs.
func TestContentNavParentHiddenByHideClosedRecovers(t *testing.T) {
	// Newest-first, all standalone Done jobs (no panels). Job 2 is closed, so
	// hide-closed removes it from the flattened rows; jobs 3 and 1 stay visible.
	newHidden := func() model {
		m := newModel(localhostEndpoint, withExternalIODisabled())
		m.jobs = []storage.ReviewJob{
			makeJob(3, withStatus(storage.JobStatusDone), withClosed(new(false))),
			makeJob(2, withStatus(storage.JobStatusDone), withClosed(new(true))),
			makeJob(1, withStatus(storage.JobStatusDone), withClosed(new(false))),
		}
		m.hideClosed = true
		// Anchor on the hidden parent (job 2 at m.jobs index 1), as if its
		// review was just closed while open in the review view.
		m.selectedJobID, m.selectedIdx = 2, 1
		m.currentView = viewReview
		m.currentReview = &storage.Review{ID: 20, Job: &m.jobs[1]}
		return m
	}

	tests := []struct {
		name    string
		key     rune
		wantID  int64
		wantIdx int
	}{
		{"older recovers to adjacent visible review", 'j', 1, 2},
		{"newer recovers to adjacent visible review", 'k', 3, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			m := newHidden()
			assert.False(m.selectedIsMember(), "the hidden selection is a parent, not a member")
			assert.Negative(m.selectedRowIndex(m.visibleQueueRows()),
				"the hidden parent is not in the flattened rows")

			m, cmd := pressKey(m, tt.key)
			assert.Equal(tt.wantID, m.selectedJobID, "recovers to the adjacent visible review")
			assert.Equal(tt.wantIdx, m.selectedIdx, "selectedIdx is re-derived from the recovered job")
			assert.NotNil(cmd, "the recovered review is fetched")
			assert.Empty(m.flashMessage, "positional recovery does not flash 'no longer visible'")
		})
	}
}

// TestContentNavHiddenMemberFlashesStable covers the complementary case: a
// selected panel member whose panel collapsed (so it is in panelMembers but not
// m.jobs and not in the flattened rows) keeps flash-and-stay — no positional
// jump, since a member has no m.jobs anchor.
func TestContentNavHiddenMemberFlashesStable(t *testing.T) {
	assert := assert.New(t)
	m := seededPanelModel(t)                // members 11,12 in panelMembers, not m.jobs
	m.selectedJobID, m.selectedIdx = 11, -1 // member, but R is NOT expanded → 11 absent from rows
	m.currentView = viewReview
	member := m.panelMembers[testUUID("R")][0] // job 11
	m.currentReview = &storage.Review{JobID: member.ID, Job: &member}
	before := m.currentReview

	assert.True(m.selectedIsMember(), "selection is a side-fetched member")
	m, cmd := pressKey(m, 'j') // older
	assert.Equal(int64(11), m.selectedJobID, "an absent member stays put")
	assert.Equal(viewReview, m.currentView, "view stays stable")
	assert.Equal(before, m.currentReview, "no review swap for an absent member")
	assert.Nil(cmd, "no fetch for an absent member")
	assert.NotEmpty(m.flashMessage, "flashes 'Selection no longer visible'")
}

// TestContentNavParentOmittedFromJobsRecovers covers the real hide-closed flow
// where the closed parent is dropped from m.jobs entirely by a server-side
// refresh (not merely marked closed in place). selectedIdx is preserved and now
// points at the shifted successor, so the positional fallback must start there
// rather than at selectedIdx+dir, or older-nav skips the true-adjacent review.
func TestContentNavParentOmittedFromJobsRecovers(t *testing.T) {
	// Originally [4,3,2,1] with job 3 selected (idx 1). Job 3 was closed and a
	// refresh omitted it: m.jobs is now [4,2,1] but selectedIdx stays 1 (now
	// job 2). selectedJobID 3 is absent from m.jobs.
	newOmitted := func() model {
		m := newModel(localhostEndpoint, withExternalIODisabled())
		m.jobs = []storage.ReviewJob{
			makeJob(4, withStatus(storage.JobStatusDone)),
			makeJob(2, withStatus(storage.JobStatusDone)),
			makeJob(1, withStatus(storage.JobStatusDone)),
		}
		omitted := makeJob(3, withStatus(storage.JobStatusDone))
		m.selectedJobID, m.selectedIdx = 3, 1
		m.currentView = viewReview
		m.currentReview = &storage.Review{ID: 30, Job: &omitted}
		return m
	}

	tests := []struct {
		name   string
		key    rune
		wantID int64
	}{
		{"older lands on the true-adjacent review, not skipping it", 'j', 2},
		{"newer lands on the adjacent newer review", 'k', 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			m := newOmitted()
			assert.False(m.selectedIsMember(), "an omitted parent is not a member")
			m, cmd := pressKey(m, tt.key)
			assert.Equal(tt.wantID, m.selectedJobID, "recovers to the adjacent review without skipping")
			assert.NotNil(cmd, "the recovered review is fetched")
			assert.Empty(m.flashMessage, "omitted-parent recovery does not flash")
		})
	}
}

func TestQueueReasoningColumn(t *testing.T) {
	assert := assert.New(t)
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.width, m.height = 200, 20
	job := makeJob(1, withAgent("test"))
	job.Reasoning = "xhigh"
	m.jobs = []storage.ReviewJob{job}
	m.selectedIdx, m.selectedJobID = 0, 1
	output := stripTestANSI(m.renderQueueView())
	assert.Contains(output, "Reasoning")
	assert.Contains(output, "xhigh")

	m.hiddenColumns = parseHiddenColumns([]string{"reasoning"})
	output = stripTestANSI(m.renderQueueView())
	assert.NotContains(output, "Reasoning")
	assert.NotContains(output, "xhigh")
}
