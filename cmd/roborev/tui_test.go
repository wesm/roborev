package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wesm/roborev/internal/storage"
)

func TestTUIFetchJobsSuccess(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/jobs" {
			t.Errorf("Expected /api/jobs, got %s", r.URL.Path)
		}
		jobs := []storage.ReviewJob{{ID: 1, GitRef: "abc123", Agent: "test"}}
		json.NewEncoder(w).Encode(map[string]interface{}{"jobs": jobs})
	}))
	defer ts.Close()

	m := newTuiModel(ts.URL)
	cmd := m.fetchJobs()
	msg := cmd()

	jobs, ok := msg.(tuiJobsMsg)
	if !ok {
		t.Fatalf("Expected tuiJobsMsg, got %T: %v", msg, msg)
	}
	if len(jobs.jobs) != 1 || jobs.jobs[0].ID != 1 {
		t.Errorf("Unexpected jobs: %+v", jobs.jobs)
	}
}

func TestTUIFetchJobsError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	m := newTuiModel(ts.URL)
	cmd := m.fetchJobs()
	msg := cmd()

	_, ok := msg.(tuiErrMsg)
	if !ok {
		t.Fatalf("Expected tuiErrMsg for 500, got %T: %v", msg, msg)
	}
}

func TestTUIFetchReviewNotFound(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	m := newTuiModel(ts.URL)
	cmd := m.fetchReview(999)
	msg := cmd()

	errMsg, ok := msg.(tuiErrMsg)
	if !ok {
		t.Fatalf("Expected tuiErrMsg for 404, got %T: %v", msg, msg)
	}
	if errMsg.Error() != "no review found" {
		t.Errorf("Expected 'no review found', got: %v", errMsg)
	}
}

func TestTUIFetchReviewServerError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	m := newTuiModel(ts.URL)
	cmd := m.fetchReview(1)
	msg := cmd()

	errMsg, ok := msg.(tuiErrMsg)
	if !ok {
		t.Fatalf("Expected tuiErrMsg for 500, got %T: %v", msg, msg)
	}
	if errMsg.Error() != "fetch review: 500 Internal Server Error" {
		t.Errorf("Expected status in error, got: %v", errMsg)
	}
}

func TestTUIAddressReviewSuccess(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("Expected POST, got %s", r.Method)
		}
		var req map[string]interface{}
		json.NewDecoder(r.Body).Decode(&req)
		if req["review_id"].(float64) != 42 {
			t.Errorf("Expected review_id 42, got %v", req["review_id"])
		}
		if req["addressed"].(bool) != true {
			t.Errorf("Expected addressed true, got %v", req["addressed"])
		}
		json.NewEncoder(w).Encode(map[string]bool{"success": true})
	}))
	defer ts.Close()

	m := newTuiModel(ts.URL)
	cmd := m.addressReview(42, 100, true, false) // reviewID=42, jobID=100, newState=true, oldState=false
	msg := cmd()

	result, ok := msg.(tuiAddressedResultMsg)
	if !ok {
		t.Fatalf("Expected tuiAddressedResultMsg, got %T: %v", msg, msg)
	}
	if result.err != nil {
		t.Errorf("Expected no error, got %v", result.err)
	}
	if !result.reviewView {
		t.Error("Expected reviewView to be true")
	}
	if result.reviewID != 42 {
		t.Errorf("Expected reviewID=42, got %d", result.reviewID)
	}
}

func TestTUIAddressReviewNotFound(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	m := newTuiModel(ts.URL)
	cmd := m.addressReview(999, 100, true, false) // reviewID=999, jobID=100, newState=true, oldState=false
	msg := cmd()

	result, ok := msg.(tuiAddressedResultMsg)
	if !ok {
		t.Fatalf("Expected tuiAddressedResultMsg for 404, got %T: %v", msg, msg)
	}
	if result.err == nil || result.err.Error() != "review not found" {
		t.Errorf("Expected 'review not found' error, got: %v", result.err)
	}
}

func TestTUIToggleAddressedForJobSuccess(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/review" {
			review := storage.Review{ID: 10, Addressed: false}
			json.NewEncoder(w).Encode(review)
		} else if r.URL.Path == "/api/review/address" {
			json.NewEncoder(w).Encode(map[string]bool{"success": true})
		}
	}))
	defer ts.Close()

	m := newTuiModel(ts.URL)
	currentState := false
	cmd := m.toggleAddressedForJob(1, &currentState)
	msg := cmd()

	addressed, ok := msg.(tuiAddressedMsg)
	if !ok {
		t.Fatalf("Expected tuiAddressedMsg, got %T: %v", msg, msg)
	}
	if !bool(addressed) {
		t.Error("Expected toggled state to be true (was false)")
	}
}

func TestTUIToggleAddressedNoReview(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	m := newTuiModel(ts.URL)
	cmd := m.toggleAddressedForJob(999, nil)
	msg := cmd()

	errMsg, ok := msg.(tuiErrMsg)
	if !ok {
		t.Fatalf("Expected tuiErrMsg, got %T: %v", msg, msg)
	}
	if errMsg.Error() != "no review for this job" {
		t.Errorf("Expected 'no review for this job', got: %v", errMsg)
	}
}

// addressRequest is used to decode and validate POST body in tests
type addressRequest struct {
	ReviewID  int64 `json:"review_id"`
	Addressed bool  `json:"addressed"`
}

func TestTUIAddressReviewInBackgroundSuccess(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/review" && r.Method == http.MethodGet:
			if r.URL.Query().Get("job_id") != "42" {
				t.Errorf("Expected job_id=42, got %s", r.URL.Query().Get("job_id"))
			}
			review := storage.Review{ID: 10, Addressed: false}
			json.NewEncoder(w).Encode(review)
		case r.URL.Path == "/api/review/address" && r.Method == http.MethodPost:
			var req addressRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("Failed to decode request body: %v", err)
			}
			if req.ReviewID != 10 {
				t.Errorf("Expected review_id=10, got %d", req.ReviewID)
			}
			if req.Addressed != true {
				t.Errorf("Expected addressed=true, got %v", req.Addressed)
			}
			json.NewEncoder(w).Encode(map[string]bool{"success": true})
		default:
			t.Fatalf("Unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer ts.Close()

	m := newTuiModel(ts.URL)
	cmd := m.addressReviewInBackground(42, true, false) // jobID=42, newState=true, oldState=false
	msg := cmd()

	result, ok := msg.(tuiAddressedResultMsg)
	if !ok {
		t.Fatalf("Expected tuiAddressedResultMsg, got %T: %v", msg, msg)
	}
	if result.err != nil {
		t.Errorf("Expected no error, got %v", result.err)
	}
	if result.jobID != 42 {
		t.Errorf("Expected jobID=42, got %d", result.jobID)
	}
	if result.oldState != false {
		t.Errorf("Expected oldState=false, got %v", result.oldState)
	}
	if result.reviewView {
		t.Error("Expected reviewView=false for queue view command")
	}
	// reviewID is intentionally 0 for queue view commands (only jobID is set)
	if result.reviewID != 0 {
		t.Errorf("Expected reviewID=0 for queue view, got %d", result.reviewID)
	}
}

func TestTUIAddressReviewInBackgroundNotFound(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/review" {
			t.Fatalf("Unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	m := newTuiModel(ts.URL)
	cmd := m.addressReviewInBackground(42, true, false)
	msg := cmd()

	result, ok := msg.(tuiAddressedResultMsg)
	if !ok {
		t.Fatalf("Expected tuiAddressedResultMsg, got %T: %v", msg, msg)
	}
	if result.err == nil || !strings.Contains(result.err.Error(), "no review") {
		t.Errorf("Expected error containing 'no review', got: %v", result.err)
	}
	if result.jobID != 42 {
		t.Errorf("Expected jobID=42 for rollback, got %d", result.jobID)
	}
	if result.oldState != false {
		t.Errorf("Expected oldState=false for rollback, got %v", result.oldState)
	}
}

func TestTUIAddressReviewInBackgroundFetchError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/review" {
			t.Fatalf("Unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	m := newTuiModel(ts.URL)
	cmd := m.addressReviewInBackground(42, true, false)
	msg := cmd()

	result, ok := msg.(tuiAddressedResultMsg)
	if !ok {
		t.Fatalf("Expected tuiAddressedResultMsg, got %T: %v", msg, msg)
	}
	if result.err == nil {
		t.Error("Expected error for 500 response")
	}
	if result.jobID != 42 {
		t.Errorf("Expected jobID=42 for rollback, got %d", result.jobID)
	}
}

func TestTUIAddressReviewInBackgroundBadJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/review" {
			t.Fatalf("Unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Write([]byte("not valid json"))
	}))
	defer ts.Close()

	m := newTuiModel(ts.URL)
	cmd := m.addressReviewInBackground(42, true, false)
	msg := cmd()

	result, ok := msg.(tuiAddressedResultMsg)
	if !ok {
		t.Fatalf("Expected tuiAddressedResultMsg, got %T: %v", msg, msg)
	}
	if result.err == nil {
		t.Error("Expected error for bad JSON")
	}
	if result.jobID != 42 {
		t.Errorf("Expected jobID=42 for rollback, got %d", result.jobID)
	}
}

func TestTUIAddressReviewInBackgroundAddressError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/review" && r.Method == http.MethodGet:
			review := storage.Review{ID: 10, Addressed: false}
			json.NewEncoder(w).Encode(review)
		case r.URL.Path == "/api/review/address" && r.Method == http.MethodPost:
			// Validate request body before returning error
			var req addressRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("Failed to decode request body: %v", err)
			}
			if req.ReviewID != 10 {
				t.Errorf("Expected review_id=10, got %d", req.ReviewID)
			}
			if req.Addressed != true {
				t.Errorf("Expected addressed=true, got %v", req.Addressed)
			}
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Fatalf("Unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer ts.Close()

	m := newTuiModel(ts.URL)
	cmd := m.addressReviewInBackground(42, true, false)
	msg := cmd()

	result, ok := msg.(tuiAddressedResultMsg)
	if !ok {
		t.Fatalf("Expected tuiAddressedResultMsg, got %T: %v", msg, msg)
	}
	if result.err == nil {
		t.Error("Expected error for address 500 response")
	}
	if result.jobID != 42 {
		t.Errorf("Expected jobID=42 for rollback, got %d", result.jobID)
	}
	if result.oldState != false {
		t.Errorf("Expected oldState=false for rollback, got %v", result.oldState)
	}
}

func TestTUIHTTPTimeout(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Delay longer than client timeout
		time.Sleep(200 * time.Millisecond)
		json.NewEncoder(w).Encode(map[string]interface{}{"jobs": []storage.ReviewJob{}})
	}))
	defer ts.Close()

	m := newTuiModel(ts.URL)
	// Override with short timeout for test
	m.client.Timeout = 50 * time.Millisecond

	cmd := m.fetchJobs()
	msg := cmd()

	_, ok := msg.(tuiErrMsg)
	if !ok {
		t.Fatalf("Expected tuiErrMsg for timeout, got %T: %v", msg, msg)
	}
}

