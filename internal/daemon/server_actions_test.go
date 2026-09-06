package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

type commandTestAgent struct {
	name    string
	command string
}

func (a *commandTestAgent) Name() string { return a.name }

func (a *commandTestAgent) Review(ctx context.Context, repoPath, commitSHA, prompt string, output io.Writer) (string, error) {
	return "No issues found.", nil
}

func (a *commandTestAgent) WithReasoning(level agent.ReasoningLevel) agent.Agent {
	return a
}

func (a *commandTestAgent) WithAgentic(agentic bool) agent.Agent { return a }

func (a *commandTestAgent) WithModel(model string) agent.Agent { return a }

func (a *commandTestAgent) CommandLine() string { return a.command }

func (a *commandTestAgent) CommandName() string { return a.command }

func TestHandleStatus(t *testing.T) {
	server, db, _ := newTestServer(t)

	t.Run("returns status with version", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			require.Condition(t, func() bool {
				return false
			}, "Expected status 200, got %d: %s", w.Code, w.Body.String())
		}

		var status storage.DaemonStatus
		testutil.DecodeJSON(t, w, &status)

		// Version should be set (non-empty)
		if status.Version == "" {
			assert.Condition(t, func() bool {
				return false
			}, "Expected Version to be set in status response")
		}
	})

	t.Run("wrong method fails", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/status", nil)
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusMethodNotAllowed {
			assert.Condition(t, func() bool {
				return false
			}, "Expected status 405 for POST, got %d", w.Code)
		}
	})

	t.Run("returns max_workers from pool not config", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		var status storage.DaemonStatus
		testutil.DecodeJSON(t, w, &status)

		// MaxWorkers should match the pool size (config default), not a potentially reloaded config value
		expectedWorkers := config.DefaultConfig().MaxWorkers
		if status.MaxWorkers != expectedWorkers {
			assert.Condition(t, func() bool {
				return false
			}, "Expected MaxWorkers %d from pool, got %d", expectedWorkers, status.MaxWorkers)
		}
	})

	t.Run("includes queue paused state", func(t *testing.T) {
		require.NoError(t, db.SetQueuePaused(true))
		req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		var status storage.DaemonStatus
		testutil.DecodeJSON(t, w, &status)
		assert.True(t, status.QueuePaused)
		require.NoError(t, db.SetQueuePaused(false))
	})

	t.Run("config_reloaded_at empty initially", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		var status storage.DaemonStatus
		testutil.DecodeJSON(t, w, &status)

		// ConfigReloadedAt should be empty when no reload has occurred
		if status.ConfigReloadedAt != "" {
			assert.Condition(t, func() bool {
				return false
			}, "Expected ConfigReloadedAt to be empty initially, got %q", status.ConfigReloadedAt)
		}
	})
}

func TestHandleQueuePause(t *testing.T) {
	server, db, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/queue/pause", nil)
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	paused, err := db.IsQueuePaused()
	require.NoError(t, err)
	assert.True(t, paused)

	req = httptest.NewRequest(http.MethodPost, "/api/queue/unpause", nil)
	w = httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	paused, err = db.IsQueuePaused()
	require.NoError(t, err)
	assert.False(t, paused)
}

func TestHandlePing(t *testing.T) {
	server, _, _ := newTestServer(t)

	t.Run("returns daemon identity", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/ping", nil)
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			require.Condition(t, func() bool {
				return false
			}, "Expected status 200, got %d: %s", w.Code, w.Body.String())
		}

		var ping PingInfo
		testutil.DecodeJSON(t, w, &ping)
		if ping.Service != daemonServiceName {
			require.Condition(t, func() bool {
				return false
			}, "Expected service %q, got %q", daemonServiceName, ping.Service)
		}
		if ping.Version == "" {
			require.Condition(t, func() bool {
				return false
			}, "Expected ping version to be set")
		}
		if ping.PID == 0 {
			require.Condition(t, func() bool {
				return false
			}, "Expected ping PID to be set")
		}
	})

	t.Run("wrong method fails", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/ping", nil)
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusMethodNotAllowed {
			require.Condition(t, func() bool {
				return false
			}, "Expected status 405, got %d", w.Code)
		}
	})
}

