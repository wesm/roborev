package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	neturl "net/url"
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
	"github.com/wesm/roborev/internal/storage"
	"github.com/wesm/roborev/internal/update"
	"github.com/wesm/roborev/internal/version"
)

// TUI styles
var (
	tuiTitleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("205"))

	tuiStatusStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241"))

	tuiSelectedStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("212"))

	tuiQueuedStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("226")) // Yellow
	tuiRunningStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("33"))  // Blue
	tuiDoneStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("46"))  // Green
	tuiFailedStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("196")) // Red
	tuiCanceledStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("208")) // Orange

	tuiHelpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241"))
)

type tuiView int

const (
	tuiViewQueue tuiView = iota
	tuiViewReview
	tuiViewPrompt
	tuiViewFilter
)

// repoFilterItem represents a repo in the filter modal with its review count
type repoFilterItem struct {
	name  string // Empty string means "All repos"
	count int
}

type tuiModel struct {
	serverAddr    string
	daemonVersion string
	client        *http.Client
	jobs            []storage.ReviewJob
	status          storage.DaemonStatus
	selectedIdx     int
	selectedJobID   int64 // Track selected job by ID to maintain position on refresh
	currentView     tuiView
	currentReview   *storage.Review
	reviewScroll    int
	promptScroll    int
	promptFromQueue bool // true if prompt view was entered from queue (not review)
	width           int
	height          int
	err             error
	updateAvailable string // Latest version if update available, empty if up to date

	// Filter modal state
	filterRepos       []repoFilterItem // Available repos with counts
	filterSelectedIdx int              // Currently highlighted repo in filter list
	filterSearch      string           // Search/filter text typed by user

	// Active filter (applied to queue view)
	activeRepoFilter string // Empty = show all, otherwise repo name to filter by
}

type tuiTickMsg time.Time
type tuiJobsMsg []storage.ReviewJob
type tuiStatusMsg storage.DaemonStatus
type tuiReviewMsg *storage.Review
type tuiPromptMsg *storage.Review
type tuiAddressedMsg bool
type tuiAddressedResultMsg struct {
	jobID      int64 // job ID for queue view rollback
	reviewID   int64 // review ID for review view rollback
	reviewView bool  // true if from review view (rollback currentReview)
	oldState   bool
	err        error
}
type tuiCancelResultMsg struct {
	jobID         int64
	oldState      storage.JobStatus
	oldFinishedAt *time.Time
	err           error
}
type tuiErrMsg error
type tuiUpdateCheckMsg string // Latest version if available, empty if up to date
type tuiReposMsg struct {
	repos      []repoFilterItem
	totalCount int
}

func newTuiModel(serverAddr string) tuiModel {
	return tuiModel{
		serverAddr:    serverAddr,
		daemonVersion: "?", // Updated from /api/status response
		client:        &http.Client{Timeout: 10 * time.Second},
		jobs:          []storage.ReviewJob{},
		currentView:   tuiViewQueue,
		width:         80, // sensible defaults until we get WindowSizeMsg
		height:        24,
	}
}

func (m tuiModel) Init() tea.Cmd {
	return tea.Batch(
		tea.WindowSize(), // request initial window size
		m.tick(),
		m.fetchJobs(),
		m.fetchStatus(),
		m.checkForUpdate(),
	)
}

func (m tuiModel) tick() tea.Cmd {
	return tea.Tick(2*time.Second, func(t time.Time) tea.Msg {
		return tuiTickMsg(t)
	})
}