func TestTUISelectionMaintainedOnInsert(t *testing.T) {
	m := newTuiModel("http://localhost")

	// Initial state with 3 jobs, select the middle one (ID=2)
	m.jobs = []storage.ReviewJob{
		{ID: 3}, {ID: 2}, {ID: 1},
	}
	m.selectedIdx = 1
	m.selectedJobID = 2

	// New jobs added at the top (newer jobs first)
	newJobs := tuiJobsMsg{jobs: []storage.ReviewJob{
		{ID: 5}, {ID: 4}, {ID: 3}, {ID: 2}, {ID: 1},
	}}

	updated, _ := m.Update(newJobs)
	m = updated.(tuiModel)

	// Should still be on job ID=2, now at index 3
	if m.selectedJobID != 2 {
		t.Errorf("Expected selectedJobID=2, got %d", m.selectedJobID)
	}
	if m.selectedIdx != 3 {
		t.Errorf("Expected selectedIdx=3 (ID=2 moved), got %d", m.selectedIdx)
	}
}

func TestTUISelectionClampsOnRemoval(t *testing.T) {
	m := newTuiModel("http://localhost")

	// Initial state with 3 jobs, select the last one (ID=1)
	m.jobs = []storage.ReviewJob{
		{ID: 3}, {ID: 2}, {ID: 1},
	}
	m.selectedIdx = 2
	m.selectedJobID = 1

	// Job ID=1 is removed
	newJobs := tuiJobsMsg{jobs: []storage.ReviewJob{
		{ID: 3}, {ID: 2},
	}}

	updated, _ := m.Update(newJobs)
	m = updated.(tuiModel)

	// Should clamp to last valid index and update selectedJobID
	if m.selectedIdx != 1 {
		t.Errorf("Expected selectedIdx=1 (clamped), got %d", m.selectedIdx)
	}
	if m.selectedJobID != 2 {
		t.Errorf("Expected selectedJobID=2 (new selection), got %d", m.selectedJobID)
	}
}

func TestTUISelectionFirstJobOnEmpty(t *testing.T) {
	m := newTuiModel("http://localhost")

	// No prior selection (empty jobs list, zero selectedJobID)
	m.jobs = []storage.ReviewJob{}
	m.selectedIdx = 0
	m.selectedJobID = 0

	// Jobs arrive
	newJobs := tuiJobsMsg{jobs: []storage.ReviewJob{
		{ID: 5}, {ID: 4}, {ID: 3},
	}}

	updated, _ := m.Update(newJobs)
	m = updated.(tuiModel)

	// Should select first job
	if m.selectedIdx != 0 {
		t.Errorf("Expected selectedIdx=0, got %d", m.selectedIdx)
	}
	if m.selectedJobID != 5 {
		t.Errorf("Expected selectedJobID=5 (first job), got %d", m.selectedJobID)
	}
}

func TestTUISelectionEmptyList(t *testing.T) {
	m := newTuiModel("http://localhost")

	// Had jobs, now empty
	m.jobs = []storage.ReviewJob{{ID: 1}}
	m.selectedIdx = 0
	m.selectedJobID = 1

	newJobs := tuiJobsMsg{jobs: []storage.ReviewJob{}}

	updated, _ := m.Update(newJobs)
	m = updated.(tuiModel)

	// Empty list should have selectedIdx=-1 (no valid selection)
	if m.selectedIdx != -1 {
		t.Errorf("Expected selectedIdx=-1, got %d", m.selectedIdx)
	}
	if m.selectedJobID != 0 {
		t.Errorf("Expected selectedJobID=0, got %d", m.selectedJobID)
	}
}

func TestTUIAddressedRollbackOnError(t *testing.T) {
	m := newTuiModel("http://localhost")

	// Initial state with job addressed=false
	addressed := false
	m.jobs = []storage.ReviewJob{
		{ID: 42, Status: storage.JobStatusDone, Addressed: &addressed},
	}
	m.selectedIdx = 0
	m.selectedJobID = 42

	// Simulate error result from background update
	// This would happen if server returned error after optimistic update
	errMsg := tuiAddressedResultMsg{
		jobID:    42,
		oldState: false, // Was false before optimistic update
		err:      fmt.Errorf("server error"),
	}

	// First, simulate the optimistic update (what happens when 'a' is pressed)
	*m.jobs[0].Addressed = true

	// Now handle the error result - should rollback
	updated, _ := m.Update(errMsg)
	m = updated.(tuiModel)

	// Should have rolled back to false
	if m.jobs[0].Addressed == nil || *m.jobs[0].Addressed != false {
		t.Errorf("Expected addressed=false after rollback, got %v", m.jobs[0].Addressed)
	}
	if m.err == nil {
		t.Error("Expected error to be set")
	}
}

func TestTUIAddressedSuccessNoRollback(t *testing.T) {
	m := newTuiModel("http://localhost")

	// Initial state
	addressed := false
	m.jobs = []storage.ReviewJob{
		{ID: 42, Status: storage.JobStatusDone, Addressed: &addressed},
	}

	// Simulate optimistic update
	*m.jobs[0].Addressed = true

	// Success result (err is nil)
	successMsg := tuiAddressedResultMsg{
		jobID:    42,
		oldState: false,
		err:      nil,
	}

	updated, _ := m.Update(successMsg)
	m = updated.(tuiModel)

	// Should stay true (no rollback on success)
	if m.jobs[0].Addressed == nil || *m.jobs[0].Addressed != true {
		t.Errorf("Expected addressed=true after success, got %v", m.jobs[0].Addressed)
	}
	if m.err != nil {
		t.Errorf("Expected no error, got %v", m.err)
	}
}

func TestTUISelectionMaintainedOnLargeBatch(t *testing.T) {
	m := newTuiModel("http://localhost")

	// Initial state with 1 job selected
	m.jobs = []storage.ReviewJob{{ID: 1}}
	m.selectedIdx = 0
	m.selectedJobID = 1

	// 30 new jobs added at the top (simulating large batch)
	newJobs := make([]storage.ReviewJob, 31)
	for i := 0; i < 30; i++ {
		newJobs[i] = storage.ReviewJob{ID: int64(31 - i)} // IDs 31, 30, 29, ..., 2
	}
	newJobs[30] = storage.ReviewJob{ID: 1} // Original job at the end

	updated, _ := m.Update(tuiJobsMsg{jobs: newJobs})
	m = updated.(tuiModel)

	// Should still follow job ID=1, now at index 30
	if m.selectedJobID != 1 {
		t.Errorf("Expected selectedJobID=1, got %d", m.selectedJobID)
	}
	if m.selectedIdx != 30 {
		t.Errorf("Expected selectedIdx=30 (ID=1 at end), got %d", m.selectedIdx)
	}
}

func TestTUIReviewViewAddressedRollbackOnError(t *testing.T) {
	m := newTuiModel("http://localhost")

	// Initial state with review view showing an unaddressed review
	m.currentView = tuiViewReview
	m.currentReview = &storage.Review{ID: 42, Addressed: false}

	// Simulate optimistic update (what happens when 'a' is pressed in review view)
	m.currentReview.Addressed = true

	// Error result from server (reviewID must match currentReview.ID for rollback)
	errMsg := tuiAddressedResultMsg{
		reviewID:   42, // Must match currentReview.ID
		reviewView: true,
		oldState:   false, // Was false before optimistic update
		err:        fmt.Errorf("server error"),
	}

	updated, _ := m.Update(errMsg)
	m = updated.(tuiModel)

	// Should have rolled back to false
	if m.currentReview.Addressed != false {
		t.Errorf("Expected currentReview.Addressed=false after rollback, got %v", m.currentReview.Addressed)
	}
	if m.err == nil {
		t.Error("Expected error to be set")
	}
}

func TestTUIReviewViewAddressedSuccessNoRollback(t *testing.T) {
	m := newTuiModel("http://localhost")

	// Initial state with review view
	m.currentView = tuiViewReview
	m.currentReview = &storage.Review{ID: 42, Addressed: false}

	// Simulate optimistic update
	m.currentReview.Addressed = true

	// Success result (err is nil)
	successMsg := tuiAddressedResultMsg{
		reviewView: true,
		oldState:   false,
		err:        nil,
	}

	updated, _ := m.Update(successMsg)
	m = updated.(tuiModel)

	// Should stay true (no rollback on success)
	if m.currentReview.Addressed != true {
		t.Errorf("Expected currentReview.Addressed=true after success, got %v", m.currentReview.Addressed)
	}
	if m.err != nil {
		t.Errorf("Expected no error, got %v", m.err)
	}
}

func TestTUIReviewViewNavigateAwayBeforeError(t *testing.T) {
	m := newTuiModel("http://localhost")

	// Setup: jobs in queue with addressed=false
	addrA := false
	addrB := false
	m.jobs = []storage.ReviewJob{
		{ID: 100, Status: storage.JobStatusDone, Addressed: &addrA}, // Job for review A
		{ID: 200, Status: storage.JobStatusDone, Addressed: &addrB}, // Job for review B
	}

	// User views review A, toggles addressed (optimistic update)
	m.currentView = tuiViewReview
	m.currentReview = &storage.Review{ID: 42, Addressed: false, Job: &storage.ReviewJob{ID: 100}}
	m.currentReview.Addressed = true  // Optimistic update to review
	*m.jobs[0].Addressed = true       // Optimistic update to job in queue

	// User navigates to review B before error response arrives
	m.currentReview = &storage.Review{ID: 99, Addressed: false, Job: &storage.ReviewJob{ID: 200}}

	// Error arrives for review A's toggle
	errMsg := tuiAddressedResultMsg{
		reviewID:   42,  // Review A
		jobID:      100, // Job A
		reviewView: true,
		oldState:   false,
		err:        fmt.Errorf("server error"),
	}

	updated, _ := m.Update(errMsg)
	m = updated.(tuiModel)

	// Review B should be unchanged (still false)
	if m.currentReview.Addressed != false {
		t.Errorf("Review B should be unchanged, got Addressed=%v", m.currentReview.Addressed)
	}

	// Job A in queue should be rolled back to false
	if *m.jobs[0].Addressed != false {
		t.Errorf("Job A should be rolled back, got Addressed=%v", *m.jobs[0].Addressed)
	}

	// Job B in queue should be unchanged
	if *m.jobs[1].Addressed != false {
		t.Errorf("Job B should be unchanged, got Addressed=%v", *m.jobs[1].Addressed)
	}
}

func TestTUISetJobAddressedHelper(t *testing.T) {
	m := newTuiModel("http://localhost")

	// Test with nil Addressed pointer - should allocate
	m.jobs = []storage.ReviewJob{
		{ID: 100, Status: storage.JobStatusDone, Addressed: nil},
	}

	m.setJobAddressed(100, true)

	if m.jobs[0].Addressed == nil {
		t.Fatal("Expected Addressed to be allocated")
	}
	if *m.jobs[0].Addressed != true {
		t.Errorf("Expected Addressed=true, got %v", *m.jobs[0].Addressed)
	}

	// Test toggle back
	m.setJobAddressed(100, false)
	if *m.jobs[0].Addressed != false {
		t.Errorf("Expected Addressed=false, got %v", *m.jobs[0].Addressed)
	}

	// Test with non-existent job ID - should be no-op
	m.setJobAddressed(999, true)
	if *m.jobs[0].Addressed != false {
		t.Errorf("Non-existent job should not affect existing job")
	}
}