func TestHandleCancelJob(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(t *testing.T, server *Server, db *storage.DB, tmpDir string) int64 // returns job_id or 0
		request    func(t *testing.T, jobID int64) *http.Request                           // builds the request
		wantStatus int
		verify     func(t *testing.T, db *storage.DB, jobID int64) // optional post-cancel check
	}{
		{
			name: "cancel queued job",
			setup: func(t *testing.T, server *Server, db *storage.DB, tmpDir string) int64 {
				job := createTestJob(t, db, tmpDir, "cancelqueued", "test")
				return job.ID
			},
			request: func(t *testing.T, jobID int64) *http.Request {
				return testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/cancel", CancelJobRequest{JobID: jobID})
			},
			wantStatus: http.StatusOK,
			verify: func(t *testing.T, db *storage.DB, jobID int64) {
				updated, err := db.GetJobByID(jobID)
				require.NoError(t, err, "GetJobByID failed")
				assert.Equal(t, storage.JobStatusCanceled, updated.Status)
			},
		},
		{
			name: "cancel already canceled job",
			setup: func(t *testing.T, server *Server, db *storage.DB, tmpDir string) int64 {
				job := createTestJob(t, db, tmpDir, "alreadycanceled", "test")
				// Cancel through the same server's handler to exercise
				// the full code path including workerPool side-effects.
				req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/cancel", CancelJobRequest{JobID: job.ID})
				w := httptest.NewRecorder()
				server.httpServer.Handler.ServeHTTP(w, req)
				require.Equal(t, http.StatusOK, w.Code, "first cancel should succeed")
				return job.ID
			},
			request: func(t *testing.T, jobID int64) *http.Request {
				return testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/cancel", CancelJobRequest{JobID: jobID})
			},
			wantStatus: http.StatusNotFound,
		},
		{
			name: "cancel nonexistent job",
			setup: func(t *testing.T, _ *Server, db *storage.DB, tmpDir string) int64 {
				return 99999
			},
			request: func(t *testing.T, jobID int64) *http.Request {
				return testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/cancel", CancelJobRequest{JobID: jobID})
			},
			wantStatus: http.StatusNotFound,
		},
		{
			name: "cancel with missing job_id",
			setup: func(t *testing.T, _ *Server, db *storage.DB, tmpDir string) int64 {
				return 0
			},
			request: func(t *testing.T, jobID int64) *http.Request {
				return testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/cancel", map[string]any{})
			},
			wantStatus: http.StatusUnprocessableEntity,
		},
		{
			name: "cancel with wrong method",
			setup: func(t *testing.T, _ *Server, db *storage.DB, tmpDir string) int64 {
				return 0
			},
			request: func(t *testing.T, jobID int64) *http.Request {
				return httptest.NewRequest(http.MethodGet, "/api/job/cancel", nil)
			},
			wantStatus: http.StatusMethodNotAllowed,
		},
		{
			name: "cancel running job",
			setup: func(t *testing.T, _ *Server, db *storage.DB, tmpDir string) int64 {
				job := createTestJob(t, db, tmpDir, "cancelrunning", "test")
				_, err := db.ClaimJob("worker-1")
				require.NoError(t, err, "ClaimJob failed")
				return job.ID
			},
			request: func(t *testing.T, jobID int64) *http.Request {
				return testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/cancel", CancelJobRequest{JobID: jobID})
			},
			wantStatus: http.StatusOK,
			verify: func(t *testing.T, db *storage.DB, jobID int64) {
				updated, err := db.GetJobByID(jobID)
				require.NoError(t, err, "GetJobByID failed")
				assert.Equal(t, storage.JobStatusCanceled, updated.Status)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, db, tmpDir := newTestServer(t)
			jobID := tt.setup(t, server, db, tmpDir)

			req := tt.request(t, jobID)
			w := httptest.NewRecorder()

			server.httpServer.Handler.ServeHTTP(w, req)

			assert.Equal(t, tt.wantStatus, w.Code, "response body: %s", w.Body.String())

			if tt.verify != nil {
				tt.verify(t, db, jobID)
			}
		})
	}
}

func TestRunningJobCancellationBroadcastsOnce(t *testing.T) {
	server, db, tempDir := newTestServer(t)
	testutil.InitTestGitRepo(t, tempDir)
	markerFile := filepath.Join(tempDir, "local-running-cancel-hook")
	server.configWatcher.Config().Hooks = []config.HookConfig{{
		Event: "review.canceled", Command: touchCmd(markerFile),
	}}

	started := make(chan struct{})
	finished := make(chan struct{})
	const agentName = "local-cancel-blocking"
	agent.Register(&agent.FakeAgent{
		NameStr: agentName,
		ReviewFn: func(ctx context.Context, _, _, _ string, _ io.Writer) (string, error) {
			close(started)
			<-ctx.Done()
			return "", ctx.Err()
		},
	})
	t.Cleanup(func() { agent.Unregister(agentName) })

	job := createTestJob(
		t, db, tempDir, testutil.GetHeadSHA(t, tempDir), agentName,
	)
	claimed, err := db.ClaimJob("local-cancel-worker")
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)
	go func() {
		defer close(finished)
		server.workerPool.processJob("local-cancel-worker", claimed)
	}()
	t.Cleanup(func() {
		server.workerPool.CancelJob(job.ID)
		<-finished
	})
	require.Eventually(t, func() bool {
		select {
		case <-started:
			return true
		default:
			return false
		}
	}, 5*time.Second, 10*time.Millisecond)

	_, eventCh := server.broadcaster.Subscribe("")
	req := testutil.MakeJSONRequest(
		t, http.MethodPost, "/api/job/cancel", CancelJobRequest{JobID: job.ID},
	)
	recorder := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(recorder, req)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Eventually(t, func() bool {
		select {
		case <-finished:
			return true
		default:
			return false
		}
	}, 5*time.Second, 10*time.Millisecond)
	server.hookRunner.WaitUntilIdle()
	assert.FileExists(t, markerFile)
	require.Len(t, eventCh, 1)
	event := <-eventCh
	assert.Equal(t, "review.canceled", event.Type)
	assert.Equal(t, job.ID, event.JobID)
}