func (m tuiModel) fetchJobs() tea.Cmd {
	return func() tea.Msg {
		// No limit (limit=0) when filtering to show full repo history, otherwise limit to 50
		var url string
		if m.activeRepoFilter != "" {
			url = fmt.Sprintf("%s/api/jobs?limit=0&repo=%s", m.serverAddr, neturl.QueryEscape(m.activeRepoFilter))
		} else {
			url = fmt.Sprintf("%s/api/jobs?limit=50", m.serverAddr)
		}
		resp, err := m.client.Get(url)
		if err != nil {
			return tuiErrMsg(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return tuiErrMsg(fmt.Errorf("fetch jobs: %s", resp.Status))
		}

		var result struct {
			Jobs []storage.ReviewJob `json:"jobs"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return tuiErrMsg(err)
		}
		return tuiJobsMsg(result.Jobs)
	}
}

func (m tuiModel) fetchStatus() tea.Cmd {
	return func() tea.Msg {
		resp, err := m.client.Get(m.serverAddr + "/api/status")
		if err != nil {
			return tuiErrMsg(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return tuiErrMsg(fmt.Errorf("fetch status: %s", resp.Status))
		}

		var status storage.DaemonStatus
		if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
			return tuiErrMsg(err)
		}
		return tuiStatusMsg(status)
	}
}

func (m tuiModel) checkForUpdate() tea.Cmd {
	return func() tea.Msg {
		info, err := update.CheckForUpdate(false) // Use cache
		if err != nil || info == nil {
			return tuiUpdateCheckMsg("") // No update or error
		}
		return tuiUpdateCheckMsg(info.LatestVersion)
	}
}

func (m tuiModel) fetchRepos() tea.Cmd {
	return func() tea.Msg {
		resp, err := m.client.Get(m.serverAddr + "/api/repos")
		if err != nil {
			return tuiErrMsg(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return tuiErrMsg(fmt.Errorf("fetch repos: %s", resp.Status))
		}

		var result struct {
			Repos []struct {
				Name  string `json:"name"`
				Count int    `json:"count"`
			} `json:"repos"`
			TotalCount int `json:"total_count"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return tuiErrMsg(err)
		}

		// Convert to repoFilterItem slice
		repos := make([]repoFilterItem, len(result.Repos))
		for i, r := range result.Repos {
			repos[i] = repoFilterItem{name: r.Name, count: r.Count}
		}
		return tuiReposMsg{repos: repos, totalCount: result.TotalCount}
	}
}

func (m tuiModel) fetchReview(jobID int64) tea.Cmd {
	return func() tea.Msg {
		resp, err := m.client.Get(fmt.Sprintf("%s/api/review?job_id=%d", m.serverAddr, jobID))
		if err != nil {
			return tuiErrMsg(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusNotFound {
			return tuiErrMsg(fmt.Errorf("no review found"))
		}
		if resp.StatusCode != http.StatusOK {
			return tuiErrMsg(fmt.Errorf("fetch review: %s", resp.Status))
		}

		var review storage.Review
		if err := json.NewDecoder(resp.Body).Decode(&review); err != nil {
			return tuiErrMsg(err)
		}
		return tuiReviewMsg(&review)
	}
}

func (m tuiModel) fetchReviewForPrompt(jobID int64) tea.Cmd {
	return func() tea.Msg {
		resp, err := m.client.Get(fmt.Sprintf("%s/api/review?job_id=%d", m.serverAddr, jobID))
		if err != nil {
			return tuiErrMsg(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusNotFound {
			return tuiErrMsg(fmt.Errorf("no review found"))
		}
		if resp.StatusCode != http.StatusOK {
			return tuiErrMsg(fmt.Errorf("fetch review: %s", resp.Status))
		}

		var review storage.Review
		if err := json.NewDecoder(resp.Body).Decode(&review); err != nil {
			return tuiErrMsg(err)
		}
		return tuiPromptMsg(&review)
	}
}

func (m tuiModel) addressReview(reviewID, jobID int64, newState, oldState bool) tea.Cmd {
	return func() tea.Msg {
		reqBody, err := json.Marshal(map[string]interface{}{
			"review_id": reviewID,
			"addressed": newState,
		})
		if err != nil {
			return tuiAddressedResultMsg{reviewID: reviewID, jobID: jobID, reviewView: true, oldState: oldState, err: err}
		}
		resp, err := m.client.Post(m.serverAddr+"/api/review/address", "application/json", bytes.NewReader(reqBody))
		if err != nil {
			return tuiAddressedResultMsg{reviewID: reviewID, jobID: jobID, reviewView: true, oldState: oldState, err: err}
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusNotFound {
			return tuiAddressedResultMsg{reviewID: reviewID, jobID: jobID, reviewView: true, oldState: oldState, err: fmt.Errorf("review not found")}
		}
		if resp.StatusCode != http.StatusOK {
			return tuiAddressedResultMsg{reviewID: reviewID, jobID: jobID, reviewView: true, oldState: oldState, err: fmt.Errorf("mark review: %s", resp.Status)}
		}
		return tuiAddressedResultMsg{reviewID: reviewID, jobID: jobID, reviewView: true, oldState: oldState, err: nil}
	}
}

// addressReviewInBackground fetches the review ID and updates addressed status.
// Used for optimistic updates from queue view - UI already updated, this syncs to server.
// On error, returns tuiAddressedResultMsg with oldState for rollback.
func (m tuiModel) addressReviewInBackground(jobID int64, newState, oldState bool) tea.Cmd {
	return func() tea.Msg {
		// Fetch the review to get its ID
		resp, err := m.client.Get(fmt.Sprintf("%s/api/review?job_id=%d", m.serverAddr, jobID))
		if err != nil {
			return tuiAddressedResultMsg{jobID: jobID, oldState: oldState, err: err}
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusNotFound {
			return tuiAddressedResultMsg{jobID: jobID, oldState: oldState, err: fmt.Errorf("no review for this job")}
		}
		if resp.StatusCode != http.StatusOK {
			return tuiAddressedResultMsg{jobID: jobID, oldState: oldState, err: fmt.Errorf("fetch review: %s", resp.Status)}
		}

		var review storage.Review
		if err := json.NewDecoder(resp.Body).Decode(&review); err != nil {
			return tuiAddressedResultMsg{jobID: jobID, oldState: oldState, err: err}
		}

		// Now mark it
		reqBody, err := json.Marshal(map[string]interface{}{
			"review_id": review.ID,
			"addressed": newState,
		})
		if err != nil {
			return tuiAddressedResultMsg{jobID: jobID, oldState: oldState, err: err}
		}
		resp2, err := m.client.Post(m.serverAddr+"/api/review/address", "application/json", bytes.NewReader(reqBody))
		if err != nil {
			return tuiAddressedResultMsg{jobID: jobID, oldState: oldState, err: err}
		}
		defer resp2.Body.Close()

		if resp2.StatusCode != http.StatusOK {
			return tuiAddressedResultMsg{jobID: jobID, oldState: oldState, err: fmt.Errorf("mark review: %s", resp2.Status)}
		}
		// Success
		return tuiAddressedResultMsg{jobID: jobID, oldState: oldState, err: nil}
	}
}

func (m tuiModel) toggleAddressedForJob(jobID int64, currentState *bool) tea.Cmd {
	return func() tea.Msg {
		// Fetch the review to get its ID
		resp, err := m.client.Get(fmt.Sprintf("%s/api/review?job_id=%d", m.serverAddr, jobID))
		if err != nil {
			return tuiErrMsg(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusNotFound {
			return tuiErrMsg(fmt.Errorf("no review for this job"))
		}
		if resp.StatusCode != http.StatusOK {
			return tuiErrMsg(fmt.Errorf("fetch review: %s", resp.Status))
		}

		var review storage.Review
		if err := json.NewDecoder(resp.Body).Decode(&review); err != nil {
			return tuiErrMsg(err)
		}

		// Toggle the state
		newState := true
		if currentState != nil && *currentState {
			newState = false
		}

		// Now mark it
		reqBody, err := json.Marshal(map[string]interface{}{
			"review_id": review.ID,
			"addressed": newState,
		})
		if err != nil {
			return tuiErrMsg(err)
		}
		resp2, err := m.client.Post(m.serverAddr+"/api/review/address", "application/json", bytes.NewReader(reqBody))
		if err != nil {
			return tuiErrMsg(err)
		}
		defer resp2.Body.Close()

		if resp2.StatusCode == http.StatusNotFound {
			return tuiErrMsg(fmt.Errorf("review not found"))
		}
		if resp2.StatusCode != http.StatusOK {
			return tuiErrMsg(fmt.Errorf("mark review: %s", resp2.Status))
		}
		return tuiAddressedMsg(newState)
	}
}

// updateSelectedJobID updates the tracked job ID after navigation
func (m *tuiModel) updateSelectedJobID() {
	if m.selectedIdx >= 0 && m.selectedIdx < len(m.jobs) {
		m.selectedJobID = m.jobs[m.selectedIdx].ID
	}
}

// setJobAddressed updates the addressed state for a job by ID.
// Handles nil pointer by allocating if necessary.
func (m *tuiModel) setJobAddressed(jobID int64, state bool) {
	for i := range m.jobs {
		if m.jobs[i].ID == jobID {
			if m.jobs[i].Addressed == nil {
				m.jobs[i].Addressed = new(bool)
			}
			*m.jobs[i].Addressed = state
			return
		}
	}
}

// setJobStatus updates the status for a job by ID
func (m *tuiModel) setJobStatus(jobID int64, status storage.JobStatus) {
	for i := range m.jobs {
		if m.jobs[i].ID == jobID {
			m.jobs[i].Status = status
			return
		}
	}
}

// setJobFinishedAt updates the FinishedAt for a job by ID
func (m *tuiModel) setJobFinishedAt(jobID int64, finishedAt *time.Time) {
	for i := range m.jobs {
		if m.jobs[i].ID == jobID {
			m.jobs[i].FinishedAt = finishedAt
			return
		}
	}
}

// cancelJob sends a cancel request to the server
func (m tuiModel) cancelJob(jobID int64, oldStatus storage.JobStatus, oldFinishedAt *time.Time) tea.Cmd {
	return func() tea.Msg {
		reqBody, err := json.Marshal(map[string]interface{}{
			"job_id": jobID,
		})
		if err != nil {
			return tuiCancelResultMsg{jobID: jobID, oldState: oldStatus, oldFinishedAt: oldFinishedAt, err: err}
		}
		resp, err := m.client.Post(m.serverAddr+"/api/job/cancel", "application/json", bytes.NewReader(reqBody))
		if err != nil {
			return tuiCancelResultMsg{jobID: jobID, oldState: oldStatus, oldFinishedAt: oldFinishedAt, err: err}
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusNotFound {
			return tuiCancelResultMsg{jobID: jobID, oldState: oldStatus, oldFinishedAt: oldFinishedAt, err: fmt.Errorf("job not cancellable")}
		}
		if resp.StatusCode != http.StatusOK {
			return tuiCancelResultMsg{jobID: jobID, oldState: oldStatus, oldFinishedAt: oldFinishedAt, err: fmt.Errorf("cancel job: %s", resp.Status)}
		}
		return tuiCancelResultMsg{jobID: jobID, oldState: oldStatus, oldFinishedAt: oldFinishedAt, err: nil}
	}
}

// getVisibleFilterRepos returns repos that match the current search filter
func (m *tuiModel) getVisibleFilterRepos() []repoFilterItem {
	if m.filterSearch == "" {
		return m.filterRepos
	}
	search := strings.ToLower(m.filterSearch)
	var visible []repoFilterItem
	for _, r := range m.filterRepos {
		// Always include "All repos" option, filter others by search
		if r.name == "" || strings.Contains(strings.ToLower(r.name), search) {
			visible = append(visible, r)
		}
	}
	return visible
}

// filterNavigateUp moves selection up in the filter modal
func (m *tuiModel) filterNavigateUp() {
	if m.filterSelectedIdx > 0 {
		m.filterSelectedIdx--
	}
}

// filterNavigateDown moves selection down in the filter modal
func (m *tuiModel) filterNavigateDown() {
	visible := m.getVisibleFilterRepos()
	if m.filterSelectedIdx < len(visible)-1 {
		m.filterSelectedIdx++
	}
}

// getSelectedFilterRepo returns the currently selected repo in the filter modal
func (m *tuiModel) getSelectedFilterRepo() *repoFilterItem {
	visible := m.getVisibleFilterRepos()
	if m.filterSelectedIdx >= 0 && m.filterSelectedIdx < len(visible) {
		return &visible[m.filterSelectedIdx]
	}
	return nil
}

// getVisibleJobs returns jobs filtered by the active repo filter
func (m tuiModel) getVisibleJobs() []storage.ReviewJob {
	if m.activeRepoFilter == "" {
		return m.jobs
	}
	var visible []storage.ReviewJob
	for _, job := range m.jobs {
		if job.RepoName == m.activeRepoFilter {
			visible = append(visible, job)
		}
	}
	return visible
}

// findVisibleJobIndex finds the index in m.jobs for the nth visible job
func (m tuiModel) findVisibleJobIndex(visibleIdx int) int {
	if m.activeRepoFilter == "" {
		return visibleIdx
	}
	count := 0
	for i, job := range m.jobs {
		if job.RepoName == m.activeRepoFilter {
			if count == visibleIdx {
				return i
			}
			count++
		}
	}
	return -1
}

// getVisibleSelectedIdx returns the index within visible jobs for the current selection
func (m tuiModel) getVisibleSelectedIdx() int {
	if m.activeRepoFilter == "" {
		return m.selectedIdx
	}
	count := 0
	for i, job := range m.jobs {
		if job.RepoName == m.activeRepoFilter {
			if i == m.selectedIdx {
				return count
			}
			count++
		}
	}
	return 0
}

// findNextVisibleJob finds the next job index in m.jobs that matches the filter
// Returns -1 if no next visible job exists
func (m tuiModel) findNextVisibleJob(currentIdx int) int {
	for i := currentIdx + 1; i < len(m.jobs); i++ {
		if m.activeRepoFilter == "" || m.jobs[i].RepoName == m.activeRepoFilter {
			return i
		}
	}
	return -1
}

// findPrevVisibleJob finds the previous job index in m.jobs that matches the filter
// Returns -1 if no previous visible job exists
func (m tuiModel) findPrevVisibleJob(currentIdx int) int {
	for i := currentIdx - 1; i >= 0; i-- {
		if m.activeRepoFilter == "" || m.jobs[i].RepoName == m.activeRepoFilter {
			return i
		}
	}
	return -1
}

// findFirstVisibleJob finds the first job index that matches the filter
func (m tuiModel) findFirstVisibleJob() int {
	for i, job := range m.jobs {
		if m.activeRepoFilter == "" || job.RepoName == m.activeRepoFilter {
			return i
		}
	}
	return -1
}

// findLastVisibleJob finds the last job index that matches the filter
func (m tuiModel) findLastVisibleJob() int {
	for i := len(m.jobs) - 1; i >= 0; i-- {
		if m.activeRepoFilter == "" || m.jobs[i].RepoName == m.activeRepoFilter {
			return i
		}
	}
	return -1
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		// Handle filter view first (it captures most keys for typing)
		if m.currentView == tuiViewFilter {
			switch msg.String() {
			case "ctrl+c":
				return m, tea.Quit
			case "esc", "q":
				m.currentView = tuiViewQueue
				m.filterSearch = ""
				return m, nil
			case "up", "k":
				m.filterNavigateUp()
				return m, nil
			case "down", "j":
				m.filterNavigateDown()
				return m, nil
			case "enter":
				selected := m.getSelectedFilterRepo()
				if selected != nil {
					m.activeRepoFilter = selected.name
					m.currentView = tuiViewQueue
					m.filterSearch = ""
					// Invalidate selection until refetch completes - prevents
					// actions on stale jobs list before new data arrives
					m.selectedIdx = -1
					m.selectedJobID = 0
					// Refetch jobs with the new filter applied at the API level
					return m, m.fetchJobs()
				}
				return m, nil
			case "backspace":
				if len(m.filterSearch) > 0 {
					m.filterSearch = m.filterSearch[:len(m.filterSearch)-1]
					m.filterSelectedIdx = 0 // Reset selection when search changes
				}
				return m, nil
			default:
				// Handle typing for search (supports non-ASCII runes)
				if len(msg.Runes) > 0 {
					for _, r := range msg.Runes {
						if unicode.IsPrint(r) && !unicode.IsControl(r) {
							m.filterSearch += string(r)
							m.filterSelectedIdx = 0 // Reset selection when search changes
						}
					}
				}
				return m, nil
			}
		}

		switch msg.String() {
		case "ctrl+c", "q":
			if m.currentView == tuiViewReview {
				m.currentView = tuiViewQueue
				m.currentReview = nil
				m.reviewScroll = 0
				return m, nil
			}
			if m.currentView == tuiViewPrompt {
				// Go back to where we came from
				if m.promptFromQueue {
					m.currentView = tuiViewQueue
					m.currentReview = nil
					m.promptScroll = 0
				} else {
					m.currentView = tuiViewReview
					m.promptScroll = 0
				}
				return m, nil
			}
			return m, tea.Quit

		case "up", "k":
			if m.currentView == tuiViewQueue {
				// Navigate to previous visible job (respects filter)
				prevIdx := m.findPrevVisibleJob(m.selectedIdx)
				if prevIdx >= 0 {
					m.selectedIdx = prevIdx
					m.updateSelectedJobID()
				}
			} else if m.currentView == tuiViewReview {
				if m.reviewScroll > 0 {
					m.reviewScroll--
				}
			} else if m.currentView == tuiViewPrompt {
				if m.promptScroll > 0 {
					m.promptScroll--
				}
			}

		case "down", "j":
			if m.currentView == tuiViewQueue {
				// Navigate to next visible job (respects filter)
				nextIdx := m.findNextVisibleJob(m.selectedIdx)
				if nextIdx >= 0 {
					m.selectedIdx = nextIdx
					m.updateSelectedJobID()
				}
			} else if m.currentView == tuiViewReview {
				m.reviewScroll++
			} else if m.currentView == tuiViewPrompt {
				m.promptScroll++
			}

		case "pgup":
			pageSize := max(1, m.height-10)
			if m.currentView == tuiViewQueue {
				// Move up by pageSize visible jobs
				for i := 0; i < pageSize; i++ {
					prevIdx := m.findPrevVisibleJob(m.selectedIdx)
					if prevIdx < 0 {
						break
					}
					m.selectedIdx = prevIdx
				}
				m.updateSelectedJobID()
			} else if m.currentView == tuiViewReview {
				m.reviewScroll = max(0, m.reviewScroll-pageSize)
			} else if m.currentView == tuiViewPrompt {
				m.promptScroll = max(0, m.promptScroll-pageSize)
			}

		case "pgdown":
			pageSize := max(1, m.height-10)
			if m.currentView == tuiViewQueue {
				// Move down by pageSize visible jobs
				for i := 0; i < pageSize; i++ {
					nextIdx := m.findNextVisibleJob(m.selectedIdx)
					if nextIdx < 0 {
						break
					}
					m.selectedIdx = nextIdx
				}
				m.updateSelectedJobID()
			} else if m.currentView == tuiViewReview {
				m.reviewScroll += pageSize
			} else if m.currentView == tuiViewPrompt {
				m.promptScroll += pageSize
			}

		case "enter":
			if m.currentView == tuiViewQueue && len(m.jobs) > 0 && m.selectedIdx >= 0 && m.selectedIdx < len(m.jobs) {
				job := m.jobs[m.selectedIdx]
				if job.Status == storage.JobStatusDone {
					return m, m.fetchReview(job.ID)
				} else if job.Status == storage.JobStatusFailed {
					// Show error inline for failed jobs
					m.currentReview = &storage.Review{
						Agent:  job.Agent,
						Output: "Job failed:\n\n" + job.Error,
						Job:    &job,
					}
					m.currentView = tuiViewReview
					m.reviewScroll = 0
					return m, nil
				}
			}

		case "p":
			if m.currentView == tuiViewQueue && len(m.jobs) > 0 && m.selectedIdx >= 0 && m.selectedIdx < len(m.jobs) {
				job := m.jobs[m.selectedIdx]
				if job.Status == storage.JobStatusDone {
					// Fetch review and go directly to prompt view
					m.promptFromQueue = true
					return m, m.fetchReviewForPrompt(job.ID)
				} else if job.Status == storage.JobStatusRunning && job.Prompt != "" {
					// Show prompt from job directly for running jobs
					m.currentReview = &storage.Review{
						Agent:  job.Agent,
						Prompt: job.Prompt,
						Job:    &job,
					}
					m.currentView = tuiViewPrompt
					m.promptScroll = 0
					m.promptFromQueue = true
					return m, nil
				}
			} else if m.currentView == tuiViewReview && m.currentReview != nil && m.currentReview.Prompt != "" {
				m.currentView = tuiViewPrompt
				m.promptScroll = 0
				m.promptFromQueue = false
			} else if m.currentView == tuiViewPrompt {
				// Toggle back: go to review if came from review, queue if came from queue
				if m.promptFromQueue {
					m.currentView = tuiViewQueue
					m.currentReview = nil
					m.promptScroll = 0
				} else {
					m.currentView = tuiViewReview
					m.promptScroll = 0
				}
			}

		case "a":
			// Toggle addressed status (optimistic update - UI updates immediately)
			if m.currentView == tuiViewReview && m.currentReview != nil && m.currentReview.ID > 0 {
				oldState := m.currentReview.Addressed
				newState := !oldState
				m.currentReview.Addressed = newState // Optimistic update
				// Also update the job in queue so it's consistent when returning
				var jobID int64
				if m.currentReview.Job != nil {
					jobID = m.currentReview.Job.ID
					m.setJobAddressed(jobID, newState)
				}
				return m, m.addressReview(m.currentReview.ID, jobID, newState, oldState)
			} else if m.currentView == tuiViewQueue && len(m.jobs) > 0 && m.selectedIdx >= 0 && m.selectedIdx < len(m.jobs) {
				job := &m.jobs[m.selectedIdx]
				if job.Status == storage.JobStatusDone && job.Addressed != nil {
					oldState := *job.Addressed
					newState := !oldState
					*job.Addressed = newState // Optimistic update
					return m, m.addressReviewInBackground(job.ID, newState, oldState)
				}
			}

		case "x":
			// Cancel a running or queued job (optimistic update)
			if m.currentView == tuiViewQueue && len(m.jobs) > 0 && m.selectedIdx >= 0 && m.selectedIdx < len(m.jobs) {
				job := &m.jobs[m.selectedIdx]
				if job.Status == storage.JobStatusRunning || job.Status == storage.JobStatusQueued {
					oldStatus := job.Status
					oldFinishedAt := job.FinishedAt // Save for rollback
					job.Status = storage.JobStatusCanceled // Optimistic update
					now := time.Now()
					job.FinishedAt = &now // Stop elapsed time from ticking
					return m, m.cancelJob(job.ID, oldStatus, oldFinishedAt)
				}
			}

		case "f":
			// Open filter modal
			if m.currentView == tuiViewQueue {
				m.filterRepos = nil // Clear previous repos (will show loading)
				m.filterSelectedIdx = 0
				m.filterSearch = ""
				m.currentView = tuiViewFilter
				return m, m.fetchRepos()
			}

		case "esc":
			if m.currentView == tuiViewQueue && m.activeRepoFilter != "" {
				// Clear filter and refetch all jobs
				m.activeRepoFilter = ""
				// Invalidate selection until refetch completes
				m.selectedIdx = -1
				m.selectedJobID = 0
				return m, m.fetchJobs()
			} else if m.currentView == tuiViewReview {
				m.currentView = tuiViewQueue
				m.currentReview = nil
				m.reviewScroll = 0
			} else if m.currentView == tuiViewPrompt {
				// Go back to where we came from
				if m.promptFromQueue {
					m.currentView = tuiViewQueue
					m.currentReview = nil
					m.promptScroll = 0
				} else {
					m.currentView = tuiViewReview
					m.promptScroll = 0
				}
			}
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case tuiTickMsg:
		return m, tea.Batch(m.tick(), m.fetchJobs(), m.fetchStatus())

	case tuiJobsMsg:
		m.jobs = msg

		if len(m.jobs) == 0 {
			m.selectedIdx = -1
			m.selectedJobID = 0
		} else if m.selectedJobID > 0 {
			// Try to find the previously selected job by ID
			found := false
			for i, job := range m.jobs {
				if job.ID == m.selectedJobID {
					m.selectedIdx = i
					found = true
					break
				}
			}

			if !found {
				// Job was removed - clamp index to valid range
				m.selectedIdx = max(0, min(len(m.jobs)-1, m.selectedIdx))
				// If filter is active, ensure we're on a visible job
				if m.activeRepoFilter != "" {
					firstVisible := m.findFirstVisibleJob()
					if firstVisible >= 0 {
						m.selectedIdx = firstVisible
						m.selectedJobID = m.jobs[firstVisible].ID
					} else {
						// No visible jobs for this filter
						m.selectedIdx = -1
						m.selectedJobID = 0
					}
				} else {
					m.selectedJobID = m.jobs[m.selectedIdx].ID
				}
			} else if m.activeRepoFilter != "" {
				// Job exists but check if it matches the filter
				if m.jobs[m.selectedIdx].RepoName != m.activeRepoFilter {
					// Selected job doesn't match filter, select first visible
					firstVisible := m.findFirstVisibleJob()
					if firstVisible >= 0 {
						m.selectedIdx = firstVisible
						m.selectedJobID = m.jobs[firstVisible].ID
					} else {
						// No visible jobs for this filter
						m.selectedIdx = -1
						m.selectedJobID = 0
					}
				}
			}
		} else {
			// No job was selected yet, select first visible job
			firstVisible := m.findFirstVisibleJob()
			if firstVisible >= 0 {
				m.selectedIdx = firstVisible
				m.selectedJobID = m.jobs[firstVisible].ID
			} else if m.activeRepoFilter == "" && len(m.jobs) > 0 {
				// No filter, just select first job
				m.selectedIdx = 0
				m.selectedJobID = m.jobs[0].ID
			} else {
				// No visible jobs
				m.selectedIdx = -1
				m.selectedJobID = 0
			}
		}

	case tuiStatusMsg:
		m.status = storage.DaemonStatus(msg)
		if m.status.Version != "" {
			m.daemonVersion = m.status.Version
		}

	case tuiUpdateCheckMsg:
		m.updateAvailable = string(msg)

	case tuiReviewMsg:
		m.currentReview = msg
		m.currentView = tuiViewReview
		m.reviewScroll = 0

	case tuiPromptMsg:
		m.currentReview = msg
		m.currentView = tuiViewPrompt
		m.promptScroll = 0

	case tuiAddressedMsg:
		if m.currentReview != nil {
			m.currentReview.Addressed = bool(msg)
		}

	case tuiAddressedResultMsg:
		if msg.err != nil {
			// Rollback optimistic update on error
			if msg.reviewView {
				// Rollback review view only if still viewing the same review
				if m.currentReview != nil && m.currentReview.ID == msg.reviewID {
					m.currentReview.Addressed = msg.oldState
				}
			}
			// Always rollback the job in queue
			if msg.jobID > 0 {
				m.setJobAddressed(msg.jobID, msg.oldState)
			}
			m.err = msg.err
		}

	case tuiCancelResultMsg:
		if msg.err != nil {
			// Rollback optimistic update on error (both status and finishedAt)
			m.setJobStatus(msg.jobID, msg.oldState)
			m.setJobFinishedAt(msg.jobID, msg.oldFinishedAt)
			m.err = msg.err
		}

	case tuiReposMsg:
		// Populate filter repos with "All repos" as first option
		m.filterRepos = []repoFilterItem{{name: "", count: msg.totalCount}}
		m.filterRepos = append(m.filterRepos, msg.repos...)
		// Pre-select current filter if active
		if m.activeRepoFilter != "" {
			for i, r := range m.filterRepos {
				if r.name == m.activeRepoFilter {
					m.filterSelectedIdx = i
					break
				}
			}
		}

	case tuiErrMsg:
		m.err = msg
	}

	return m, nil
}

func (m tuiModel) View() string {
	if m.currentView == tuiViewFilter {
		return m.renderFilterView()
	}
	if m.currentView == tuiViewPrompt && m.currentReview != nil {
		return m.renderPromptView()
	}
	if m.currentView == tuiViewReview && m.currentReview != nil {
		return m.renderReviewView()
	}
	return m.renderQueueView()
}

func (m tuiModel) renderQueueView() string {
	var b strings.Builder

	// Title with version, optional update notification, and filter indicator
	title := fmt.Sprintf("RoboRev Queue (%s)", version.Version)
	if m.activeRepoFilter != "" {
		title += fmt.Sprintf(" [f: %s]", m.activeRepoFilter)
	}
	b.WriteString(tuiTitleStyle.Render(title))
	b.WriteString("\n")

	// Status line - show filtered counts when filter is active
	var statusLine string
	if m.activeRepoFilter != "" {
		// Calculate counts from jobs (all pre-filtered by API)
		var queued, running, done, failed, canceled int
		for _, job := range m.jobs {
			switch job.Status {
			case storage.JobStatusQueued:
				queued++
			case storage.JobStatusRunning:
				running++
			case storage.JobStatusDone:
				done++
			case storage.JobStatusFailed:
				failed++
			case storage.JobStatusCanceled:
				canceled++
			}
		}
		statusLine = fmt.Sprintf("Daemon: %s | Queued: %d | Running: %d | Done: %d | Failed: %d | Canceled: %d",
			m.daemonVersion, queued, running, done, failed, canceled)
	} else {
		statusLine = fmt.Sprintf("Daemon: %s | Workers: %d/%d | Queued: %d | Running: %d | Done: %d | Failed: %d | Canceled: %d",
			m.daemonVersion,
			m.status.ActiveWorkers, m.status.MaxWorkers,
			m.status.QueuedJobs, m.status.RunningJobs,
			m.status.CompletedJobs, m.status.FailedJobs,
			m.status.CanceledJobs)
	}
	b.WriteString(tuiStatusStyle.Render(statusLine))
	b.WriteString("\n\n")

	visibleJobList := m.getVisibleJobs()
	visibleSelectedIdx := m.getVisibleSelectedIdx()

	if len(visibleJobList) == 0 {
		if m.activeRepoFilter != "" {
			b.WriteString("No jobs matching filter\n")
		} else {
			b.WriteString("No jobs in queue\n")
		}
	} else {
		// Calculate ID column width based on max ID
		idWidth := 2 // minimum width
		for _, job := range visibleJobList {
			w := len(fmt.Sprintf("%d", job.ID))
			if w > idWidth {
				idWidth = w
			}
		}

		// Header (with 2-char prefix to align with row selector)
		header := fmt.Sprintf("  %-*s %-17s %-15s %-8s %-10s %-12s %-8s %s",
			idWidth, "ID", "Ref", "Repo", "Agent", "Status", "Queued", "Elapsed", "Addr'd")
		b.WriteString(tuiStatusStyle.Render(header))
		b.WriteString("\n")
		b.WriteString("  " + strings.Repeat("-", min(m.width-4, 80)))
		b.WriteString("\n")

		// Calculate visible job range based on terminal height
		// Reserve lines for: title(1) + status(2) + header(2) + help(3) + scroll indicator(1)
		reservedLines := 9
		visibleRows := m.height - reservedLines
		if visibleRows < 3 {
			visibleRows = 3 // Show at least 3 jobs
		}

		// Determine which jobs to show, keeping selected item visible
		start := 0
		end := len(visibleJobList)

		if len(visibleJobList) > visibleRows {
			// Center the selected item when possible
			start = visibleSelectedIdx - visibleRows/2
			if start < 0 {
				start = 0
			}
			end = start + visibleRows
			if end > len(visibleJobList) {
				end = len(visibleJobList)
				start = end - visibleRows
			}
		}

		// Jobs
		for i := start; i < end; i++ {
			job := visibleJobList[i]
			selected := i == visibleSelectedIdx
			line := m.renderJobLine(job, selected, idWidth)
			if selected {
				line = tuiSelectedStyle.Render("> " + line)
			} else {
				line = "  " + line
			}
			b.WriteString(line)
			b.WriteString("\n")
		}

		// Show scroll indicator if not all jobs visible
		if len(visibleJobList) > visibleRows {
			scrollInfo := fmt.Sprintf("[showing %d-%d of %d]", start+1, end, len(visibleJobList))
			b.WriteString(tuiStatusStyle.Render(scrollInfo))
			b.WriteString("\n")
		}
	}

	// Update notification (or blank line if no update)
	if m.updateAvailable != "" {
		updateStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("226")).Bold(true)
		b.WriteString(updateStyle.Render(fmt.Sprintf("Update available: %s - run 'roborev update'", m.updateAvailable)))
		b.WriteString("\n")
	} else {
		b.WriteString("\n")
	}

	// Help (two lines)
	helpText := "up/down/pgup/pgdn: navigate | enter: review | p: prompt | f: filter | q: quit\n" +
		"a: toggle addressed | x: cancel running/queued job"
	if m.activeRepoFilter != "" {
		helpText += " | esc: clear filter"
	}
	b.WriteString(tuiHelpStyle.Render(helpText))

	return b.String()
}

func (m tuiModel) renderJobLine(job storage.ReviewJob, selected bool, idWidth int) string {
	ref := shortRef(job.GitRef)

	repo := job.RepoName
	if len(repo) > 15 {
		repo = repo[:12] + "..."
	}

	agent := job.Agent
	if len(agent) > 8 {
		agent = agent[:8]
	}

	// Format enqueue time as compact timestamp in local time
	enqueued := job.EnqueuedAt.Local().Format("Jan 02 15:04")

	// Format elapsed time
	elapsed := ""
	if job.StartedAt != nil {
		if job.FinishedAt != nil {
			elapsed = job.FinishedAt.Sub(*job.StartedAt).Round(time.Second).String()
		} else {
			elapsed = time.Since(*job.StartedAt).Round(time.Second).String()
		}
	}

	// Format status with retry count for queued/running jobs (e.g., "queued(1)")
	status := string(job.Status)
	if job.RetryCount > 0 && (job.Status == storage.JobStatusQueued || job.Status == storage.JobStatusRunning) {
		status = fmt.Sprintf("%s(%d)", job.Status, job.RetryCount)
	}

	// Color the status only when not selected (selection style should be uniform)
	var styledStatus string
	if selected {
		styledStatus = status
	} else {
		switch job.Status {
		case storage.JobStatusQueued:
			styledStatus = tuiQueuedStyle.Render(status)
		case storage.JobStatusRunning:
			styledStatus = tuiRunningStyle.Render(status)
		case storage.JobStatusDone:
			styledStatus = tuiDoneStyle.Render(status)
		case storage.JobStatusFailed:
			styledStatus = tuiFailedStyle.Render(status)
		case storage.JobStatusCanceled:
			styledStatus = tuiCanceledStyle.Render(status)
		default:
			styledStatus = status
		}
	}
	// Pad after coloring since lipgloss strips trailing spaces
	// Width 10 accommodates "running(3)" (10 chars)
	padding := 10 - len(status)
	if padding > 0 {
		styledStatus += strings.Repeat(" ", padding)
	}

	// Addressed status: nil means no review yet, true/false for reviewed jobs
	addr := ""
	if job.Addressed != nil {
		if *job.Addressed {
			addr = "true"
		} else {
			addr = "false"
		}
	}

	return fmt.Sprintf("%-*d %-17s %-15s %-8s %s %-12s %-8s %s",
		idWidth, job.ID, ref, repo, agent, styledStatus, enqueued, elapsed, addr)
}

// wrapText wraps text to the specified width, preserving existing line breaks
// and breaking at word boundaries when possible
func wrapText(text string, width int) []string {
	if width <= 0 {
		width = 100
	}

	var result []string
	for _, line := range strings.Split(text, "\n") {
		if len(line) <= width {
			result = append(result, line)
			continue
		}

		// Wrap long lines
		for len(line) > width {
			// Find a good break point (space) near the width
			breakPoint := width
			for i := width; i > width/2; i-- {
				if i < len(line) && line[i] == ' ' {
					breakPoint = i
					break
				}
			}

			result = append(result, line[:breakPoint])
			line = strings.TrimLeft(line[breakPoint:], " ")
		}
		if len(line) > 0 {
			result = append(result, line)
		}
	}

	return result
}

func (m tuiModel) renderReviewView() string {
	var b strings.Builder

	review := m.currentReview
	if review.Job != nil {
		ref := shortRef(review.Job.GitRef)
		addressedStr := ""
		if review.Addressed {
			addressedStr = " [ADDRESSED]"
		}
		title := fmt.Sprintf("Review: %s (%s)%s", ref, review.Agent, addressedStr)
		b.WriteString(tuiTitleStyle.Render(title))
	} else {
		b.WriteString(tuiTitleStyle.Render("Review"))
	}
	b.WriteString("\n")

	// Wrap text to terminal width (max 100 chars)
	wrapWidth := min(m.width-2, 100)
	lines := wrapText(review.Output, wrapWidth)

	visibleLines := m.height - 5 // Leave room for title and help

	start := m.reviewScroll
	if start >= len(lines) {
		start = max(0, len(lines)-1)
	}
	end := min(start+visibleLines, len(lines))

	for i := start; i < end; i++ {
		b.WriteString(lines[i])
		b.WriteString("\n")
	}

	// Scroll indicator
	if len(lines) > visibleLines {
		scrollInfo := fmt.Sprintf("[%d-%d of %d lines]", start+1, end, len(lines))
		b.WriteString(tuiStatusStyle.Render(scrollInfo))
		b.WriteString("\n")
	}

	b.WriteString(tuiHelpStyle.Render("up/down: scroll | a: toggle addressed | p: view prompt | esc/q: back"))

	return b.String()
}

func (m tuiModel) renderPromptView() string {
	var b strings.Builder

	review := m.currentReview
	if review.Job != nil {
		ref := shortRef(review.Job.GitRef)
		title := fmt.Sprintf("Prompt: %s (%s)", ref, review.Agent)
		b.WriteString(tuiTitleStyle.Render(title))
	} else {
		b.WriteString(tuiTitleStyle.Render("Prompt"))
	}
	b.WriteString("\n")

	// Wrap text to terminal width (max 100 chars)
	wrapWidth := min(m.width-2, 100)
	lines := wrapText(review.Prompt, wrapWidth)

	visibleLines := m.height - 5 // Leave room for title and help

	start := m.promptScroll
	if start >= len(lines) {
		start = max(0, len(lines)-1)
	}
	end := min(start+visibleLines, len(lines))

	for i := start; i < end; i++ {
		b.WriteString(lines[i])
		b.WriteString("\n")
	}

	// Scroll indicator
	if len(lines) > visibleLines {
		scrollInfo := fmt.Sprintf("[%d-%d of %d lines]", start+1, end, len(lines))
		b.WriteString(tuiStatusStyle.Render(scrollInfo))
		b.WriteString("\n")
	}

	b.WriteString(tuiHelpStyle.Render("up/down: scroll | p: back to review | esc/q: back"))

	return b.String()
}

func (m tuiModel) renderFilterView() string {
	var b strings.Builder

	b.WriteString(tuiTitleStyle.Render("Filter by Repository"))
	b.WriteString("\n\n")

	// Show loading state if repos haven't been fetched yet
	if m.filterRepos == nil {
		b.WriteString(tuiStatusStyle.Render("Loading repos..."))
		b.WriteString("\n\n")
		b.WriteString(tuiHelpStyle.Render("esc: cancel"))
		return b.String()
	}

	// Search box
	searchDisplay := m.filterSearch
	if searchDisplay == "" {
		searchDisplay = tuiStatusStyle.Render("Type to search...")
	}
	b.WriteString(fmt.Sprintf("Search: %s", searchDisplay))
	b.WriteString("\n\n")

	visible := m.getVisibleFilterRepos()

	// Calculate visible rows
	// Reserve: title(1) + blank(1) + search(1) + blank(1) + scroll-info(1) + blank(1) + help(1) = 7
	reservedLines := 7
	visibleRows := m.height - reservedLines
	if visibleRows < 0 {
		visibleRows = 0
	}

	// Determine which repos to show, keeping selected item visible
	start := 0
	end := len(visible)
	needsScroll := len(visible) > visibleRows && visibleRows > 0
	if needsScroll {
		start = m.filterSelectedIdx - visibleRows/2
		if start < 0 {
			start = 0
		}
		end = start + visibleRows
		if end > len(visible) {
			end = len(visible)
			start = end - visibleRows
			if start < 0 {
				start = 0
			}
		}
	} else if visibleRows > 0 {
		// No scrolling needed, show all (up to visibleRows)
		if end > visibleRows {
			end = visibleRows
		}
	} else {
		// No room for repos
		end = 0
	}

	for i := start; i < end; i++ {
		repo := visible[i]
		var line string
		if repo.name == "" {
			line = fmt.Sprintf("All repos (%d)", repo.count)
		} else {
			line = fmt.Sprintf("%s (%d)", repo.name, repo.count)
		}

		if i == m.filterSelectedIdx {
			b.WriteString(tuiSelectedStyle.Render("> " + line))
		} else {
			b.WriteString("  " + line)
		}
		b.WriteString("\n")
	}

	if len(visible) == 0 {
		b.WriteString(tuiStatusStyle.Render("  No matching repos"))
		b.WriteString("\n")
	} else if visibleRows == 0 {
		b.WriteString(tuiStatusStyle.Render("  (terminal too small)"))
		b.WriteString("\n")
	} else if needsScroll {
		scrollInfo := fmt.Sprintf("[showing %d-%d of %d]", start+1, end, len(visible))
		b.WriteString(tuiStatusStyle.Render(scrollInfo))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(tuiHelpStyle.Render("up/down: navigate | enter: select | esc: cancel | type to search"))

	return b.String()
}

func tuiCmd() *cobra.Command {
	var addr string

	cmd := &cobra.Command{
		Use:   "tui",
		Short: "Interactive terminal UI for monitoring reviews",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Ensure daemon is running (and restart if version mismatch)
			if err := ensureDaemon(); err != nil {
				return fmt.Errorf("daemon error: %w", err)
			}

			if addr == "" {
				addr = getDaemonAddr()
			} else if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
				addr = "http://" + addr
			}
			p := tea.NewProgram(newTuiModel(addr), tea.WithAltScreen())
			if _, err := p.Run(); err != nil {
				return fmt.Errorf("TUI error: %w", err)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&addr, "addr", "", "daemon address (default: auto-detect)")

	return cmd
}