func TestTUIReviewViewToggleSyncsQueueJob(t *testing.T) {
	m := newTuiModel("http://localhost")

	// Setup: job in queue with addressed=false
	addr := false
	m.jobs = []storage.ReviewJob{
		{ID: 100, Status: storage.JobStatusDone, Addressed: &addr},
	}

	// User views review for job 100 and presses 'a'
	m.currentView = tuiViewReview
	m.currentReview = &storage.Review{
		ID:        42,
		Addressed: false,
		Job:       &storage.ReviewJob{ID: 100},
	}

	// Simulate the optimistic update that happens when 'a' is pressed
	oldState := m.currentReview.Addressed
	newState := !oldState
	m.currentReview.Addressed = newState
	m.setJobAddressed(100, newState)

	// Both should be updated
	if m.currentReview.Addressed != true {
		t.Errorf("Expected currentReview.Addressed=true, got %v", m.currentReview.Addressed)
	}
	if *m.jobs[0].Addressed != true {
		t.Errorf("Expected job.Addressed=true, got %v", *m.jobs[0].Addressed)
	}
}

func TestTUICancelJobSuccess(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/job/cancel" {
			t.Errorf("Expected /api/job/cancel, got %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("Expected POST, got %s", r.Method)
		}
		var req struct {
			JobID int64 `json:"job_id"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if req.JobID != 42 {
			t.Errorf("Expected job_id=42, got %d", req.JobID)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
	}))
	defer ts.Close()

	m := newTuiModel(ts.URL)
	oldFinishedAt := time.Now().Add(-1 * time.Hour)
	cmd := m.cancelJob(42, storage.JobStatusRunning, &oldFinishedAt)
	msg := cmd()

	result, ok := msg.(tuiCancelResultMsg)
	if !ok {
		t.Fatalf("Expected tuiCancelResultMsg, got %T: %v", msg, msg)
	}
	if result.err != nil {
		t.Errorf("Expected no error, got %v", result.err)
	}
	if result.jobID != 42 {
		t.Errorf("Expected jobID=42, got %d", result.jobID)
	}
	if result.oldState != storage.JobStatusRunning {
		t.Errorf("Expected oldState=running, got %s", result.oldState)
	}
	if result.oldFinishedAt == nil || !result.oldFinishedAt.Equal(oldFinishedAt) {
		t.Errorf("Expected oldFinishedAt to be preserved")
	}
}

func TestTUICancelJobNotFound(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
	}))
	defer ts.Close()

	m := newTuiModel(ts.URL)
	cmd := m.cancelJob(99, storage.JobStatusQueued, nil)
	msg := cmd()

	result, ok := msg.(tuiCancelResultMsg)
	if !ok {
		t.Fatalf("Expected tuiCancelResultMsg, got %T: %v", msg, msg)
	}
	if result.err == nil {
		t.Error("Expected error for 404, got nil")
	}
	if result.oldState != storage.JobStatusQueued {
		t.Errorf("Expected oldState=queued for rollback, got %s", result.oldState)
	}
	if result.oldFinishedAt != nil {
		t.Errorf("Expected oldFinishedAt=nil for queued job, got %v", result.oldFinishedAt)
	}
}

func TestTUICancelRollbackOnError(t *testing.T) {
	m := newTuiModel("http://localhost")

	// Setup: running job with no FinishedAt (still running)
	startTime := time.Now().Add(-5 * time.Minute)
	m.jobs = []storage.ReviewJob{
		{ID: 42, Status: storage.JobStatusRunning, StartedAt: &startTime, FinishedAt: nil},
	}
	m.selectedIdx = 0
	m.selectedJobID = 42

	// Simulate the optimistic update that would have happened
	now := time.Now()
	m.jobs[0].Status = storage.JobStatusCanceled
	m.jobs[0].FinishedAt = &now

	// Simulate cancel error result - should rollback both status and FinishedAt
	errResult := tuiCancelResultMsg{
		jobID:         42,
		oldState:      storage.JobStatusRunning,
		oldFinishedAt: nil, // Was nil before optimistic update
		err:           fmt.Errorf("server error"),
	}

	updated, _ := m.Update(errResult)
	m2 := updated.(tuiModel)

	if m2.jobs[0].Status != storage.JobStatusRunning {
		t.Errorf("Expected status to rollback to 'running', got '%s'", m2.jobs[0].Status)
	}
	if m2.jobs[0].FinishedAt != nil {
		t.Errorf("Expected FinishedAt to rollback to nil, got %v", m2.jobs[0].FinishedAt)
	}
	if m2.err == nil {
		t.Error("Expected error to be set")
	}
}

func TestTUICancelRollbackWithNonNilFinishedAt(t *testing.T) {
	// Test rollback when original FinishedAt is non-nil (edge case: corrupted state
	// or queued job that somehow has a timestamp)
	m := newTuiModel("http://localhost")

	// Setup: job with an existing FinishedAt (unusual but possible edge case)
	startTime := time.Now().Add(-5 * time.Minute)
	originalFinished := time.Now().Add(-2 * time.Minute)
	m.jobs = []storage.ReviewJob{
		{ID: 42, Status: storage.JobStatusQueued, StartedAt: &startTime, FinishedAt: &originalFinished},
	}
	m.selectedIdx = 0
	m.selectedJobID = 42

	// Simulate the optimistic update that would have happened
	now := time.Now()
	m.jobs[0].Status = storage.JobStatusCanceled
	m.jobs[0].FinishedAt = &now

	// Simulate cancel error result - should rollback to original FinishedAt
	errResult := tuiCancelResultMsg{
		jobID:         42,
		oldState:      storage.JobStatusQueued,
		oldFinishedAt: &originalFinished, // Was non-nil before optimistic update
		err:           fmt.Errorf("server error"),
	}

	updated, _ := m.Update(errResult)
	m2 := updated.(tuiModel)

	if m2.jobs[0].Status != storage.JobStatusQueued {
		t.Errorf("Expected status to rollback to 'queued', got '%s'", m2.jobs[0].Status)
	}
	if m2.jobs[0].FinishedAt == nil {
		t.Error("Expected FinishedAt to rollback to original non-nil value, got nil")
	} else if !m2.jobs[0].FinishedAt.Equal(originalFinished) {
		t.Errorf("Expected FinishedAt to rollback to %v, got %v", originalFinished, *m2.jobs[0].FinishedAt)
	}
	if m2.err == nil {
		t.Error("Expected error to be set")
	}
}

func TestTUICancelOptimisticUpdate(t *testing.T) {
	m := newTuiModel("http://localhost")

	// Setup: running job with no FinishedAt
	startTime := time.Now().Add(-5 * time.Minute)
	m.jobs = []storage.ReviewJob{
		{ID: 42, Status: storage.JobStatusRunning, StartedAt: &startTime, FinishedAt: nil},
	}
	m.selectedIdx = 0
	m.selectedJobID = 42
	m.currentView = tuiViewQueue

	// Simulate pressing 'x' key
	beforeUpdate := time.Now()
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	m2 := updated.(tuiModel)

	// Should have optimistically set status to canceled
	if m2.jobs[0].Status != storage.JobStatusCanceled {
		t.Errorf("Expected status 'canceled', got '%s'", m2.jobs[0].Status)
	}

	// Should have set FinishedAt to stop elapsed time from ticking
	if m2.jobs[0].FinishedAt == nil {
		t.Error("Expected FinishedAt to be set during optimistic cancel")
	} else if m2.jobs[0].FinishedAt.Before(beforeUpdate) {
		t.Error("Expected FinishedAt to be set to current time")
	}

	// Should return a command (the cancel HTTP request)
	if cmd == nil {
		t.Error("Expected a command to be returned for the cancel request")
	}
}

func TestTUICancelOnlyRunningOrQueued(t *testing.T) {
	// Test that pressing 'x' on done/failed/canceled jobs is a no-op
	testCases := []storage.JobStatus{
		storage.JobStatusDone,
		storage.JobStatusFailed,
		storage.JobStatusCanceled,
	}

	for _, status := range testCases {
		t.Run(string(status), func(t *testing.T) {
			m := newTuiModel("http://localhost")
			finishedAt := time.Now().Add(-1 * time.Hour)
			m.jobs = []storage.ReviewJob{
				{ID: 1, Status: status, FinishedAt: &finishedAt},
			}
			m.selectedIdx = 0
			m.currentView = tuiViewQueue

			// Simulate pressing 'x' key
			updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
			m2 := updated.(tuiModel)

			// Status should not change
			if m2.jobs[0].Status != status {
				t.Errorf("Expected status to remain '%s', got '%s'", status, m2.jobs[0].Status)
			}

			// FinishedAt should not change
			if m2.jobs[0].FinishedAt == nil || !m2.jobs[0].FinishedAt.Equal(finishedAt) {
				t.Errorf("Expected FinishedAt to remain unchanged")
			}

			// No command should be returned (no HTTP request triggered)
			if cmd != nil {
				t.Errorf("Expected no command for non-cancellable job, got %v", cmd)
			}
		})
	}
}

// Tests for filter functionality

func TestTUIFilterOpenModal(t *testing.T) {
	m := newTuiModel("http://localhost")

	m.jobs = []storage.ReviewJob{
		{ID: 1, RepoName: "repo-a"},
		{ID: 2, RepoName: "repo-b"},
		{ID: 3, RepoName: "repo-a"},
	}
	m.selectedIdx = 0
	m.selectedJobID = 1
	m.currentView = tuiViewQueue

	// Press 'f' to open filter modal
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	m2 := updated.(tuiModel)

	if m2.currentView != tuiViewFilter {
		t.Errorf("Expected tuiViewFilter, got %d", m2.currentView)
	}
	// filterRepos should be nil (loading state) until async fetch completes
	if m2.filterRepos != nil {
		t.Errorf("Expected filterRepos=nil (loading), got %d repos", len(m2.filterRepos))
	}
	if m2.filterSelectedIdx != 0 {
		t.Errorf("Expected filterSelectedIdx=0 (All repos), got %d", m2.filterSelectedIdx)
	}
	if m2.filterSearch != "" {
		t.Errorf("Expected empty filterSearch, got '%s'", m2.filterSearch)
	}
	if cmd == nil {
		t.Error("Expected a fetch command to be returned")
	}
}

func TestTUIFilterReposMsg(t *testing.T) {
	m := newTuiModel("http://localhost")
	m.currentView = tuiViewFilter

	// Simulate receiving repos from API
	repos := []repoFilterItem{
		{name: "repo-a", count: 2},
		{name: "repo-b", count: 1},
		{name: "repo-c", count: 1},
	}
	msg := tuiReposMsg{repos: repos, totalCount: 4}

	updated, _ := m.Update(msg)
	m2 := updated.(tuiModel)

	// Should have: All repos (prepended), then the 3 repos from API
	if len(m2.filterRepos) != 4 {
		t.Fatalf("Expected 4 filter repos, got %d", len(m2.filterRepos))
	}
	if m2.filterRepos[0].name != "" || m2.filterRepos[0].count != 4 {
		t.Errorf("Expected All repos with count 4, got name='%s' count=%d", m2.filterRepos[0].name, m2.filterRepos[0].count)
	}
	if m2.filterRepos[1].name != "repo-a" || m2.filterRepos[1].count != 2 {
		t.Errorf("Expected repo-a with count 2, got name='%s' count=%d", m2.filterRepos[1].name, m2.filterRepos[1].count)
	}
	if m2.filterRepos[2].name != "repo-b" || m2.filterRepos[2].count != 1 {
		t.Errorf("Expected repo-b with count 1, got name='%s' count=%d", m2.filterRepos[2].name, m2.filterRepos[2].count)
	}
	if m2.filterRepos[3].name != "repo-c" || m2.filterRepos[3].count != 1 {
		t.Errorf("Expected repo-c with count 1, got name='%s' count=%d", m2.filterRepos[3].name, m2.filterRepos[3].count)
	}
}

func TestTUIFilterSearch(t *testing.T) {
	m := newTuiModel("http://localhost")

	m.filterRepos = []repoFilterItem{
		{name: "", count: 10},
		{name: "repo-alpha", count: 5},
		{name: "repo-beta", count: 3},
		{name: "something-else", count: 2},
	}

	// No search - all visible
	visible := m.getVisibleFilterRepos()
	if len(visible) != 4 {
		t.Errorf("No search: expected 4 visible, got %d", len(visible))
	}

	// Search for "repo"
	m.filterSearch = "repo"
	visible = m.getVisibleFilterRepos()
	if len(visible) != 3 { // All repos + repo-alpha + repo-beta
		t.Errorf("Search 'repo': expected 3 visible, got %d", len(visible))
	}

	// Search for "alpha"
	m.filterSearch = "alpha"
	visible = m.getVisibleFilterRepos()
	if len(visible) != 2 { // All repos + repo-alpha
		t.Errorf("Search 'alpha': expected 2 visible, got %d", len(visible))
	}

	// Search for "xyz" - no matches
	m.filterSearch = "xyz"
	visible = m.getVisibleFilterRepos()
	if len(visible) != 1 { // Only "All repos" always included
		t.Errorf("Search 'xyz': expected 1 visible (All repos), got %d", len(visible))
	}
}

func TestTUIFilterNavigation(t *testing.T) {
	m := newTuiModel("http://localhost")
	m.currentView = tuiViewFilter
	m.filterRepos = []repoFilterItem{
		{name: "", count: 10},
		{name: "repo-a", count: 5},
		{name: "repo-b", count: 3},
	}
	m.filterSelectedIdx = 0

	// Navigate down
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m2 := updated.(tuiModel)
	if m2.filterSelectedIdx != 1 {
		t.Errorf("j key: expected filterSelectedIdx=1, got %d", m2.filterSelectedIdx)
	}

	// Navigate down again
	updated, _ = m2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m3 := updated.(tuiModel)
	if m3.filterSelectedIdx != 2 {
		t.Errorf("j key: expected filterSelectedIdx=2, got %d", m3.filterSelectedIdx)
	}

	// Navigate down at boundary - should stay at 2
	updated, _ = m3.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m4 := updated.(tuiModel)
	if m4.filterSelectedIdx != 2 {
		t.Errorf("j key at boundary: expected filterSelectedIdx=2, got %d", m4.filterSelectedIdx)
	}

	// Navigate up
	updated, _ = m4.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	m5 := updated.(tuiModel)
	if m5.filterSelectedIdx != 1 {
		t.Errorf("k key: expected filterSelectedIdx=1, got %d", m5.filterSelectedIdx)
	}
}

func TestTUIFilterSelectRepo(t *testing.T) {
	m := newTuiModel("http://localhost")

	m.jobs = []storage.ReviewJob{
		{ID: 1, RepoName: "repo-a"},
		{ID: 2, RepoName: "repo-b"},
		{ID: 3, RepoName: "repo-a"},
	}
	m.currentView = tuiViewFilter
	m.filterRepos = []repoFilterItem{
		{name: "", rootPath: "", count: 3},
		{name: "repo-a", rootPath: "/path/to/repo-a", count: 2},
		{name: "repo-b", rootPath: "/path/to/repo-b", count: 1},
	}
	m.filterSelectedIdx = 1 // repo-a

	// Press enter to select
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m2 := updated.(tuiModel)

	if m2.currentView != tuiViewQueue {
		t.Errorf("Expected tuiViewQueue, got %d", m2.currentView)
	}
	if m2.activeRepoFilter != "/path/to/repo-a" {
		t.Errorf("Expected activeRepoFilter='/path/to/repo-a', got '%s'", m2.activeRepoFilter)
	}
	// Selection is invalidated until refetch completes (prevents race condition)
	if m2.selectedIdx != -1 {
		t.Errorf("Expected selectedIdx=-1 (invalidated pending refetch), got %d", m2.selectedIdx)
	}
}

func TestTUIFilterClearWithEsc(t *testing.T) {
	m := newTuiModel("http://localhost")

	m.jobs = []storage.ReviewJob{
		{ID: 1, RepoName: "repo-a", RepoPath: "/path/to/repo-a"},
		{ID: 2, RepoName: "repo-b", RepoPath: "/path/to/repo-b"},
	}
	m.selectedIdx = 0
	m.selectedJobID = 1
	m.currentView = tuiViewQueue
	m.activeRepoFilter = "/path/to/repo-a"

	// Press Esc to clear filter
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEscape})
	m2 := updated.(tuiModel)

	if m2.activeRepoFilter != "" {
		t.Errorf("Expected activeRepoFilter to be cleared, got '%s'", m2.activeRepoFilter)
	}
	// Selection is invalidated until refetch completes (prevents race condition)
	if m2.selectedIdx != -1 {
		t.Errorf("Expected selectedIdx=-1 (invalidated pending refetch), got %d", m2.selectedIdx)
	}
}

func TestTUIFilterEscapeCloses(t *testing.T) {
	m := newTuiModel("http://localhost")
	m.currentView = tuiViewFilter
	m.filterSearch = "test"
	m.filterRepos = []repoFilterItem{{name: "", count: 1}}

	// Press 'esc' to close without selecting
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEscape})
	m2 := updated.(tuiModel)

	if m2.currentView != tuiViewQueue {
		t.Errorf("Expected tuiViewQueue, got %d", m2.currentView)
	}
	if m2.filterSearch != "" {
		t.Errorf("Expected filterSearch to be cleared, got '%s'", m2.filterSearch)
	}
}

func TestTUIFilterTypingSearch(t *testing.T) {
	m := newTuiModel("http://localhost")
	m.currentView = tuiViewFilter
	m.filterRepos = []repoFilterItem{
		{name: "", count: 10},
		{name: "repo-a", count: 5},
	}
	m.filterSelectedIdx = 1

	// Type 'a'
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m2 := updated.(tuiModel)

	if m2.filterSearch != "a" {
		t.Errorf("Expected filterSearch='a', got '%s'", m2.filterSearch)
	}
	if m2.filterSelectedIdx != 0 {
		t.Errorf("Expected filterSelectedIdx reset to 0, got %d", m2.filterSelectedIdx)
	}

	// Type 'b'
	updated, _ = m2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	m3 := updated.(tuiModel)

	if m3.filterSearch != "ab" {
		t.Errorf("Expected filterSearch='ab', got '%s'", m3.filterSearch)
	}

	// Backspace
	updated, _ = m3.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	m4 := updated.(tuiModel)

	if m4.filterSearch != "a" {
		t.Errorf("Expected filterSearch='a' after backspace, got '%s'", m4.filterSearch)
	}
}

func TestTUIQueueNavigationWithFilter(t *testing.T) {
	m := newTuiModel("http://localhost")

	// Jobs from two repos, interleaved
	m.jobs = []storage.ReviewJob{
		{ID: 1, RepoName: "repo-a", RepoPath: "/path/to/repo-a"},
		{ID: 2, RepoName: "repo-b", RepoPath: "/path/to/repo-b"},
		{ID: 3, RepoName: "repo-a", RepoPath: "/path/to/repo-a"},
		{ID: 4, RepoName: "repo-b", RepoPath: "/path/to/repo-b"},
		{ID: 5, RepoName: "repo-a", RepoPath: "/path/to/repo-a"},
	}
	m.selectedIdx = 0
	m.selectedJobID = 1
	m.currentView = tuiViewQueue
	m.activeRepoFilter = "/path/to/repo-a" // Filter to only repo-a jobs

	// Navigate down - should skip repo-b jobs
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m2 := updated.(tuiModel)

	// Should jump from ID=1 (idx 0) to ID=3 (idx 2), skipping ID=2 (repo-b)
	if m2.selectedIdx != 2 {
		t.Errorf("Expected selectedIdx=2, got %d", m2.selectedIdx)
	}
	if m2.selectedJobID != 3 {
		t.Errorf("Expected selectedJobID=3, got %d", m2.selectedJobID)
	}

	// Navigate down again - should go to ID=5
	updated, _ = m2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m3 := updated.(tuiModel)

	if m3.selectedIdx != 4 {
		t.Errorf("Expected selectedIdx=4, got %d", m3.selectedIdx)
	}
	if m3.selectedJobID != 5 {
		t.Errorf("Expected selectedJobID=5, got %d", m3.selectedJobID)
	}

	// Navigate up - should go back to ID=3
	updated, _ = m3.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	m4 := updated.(tuiModel)

	if m4.selectedIdx != 2 {
		t.Errorf("Expected selectedIdx=2, got %d", m4.selectedIdx)
	}
}

func TestTUIGetVisibleJobs(t *testing.T) {
	m := newTuiModel("http://localhost")

	m.jobs = []storage.ReviewJob{
		{ID: 1, RepoName: "repo-a", RepoPath: "/path/to/repo-a"},
		{ID: 2, RepoName: "repo-b", RepoPath: "/path/to/repo-b"},
		{ID: 3, RepoName: "repo-a", RepoPath: "/path/to/repo-a"},
	}

	// No filter - all jobs visible
	visible := m.getVisibleJobs()
	if len(visible) != 3 {
		t.Errorf("No filter: expected 3 visible, got %d", len(visible))
	}

	// Filter to repo-a
	m.activeRepoFilter = "/path/to/repo-a"
	visible = m.getVisibleJobs()
	if len(visible) != 2 {
		t.Errorf("Filter repo-a: expected 2 visible, got %d", len(visible))
	}
	if visible[0].ID != 1 || visible[1].ID != 3 {
		t.Errorf("Expected IDs 1 and 3, got %d and %d", visible[0].ID, visible[1].ID)
	}

	// Filter to non-existent repo
	m.activeRepoFilter = "/path/to/repo-xyz"
	visible = m.getVisibleJobs()
	if len(visible) != 0 {
		t.Errorf("Filter repo-xyz: expected 0 visible, got %d", len(visible))
	}
}

func TestTUIGetVisibleSelectedIdx(t *testing.T) {
	m := newTuiModel("http://localhost")

	m.jobs = []storage.ReviewJob{
		{ID: 1, RepoName: "repo-a", RepoPath: "/path/to/repo-a"},
		{ID: 2, RepoName: "repo-b", RepoPath: "/path/to/repo-b"},
		{ID: 3, RepoName: "repo-a", RepoPath: "/path/to/repo-a"},
	}

	// No filter, valid selection
	m.selectedIdx = 1
	if idx := m.getVisibleSelectedIdx(); idx != 1 {
		t.Errorf("No filter, selectedIdx=1: expected 1, got %d", idx)
	}

	// No filter, selectedIdx=-1 returns -1
	m.selectedIdx = -1
	if idx := m.getVisibleSelectedIdx(); idx != -1 {
		t.Errorf("No filter, selectedIdx=-1: expected -1, got %d", idx)
	}

	// With filter, selectedIdx=-1 returns -1
	m.activeRepoFilter = "/path/to/repo-a"
	m.selectedIdx = -1
	if idx := m.getVisibleSelectedIdx(); idx != -1 {
		t.Errorf("Filter active, selectedIdx=-1: expected -1, got %d", idx)
	}

	// With filter, selection matches visible job (job ID=3 is second visible in repo-a)
	m.selectedIdx = 2 // index in m.jobs for job ID=3
	if idx := m.getVisibleSelectedIdx(); idx != 1 {
		t.Errorf("Filter active, selectedIdx=2 (ID=3): expected visible idx 1, got %d", idx)
	}

	// With filter, selection doesn't match filter - returns -1
	m.selectedIdx = 1 // index in m.jobs for job ID=2 (repo-b, not visible)
	if idx := m.getVisibleSelectedIdx(); idx != -1 {
		t.Errorf("Filter active, selection not visible: expected -1, got %d", idx)
	}
}

func TestTUIJobsRefreshWithFilter(t *testing.T) {
	m := newTuiModel("http://localhost")

	// Initial state with filter active
	m.jobs = []storage.ReviewJob{
		{ID: 1, RepoName: "repo-a", RepoPath: "/path/to/repo-a"},
		{ID: 2, RepoName: "repo-b", RepoPath: "/path/to/repo-b"},
		{ID: 3, RepoName: "repo-a", RepoPath: "/path/to/repo-a"},
	}
	m.selectedIdx = 2
	m.selectedJobID = 3
	m.activeRepoFilter = "/path/to/repo-a"

	// Jobs refresh - same jobs
	newJobs := tuiJobsMsg{jobs: []storage.ReviewJob{
		{ID: 1, RepoName: "repo-a", RepoPath: "/path/to/repo-a"},
		{ID: 2, RepoName: "repo-b", RepoPath: "/path/to/repo-b"},
		{ID: 3, RepoName: "repo-a", RepoPath: "/path/to/repo-a"},
	}}

	updated, _ := m.Update(newJobs)
	m2 := updated.(tuiModel)

	// Selection should be maintained
	if m2.selectedIdx != 2 {
		t.Errorf("Expected selectedIdx=2, got %d", m2.selectedIdx)
	}
	if m2.selectedJobID != 3 {
		t.Errorf("Expected selectedJobID=3, got %d", m2.selectedJobID)
	}

	// Now the selected job is removed
	newJobs = tuiJobsMsg{jobs: []storage.ReviewJob{
		{ID: 1, RepoName: "repo-a", RepoPath: "/path/to/repo-a"},
		{ID: 2, RepoName: "repo-b", RepoPath: "/path/to/repo-b"},
	}}

	updated, _ = m2.Update(newJobs)
	m3 := updated.(tuiModel)

	// Should select first visible job (ID=1, repo-a)
	if m3.selectedIdx != 0 {
		t.Errorf("Expected selectedIdx=0, got %d", m3.selectedIdx)
	}
	if m3.selectedJobID != 1 {
		t.Errorf("Expected selectedJobID=1, got %d", m3.selectedJobID)
	}
}

func TestTUIFilterPreselectsCurrent(t *testing.T) {
	m := newTuiModel("http://localhost")
	m.currentView = tuiViewFilter
	m.activeRepoFilter = "/path/to/repo-b" // Already filtering to repo-b

	// Simulate receiving repos from API (should pre-select repo-b)
	repos := []repoFilterItem{
		{name: "repo-a", rootPath: "/path/to/repo-a", count: 1},
		{name: "repo-b", rootPath: "/path/to/repo-b", count: 1},
	}
	msg := tuiReposMsg{repos: repos, totalCount: 2}

	updated, _ := m.Update(msg)
	m2 := updated.(tuiModel)

	// filterRepos should be: All repos, repo-a, repo-b
	// repo-b should be at index 2, which should be pre-selected
	if m2.filterSelectedIdx != 2 {
		t.Errorf("Expected filterSelectedIdx=2 (repo-b), got %d", m2.filterSelectedIdx)
	}
}

func TestTUIFilterToZeroVisibleJobs(t *testing.T) {
	m := newTuiModel("http://localhost")

	// Jobs only in repo-a
	m.jobs = []storage.ReviewJob{
		{ID: 1, RepoName: "repo-a", RepoPath: "/path/to/repo-a"},
		{ID: 2, RepoName: "repo-a", RepoPath: "/path/to/repo-a"},
	}
	m.selectedIdx = 0
	m.selectedJobID = 1
	m.currentView = tuiViewFilter
	m.filterRepos = []repoFilterItem{
		{name: "", rootPath: "", count: 2},
		{name: "repo-a", rootPath: "/path/to/repo-a", count: 2},
		{name: "repo-b", rootPath: "/path/to/repo-b", count: 0}, // No jobs
	}
	m.filterSelectedIdx = 2 // Select repo-b

	// Press enter to select repo-b (triggers refetch)
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m2 := updated.(tuiModel)

	// Filter should be applied and a fetchJobs command should be returned
	if m2.activeRepoFilter != "/path/to/repo-b" {
		t.Errorf("Expected activeRepoFilter='/path/to/repo-b', got '%s'", m2.activeRepoFilter)
	}
	if cmd == nil {
		t.Error("Expected fetchJobs command to be returned")
	}
	// Selection is invalidated until refetch completes (prevents race condition)
	if m2.selectedIdx != -1 {
		t.Errorf("Expected selectedIdx=-1 pending refetch, got %d", m2.selectedIdx)
	}
	if m2.selectedJobID != 0 {
		t.Errorf("Expected selectedJobID=0 pending refetch, got %d", m2.selectedJobID)
	}

	// Simulate receiving empty jobs from API (repo-b has no jobs)
	updated2, _ := m2.Update(tuiJobsMsg{jobs: []storage.ReviewJob{}})
	m3 := updated2.(tuiModel)

	// Now selection should be cleared since no jobs
	if m3.selectedIdx != -1 {
		t.Errorf("Expected selectedIdx=-1 after receiving empty jobs, got %d", m3.selectedIdx)
	}
	if m3.selectedJobID != 0 {
		t.Errorf("Expected selectedJobID=0 after receiving empty jobs, got %d", m3.selectedJobID)
	}
}

func TestTUIRefreshWithZeroVisibleJobs(t *testing.T) {
	m := newTuiModel("http://localhost")

	// Start with jobs in repo-a, filter active for repo-b
	m.jobs = []storage.ReviewJob{
		{ID: 1, RepoName: "repo-a", RepoPath: "/path/to/repo-a"},
	}
	m.activeRepoFilter = "/path/to/repo-b" // Filter to repo with no jobs
	m.selectedIdx = 0
	m.selectedJobID = 1

	// Simulate jobs refresh
	newJobs := []storage.ReviewJob{
		{ID: 1, RepoName: "repo-a", RepoPath: "/path/to/repo-a"},
		{ID: 2, RepoName: "repo-a", RepoPath: "/path/to/repo-a"},
	}
	updated, _ := m.Update(tuiJobsMsg{jobs: newJobs})
	m2 := updated.(tuiModel)

	// Selection should be cleared since no jobs match filter
	if m2.selectedIdx != -1 {
		t.Errorf("Expected selectedIdx=-1 for zero visible jobs after refresh, got %d", m2.selectedIdx)
	}
	if m2.selectedJobID != 0 {
		t.Errorf("Expected selectedJobID=0 for zero visible jobs after refresh, got %d", m2.selectedJobID)
	}
}

func TestTUIActionsNoOpWithZeroVisibleJobs(t *testing.T) {
	m := newTuiModel("http://localhost")

	// Setup: filter active with no matching jobs
	m.jobs = []storage.ReviewJob{
		{ID: 1, RepoName: "repo-a", RepoPath: "/path/to/repo-a", Status: storage.JobStatusDone},
	}
	m.activeRepoFilter = "/path/to/repo-b"
	m.selectedIdx = -1
	m.selectedJobID = 0
	m.currentView = tuiViewQueue

	// Press enter - should be no-op
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m2 := updated.(tuiModel)
	if cmd != nil {
		t.Error("Expected no command for enter with no visible jobs")
	}
	if m2.currentView != tuiViewQueue {
		t.Errorf("Expected to stay in queue view, got %d", m2.currentView)
	}

	// Press 'x' (cancel) - should be no-op
	updated, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	if cmd != nil {
		t.Error("Expected no command for cancel with no visible jobs")
	}

	// Press 'a' (address) - should be no-op
	updated, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	if cmd != nil {
		t.Error("Expected no command for address with no visible jobs")
	}
}

func TestTUIFilterViewSmallTerminal(t *testing.T) {
	m := newTuiModel("http://localhost")
	m.currentView = tuiViewFilter
	m.filterRepos = []repoFilterItem{
		{name: "", count: 10},
		{name: "repo-a", count: 5},
		{name: "repo-b", count: 3},
		{name: "repo-c", count: 2},
	}
	m.filterSelectedIdx = 0

	t.Run("tiny terminal shows message", func(t *testing.T) {
		m.height = 5 // Less than reservedLines (7)
		output := m.renderFilterView()

		if !strings.Contains(output, "(terminal too small)") {
			t.Errorf("Expected 'terminal too small' message for height=5, got: %s", output)
		}
		// Should not contain any repo names
		if strings.Contains(output, "repo-a") {
			t.Error("Should not render repo names when terminal too small")
		}
	})

	t.Run("exactly reservedLines shows no repos", func(t *testing.T) {
		m.height = 7 // Exactly reservedLines, visibleRows = 0
		output := m.renderFilterView()

		if !strings.Contains(output, "(terminal too small)") {
			t.Errorf("Expected 'terminal too small' message for height=7, got: %s", output)
		}
	})

	t.Run("one row available", func(t *testing.T) {
		m.height = 8 // reservedLines + 1 = visibleRows of 1
		output := m.renderFilterView()

		if strings.Contains(output, "(terminal too small)") {
			t.Error("Should not show 'terminal too small' when 1 row available")
		}
		// Should show exactly one repo line (All repos)
		if !strings.Contains(output, "All repos") {
			t.Error("Should show 'All repos' when 1 row available")
		}
		// Should show scroll info since 4 repos > 1 visible row
		if !strings.Contains(output, "[showing 1-1 of 4]") {
			t.Errorf("Expected scroll info '[showing 1-1 of 4]', got: %s", output)
		}
	})

	t.Run("fits all repos without scroll", func(t *testing.T) {
		m.height = 15 // reservedLines(7) + 8 = visibleRows of 8, enough for 4 repos
		output := m.renderFilterView()

		// Should show all repos
		if !strings.Contains(output, "All repos") {
			t.Error("Should show 'All repos'")
		}
		if !strings.Contains(output, "repo-a") {
			t.Error("Should show 'repo-a'")
		}
		if !strings.Contains(output, "repo-c") {
			t.Error("Should show 'repo-c'")
		}
		// Should NOT show scroll info
		if strings.Contains(output, "[showing") {
			t.Error("Should not show scroll info when all repos fit")
		}
	})

	t.Run("needs scrolling shows scroll info", func(t *testing.T) {
		m.height = 9  // visibleRows = 2
		m.filterSelectedIdx = 2 // Select repo-b
		output := m.renderFilterView()

		// Should show scroll info
		if !strings.Contains(output, "[showing") {
			t.Error("Expected scroll info when repos exceed visible rows")
		}
		// Selected item (repo-b) should be visible
		if !strings.Contains(output, "repo-b") {
			t.Error("Selected repo should be visible in scroll window")
		}
	})
}

func TestTUIFilterViewScrollWindow(t *testing.T) {
	m := newTuiModel("http://localhost")
	m.currentView = tuiViewFilter
	m.filterRepos = []repoFilterItem{
		{name: "", count: 20},
		{name: "repo-1", count: 5},
		{name: "repo-2", count: 4},
		{name: "repo-3", count: 3},
		{name: "repo-4", count: 2},
		{name: "repo-5", count: 1},
	}
	m.height = 10 // visibleRows = 3

	t.Run("scroll keeps selected item visible at top", func(t *testing.T) {
		m.filterSelectedIdx = 0
		output := m.renderFilterView()

		if !strings.Contains(output, "[showing 1-3 of 6]") {
			t.Errorf("Expected '[showing 1-3 of 6]' for top selection, got: %s", output)
		}
	})

	t.Run("scroll keeps selected item visible at bottom", func(t *testing.T) {
		m.filterSelectedIdx = 5 // repo-5
		output := m.renderFilterView()

		if !strings.Contains(output, "[showing 4-6 of 6]") {
			t.Errorf("Expected '[showing 4-6 of 6]' for bottom selection, got: %s", output)
		}
		if !strings.Contains(output, "repo-5") {
			t.Error("repo-5 should be visible when selected")
		}
	})

	t.Run("scroll centers selected item in middle", func(t *testing.T) {
		m.filterSelectedIdx = 3 // repo-3
		output := m.renderFilterView()

		// With 3 visible rows and selecting item 3 (0-indexed), centering puts start at 2
		if !strings.Contains(output, "repo-3") {
			t.Error("repo-3 should be visible when selected")
		}
	})
}

// Tests for j/k and left/right review navigation

func TestTUIReviewNavigationJNext(t *testing.T) {
	// Test 'j' navigates to next viewable job (higher index) in review view
	m := newTuiModel("http://localhost")

	// Setup: 5 jobs, middle ones are queued/running (not viewable)
	m.jobs = []storage.ReviewJob{
		{ID: 1, Status: storage.JobStatusDone},
		{ID: 2, Status: storage.JobStatusQueued},
		{ID: 3, Status: storage.JobStatusRunning},
		{ID: 4, Status: storage.JobStatusFailed},
		{ID: 5, Status: storage.JobStatusDone},
	}
	m.selectedIdx = 0
	m.selectedJobID = 1
	m.currentView = tuiViewReview
	m.currentReview = &storage.Review{ID: 10, Job: &storage.ReviewJob{ID: 1}}
	m.reviewScroll = 5 // Ensure scroll resets

	// Press 'j' - should skip to job 4 (failed, viewable)
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m2 := updated.(tuiModel)

	if m2.selectedIdx != 3 {
		t.Errorf("Expected selectedIdx=3 (job ID=4), got %d", m2.selectedIdx)
	}
	if m2.selectedJobID != 4 {
		t.Errorf("Expected selectedJobID=4, got %d", m2.selectedJobID)
	}
	if m2.reviewScroll != 0 {
		t.Errorf("Expected reviewScroll to reset to 0, got %d", m2.reviewScroll)
	}
	// For failed jobs, currentReview is set inline (no fetch command)
	if m2.currentReview == nil {
		t.Error("Expected currentReview to be set for failed job")
	}
	if cmd != nil {
		t.Error("Expected no command for failed job (inline display)")
	}
}

func TestTUIReviewNavigationKPrev(t *testing.T) {
	// Test 'k' navigates to previous viewable job (lower index) in review view
	m := newTuiModel("http://localhost")

	m.jobs = []storage.ReviewJob{
		{ID: 1, Status: storage.JobStatusDone},
		{ID: 2, Status: storage.JobStatusQueued},
		{ID: 3, Status: storage.JobStatusRunning},
		{ID: 4, Status: storage.JobStatusFailed},
		{ID: 5, Status: storage.JobStatusDone},
	}
	m.selectedIdx = 4
	m.selectedJobID = 5
	m.currentView = tuiViewReview
	m.currentReview = &storage.Review{ID: 50, Job: &storage.ReviewJob{ID: 5}}
	m.reviewScroll = 10

	// Press 'k' - should skip to job 4 (failed, viewable)
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	m2 := updated.(tuiModel)

	if m2.selectedIdx != 3 {
		t.Errorf("Expected selectedIdx=3 (job ID=4), got %d", m2.selectedIdx)
	}
	if m2.selectedJobID != 4 {
		t.Errorf("Expected selectedJobID=4, got %d", m2.selectedJobID)
	}
	if m2.reviewScroll != 0 {
		t.Errorf("Expected reviewScroll to reset to 0, got %d", m2.reviewScroll)
	}
}

func TestTUIReviewNavigationLeftRight(t *testing.T) {
	// Test left/right arrows mirror j/k in review view
	m := newTuiModel("http://localhost")

	m.jobs = []storage.ReviewJob{
		{ID: 1, Status: storage.JobStatusDone},
		{ID: 2, Status: storage.JobStatusDone},
		{ID: 3, Status: storage.JobStatusDone},
	}
	m.selectedIdx = 1
	m.selectedJobID = 2
	m.currentView = tuiViewReview
	m.currentReview = &storage.Review{ID: 20, Job: &storage.ReviewJob{ID: 2}}

	// Press 'left' - should navigate to next (higher index), like 'j'
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	m2 := updated.(tuiModel)

	if m2.selectedIdx != 2 {
		t.Errorf("Left arrow: expected selectedIdx=2, got %d", m2.selectedIdx)
	}
	if m2.selectedJobID != 3 {
		t.Errorf("Left arrow: expected selectedJobID=3, got %d", m2.selectedJobID)
	}
	// Should trigger fetch for done job
	if cmd == nil {
		t.Error("Left arrow: expected fetch command for done job")
	}

	// Reset and test 'right' - should navigate to prev (lower index), like 'k'
	m.selectedIdx = 1
	m.selectedJobID = 2
	m.currentReview = &storage.Review{ID: 20, Job: &storage.ReviewJob{ID: 2}}

	updated, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRight})
	m3 := updated.(tuiModel)

	if m3.selectedIdx != 0 {
		t.Errorf("Right arrow: expected selectedIdx=0, got %d", m3.selectedIdx)
	}
	if m3.selectedJobID != 1 {
		t.Errorf("Right arrow: expected selectedJobID=1, got %d", m3.selectedJobID)
	}
	if cmd == nil {
		t.Error("Right arrow: expected fetch command for done job")
	}
}

func TestTUIReviewNavigationBoundaries(t *testing.T) {
	// Test navigation at boundaries (first/last viewable job)
	m := newTuiModel("http://localhost")

	m.jobs = []storage.ReviewJob{
		{ID: 1, Status: storage.JobStatusDone},
		{ID: 2, Status: storage.JobStatusQueued}, // Not viewable
		{ID: 3, Status: storage.JobStatusDone},
	}
	m.selectedIdx = 0
	m.selectedJobID = 1
	m.currentView = tuiViewReview
	m.currentReview = &storage.Review{ID: 10, Job: &storage.ReviewJob{ID: 1}}

	// Press 'k' at first viewable job - should be no-op
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	m2 := updated.(tuiModel)

	if m2.selectedIdx != 0 {
		t.Errorf("Expected selectedIdx to remain 0 at boundary, got %d", m2.selectedIdx)
	}
	if m2.selectedJobID != 1 {
		t.Errorf("Expected selectedJobID to remain 1 at boundary, got %d", m2.selectedJobID)
	}
	if cmd != nil {
		t.Error("Expected no command at boundary")
	}

	// Now at last viewable job
	m.selectedIdx = 2
	m.selectedJobID = 3
	m.currentReview = &storage.Review{ID: 30, Job: &storage.ReviewJob{ID: 3}}

	// Press 'j' at last viewable job - should be no-op
	updated, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m3 := updated.(tuiModel)

	if m3.selectedIdx != 2 {
		t.Errorf("Expected selectedIdx to remain 2 at boundary, got %d", m3.selectedIdx)
	}
	if m3.selectedJobID != 3 {
		t.Errorf("Expected selectedJobID to remain 3 at boundary, got %d", m3.selectedJobID)
	}
	if cmd != nil {
		t.Error("Expected no command at boundary")
	}
}

func TestTUIReviewNavigationFailedJobInline(t *testing.T) {
	// Test that navigating to a failed job displays error inline
	m := newTuiModel("http://localhost")

	m.jobs = []storage.ReviewJob{
		{ID: 1, Status: storage.JobStatusDone},
		{ID: 2, Status: storage.JobStatusFailed, Agent: "codex", Error: "something went wrong"},
	}
	m.selectedIdx = 0
	m.selectedJobID = 1
	m.currentView = tuiViewReview
	m.currentReview = &storage.Review{ID: 10, Job: &storage.ReviewJob{ID: 1}}

	// Press 'j' - should navigate to failed job and display inline
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m2 := updated.(tuiModel)

	if m2.selectedIdx != 1 {
		t.Errorf("Expected selectedIdx=1, got %d", m2.selectedIdx)
	}
	if m2.currentReview == nil {
		t.Fatal("Expected currentReview to be set for failed job")
	}
	if m2.currentReview.Agent != "codex" {
		t.Errorf("Expected agent='codex', got '%s'", m2.currentReview.Agent)
	}
	if !strings.Contains(m2.currentReview.Output, "something went wrong") {
		t.Errorf("Expected output to contain error, got '%s'", m2.currentReview.Output)
	}
	// No fetch command for failed jobs - displayed inline
	if cmd != nil {
		t.Error("Expected no command for failed job (inline display)")
	}
}

func TestTUIReviewStaleResponseIgnored(t *testing.T) {
	// Test that stale review responses are ignored (race condition fix)
	m := newTuiModel("http://localhost")

	m.jobs = []storage.ReviewJob{
		{ID: 1, Status: storage.JobStatusDone},
		{ID: 2, Status: storage.JobStatusDone},
	}
	m.selectedIdx = 1
	m.selectedJobID = 2 // Currently viewing job 2
	m.currentView = tuiViewReview
	m.currentReview = &storage.Review{ID: 20, Output: "Review for job 2", Job: &storage.ReviewJob{ID: 2}}

	// Simulate a stale response arriving for job 1 (user navigated away)
	staleMsg := tuiReviewMsg{
		review: &storage.Review{ID: 10, Output: "Stale review for job 1", Job: &storage.ReviewJob{ID: 1}},
		jobID:  1, // This doesn't match selectedJobID (2)
	}

	updated, _ := m.Update(staleMsg)
	m2 := updated.(tuiModel)

	// Should ignore the stale response
	if m2.currentReview.Output != "Review for job 2" {
		t.Errorf("Expected stale response to be ignored, got output: %s", m2.currentReview.Output)
	}
	if m2.currentReview.ID != 20 {
		t.Errorf("Expected review ID to remain 20, got %d", m2.currentReview.ID)
	}
}

func TestTUIReviewMsgWithMatchingJobID(t *testing.T) {
	// Test that review responses with matching job ID are accepted
	m := newTuiModel("http://localhost")

	m.jobs = []storage.ReviewJob{
		{ID: 1, Status: storage.JobStatusDone},
	}
	m.selectedIdx = 0
	m.selectedJobID = 1
	m.currentView = tuiViewQueue // Still in queue view, waiting for fetch

	validMsg := tuiReviewMsg{
		review: &storage.Review{ID: 10, Output: "New review", Job: &storage.ReviewJob{ID: 1}},
		jobID:  1,
	}

	updated, _ := m.Update(validMsg)
	m2 := updated.(tuiModel)

	// Should accept the response and switch to review view
	if m2.currentView != tuiViewReview {
		t.Errorf("Expected to switch to review view, got %d", m2.currentView)
	}
	if m2.currentReview == nil || m2.currentReview.Output != "New review" {
		t.Error("Expected currentReview to be updated")
	}
	if m2.reviewScroll != 0 {
		t.Errorf("Expected reviewScroll to be 0, got %d", m2.reviewScroll)
	}
}

func TestTUISelectionSyncInReviewView(t *testing.T) {
	// Test that selectedIdx syncs with currentReview.Job.ID when jobs refresh
	m := newTuiModel("http://localhost")

	// Initial state: viewing review for job 2
	m.jobs = []storage.ReviewJob{
		{ID: 3, Status: storage.JobStatusDone},
		{ID: 2, Status: storage.JobStatusDone},
		{ID: 1, Status: storage.JobStatusDone},
	}
	m.selectedIdx = 1
	m.selectedJobID = 2
	m.currentView = tuiViewReview
	m.currentReview = &storage.Review{ID: 20, Job: &storage.ReviewJob{ID: 2}}

	// New job arrives at the top, shifting indices
	newJobs := tuiJobsMsg{jobs: []storage.ReviewJob{
		{ID: 4, Status: storage.JobStatusDone}, // New job at top
		{ID: 3, Status: storage.JobStatusDone},
		{ID: 2, Status: storage.JobStatusDone}, // Now at index 2
		{ID: 1, Status: storage.JobStatusDone},
	}}

	updated, _ := m.Update(newJobs)
	m2 := updated.(tuiModel)

	// selectedIdx should sync with currentReview.Job.ID (2), now at index 2
	if m2.selectedIdx != 2 {
		t.Errorf("Expected selectedIdx=2 (synced with review job), got %d", m2.selectedIdx)
	}
	if m2.selectedJobID != 2 {
		t.Errorf("Expected selectedJobID=2, got %d", m2.selectedJobID)
	}
}

func TestTUIQueueViewNavigationUpDown(t *testing.T) {
	// Test up/down/j/k navigation in queue view
	m := newTuiModel("http://localhost")

	m.jobs = []storage.ReviewJob{
		{ID: 1, Status: storage.JobStatusDone},
		{ID: 2, Status: storage.JobStatusQueued},
		{ID: 3, Status: storage.JobStatusDone},
	}
	m.selectedIdx = 1
	m.selectedJobID = 2
	m.currentView = tuiViewQueue

	// 'j' in queue view moves down (higher index)
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m2 := updated.(tuiModel)

	if m2.selectedIdx != 2 {
		t.Errorf("j key: expected selectedIdx=2, got %d", m2.selectedIdx)
	}
	if m2.selectedJobID != 3 {
		t.Errorf("j key: expected selectedJobID=3, got %d", m2.selectedJobID)
	}

	// 'k' in queue view moves up (lower index)
	updated, _ = m2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	m3 := updated.(tuiModel)

	if m3.selectedIdx != 1 {
		t.Errorf("k key: expected selectedIdx=1, got %d", m3.selectedIdx)
	}
}

func TestTUIQueueViewArrowsMatchUpDown(t *testing.T) {
	// Test that left/right in queue view work like k/j (up/down)
	m := newTuiModel("http://localhost")

	m.jobs = []storage.ReviewJob{
		{ID: 1, Status: storage.JobStatusDone},
		{ID: 2, Status: storage.JobStatusDone},
		{ID: 3, Status: storage.JobStatusDone},
	}
	m.selectedIdx = 1
	m.selectedJobID = 2
	m.currentView = tuiViewQueue

	// 'left' in queue view should move down (like j)
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	m2 := updated.(tuiModel)

	if m2.selectedIdx != 2 {
		t.Errorf("Left arrow: expected selectedIdx=2, got %d", m2.selectedIdx)
	}

	// Reset
	m2.selectedIdx = 1
	m2.selectedJobID = 2

	// 'right' in queue view should move up (like k)
	updated, _ = m2.Update(tea.KeyMsg{Type: tea.KeyRight})
	m3 := updated.(tuiModel)

	if m3.selectedIdx != 0 {
		t.Errorf("Right arrow: expected selectedIdx=0, got %d", m3.selectedIdx)
	}
}

func TestTUIJobsRefreshDuringReviewNavigation(t *testing.T) {
	// Test that jobs refresh during review navigation doesn't reset selection
	// This tests the race condition fix: user navigates to job 3, but jobs refresh
	// arrives before the review loads. Selection should stay on job 3, not revert
	// to the currently displayed review's job (job 2).
	m := newTuiModel("http://localhost")

	m.jobs = []storage.ReviewJob{
		{ID: 1, Status: storage.JobStatusDone},
		{ID: 2, Status: storage.JobStatusDone},
		{ID: 3, Status: storage.JobStatusDone},
	}
	m.selectedIdx = 1
	m.selectedJobID = 2
	m.currentView = tuiViewReview
	m.currentReview = &storage.Review{ID: 20, Output: "Review for job 2", Job: &storage.ReviewJob{ID: 2}}

	// Simulate user navigating to next review (job 3)
	// This updates selectedIdx and selectedJobID but doesn't update currentReview yet
	m.selectedIdx = 2
	m.selectedJobID = 3

	// Before the review for job 3 arrives, a jobs refresh comes in
	refreshedJobs := tuiJobsMsg{jobs: []storage.ReviewJob{
		{ID: 1, Status: storage.JobStatusDone},
		{ID: 2, Status: storage.JobStatusDone},
		{ID: 3, Status: storage.JobStatusDone},
	}}

	updated, _ := m.Update(refreshedJobs)
	m2 := updated.(tuiModel)

	// Selection should stay on job 3 (user's navigation intent), not revert to job 2
	if m2.selectedJobID != 3 {
		t.Errorf("Expected selectedJobID=3 (user's navigation), got %d", m2.selectedJobID)
	}
	if m2.selectedIdx != 2 {
		t.Errorf("Expected selectedIdx=2 (job 3's index), got %d", m2.selectedIdx)
	}

	// currentReview should still be the old one (review for job 3 hasn't loaded)
	if m2.currentReview.Job.ID != 2 {
		t.Errorf("Expected currentReview to still be job 2, got job %d", m2.currentReview.Job.ID)
	}

	// Now when the review for job 3 arrives, it should be accepted
	newReviewMsg := tuiReviewMsg{
		review: &storage.Review{ID: 30, Output: "Review for job 3", Job: &storage.ReviewJob{ID: 3}},
		jobID:  3,
	}

	updated, _ = m2.Update(newReviewMsg)
	m3 := updated.(tuiModel)

	if m3.currentReview.ID != 30 {
		t.Errorf("Expected new review ID=30, got %d", m3.currentReview.ID)
	}
	if m3.currentReview.Output != "Review for job 3" {
		t.Errorf("Expected new review output, got %s", m3.currentReview.Output)
	}
}

func TestTUIEmptyRefreshWhileViewingReview(t *testing.T) {
	// Test that transient empty jobs refresh doesn't break selection
	// when viewing a review. Selection should restore to displayed review
	// when jobs repopulate.
	m := newTuiModel("http://localhost")

	m.jobs = []storage.ReviewJob{
		{ID: 1, Status: storage.JobStatusDone},
		{ID: 2, Status: storage.JobStatusDone},
		{ID: 3, Status: storage.JobStatusDone},
	}
	m.selectedIdx = 1
	m.selectedJobID = 2
	m.currentView = tuiViewReview
	m.currentReview = &storage.Review{ID: 20, Output: "Review for job 2", Job: &storage.ReviewJob{ID: 2}}

	// Transient empty refresh arrives
	emptyJobs := tuiJobsMsg{jobs: []storage.ReviewJob{}}

	updated, _ := m.Update(emptyJobs)
	m2 := updated.(tuiModel)

	// selectedJobID should be preserved (not cleared) while viewing a review
	if m2.selectedJobID != 2 {
		t.Errorf("Expected selectedJobID=2 preserved during empty refresh, got %d", m2.selectedJobID)
	}

	// Jobs repopulate
	repopulatedJobs := tuiJobsMsg{jobs: []storage.ReviewJob{
		{ID: 1, Status: storage.JobStatusDone},
		{ID: 2, Status: storage.JobStatusDone},
		{ID: 3, Status: storage.JobStatusDone},
	}}

	updated, _ = m2.Update(repopulatedJobs)
	m3 := updated.(tuiModel)

	// Selection should restore to job 2 (the displayed review)
	if m3.selectedJobID != 2 {
		t.Errorf("Expected selectedJobID=2 after repopulate, got %d", m3.selectedJobID)
	}
	if m3.selectedIdx != 1 {
		t.Errorf("Expected selectedIdx=1 (job 2's index), got %d", m3.selectedIdx)
	}
}

func TestTUIEmptyRefreshSeedsFromCurrentReview(t *testing.T) {
	// Test that if selectedJobID somehow becomes 0 while viewing a review,
	// it gets seeded from the current review when jobs repopulate
	m := newTuiModel("http://localhost")

	m.jobs = []storage.ReviewJob{}
	m.selectedIdx = 0
	m.selectedJobID = 0 // Somehow cleared
	m.currentView = tuiViewReview
	m.currentReview = &storage.Review{ID: 20, Output: "Review for job 2", Job: &storage.ReviewJob{ID: 2}}

	// Jobs repopulate
	repopulatedJobs := tuiJobsMsg{jobs: []storage.ReviewJob{
		{ID: 1, Status: storage.JobStatusDone},
		{ID: 2, Status: storage.JobStatusDone},
		{ID: 3, Status: storage.JobStatusDone},
	}}

	updated, _ := m.Update(repopulatedJobs)
	m2 := updated.(tuiModel)

	// Selection should be seeded from currentReview.Job.ID
	if m2.selectedJobID != 2 {
		t.Errorf("Expected selectedJobID=2 (seeded from currentReview), got %d", m2.selectedJobID)
	}
	if m2.selectedIdx != 1 {
		t.Errorf("Expected selectedIdx=1 (job 2's index), got %d", m2.selectedIdx)
	}
}

func TestTUICalculateColumnWidths(t *testing.T) {
	// Test that column widths fit within terminal for usable sizes (>= 80)
	// Narrower terminals may overflow - users should widen their terminal
	tests := []struct {
		name           string
		termWidth      int
		idWidth        int
		expectOverflow bool // true if overflow is acceptable for this width
	}{
		{
			name:           "wide terminal",
			termWidth:      200,
			idWidth:        3,
			expectOverflow: false,
		},
		{
			name:           "medium terminal",
			termWidth:      100,
			idWidth:        3,
			expectOverflow: false,
		},
		{
			name:           "standard terminal",
			termWidth:      80,
			idWidth:        3,
			expectOverflow: false,
		},
		{
			name:           "narrow terminal - overflow acceptable",
			termWidth:      60,
			idWidth:        3,
			expectOverflow: true, // Fixed columns alone need ~48 chars
		},
		{
			name:           "very narrow terminal - overflow expected",
			termWidth:      40,
			idWidth:        3,
			expectOverflow: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := tuiModel{width: tt.termWidth}
			widths := m.calculateColumnWidths(tt.idWidth)

			// All columns must have positive widths
			if widths.ref < 1 {
				t.Errorf("ref width %d < 1", widths.ref)
			}
			if widths.repo < 1 {
				t.Errorf("repo width %d < 1", widths.repo)
			}
			if widths.agent < 1 {
				t.Errorf("agent width %d < 1", widths.agent)
			}

			// Fixed widths: ID (idWidth), Status (10), Queued (12), Elapsed (8), Addr'd (6)
			// Plus spacing: 2 (prefix) + 7 spaces between columns
			fixedWidth := 2 + tt.idWidth + 10 + 12 + 8 + 6 + 7
			flexibleTotal := widths.ref + widths.repo + widths.agent
			totalWidth := fixedWidth + flexibleTotal

			if !tt.expectOverflow && totalWidth > tt.termWidth {
				t.Errorf("total width %d exceeds terminal width %d", totalWidth, tt.termWidth)
			}

			// Even with overflow, flexible columns should be minimal
			if tt.expectOverflow && flexibleTotal > 15 {
				t.Errorf("narrow terminal should minimize flexible columns, got %d", flexibleTotal)
			}
		})
	}
}

func TestTUICalculateColumnWidthsProportions(t *testing.T) {
	// On wide terminals, columns should use higher minimums
	m := tuiModel{width: 200}
	widths := m.calculateColumnWidths(3)

	if widths.ref < 10 {
		t.Errorf("wide terminal ref width %d < 10", widths.ref)
	}
	if widths.repo < 15 {
		t.Errorf("wide terminal repo width %d < 15", widths.repo)
	}
	if widths.agent < 10 {
		t.Errorf("wide terminal agent width %d < 10", widths.agent)
	}
}

func TestTUIRenderJobLineTruncation(t *testing.T) {
	m := tuiModel{width: 80}
	// Use a git range - shortRef truncates ranges to 17 chars max, then renderJobLine
	// truncates further based on colWidths.ref. Use a range longer than 17 chars.
	job := storage.ReviewJob{
		ID:         1,
		GitRef:     "abcdef1234567..ghijkl7890123", // 28 char range, shortRef -> 17 chars
		RepoName:   "very-long-repository-name-that-exceeds-width",
		Agent:      "super-long-agent-name",
		Status:     storage.JobStatusDone,
		EnqueuedAt: time.Now(),
	}

	// Use narrow column widths to force truncation
	// ref=10 will truncate the 17-char shortRef output
	colWidths := columnWidths{
		ref:   10,
		repo:  15,
		agent: 10,
	}

	line := m.renderJobLine(job, false, 3, colWidths)

	// Check that truncated values contain "..."
	if !strings.Contains(line, "...") {
		t.Errorf("Expected truncation with '...' in line: %s", line)
	}

	// The line should contain truncated versions, not full strings
	// shortRef reduces "abcdef1234567..ghijkl7890123" to "abcdef1234567..gh" (17 chars)
	// then renderJobLine truncates to colWidths.ref (10)
	if strings.Contains(line, "abcdef1234567..gh") {
		t.Error("Full git ref (after shortRef) should have been truncated")
	}
	if strings.Contains(line, "very-long-repository-name-that-exceeds-width") {
		t.Error("Full repo name should have been truncated")
	}
	if strings.Contains(line, "super-long-agent-name") {
		t.Error("Full agent name should have been truncated")
	}
}

func TestTUIRenderJobLineLength(t *testing.T) {
	// Test that rendered line length respects column widths
	m := tuiModel{width: 100}
	job := storage.ReviewJob{
		ID:         123,
		GitRef:     "abc1234..def5678901234567890", // Long range
		RepoName:   "my-very-long-repository-name-here",
		Agent:      "claude-code-agent",
		Status:     storage.JobStatusDone,
		EnqueuedAt: time.Now(),
	}

	idWidth := 4
	colWidths := columnWidths{
		ref:   12,
		repo:  15,
		agent: 10,
	}

	line := m.renderJobLine(job, false, idWidth, colWidths)

	// Fixed widths: ID (idWidth=4), Status (10), Queued (12), Elapsed (8), Addr'd (varies)
	// Plus spacing between columns
	// The line should not be excessively long
	// Note: line includes ANSI codes for status styling, so we check a reasonable max
	maxExpectedLen := idWidth + colWidths.ref + colWidths.repo + colWidths.agent + 10 + 12 + 8 + 10 + 20 // generous margin for spacing and ANSI
	if len(line) > maxExpectedLen {
		t.Errorf("Line length %d exceeds expected max %d: %s", len(line), maxExpectedLen, line)
	}

	// Verify truncation happened - original values should not appear
	if strings.Contains(line, "my-very-long-repository-name-here") {
		t.Error("Repo name should have been truncated")
	}
	if strings.Contains(line, "claude-code-agent") {
		t.Error("Agent name should have been truncated")
	}
}

func TestTUIRenderJobLineNoTruncation(t *testing.T) {
	m := tuiModel{width: 200}
	job := storage.ReviewJob{
		ID:         1,
		GitRef:     "abc1234",
		RepoName:   "myrepo",
		Agent:      "test",
		Status:     storage.JobStatusDone,
		EnqueuedAt: time.Now(),
	}

	// Use wide column widths - no truncation needed
	colWidths := columnWidths{
		ref:   20,
		repo:  20,
		agent: 15,
	}

	line := m.renderJobLine(job, false, 3, colWidths)

	// Short values should not be truncated
	if strings.Contains(line, "...") {
		t.Errorf("Short values should not be truncated: %s", line)
	}

	// Original values should appear
	if !strings.Contains(line, "abc1234") {
		t.Error("Git ref should appear untruncated")
	}
	if !strings.Contains(line, "myrepo") {
		t.Error("Repo name should appear untruncated")
	}
	if !strings.Contains(line, "test") {
		t.Error("Agent name should appear untruncated")
	}
}

func TestTUIPaginationAppendMode(t *testing.T) {
	m := newTuiModel("http://localhost")

	// Start with 50 jobs
	initialJobs := make([]storage.ReviewJob, 50)
	for i := 0; i < 50; i++ {
		initialJobs[i] = storage.ReviewJob{ID: int64(50 - i)}
	}
	m.jobs = initialJobs
	m.selectedIdx = 0
	m.selectedJobID = 50
	m.hasMore = true

	// Append 25 more jobs
	moreJobs := make([]storage.ReviewJob, 25)
	for i := 0; i < 25; i++ {
		moreJobs[i] = storage.ReviewJob{ID: int64(i + 1)} // IDs 1-25 (older)
	}
	appendMsg := tuiJobsMsg{jobs: moreJobs, hasMore: false, append: true}

	updated, _ := m.Update(appendMsg)
	m2 := updated.(tuiModel)

	// Should now have 75 jobs
	if len(m2.jobs) != 75 {
		t.Errorf("Expected 75 jobs after append, got %d", len(m2.jobs))
	}

	// hasMore should be updated
	if m2.hasMore {
		t.Error("hasMore should be false after append with hasMore=false")
	}

	// loadingMore should be cleared
	if m2.loadingMore {
		t.Error("loadingMore should be cleared after append")
	}

	// Selection should be maintained
	if m2.selectedJobID != 50 {
		t.Errorf("Expected selectedJobID=50 maintained, got %d", m2.selectedJobID)
	}
}

func TestTUIPaginationRefreshMaintainsView(t *testing.T) {
	m := newTuiModel("http://localhost")

	// Simulate user has paginated to 100 jobs
	jobs := make([]storage.ReviewJob, 100)
	for i := 0; i < 100; i++ {
		jobs[i] = storage.ReviewJob{ID: int64(100 - i)}
	}
	m.jobs = jobs
	m.selectedIdx = 50
	m.selectedJobID = 50

	// Refresh arrives (replace mode, not append)
	refreshedJobs := make([]storage.ReviewJob, 100)
	for i := 0; i < 100; i++ {
		refreshedJobs[i] = storage.ReviewJob{ID: int64(101 - i)} // New job at top
	}
	refreshMsg := tuiJobsMsg{jobs: refreshedJobs, hasMore: true, append: false}

	updated, _ := m.Update(refreshMsg)
	m2 := updated.(tuiModel)

	// Should still have 100 jobs
	if len(m2.jobs) != 100 {
		t.Errorf("Expected 100 jobs after refresh, got %d", len(m2.jobs))
	}

	// Selection should find job ID=50 at new index
	if m2.selectedJobID != 50 {
		t.Errorf("Expected selectedJobID=50 maintained, got %d", m2.selectedJobID)
	}
	if m2.selectedIdx != 51 {
		t.Errorf("Expected selectedIdx=51 (shifted by new job), got %d", m2.selectedIdx)
	}
}

func TestTUILoadingMoreClearedOnPaginationError(t *testing.T) {
	m := newTuiModel("http://localhost")
	m.loadingMore = true

	// Pagination error arrives (only pagination errors clear loadingMore)
	errMsg := tuiPaginationErrMsg{err: fmt.Errorf("network error")}
	updated, _ := m.Update(errMsg)
	m2 := updated.(tuiModel)

	// loadingMore should be cleared so user can retry
	if m2.loadingMore {
		t.Error("loadingMore should be cleared on pagination error")
	}

	// Error should be set
	if m2.err == nil {
		t.Error("err should be set")
	}
}

func TestTUILoadingMoreNotClearedOnGenericError(t *testing.T) {
	m := newTuiModel("http://localhost")
	m.loadingMore = true

	// Generic error arrives (should NOT clear loadingMore)
	errMsg := tuiErrMsg(fmt.Errorf("some other error"))
	updated, _ := m.Update(errMsg)
	m2 := updated.(tuiModel)

	// loadingMore should remain true - only pagination errors clear it
	if !m2.loadingMore {
		t.Error("loadingMore should NOT be cleared on generic error")
	}

	// Error should still be set
	if m2.err == nil {
		t.Error("err should be set")
	}
}

func TestTUINavigateDownTriggersLoadMore(t *testing.T) {
	m := newTuiModel("http://localhost")

	// Set up at last job with more available
	m.jobs = []storage.ReviewJob{{ID: 1}}
	m.selectedIdx = 0
	m.selectedJobID = 1
	m.hasMore = true
	m.loadingMore = false
	m.currentView = tuiViewQueue

	// Press down at bottom - should trigger load more
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m2 := updated.(tuiModel)

	if !m2.loadingMore {
		t.Error("loadingMore should be set when navigating past last job")
	}
	if cmd == nil {
		t.Error("Should return fetchMoreJobs command")
	}
}

func TestTUINavigateDownNoLoadMoreWhenFiltered(t *testing.T) {
	m := newTuiModel("http://localhost")

	// Set up at last job with filter active
	m.jobs = []storage.ReviewJob{{ID: 1, RepoPath: "/path/to/repo"}}
	m.selectedIdx = 0
	m.selectedJobID = 1
	m.hasMore = true
	m.loadingMore = false
	m.activeRepoFilter = "/path/to/repo" // Filter active
	m.currentView = tuiViewQueue

	// Press down at bottom - should NOT trigger load more (filtered view loads all)
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m2 := updated.(tuiModel)

	if m2.loadingMore {
		t.Error("loadingMore should not be set when filter is active")
	}
	if cmd != nil {
		t.Error("Should not return command when filter is active")
	}
}