func TestHandleRerunJob(t *testing.T) {
	server, db, tmpDir := newTestServer(t)

	// Create a repo
	repo, err := db.GetOrCreateRepo(tmpDir)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetOrCreateRepo failed: %v", err)
	}

	t.Run("rerun failed job", func(t *testing.T) {
		commit, _ := db.GetOrCreateCommit(repo.ID, "rerun-failed", "Author", "Subject", time.Now())
		job, _ := db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, CommitID: commit.ID, GitRef: "rerun-failed", Agent: "test"})
		db.ClaimJob("worker-1")
		db.FailJob(job.ID, "", "some error")

		req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/rerun", RerunJobRequest{JobID: job.ID})
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			assert.Condition(t, func() bool {
				return false
			}, "Expected status 200, got %d: %s", w.Code, w.Body.String())
		}

		updated, err := db.GetJobByID(job.ID)
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "GetJobByID failed: %v", err)
		}
		if updated.Status != storage.JobStatusQueued {
			assert.Condition(t, func() bool {
				return false
			}, "Expected status 'queued', got '%s'", updated.Status)
		}
	})

	t.Run("rerun request is idempotent", func(t *testing.T) {
		commit, err := db.GetOrCreateCommit(repo.ID, "rerun-idempotent", "Author", "Subject", time.Now())
		require.NoError(t, err)
		job, err := db.EnqueueJob(storage.EnqueueOpts{
			RepoID: repo.ID, CommitID: commit.ID, GitRef: "rerun-idempotent", Agent: "test",
		})
		require.NoError(t, err)
		require.NoError(t, db.CancelJob(job.ID))

		body := RerunJobRequest{JobID: job.ID, RequestID: testUUIDPtr("request-one")}
		var responses []RerunJobOutput
		for range 2 {
			req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/rerun", body)
			w := httptest.NewRecorder()
			server.httpServer.Handler.ServeHTTP(w, req)
			testutil.AssertStatusCode(t, w, http.StatusOK)
			var response RerunJobOutput
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response.Body))
			responses = append(responses, response)
		}

		assert.Equal(t, responses[0].Body, responses[1].Body)
		assert.Equal(t, job.ID, responses[0].Body.JobID)
		assert.Equal(t, *body.RequestID, responses[0].Body.RequestID)
	})

	t.Run("rerun canceled job", func(t *testing.T) {
		commit, _ := db.GetOrCreateCommit(repo.ID, "rerun-canceled", "Author", "Subject", time.Now())
		job, _ := db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, CommitID: commit.ID, GitRef: "rerun-canceled", Agent: "test"})
		db.CancelJob(job.ID)

		req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/rerun", RerunJobRequest{JobID: job.ID})
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			assert.Condition(t, func() bool {
				return false
			}, "Expected status 200, got %d: %s", w.Code, w.Body.String())
		}

		updated, err := db.GetJobByID(job.ID)
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "GetJobByID failed: %v", err)
		}
		if updated.Status != storage.JobStatusQueued {
			assert.Condition(t, func() bool {
				return false
			}, "Expected status 'queued', got '%s'", updated.Status)
		}
	})

	t.Run("rerun canceled job waits for worker teardown", func(t *testing.T) {
		isolatedDB, isolatedDir := testutil.OpenTestDBWithDir(t)
		isolatedServer := NewServer(isolatedDB, config.DefaultConfig(), "")
		t.Cleanup(func() { require.NoError(t, isolatedServer.Close()) })
		repo, err := isolatedDB.GetOrCreateRepo(isolatedDir)
		require.NoError(t, err)
		commit, err := isolatedDB.GetOrCreateCommit(
			repo.ID, "rerun-canceled-running", "Author", "Subject", time.Now(),
		)
		require.NoError(t, err)
		job, err := isolatedDB.EnqueueJob(storage.EnqueueOpts{
			RepoID: repo.ID, CommitID: commit.ID, GitRef: "rerun-canceled-running", Agent: "test",
		})
		require.NoError(t, err)
		claimed, err := isolatedDB.ClaimJob("worker-canceled-running")
		require.NoError(t, err)
		require.Equal(t, job.ID, claimed.ID)
		require.NoError(t, isolatedDB.CancelJob(job.ID))

		req := testutil.MakeJSONRequest(
			t, http.MethodPost, "/api/job/rerun", RerunJobRequest{JobID: job.ID},
		)
		w := httptest.NewRecorder()
		isolatedServer.httpServer.Handler.ServeHTTP(w, req)

		assert.Equal(t, http.StatusConflict, w.Code)
		assert.Contains(t, w.Body.String(), "still stopping")
	})

	t.Run("rerun done job", func(t *testing.T) {
		commit, _ := db.GetOrCreateCommit(repo.ID, "rerun-done", "Author", "Subject", time.Now())
		job, _ := db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, CommitID: commit.ID, GitRef: "rerun-done", Agent: "test"})
		// Claim and complete job
		var claimed *storage.ReviewJob
		for {
			claimed, _ = db.ClaimJob("worker-1")
			require.NotNil(t, claimed, "No job to claim")
			if claimed.ID == job.ID {
				break
			}
			db.CompleteJob(claimed.ID, "test", "prompt", "output")
		}
		db.CompleteJob(job.ID, "test", "prompt", "output")

		req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/rerun", RerunJobRequest{JobID: job.ID})
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			assert.Condition(t, func() bool {
				return false
			}, "Expected status 200, got %d: %s", w.Code, w.Body.String())
		}

		updated, err := db.GetJobByID(job.ID)
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "GetJobByID failed: %v", err)
		}
		if updated.Status != storage.JobStatusQueued {
			assert.Condition(t, func() bool {
				return false
			}, "Expected status 'queued', got '%s'", updated.Status)
		}
	})

	t.Run("rerun reevaluates implicit effective model", func(t *testing.T) {
		isolatedDB, isolatedDir := testutil.OpenTestDBWithDir(t)
		server := NewServer(isolatedDB, config.DefaultConfig(), "")
		agentName := "rerun-implicit-model"
		agent.Register(&commandTestAgent{name: agentName, command: "go"})
		t.Cleanup(func() {
			agent.Unregister(agentName)
		})

		repo, err := isolatedDB.GetOrCreateRepo(isolatedDir)
		require.NoError(t, err)
		commit, err := isolatedDB.GetOrCreateCommit(repo.ID, "rerun-implicit-model", "Author", "Subject", time.Now())
		require.NoError(t, err)
		job, err := isolatedDB.EnqueueJob(storage.EnqueueOpts{
			RepoID:   repo.ID,
			CommitID: commit.ID,
			GitRef:   "rerun-implicit-model",
			Agent:    agentName,
			Model:    "minimax-m2.5-free",
		})
		require.NoError(t, err)

		claimed, err := isolatedDB.ClaimJob("worker-1")
		require.NoError(t, err)
		require.NotNil(t, claimed)
		require.Equal(t, job.ID, claimed.ID)
		require.NoError(t, isolatedDB.CompleteJob(job.ID, agentName, "prompt", "output"))

		req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/rerun", RerunJobRequest{JobID: job.ID})
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)
		testutil.AssertStatusCode(t, w, http.StatusOK)

		updated, err := isolatedDB.GetJobByID(job.ID)
		require.NoError(t, err)
		assert.Equal(t, storage.JobStatusQueued, updated.Status)
		assert.Empty(t, updated.Model, "rerun should recompute implicit model instead of preserving stale effective value")
	})

	t.Run("rerun with exact agent replaces only effective execution identity", func(t *testing.T) {
		const selectedAgent = "rerun-selected-agent"
		agent.Register(&agent.FakeAgent{NameStr: selectedAgent})
		t.Cleanup(func() { agent.Unregister(selectedAgent) })

		commit, err := db.GetOrCreateCommit(
			repo.ID, "rerun-selected-agent", "Author", "Subject", time.Now(),
		)
		require.NoError(t, err)
		job, err := db.EnqueueJob(storage.EnqueueOpts{
			RepoID: repo.ID, CommitID: commit.ID,
			GitRef: "rerun-selected-agent", Agent: "test",
			Model: "old-effective", Provider: "old-provider",
			RequestedModel: "requested-model", RequestedProvider: "requested-provider",
			BackupAgent: "backup-agent", BackupModel: "backup-model",
		})
		require.NoError(t, err)
		require.NoError(t, db.CancelJob(job.ID))
		subscriberID, events := server.broadcaster.Subscribe("")
		defer server.broadcaster.Unsubscribe(subscriberID)

		req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/rerun", RerunJobRequest{
			JobID: job.ID, Agent: selectedAgent,
		})
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		testutil.AssertStatusCode(t, w, http.StatusOK)

		updated, err := db.GetJobByID(job.ID)
		require.NoError(t, err)
		assert.Equal(t, storage.JobStatusQueued, updated.Status)
		assert.Equal(t, selectedAgent, updated.Agent)
		assert.Empty(t, updated.Model, "the selected agent should use its default model")
		assert.Empty(t, updated.Provider, "the selected agent should use its default provider")
		assert.Equal(t, "requested-model", updated.RequestedModel)
		assert.Equal(t, "requested-provider", updated.RequestedProvider)
		assert.Equal(t, "backup-agent", updated.BackupAgent)
		assert.Equal(t, "backup-model", updated.BackupModel)
		require.Len(t, events, 1)
		assert.Equal(t, selectedAgent, (<-events).Agent)
	})

	t.Run("rerun rejects invalid agent changes without mutating the job", func(t *testing.T) {
		agent.Register(&agent.FakeAgent{NameStr: "rerun-unstructured"})
		agent.Register(&commandTestAgent{name: "rerun-unavailable", command: "roborev-command-that-does-not-exist"})
		t.Cleanup(func() {
			agent.Unregister("rerun-unstructured")
			agent.Unregister("rerun-unavailable")
		})
		for _, tt := range []struct {
			name, selected, reviewType, jobType, wantError string
			experiment                                     *storage.ExperimentAssignmentInput
		}{
			{name: "unknown", selected: "missing-rerun-agent", wantError: "unknown agent"},
			{name: "unavailable", selected: "rerun-unavailable", wantError: "unavailable"},
			{name: "structured", selected: "rerun-unstructured", reviewType: "custom", wantError: "schema-constrained reviews"},
			{name: "classifier", selected: "rerun-unstructured", reviewType: "design", jobType: storage.JobTypeClassify, wantError: "SchemaAgent"},
			{name: "experiment", selected: "test", wantError: "frozen experiment", experiment: &storage.ExperimentAssignmentInput{
				ExperimentID: "rerun-agent", DefinitionHash: "definition", DefinitionJSON: `{}`,
				Arm: "experiment", SubjectHash: "subject", EffectiveConfigHash: "effective", EffectiveConfigJSON: `{}`,
			}},
		} {
			t.Run(tt.name, func(t *testing.T) {
				job, err := db.EnqueueJob(storage.EnqueueOpts{
					RepoID: repo.ID, GitRef: "rerun-" + tt.name, Agent: "test",
					ReviewType: tt.reviewType, JobType: tt.jobType, Experiment: tt.experiment,
				})
				require.NoError(t, err)
				require.NoError(t, db.CancelJob(job.ID))
				req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/rerun", RerunJobRequest{
					JobID: job.ID, Agent: tt.selected,
				})
				w := httptest.NewRecorder()
				server.httpServer.Handler.ServeHTTP(w, req)
				testutil.AssertStatusCode(t, w, http.StatusBadRequest)
				assert.Contains(t, w.Body.String(), tt.wantError)
				updated, err := db.GetJobByID(job.ID)
				require.NoError(t, err)
				assert.Equal(t, storage.JobStatusCanceled, updated.Status)
				assert.Equal(t, "test", updated.Agent)
			})
		}
	})

	t.Run("rerun stores configured ACP identity", func(t *testing.T) {
		isolatedDB, isolatedDir := testutil.OpenTestDBWithDir(t)
		cfg := config.DefaultConfig()
		cfg.ACP = config.ACPAgentConfigs{
			"rerun-acp": {Command: "go", Model: "acp-model"},
		}
		isolatedServer := NewServer(isolatedDB, cfg, "")
		t.Cleanup(func() { require.NoError(t, isolatedServer.Close()) })
		repo, err := isolatedDB.GetOrCreateRepo(isolatedDir)
		require.NoError(t, err)
		commit, err := isolatedDB.GetOrCreateCommit(
			repo.ID, "rerun-acp-agent", "Author", "Subject", time.Now(),
		)
		require.NoError(t, err)
		job, err := isolatedDB.EnqueueJob(storage.EnqueueOpts{
			RepoID: repo.ID, CommitID: commit.ID,
			GitRef: "rerun-acp-agent", Agent: "test",
			RequestedModel: "original-model", RequestedProvider: "original-provider",
		})
		require.NoError(t, err)
		require.NoError(t, isolatedDB.CancelJob(job.ID))

		req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/rerun", RerunJobRequest{
			JobID: job.ID, Agent: "acp.rerun-acp",
		})
		w := httptest.NewRecorder()
		isolatedServer.httpServer.Handler.ServeHTTP(w, req)
		testutil.AssertStatusCode(t, w, http.StatusOK)

		updated, err := isolatedDB.GetJobByID(job.ID)
		require.NoError(t, err)
		assert.Equal(t, "acp.rerun-acp", updated.Agent)
		assert.Equal(t, "acp-model", updated.Model)
	})

	t.Run("rerun queued job fails", func(t *testing.T) {
		commit, _ := db.GetOrCreateCommit(repo.ID, "rerun-queued", "Author", "Subject", time.Now())
		job, _ := db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, CommitID: commit.ID, GitRef: "rerun-queued", Agent: "test"})

		req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/rerun", RerunJobRequest{JobID: job.ID})
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			assert.Condition(t, func() bool {
				return false
			}, "Expected status 404 for queued job, got %d", w.Code)
		}
	})

	t.Run("rerun with invalid worktree path fails", func(t *testing.T) {
		repoDir := filepath.Join(tmpDir, "rerun-invalid-worktree")
		testutil.InitTestGitRepo(t, repoDir)

		repo, err := db.GetOrCreateRepo(repoDir)
		require.NoError(t, err)
		commit, err := db.GetOrCreateCommit(repo.ID, "rerun-stale-worktree", "Author", "Subject", time.Now())
		require.NoError(t, err)
		job, err := db.EnqueueJob(storage.EnqueueOpts{
			RepoID:       repo.ID,
			CommitID:     commit.ID,
			GitRef:       "rerun-stale-worktree",
			Agent:        "test",
			WorktreePath: filepath.Join(tmpDir, "stale-worktree"),
		})
		require.NoError(t, err)

		for {
			claimed, err := db.ClaimJob("worker-stale-rerun")
			require.NoError(t, err)
			require.NotNil(t, claimed)
			if claimed.ID == job.ID {
				break
			}
			require.NoError(t, db.CompleteJob(claimed.ID, "test", "prompt", "output"))
		}
		require.NoError(t, db.CompleteJob(job.ID, "test", "prompt", "output"))

		req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/rerun", RerunJobRequest{JobID: job.ID})
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)
		testutil.AssertStatusCode(t, w, http.StatusBadRequest)

		assert.Contains(t, w.Body.String(), "rerun job worktree path is stale or invalid")

		updated, err := db.GetJobByID(job.ID)
		require.NoError(t, err)
		assert.Equal(t, storage.JobStatusDone, updated.Status)
	})

	t.Run("rerun nonexistent job fails", func(t *testing.T) {
		req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/rerun", RerunJobRequest{JobID: 99999})
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			assert.Condition(t, func() bool {
				return false
			}, "Expected status 404 for nonexistent job, got %d", w.Code)
		}
	})

	t.Run("rerun with missing job_id fails", func(t *testing.T) {
		req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/rerun", map[string]any{})
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusUnprocessableEntity {
			assert.Condition(t, func() bool {
				return false
			}, "Expected status 422 for missing job_id, got %d", w.Code)
		}
	})

	t.Run("rerun with invalid method fails", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/job/rerun", nil)
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusMethodNotAllowed {
			assert.Condition(t, func() bool {
				return false
			}, "Expected status 405 for GET, got %d", w.Code)
		}
	})
}

func TestRerunJobBroadcastsOnlyAcceptedRequest(t *testing.T) {
	server, db, tempDir := newTestServer(t)
	repo, err := db.GetOrCreateRepo(tempDir)
	require.NoError(t, err)
	commit, err := db.GetOrCreateCommit(
		repo.ID, "rerun-broadcast", "Author", "Subject", time.Now(),
	)
	require.NoError(t, err)
	job, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID: repo.ID, CommitID: commit.ID,
		GitRef: "rerun-broadcast", Agent: "test",
	})
	require.NoError(t, err)
	require.NoError(t, db.CancelJob(job.ID))

	subscriberID, events := server.broadcaster.Subscribe("")
	defer server.broadcaster.Unsubscribe(subscriberID)
	body := RerunJobRequest{JobID: job.ID, RequestID: testUUIDPtr("rerun-broadcast-request")}

	first := testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/rerun", body)
	firstResponse := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(firstResponse, first)
	require.Equal(t, http.StatusOK, firstResponse.Code, firstResponse.Body.String())
	require.Len(t, events, 1)
	event := <-events
	assert.Equal(t, "job.enqueued", event.Type)
	assert.Equal(t, job.ID, event.JobID)
	assert.Equal(t, repo.RootPath, event.Repo)
	assert.Equal(t, repo.Name, event.RepoName)
	assert.Equal(t, job.GitRef, event.SHA)
	assert.Equal(t, job.Agent, event.Agent)

	replay := testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/rerun", body)
	replayResponse := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(replayResponse, replay)
	require.Equal(t, http.StatusOK, replayResponse.Code, replayResponse.Body.String())
	assert.Empty(t, events, "idempotent replay must not broadcast again")
}

func TestResolveRerunClassifierModelUsesClassifierConfig(t *testing.T) {
	repoPath := t.TempDir()
	testutil.InitTestGitRepo(t, repoPath)
	require.NoError(t, os.WriteFile(filepath.Join(repoPath, ".roborev.toml"), []byte(
		"classify_model = \"classify-model\"\ndesign_model = \"design-model\"\n",
	), 0o644))

	const selectedAgent = "rerun-classifier-model"
	agent.Register(&fakeSchemaAgent{name: selectedAgent})
	t.Cleanup(func() { agent.Unregister(selectedAgent) })

	job := &storage.ReviewJob{
		Agent: selectedAgent, JobType: storage.JobTypeClassify,
		ReviewType: "design", Reasoning: "fast", RepoPath: repoPath,
	}
	opts, err := resolveRerunOpts(job, config.DefaultConfig(), nil, selectedAgent)
	require.NoError(t, err)
	assert.Equal(t, "classify-model", opts.Model)

	job.RequestedModel = "requested-model"
	opts, err = resolveRerunOpts(job, config.DefaultConfig(), nil, selectedAgent)
	require.NoError(t, err)
	assert.Equal(t, "classify-model", opts.Model)
}

func TestWorkflowForJobFixType(t *testing.T) {
	assert := assert.New(t)
	assert.Equal("fix", workflowForJob(storage.JobTypeFix, config.ReviewTypeDefault))
	assert.Equal("fix", workflowForJob(storage.JobTypeCompact, config.ReviewTypeDefault))
	assert.Equal("review", workflowForJob(storage.JobTypeReview, config.ReviewTypeDefault))
	assert.Equal("security", workflowForJob(storage.JobTypeReview, "security"))
	assert.Equal("lookahead", workflowForJob(storage.JobTypeReview, "lookahead"))
}

func TestResolveRerunModelProviderUsesWorktreeConfig(t *testing.T) {
	mainRepo := t.TempDir()
	testutil.InitTestGitRepo(t, mainRepo)
	worktreeRepo := filepath.Join(t.TempDir(), "worktree")
	worktreeAdd := exec.Command(
		"git", "-C", mainRepo, "worktree", "add", "--detach", worktreeRepo, "HEAD",
	)
	out, err := worktreeAdd.CombinedOutput()
	require.NoError(t, err, "git worktree add failed: %s", out)

	require.NoError(t, os.WriteFile(filepath.Join(mainRepo, ".roborev.toml"), []byte("review_model = \"main-model\"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(worktreeRepo, ".roborev.toml"), []byte("review_model = \"worktree-model\"\n"), 0o644))

	job := &storage.ReviewJob{
		Agent:        "test",
		JobType:      storage.JobTypeReview,
		ReviewType:   config.ReviewTypeDefault,
		Reasoning:    "thorough",
		RepoPath:     mainRepo,
		WorktreePath: worktreeRepo,
	}

	model, provider, err := resolveRerunModelProvider(
		job, config.DefaultConfig(),
	)
	require.NoError(t, err)
	assert.Equal(t, "worktree-model", model)
	assert.Empty(t, provider)
}

func TestResolveRerunModelProviderRejectsInvalidWorktreeConfig(t *testing.T) {
	mainRepo := t.TempDir()
	stalePath := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(mainRepo, ".roborev.toml"), []byte("review_model = \"main-model\"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(stalePath, ".roborev.toml"), []byte("review_model = \"stale-model\"\n"), 0o644))

	job := &storage.ReviewJob{
		Agent:        "test",
		JobType:      storage.JobTypeReview,
		ReviewType:   config.ReviewTypeDefault,
		Reasoning:    "thorough",
		RepoPath:     mainRepo,
		WorktreePath: stalePath,
	}

	model, provider, err := resolveRerunModelProvider(
		job, config.DefaultConfig(),
	)
	require.Error(t, err)
	require.ErrorContains(t, err, "rerun job worktree path is stale or invalid")
	assert.Empty(t, model)
	assert.Empty(t, provider)
}

func TestResolveRerunModelProviderRejectsInvalidWorktreeWithRequestedOverrides(t *testing.T) {
	mainRepo := t.TempDir()
	stalePath := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(mainRepo, ".roborev.toml"), []byte("review_model = \"main-model\"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(stalePath, ".roborev.toml"), []byte("review_model = \"stale-model\"\n"), 0o644))

	job := &storage.ReviewJob{
		Agent:             "test",
		JobType:           storage.JobTypeReview,
		ReviewType:        config.ReviewTypeDefault,
		Reasoning:         "thorough",
		RepoPath:          mainRepo,
		WorktreePath:      stalePath,
		RequestedModel:    "requested-model",
		RequestedProvider: "anthropic",
	}

	model, provider, err := resolveRerunModelProvider(
		job, config.DefaultConfig(),
	)
	require.Error(t, err)
	require.ErrorContains(t, err, "rerun job worktree path is stale or invalid")
	assert.Empty(t, model)
	assert.Empty(t, provider)
}

func TestResolveRerunModelProviderRejectsParseableInvalidConfigWithRequestedOverrides(t *testing.T) {
	mainRepo := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(mainRepo, ".roborev.toml"), []byte("review_reasoning = \"bogus\"\n"), 0o644))

	job := &storage.ReviewJob{
		Agent:             "test",
		JobType:           storage.JobTypeReview,
		ReviewType:        config.ReviewTypeDefault,
		Reasoning:         "thorough",
		RepoPath:          mainRepo,
		RequestedModel:    "requested-model",
		RequestedProvider: "anthropic",
	}

	model, provider, err := resolveRerunModelProvider(
		job, config.DefaultConfig(),
	)
	require.Error(t, err)
	require.ErrorContains(t, err, "review_reasoning")
	assert.Empty(t, model)
	assert.Empty(t, provider)
}

func TestResolveRerunModelProviderRejectsMalformedConfigWithRequestedOverrides(t *testing.T) {
	mainRepo := t.TempDir()

	require.NoError(t, os.WriteFile(
		filepath.Join(mainRepo, ".roborev.toml"),
		[]byte("this is not valid toml [[["),
		0o644,
	))

	job := &storage.ReviewJob{
		Agent:             "test",
		JobType:           storage.JobTypeReview,
		ReviewType:        config.ReviewTypeDefault,
		Reasoning:         "thorough",
		RepoPath:          mainRepo,
		RequestedModel:    "requested-model",
		RequestedProvider: "anthropic",
	}

	model, provider, err := resolveRerunModelProvider(
		job, config.DefaultConfig(),
	)
	require.Error(t, err)
	require.ErrorContains(t, err, "resolve workflow config")
	assert.Empty(t, model)
	assert.Empty(t, provider)
}

func TestResolveRerunModelProviderRejectsInvalidAgentWithRequestedOverrides(t *testing.T) {
	mainRepo := t.TempDir()

	job := &storage.ReviewJob{
		Agent:             "missing-agent",
		JobType:           storage.JobTypeReview,
		ReviewType:        config.ReviewTypeDefault,
		Reasoning:         "thorough",
		RepoPath:          mainRepo,
		RequestedModel:    "requested-model",
		RequestedProvider: "anthropic",
	}

	model, provider, err := resolveRerunModelProvider(
		job, config.DefaultConfig(),
	)
	require.Error(t, err)
	require.ErrorContains(t, err, `invalid agent: unknown agent "missing-agent"`)
	assert.Empty(t, model)
	assert.Empty(t, provider)
}

func TestResolveRerunModelProviderAllowsUnavailablePrimaryWithBackup(t *testing.T) {
	t.Setenv("PATH", "")
	mainRepo := t.TempDir()

	job := &storage.ReviewJob{
		Agent:      "claude-code",
		JobType:    storage.JobTypeReview,
		ReviewType: config.ReviewTypeDefault,
		Reasoning:  "thorough",
		RepoPath:   mainRepo,
	}
	cfg := config.DefaultConfig()
	cfg.ReviewBackupAgent = "test"

	model, provider, err := resolveRerunModelProvider(job, cfg)

	require.NoError(t, err)
	assert.Empty(t, model)
	assert.Empty(t, provider)
}

func TestResolveRerunModelProviderAllowsUnavailablePrimaryWithStoredBackup(t *testing.T) {
	t.Setenv("PATH", "")
	mainRepo := t.TempDir()

	job := &storage.ReviewJob{
		Agent:       "claude-code",
		BackupAgent: "test",
		JobType:     storage.JobTypeReview,
		ReviewType:  config.ReviewTypeDefault,
		Reasoning:   "thorough",
		RepoPath:    mainRepo,
	}

	model, provider, err := resolveRerunModelProvider(job, config.DefaultConfig())

	require.NoError(t, err)
	assert.Empty(t, model)
	assert.Empty(t, provider)
}

// TestHandleAddCommentToJobStates tests that comments can be added to jobs
// in any state: queued, running, done, failed, and canceled.
func TestHandleAddCommentToJobStates(t *testing.T) {
	server, db, tmpDir := newTestServer(t)

	// Create repo and commit
	repo, err := db.GetOrCreateRepo(filepath.Join(tmpDir, "test-repo"))
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetOrCreateRepo failed: %v", err)
	}
	commit, err := db.GetOrCreateCommit(repo.ID, "abc123", "Author", "Test commit", time.Now())
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetOrCreateCommit failed: %v", err)
	}

	testCases := []struct {
		name   string
		status storage.JobStatus // empty string means keep as queued
	}{
		{"queued job", ""},
		{"running job", storage.JobStatusRunning},
		{"completed job", storage.JobStatusDone},
		{"failed job", storage.JobStatusFailed},
		{"canceled job", storage.JobStatusCanceled},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Create a job
			job, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, CommitID: commit.ID, GitRef: "abc123", Agent: "test-agent"})
			if err != nil {
				require.Condition(t, func() bool {
					return false
				}, "EnqueueJob failed: %v", err)
			}

			// Set job to desired state
			if tc.status != "" {
				setJobStatus(t, db, job.ID, tc.status)
			}

			// Add comment via API
			reqData := AddCommentRequest{
				JobID:     job.ID,
				Commenter: "test-user",
				Comment:   "Test comment for " + tc.name,
			}
			req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/comment", reqData)
			w := httptest.NewRecorder()

			server.httpServer.Handler.ServeHTTP(w, req)

			if w.Code != http.StatusCreated {
				assert.Condition(t, func() bool {
					return false
				}, "Expected status 201, got %d: %s", w.Code, w.Body.String())
			}

			// Verify response contains the comment
			var resp storage.Response
			testutil.DecodeJSON(t, w, &resp)
			if resp.Responder != "test-user" {
				assert.Condition(t, func() bool {
					return false
				}, "Expected responder 'test-user', got %q", resp.Responder)
			}
		})
	}
}

// TestHandleAddCommentToNonExistentJob tests that adding a comment to a
// non-existent job returns 404.
func TestHandleAddCommentToNonExistentJob(t *testing.T) {
	server, _, _ := newTestServer(t)

	reqData := AddCommentRequest{
		JobID:     99999,
		Commenter: "test-user",
		Comment:   "This should fail",
	}
	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/comment", reqData)
	w := httptest.NewRecorder()

	server.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		assert.Condition(t, func() bool {
			return false
		}, "Expected status 404, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "job not found") {
		assert.Condition(t, func() bool {
			return false
		}, "Expected 'job not found' error, got: %s", w.Body.String())
	}
}

// TestHandleAddCommentWithoutReview tests that comments can be added to jobs
// that don't have a review yet (job exists but hasn't completed).
func TestHandleAddCommentWithoutReview(t *testing.T) {
	server, db, tmpDir := newTestServer(t)

	// Create repo, commit, and job (but NO review)
	job := createTestJob(t, db, filepath.Join(tmpDir, "test-repo"), "abc123", "test-agent")

	// Set job to running (no review exists yet)
	setJobStatus(t, db, job.ID, storage.JobStatusRunning)

	// Verify no review exists
	if _, err := db.GetReviewByJobID(job.ID); err == nil {
		require.Condition(t, func() bool {
			return false
		}, "Expected no review to exist for job")
	}

	// Add comment - should succeed even without a review
	reqData := AddCommentRequest{
		JobID:     job.ID,
		Commenter: "test-user",
		Comment:   "Comment on in-progress job without review",
	}
	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/comment", reqData)
	w := httptest.NewRecorder()

	server.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		assert.Condition(t, func() bool {
			return false
		}, "Expected status 201, got %d: %s", w.Code, w.Body.String())
	}

	// Verify comment was stored
	comments, err := db.GetCommentsForJob(job.ID)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetCommentsForJob failed: %v", err)
	}
	if len(comments) != 1 {
		require.Condition(t, func() bool {
			return false
		}, "Expected 1 comment, got %d", len(comments))
	}
	if comments[0].Response != "Comment on in-progress job without review" {
		assert.Condition(t, func() bool {
			return false
		}, "Unexpected comment: %q", comments[0].Response)
	}
}

func TestHandleAddCommentBroadcastsEvent(t *testing.T) {
	t.Run("job comment", func(t *testing.T) {
		server, db, tmpDir := newTestServer(t)
		job := createTestJob(t, db, filepath.Join(tmpDir, "test-repo"), "abc123", "test")
		_, eventCh := server.broadcaster.Subscribe("")

		req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/comment", AddCommentRequest{
			JobID: job.ID, Commenter: "reviewer", Comment: "Looks good",
		})
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		require.Equal(t, http.StatusCreated, w.Code)

		select {
		case event := <-eventCh:
			assert.Equal(t, "review.commented", event.Type)
			assert.Equal(t, job.ID, event.JobID)
			assert.Equal(t, "abc123", event.SHA)
			assert.NotEmpty(t, event.Repo)
			assert.NotEmpty(t, event.RepoName)
		case <-time.After(time.Second):
			require.FailNow(t, "timed out waiting for review.commented event")
		}
	})

	t.Run("commit comment", func(t *testing.T) {
		server, db, tmpDir := newTestServer(t)
		repo, err := db.GetOrCreateRepo(filepath.Join(tmpDir, "test-repo"))
		require.NoError(t, err)
		_, err = db.GetOrCreateCommit(repo.ID, "def456", "Author", "Subject", time.Now())
		require.NoError(t, err)
		_, eventCh := server.broadcaster.Subscribe("")

		req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/comment", AddCommentRequest{
			SHA: "def456", Commenter: "reviewer", Comment: "Commit context",
		})
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		require.Equal(t, http.StatusCreated, w.Code)

		select {
		case event := <-eventCh:
			assert.Equal(t, "review.commented", event.Type)
			assert.Zero(t, event.JobID)
			assert.Equal(t, "def456", event.SHA)
			assert.Equal(t, repo.RootPath, event.Repo)
			assert.Equal(t, repo.Name, event.RepoName)
		case <-time.After(time.Second):
			require.FailNow(t, "timed out waiting for review.commented event")
		}
	})
}

func TestHandleCloseReview_BroadcastsEvent(t *testing.T) {
	assert := assert.New(t)
	server, db, tmpDir := newTestServer(t)

	// Create a completed job (which creates a review)
	job := createTestJob(t, db, tmpDir, "abc123", "test")
	claimed, err := db.ClaimJob("worker-1")
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)
	require.NoError(t, db.CompleteJob(job.ID, "test", "prompt", "output"))

	// Subscribe to broadcaster before the close call
	_, eventCh := server.broadcaster.Subscribe("")

	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/review/close", CloseReviewRequest{
		JobID:  job.ID,
		Closed: true,
	})
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)

	assert.Equal(http.StatusOK, w.Code)

	// Verify event was broadcast with full metadata
	select {
	case event := <-eventCh:
		assert.Equal("review.closed", event.Type)
		assert.Equal(job.ID, event.JobID)
		assert.NotEmpty(event.Repo)
		assert.NotEmpty(event.RepoName)
		assert.Equal("abc123", event.SHA)
		assert.Equal("test", event.Agent)
	case <-time.After(time.Second):
		require.FailNow(t, "timed out waiting for review.closed event")
	}
}

func TestHandleCloseReview_BroadcastsReopenEvent(t *testing.T) {
	assert := assert.New(t)
	server, db, tmpDir := newTestServer(t)

	job := createTestJob(t, db, tmpDir, "reopen123", "test")
	claimed, err := db.ClaimJob("worker-1")
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)
	require.NoError(t, db.CompleteJob(job.ID, "test", "prompt", "output"))

	// Close first, then reopen
	require.NoError(t, db.MarkReviewClosedByJobID(job.ID, true))

	_, eventCh := server.broadcaster.Subscribe("")

	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/review/close", CloseReviewRequest{
		JobID:  job.ID,
		Closed: false,
	})
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)

	assert.Equal(http.StatusOK, w.Code)

	select {
	case event := <-eventCh:
		assert.Equal("review.reopened", event.Type)
		assert.Equal(job.ID, event.JobID)
		assert.NotEmpty(event.Repo)
		assert.NotEmpty(event.RepoName)
		assert.Equal("reopen123", event.SHA)
		assert.Equal("test", event.Agent)
	case <-time.After(time.Second):
		require.FailNow(t, "timed out waiting for review.reopened event")
	}
}

func TestHandleCloseReview_RepoFilteredSubscriber(t *testing.T) {
	assert := assert.New(t)
	server, db, tmpDir := newTestServer(t)

	job := createTestJob(t, db, tmpDir, "filter123", "test")
	claimed, err := db.ClaimJob("worker-1")
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)
	require.NoError(t, db.CompleteJob(job.ID, "test", "prompt", "output"))

	// Look up the normalized repo path used in the DB
	loaded, err := db.GetJobByID(job.ID)
	require.NoError(t, err)

	// Subscribe with repo filter — should receive the event
	_, filteredCh := server.broadcaster.Subscribe(loaded.RepoPath)
	// Subscribe with wrong repo — should NOT receive the event
	_, wrongCh := server.broadcaster.Subscribe("/nonexistent/repo")

	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/review/close", CloseReviewRequest{
		JobID:  job.ID,
		Closed: true,
	})
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)
	assert.Equal(http.StatusOK, w.Code)

	// Filtered subscriber receives the event
	select {
	case event := <-filteredCh:
		assert.Equal("review.closed", event.Type)
		assert.Equal(job.ID, event.JobID)
	case <-time.After(time.Second):
		require.FailNow(t, "repo-filtered subscriber did not receive review.closed")
	}

	// Wrong-repo subscriber does not receive the event
	select {
	case event := <-wrongCh:
		require.FailNow(t, "wrong-repo subscriber received event", "event: %v", event)
	case <-time.After(50 * time.Millisecond):
		// expected — no event
	}
}

func TestHandleEnqueue_BroadcastsEvent(t *testing.T) {
	assert := assert.New(t)
	server, _, tmpDir := newTestServer(t)

	repoDir := filepath.Join(tmpDir, "testrepo")
	testutil.InitTestGitRepo(t, repoDir)
	sha := testutil.GetHeadSHA(t, repoDir)

	_, eventCh := server.broadcaster.Subscribe("")

	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", EnqueueRequest{
		RepoPath: repoDir,
		GitRef:   sha,
		Agent:    "test",
	})
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)

	assert.Equal(http.StatusCreated, w.Code)

	select {
	case event := <-eventCh:
		assert.Equal("job.enqueued", event.Type)
		// Repo path is resolved by git (symlinks, short names),
		// so compare non-empty rather than exact match.
		assert.NotEmpty(event.Repo)
		assert.Equal(sha, event.SHA)
		assert.Equal("test", event.Agent)
	case <-time.After(time.Second):
		require.FailNow(t, "timed out waiting for job.enqueued event")
	}
}

func TestHandleListCommentsJobIDParsing(t *testing.T) {
	server, _, _ := newTestServer(t)
	for _, id := range []string{"abc", "10abc", "1.5"} {
		t.Run("invalid_id_"+id, func(t *testing.T) {
			rr := serveHuma(t, server, http.MethodGet,
				"/api/comments?job_id="+id, nil)
			assert.GreaterOrEqual(t, rr.Code, 400,
				"expected client error for invalid id %q", id)
		})
	}
}
