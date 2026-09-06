package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/coreos/go-systemd/v22/activation"
	"github.com/coreos/go-systemd/v22/daemon"
	"github.com/danielgtaylor/huma/v2"
	"github.com/google/go-github/v90/github"
	gitrepo "go.kenn.io/kit/git/repo"
	"go.kenn.io/kit/selfupdate"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/agenthook"
	"go.kenn.io/roborev/internal/autofix"
	"go.kenn.io/roborev/internal/backfill"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/telemetry"
	"go.kenn.io/roborev/internal/tokens"
	"go.kenn.io/roborev/internal/version"
)

// Server is the HTTP API server for the daemon
type Server struct {
	db                      *storage.DB
	configWatcher           *ConfigWatcher
	broadcaster             Broadcaster
	workerPool              *WorkerPool
	httpServer              *http.Server
	browserMu               sync.Mutex
	browserServer           *http.Server
	browserListener         net.Listener
	browserRuntime          *BrowserRuntimeInfo
	browserStopping         bool
	allowWebCompilationStub bool
	webDevOrigin            string
	syncWorker              *storage.SyncWorker
	ciPoller                *CIPoller
	hookRunner              *HookRunner
	errorLog                *ErrorLog
	activityLog             *ActivityLog
	releaseNotesClient      *github.Client
	releaseNotesNow         func() time.Time
	telemetry               telemetry.Client
	telemetryOnce           sync.Once
	telemetryStop           chan struct{}
	startTime               time.Time
	endpointMu              sync.Mutex // protects endpoint (written by Start, read by Stop)
	endpoint                DaemonEndpoint
	alternateEndpoint       *DaemonEndpoint
	socketActivated         bool // true if started via systemd socket activation
	stopOnce                sync.Once
	stopErr                 error
	sweepMu                 sync.Mutex         // protects sweepCancel (written by Start, read by Stop)
	sweepCancel             context.CancelFunc // cancels the panel sweep goroutine on Stop
	shutdownCh              chan struct{}      // closed when /api/shutdown is requested
	shutdownOnce            sync.Once
	shutdownDrainMu         sync.Mutex
	shutdownDraining        bool
	updateDrain             *updateDrainLease
	updateCoordinator       *updateDrainCoordinator
	agentHookState          *agenthook.StateStore
	agentHookStateErr       error

	// Cached machine ID to avoid INSERT on every status request
	machineIDMu sync.Mutex
	machineID   uuid.UUID
}

const dailyTelemetryInterval = 24 * time.Hour

var (
	shutdownCleanupTimeout       = 35 * time.Second
	shutdownCleanupRetryInterval = 200 * time.Millisecond
)

var (
	getSystemdListenerForServer      = getSystemdListener
	listenAuxiliaryEndpointForServer = listenAuxiliaryEndpoint
)

// ServerOption customizes a daemon server before it starts.
type ServerOption func(*Server)

// WithWebDevelopmentOrigin adds one exact loopback origin for the disposable
// development server. Production callers must not set this option.
func WithWebDevelopmentOrigin(origin string) ServerOption {
	return func(server *Server) {
		server.webDevOrigin = origin
	}
}

func withWebCompilationStub() ServerOption {
	return func(server *Server) {
		server.allowWebCompilationStub = true
	}
}

// NewServer creates a new daemon server.
func NewServer(db *storage.DB, cfg *config.Config, configPath string, options ...ServerOption) *Server {
	// Initialize error log
	errorLog, err := NewErrorLog(DefaultErrorLogPath())
	if err != nil {
		log.Printf("Warning: failed to create error log: %v", err)
	}

	// Initialize activity log
	activityLog, err := NewActivityLog(DefaultActivityLogPath())
	if err != nil {
		log.Printf("Warning: failed to create activity log: %v", err)
	}

	server := newServerWithLogs(db, cfg, configPath, errorLog, activityLog)
	for _, option := range options {
		option(server)
	}
	return server
}

func newServerWithLogs(
	db *storage.DB,
	cfg *config.Config,
	configPath string,
	errorLog *ErrorLog,
	activityLog *ActivityLog,
) *Server {
	// Always set for deterministic state - default to false (conservative)
	agent.SetAllowUnsafeAgents(cfg.AllowUnsafeAgents != nil && *cfg.AllowUnsafeAgents)
	agent.SetCodexSandboxDisabled(cfg.DisableCodexSandbox)
	agent.SetAnthropicAPIKey(cfg.AnthropicAPIKey)
	broadcaster := NewBroadcaster()

	// Create config watcher for hot-reloading
	configWatcher := NewConfigWatcher(configPath, cfg, broadcaster, activityLog)

	// Create hook runner to fire hooks on review events
	hookRunner := NewHookRunner(configWatcher, broadcaster, log.Default())

	releaseNotesOptions := []github.ClientOptionsFunc{
		github.WithHTTPClient(&http.Client{Timeout: 10 * time.Second}),
	}
	if token := selfupdate.EnvironmentGitHubToken(); token != "" {
		releaseNotesOptions = append(releaseNotesOptions, github.WithAuthToken(token))
	}
	releaseNotesClient, err := github.NewClient(releaseNotesOptions...)
	if err != nil {
		panic(fmt.Sprintf("create GitHub client for release notes: %v", err))
	}

	s := &Server{
		db:                 db,
		configWatcher:      configWatcher,
		broadcaster:        broadcaster,
		workerPool:         NewWorkerPool(db, configWatcher, cfg.MaxWorkers, broadcaster, errorLog, activityLog),
		hookRunner:         hookRunner,
		errorLog:           errorLog,
		activityLog:        activityLog,
		releaseNotesClient: releaseNotesClient,
		releaseNotesNow:    time.Now,
		telemetryStop:      make(chan struct{}),
		startTime:          time.Now(),
		shutdownCh:         make(chan struct{}),
	}
	s.updateCoordinator = &updateDrainCoordinator{server: s, now: time.Now}
	s.agentHookState, s.agentHookStateErr = agenthook.LoadState(
		daemonAgentHookSource{db: db},
	)

	mux := http.NewServeMux()
	s.registerHumaAPI(mux)
	s.registerAgentHookRoutes(mux)

	s.httpServer = &http.Server{
		Addr:    cfg.ServerAddr,
		Handler: s.withRequestGuards(mux),
	}

	return s
}

func (s *Server) withRequestGuards(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/enqueue" {
			maxPromptSize := config.DefaultMaxPromptSize
			if cfg := s.configWatcher.Config(); cfg != nil && cfg.DefaultMaxPromptSize > 0 {
				maxPromptSize = cfg.DefaultMaxPromptSize
			}
			maxBodySize := int64(maxPromptSize) + 50*1024
			if r.ContentLength > maxBodySize {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusRequestEntityTooLarge)
				_ = json.NewEncoder(w).Encode(ErrorResponse{
					Error: fmt.Sprintf("request body too large (max %dKB)", maxBodySize/1024),
				})
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

// Start begins the server and worker pool
func (s *Server) Start(ctx context.Context) error {
	cfg := s.configWatcher.Config()

	// Check for socket activation before falling back to the config
	listener, ep, err := getSystemdListenerForServer()
	if err != nil {
		return err
	}
	if listener != nil {
		s.socketActivated = true
		log.Printf("Using systemd socket activation on %s", ep)
	} else {
		ep, err = ParseEndpoint(cfg.ServerAddr)
		if err != nil {
			return err
		}
	}

	// Clean up any zombie daemons first (there can be only one)
	if cleaned := CleanupZombieDaemons(ep); cleaned > 0 {
		log.Printf("Cleaned up %d zombie daemon(s)", cleaned)
		if s.activityLog != nil {
			s.activityLog.Log(
				"daemon.zombie_cleanup", "server",
				fmt.Sprintf("cleaned up %d zombie daemon(s)", cleaned),
				map[string]string{"count": strconv.Itoa(cleaned)},
			)
		}
	}
	runtimes, err := ListAllRuntimes()
	if err != nil {
		if listener != nil {
			_ = listener.Close()
		}
		return fmt.Errorf("check existing daemon runtimes: %w", err)
	}

	// Check if a responsive daemon is still running after cleanup.
	info, discoveryErr := GetAnyRunningDaemon()
	if IsDaemonAccessDenied(discoveryErr) {
		if listener != nil {
			_ = listener.Close()
		}
		return discoveryErr
	}
	if discoveryErr == nil && IsDaemonAlive(info.Endpoint()) {
		if listener != nil {
			_ = listener.Close()
		}
		return fmt.Errorf("daemon already running (pid %d on %s)", info.PID, info.Address)
	}
	for _, runtime := range runtimes {
		if runtime.PID > 0 && isProcessAlive(runtime.PID) {
			if listener != nil {
				_ = listener.Close()
			}
			return fmt.Errorf("daemon process still running (pid %d)", runtime.PID)
		}
	}
	if err := s.db.SetShutdownDraining(false); err != nil {
		if listener != nil {
			_ = listener.Close()
		}
		return fmt.Errorf("clear interrupted shutdown drain: %w", err)
	}

	// Reset stale jobs from previous runs
	if err := s.db.ResetStaleJobs(); err != nil {
		log.Printf("Warning: failed to reset stale jobs: %v", err)
	}

	// Start config watcher for hot-reloading
	if err := s.configWatcher.Start(ctx); err != nil {
		log.Printf("Warning: failed to start config watcher: %v", err)
		// Continue without hot-reloading - not a fatal error
	}

	if !s.socketActivated {
		// Bind the listener before publishing runtime metadata so concurrent CLI
		// invocations cannot race a half-started daemon and kill it as a zombie.
		if ep.IsUnix() {
			listener, err = listenUnixEndpoint(ep)
			if err != nil {
				s.configWatcher.Stop()
				return err
			}
		} else {
			// TCP: find an available port first
			addr, _, err := FindAvailablePort(ep.Address)
			if err != nil {
				s.configWatcher.Stop()
				return fmt.Errorf("find available port: %w", err)
			}
			ep = DaemonEndpoint{Network: "tcp", Address: addr}
			s.httpServer.Addr = addr

			listener, err = ep.Listener()
			if err != nil {
				s.configWatcher.Stop()
				return fmt.Errorf("listen on %s: %w", ep, err)
			}
			// Update ep with actual bound address
			ep = DaemonEndpoint{Network: "tcp", Address: listener.Addr().String()}
			s.httpServer.Addr = ep.Address
		}
	}

	s.endpointMu.Lock()
	s.endpoint = ep
	s.endpointMu.Unlock()

	serveErrCh := make(chan error, 1)
	log.Printf("Starting HTTP server on %s", ep)
	go func() {
		serveErrCh <- s.httpServer.Serve(listener)
	}()

	if err := cleanupStaleCIWorktrees(ctx); err != nil {
		log.Printf("Warning: failed to clean up stale CI worktrees: %v", err)
	}

	// Start worker pool before advertising availability.
	s.workerPool.Start()

	ready, serveExited, err := waitForServerReady(ctx, ep, 2*time.Second, serveErrCh)
	if err != nil {
		_ = listener.Close()
		s.configWatcher.Stop()
		s.workerPool.Stop()
		return err
	}
	if !ready {
		if err := awaitServeExitOnUnreadyStartup(serveExited, serveErrCh); err != nil {
			s.configWatcher.Stop()
			s.workerPool.Stop()
			return err
		}
		return nil
	}

	var alternate *DaemonEndpoint
	if !s.socketActivated {
		auxListener, candidate, auxErr := listenAuxiliaryEndpointForServer(ep)
		if auxErr != nil {
			log.Printf("Warning: auxiliary Unix listener unavailable: %v", auxErr)
		} else if auxListener != nil && candidate != nil {
			auxServeErrCh := make(chan error, 1)
			log.Printf("Starting auxiliary HTTP server on %s", candidate)
			go func() {
				auxServeErrCh <- s.httpServer.Serve(auxListener)
			}()
			auxReady, auxExited, readyErr := waitForServerReady(
				ctx, *candidate, 2*time.Second, auxServeErrCh,
			)
			if readyErr != nil || !auxReady {
				_ = auxListener.Close()
				_ = os.Remove(candidate.Address)
				if readyErr == nil {
					readyErr = fmt.Errorf("listener exited before becoming ready")
				}
				log.Printf("Warning: auxiliary Unix listener unavailable: %v", readyErr)
				if !auxExited {
					_ = awaitServeExitOnUnreadyStartup(false, auxServeErrCh)
				}
			} else {
				alternate = candidate
				go func() {
					serveErr := <-auxServeErrCh
					if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
						log.Printf("Auxiliary Unix listener stopped: %v", serveErr)
					}
				}()
			}
		}
	}

	s.endpointMu.Lock()
	s.alternateEndpoint = alternate
	s.endpointMu.Unlock()

	browserRuntime, err := s.startBrowserServer(cfg.Web)
	if err != nil {
		_ = s.httpServer.Close()
		s.configWatcher.Stop()
		s.workerPool.Stop()
		return err
	}
	s.browserMu.Lock()
	if s.browserStopping {
		s.browserMu.Unlock()
		_ = s.httpServer.Close()
		s.configWatcher.Stop()
		s.workerPool.Stop()
		return fmt.Errorf("server stopped during browser startup")
	}
	s.browserRuntime = browserRuntime
	s.startPanelSweep(ctx)

	// Write runtime info only after the HTTP server is accepting requests.
	if err := WriteRuntime(ep, alternate, version.Version, browserRuntime); err != nil {
		log.Printf("Warning: failed to write runtime info: %v", err)
	}
	s.browserMu.Unlock()

	s.captureDaemonStartedTelemetry(cfg)
	s.startDailyTelemetryLoop(ctx, cfg)

	// Notify systemd that the daemon is ready. No-op when not running
	// under systemd (NOTIFY_SOCKET is unset).
	_, _ = daemon.SdNotify(false, daemon.SdNotifyReady)

	// Log daemon start after runtime publication.
	if s.activityLog != nil {
		binary, _ := os.Executable()
		s.activityLog.Log(
			"daemon.started", "server",
			fmt.Sprintf("daemon started on %s", ep),
			map[string]string{
				"version": version.Version,
				"binary":  binary,
				"addr":    ep.Address,
				"pid":     strconv.Itoa(os.Getpid()),
				"workers": strconv.Itoa(cfg.MaxWorkers),
			},
		)
	}

	// Repair stale roborev-managed hooks in registered repos (skip in CI
	// mode where repos are fetch-only and don't need local hooks).
	if s.ciPoller == nil {
		if repos, err := s.db.ListRepos(); err == nil {
			go repairRegisteredHooks(ctx, repos)
		}
	}

	if err := <-serveErrCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
		s.configWatcher.Stop()
		s.stopPanelSweep()
		s.workerPool.Stop()
		return err
	}
	return nil
}

func (s *Server) startPanelSweep(ctx context.Context) {
	// Backstop for a missed worker release of a panel synthesis gate. Own the
	// cancel so Stop halts the sweep even when Start received a context the
	// caller never cancels (the worker pool and CI poller are likewise stopped
	// explicitly in stopOnce0).
	sweepCtx, cancelSweep := context.WithCancel(ctx)
	s.sweepMu.Lock()
	if s.sweepCancel != nil {
		s.sweepCancel()
	}
	s.sweepCancel = cancelSweep
	s.sweepMu.Unlock()
	go s.runPanelSweep(sweepCtx, panelSweepInterval)
}

func (s *Server) stopPanelSweep() {
	s.sweepMu.Lock()
	sweepCancel := s.sweepCancel
	s.sweepCancel = nil
	s.sweepMu.Unlock()
	if sweepCancel != nil {
		sweepCancel()
	}
}

func waitForServerReady(ctx context.Context, ep DaemonEndpoint, timeout time.Duration, serveErrCh <-chan error) (bool, bool, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error

	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return false, false, nil
		}
		select {
		case err := <-serveErrCh:
			if errors.Is(err, http.ErrServerClosed) && ctx.Err() != nil {
				return false, true, nil
			}
			if err == nil {
				return false, true, fmt.Errorf("daemon server exited before ready")
			}
			return false, true, err
		default:
		}
		if _, err := ProbeDaemon(ep, 200*time.Millisecond); err == nil {
			return true, false, nil
		} else {
			lastErr = err
		}
		time.Sleep(25 * time.Millisecond)
	}

	if ctx.Err() != nil {
		return false, false, nil
	}
	select {
	case err := <-serveErrCh:
		if errors.Is(err, http.ErrServerClosed) && ctx.Err() != nil {
			return false, true, nil
		}
		if err == nil {
			return false, true, fmt.Errorf("daemon server exited before ready")
		}
		return false, true, err
	default:
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("server did not respond before timeout")
	}
	return false, false, fmt.Errorf("daemon failed to become ready on %s within %s: %w", ep, timeout, lastErr)
}

func awaitServeExitOnUnreadyStartup(serveExited bool, serveErrCh <-chan error) error {
	if serveExited {
		return nil
	}

	if err := <-serveErrCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// getSystemdListener returns the listener and endpoint passed by systemd during
// socket activation, or (nil, empty, nil) if not running under socket activation.
// Validates the listener matches the daemon's local-only trust model.
func getSystemdListener() (net.Listener, DaemonEndpoint, error) {
	listeners, err := activation.Listeners()
	if err != nil {
		return nil, DaemonEndpoint{}, fmt.Errorf("socket activation: %w", err)
	}
	if len(listeners) == 0 {
		return nil, DaemonEndpoint{}, nil
	}
	if len(listeners) > 1 {
		return nil, DaemonEndpoint{}, fmt.Errorf(
			"socket activation: multiple sockets not supported")
	}

	listener := listeners[0]
	if listener == nil {
		return nil, DaemonEndpoint{}, fmt.Errorf(
			"socket activation: unsupported socket type")
	}
	addr := listener.Addr().String()
	if listener.Addr().Network() == "unix" {
		if strings.HasPrefix(addr, "@") || strings.HasPrefix(addr, "\x00") {
			_ = listener.Close()
			return nil, DaemonEndpoint{}, fmt.Errorf(
				"socket activation: abstract Unix sockets are not supported"+
					" (got %q); use a filesystem path in ListenStream=", addr)
		}
		addr = "unix://" + addr
	}
	ep, err := ParseEndpoint(addr)
	if err != nil {
		// Errors on non-localhost, etc.
		_ = listener.Close()
		return nil, ep, err
	}

	// Ensure that Unix sockets have safe permissions.
	if ep.IsUnix() {
		fi, err := os.Stat(ep.Address)
		if err != nil {
			_ = listener.Close()
			return nil, ep, fmt.Errorf("socket activation: %w", err)
		}
		if perm := fi.Mode().Perm(); perm&0o077 != 0 {
			_ = listener.Close()
			return nil, ep, fmt.Errorf(
				"socket activation: socket %q has unsafe permissions: %04o",
				ep.Address, perm)
		}
	}

	return listener, ep, nil
}

// Stop gracefully shuts down the server. Safe to call more than once;
// repeated calls return the first call's result and do nothing.
// Idempotency matters for test cleanup (t.Cleanup will fire Close even
// when the test body has already called Stop explicitly), and prevents
// the "close of closed channel" panic when hookRunner.Stop runs twice.
func (s *Server) Stop() error {
	if err := s.beginShutdownDrain(); err != nil {
		return err
	}
	s.stopOnce.Do(func() {
		s.stopErr = s.stopOnce0()
	})
	return s.stopErr
}

func (s *Server) stopOnce0() error {
	// Log daemon stop before shutting down components
	if s.activityLog != nil {
		uptime := time.Since(s.startTime)
		s.activityLog.Log(
			"daemon.stopped", "server",
			"daemon stopped",
			map[string]string{"uptime": formatDuration(uptime)},
		)
	}
	// Stop telemetry loop
	close(s.telemetryStop)

	// Stop config watcher
	s.configWatcher.Stop()

	// Prevent a browser listener that is still starting from becoming available
	// after shutdown has begun. The active listener remains available while
	// workers drain, matching the CLI listener's graceful shutdown behavior.
	s.browserMu.Lock()
	s.browserStopping = true
	browserServer := s.browserServer
	browserListener := s.browserListener
	s.browserMu.Unlock()

	// Stop new CI polling work. Keep its completion listener subscribed while
	// active workers finish so their terminal events are still finalized.
	if s.ciPoller != nil {
		s.ciPoller.BeginStop()
	}

	// Stop the panel sweep goroutine
	s.stopPanelSweep()

	// Stop worker pool
	s.workerPool.Stop()

	// Workers cannot emit more completion events. Send the listener poison pill,
	// drain its FIFO queue, and join any active CI post before teardown continues.
	if s.ciPoller != nil {
		s.ciPoller.Stop()
	}

	// Bound post-worker cleanup with one shared budget. Running reviews have
	// already finished, so this deadline applies only to daemon teardown.
	shutdownCleanupCtx, cancelShutdownCleanup := context.WithTimeout(
		context.Background(), shutdownCleanupTimeout,
	)
	defer cancelShutdownCleanup()
	var cleanupErr error

	// Stop accepting mutations once workers have finished. Runtime discovery
	// remains published until all completion work below is finalized.
	if err := s.httpServer.Shutdown(shutdownCleanupCtx); err != nil {
		log.Printf("HTTP server shutdown error: %v", err)
		cleanupErr = errors.Join(cleanupErr, fmt.Errorf("shutdown HTTP server: %w", err))
	}
	if browserServer != nil {
		if err := browserServer.Shutdown(shutdownCleanupCtx); err != nil {
			log.Printf("Browser HTTP server shutdown error: %v", err)
			cleanupErr = errors.Join(
				cleanupErr,
				fmt.Errorf("shutdown browser HTTP server: %w", err),
			)
		}
	} else if browserListener != nil {
		if err := browserListener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			cleanupErr = errors.Join(
				cleanupErr,
				fmt.Errorf("close browser listener: %w", err),
			)
		}
	}

	// Stop hook runner
	if s.hookRunner != nil {
		s.hookRunner.WaitUntilIdle()
		s.hookRunner.Stop()
	}
	if s.syncWorker != nil {
		if err := s.syncWorker.FinalPush(); err != nil {
			log.Printf("Final sync push error: %v", err)
		}
		s.syncWorker.Stop()
	}

	// Clean up Unix domain sockets after the server stops accepting requests.
	s.endpointMu.Lock()
	ep := s.endpoint
	alternate := s.alternateEndpoint
	s.endpointMu.Unlock()
	if ep.IsUnix() && !s.socketActivated {
		os.Remove(ep.Address)
	}
	if alternate != nil {
		os.Remove(alternate.Address)
	}

	// Close error log
	if s.errorLog != nil {
		s.errorLog.Close()
	}

	// Close activity log
	if s.activityLog != nil {
		s.activityLog.Close()
	}

	// Keep discovery metadata published until all daemon work and HTTP serving
	// have stopped, so another daemon cannot start during finalization.
	if err := s.clearShutdownDrain(shutdownCleanupCtx); err != nil {
		cleanupErr = errors.Join(cleanupErr, err)
	}
	RemoveRuntime()

	return cleanupErr
}

func (s *Server) clearShutdownDrain(ctx context.Context) error {
	s.shutdownDrainMu.Lock()
	draining := s.shutdownDraining
	s.shutdownDrainMu.Unlock()
	if !draining {
		return nil
	}
	var lastErr error
	for {
		if err := s.db.SetShutdownDrainingContext(ctx, false); err == nil {
			return nil
		} else {
			lastErr = err
			log.Printf("Clear shutdown drain state failed; retrying: %v", err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("clear shutdown drain state: %w", errors.Join(lastErr, ctx.Err()))
		case <-time.After(shutdownCleanupRetryInterval):
		}
	}
}

// Close shuts down the server and releases its resources.
// It is primarily provided for ease of use in test cleanup.
func (s *Server) Close() error {
	return s.Stop()
}

// ConfigWatcher returns the server's config watcher (for use by external components)
func (s *Server) ConfigWatcher() *ConfigWatcher {
	return s.configWatcher
}

// Broadcaster returns the server's event broadcaster (for use by external components)
func (s *Server) Broadcaster() Broadcaster {
	return s.broadcaster
}

// SetTelemetry sets the anonymous telemetry client used for daemon lifecycle events.
func (s *Server) SetTelemetry(client telemetry.Client) {
	s.telemetry = client
}

func (s *Server) captureDaemonStartedTelemetry(cfg *config.Config) {
	s.captureTelemetryEvent(telemetry.EventDaemonStarted, cfg)
}

func (s *Server) startDailyTelemetryLoop(ctx context.Context, cfg *config.Config) {
	if s.telemetry == nil || !s.telemetry.Enabled() {
		return
	}

	s.telemetryOnce.Do(func() {
		if s.telemetry == nil || !s.telemetry.Enabled() {
			return
		}

		s.captureDailyTelemetry(cfg)

		go func() {
			ticker := time.NewTicker(dailyTelemetryInterval)
			defer ticker.Stop()

			for {
				select {
				case <-ctx.Done():
					return
				case <-s.telemetryStop:
					return
				case <-ticker.C:
					s.captureDailyTelemetry(cfg)
				}
			}
		}()
	})
}

func (s *Server) captureDailyTelemetry(cfg *config.Config) {
	s.captureTelemetryEvent(telemetry.EventDaemonActive, cfg)
}

func (s *Server) captureTelemetryEvent(event string, cfg *config.Config) {
	if s.telemetry == nil || !s.telemetry.Enabled() {
		return
	}

	props := s.telemetryProperties(cfg)
	if err := s.telemetry.Capture(event, props); err != nil {
		log.Printf("Warning: capture telemetry event: %v", err)
	}
}

func (s *Server) telemetryProperties(cfg *config.Config) map[string]any {
	repoCount := 0
	if repos, err := s.db.ListRepos(); err != nil {
		log.Printf("Warning: failed to count repos for telemetry: %v", err)
	} else {
		repoCount = len(repos)
	}
	reviewCount := 0
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM reviews`).Scan(&reviewCount); err != nil {
		log.Printf("Warning: failed to count reviews for telemetry: %v", err)
	}

	props := map[string]any{
		"repo_count":   repoCount,
		"review_count": reviewCount,
	}
	if cfg != nil {
		props["sync_enabled"] = cfg.Sync.Enabled
		props["ci_enabled"] = cfg.CI.Enabled
		props["auto_design_enabled"] = cfg.AutoDesignReview.Enabled
	}

	return props
}

// SetSyncWorker sets the sync worker for triggering manual syncs
func (s *Server) SetSyncWorker(sw *storage.SyncWorker) {
	s.syncWorker = sw
}

// SetCIPoller sets the CI poller for status reporting and wires up
// the worker pool cancellation callback so the poller can kill running
// processes when superseding stale batches.
func (s *Server) SetCIPoller(cp *CIPoller) {
	s.ciPoller = cp
	cp.jobCancelFn = func(jobID int64) {
		s.workerPool.CancelJob(jobID)
	}
}

func workflowForJob(jobType, reviewType string) string {
	// Fix and compact jobs use the "fix" workflow since they're part of
	// that pipeline.
	if jobType == storage.JobTypeFix || jobType == storage.JobTypeCompact {
		return "fix"
	}
	return config.WorkflowForReviewType(reviewType)
}

func validatedWorktreePath(worktreePath, repoPath string) string {
	if worktreePath == "" {
		return ""
	}
	if !git.ValidateWorktreeForRepo(worktreePath, repoPath) {
		return ""
	}
	return worktreePath
}

func resolveRerunOpts(
	job *storage.ReviewJob,
	cfg *config.Config,
	assignment *storage.ExperimentAssignmentInput,
	selectedAgent string,
) (storage.ReenqueueOpts, error) {
	resolutionPath := job.RepoPath
	if job.WorktreePath != "" {
		worktreePath := validatedWorktreePath(job.WorktreePath, job.RepoPath)
		if worktreePath == "" {
			return storage.ReenqueueOpts{}, fmt.Errorf("rerun job worktree path is stale or invalid")
		}
		resolutionPath = worktreePath
	}

	if err := config.ValidateRepoConfig(resolutionPath); err != nil {
		return storage.ReenqueueOpts{}, fmt.Errorf("resolve workflow config: %w", err)
	}
	if assignment != nil {
		if strings.TrimSpace(selectedAgent) != "" {
			return storage.ReenqueueOpts{}, errors.New("frozen experiment jobs cannot change agents on rerun")
		}
		var plan experimentJobPlan
		if err := json.Unmarshal([]byte(assignment.EffectiveConfigJSON), &plan); err != nil {
			return storage.ReenqueueOpts{}, fmt.Errorf("decode frozen experiment plan: %w", err)
		}
		planHash, err := config.FingerprintExperimentConfig(plan)
		if err != nil {
			return storage.ReenqueueOpts{}, fmt.Errorf("fingerprint frozen experiment plan: %w", err)
		}
		if planHash != assignment.EffectiveConfigHash {
			return storage.ReenqueueOpts{}, errors.New("frozen experiment plan does not match its attribution")
		}
		if err := validateRerunAgent(
			resolutionPath, plan.Agent, plan.BackupAgent, cfg,
		); err != nil {
			return storage.ReenqueueOpts{}, err
		}
		return storage.ReenqueueOpts{
			Agent: plan.Agent, Model: plan.Model, Provider: plan.Provider,
			Reasoning: plan.Reasoning, ReviewType: plan.ReviewType,
			MinSeverity: plan.MinSeverity, BackupAgent: plan.BackupAgent,
			BackupModel: plan.BackupModel, RestorePlan: true,
		}, nil
	}

	workflow := workflowForJob(job.JobType, job.ReviewType)
	resolution, err := agent.ResolveWorkflowConfig(
		"", resolutionPath, cfg, workflow, job.Reasoning,
	)
	if err != nil {
		return storage.ReenqueueOpts{}, fmt.Errorf("resolve workflow config: %w", err)
	}
	selectedAgent = strings.TrimSpace(selectedAgent)
	if selectedAgent != "" {
		selected, err := agent.GetAvailableExactWithConfigFromConfig(
			resolution.RepoConfig, selectedAgent, cfg,
		)
		if err != nil {
			return storage.ReenqueueOpts{}, fmt.Errorf(
				"resolve selected agent %q: %w", selectedAgent, err,
			)
		}
		if job.JobType == storage.JobTypeClassify && !agent.IsSchemaAgent(selected) {
			return storage.ReenqueueOpts{}, fmt.Errorf(
				"classifier reruns require a SchemaAgent, got %q", selectedAgent,
			)
		}
		if err := agent.ValidateStructuredReviewSelection(job.ReviewType, selected); err != nil {
			return storage.ReenqueueOpts{}, err
		}
		storageName := agent.StorageNameFromConfig(
			agent.CanonicalName(selectedAgent), resolution.RepoConfig, cfg,
		)
		// Keep the original request as provenance, but do not carry its
		// model or provider override into a different agent's execution.
		model := resolution.ModelForSelectedAgent(storageName, "")
		if job.JobType == storage.JobTypeClassify {
			model = config.ResolveClassifyModel("", resolutionPath, cfg)
		}
		return storage.ReenqueueOpts{
			Agent: storageName,
			Model: model,
		}, nil
	}

	backupAgent := resolution.BackupAgent
	if strings.TrimSpace(job.BackupAgent) != "" {
		backupAgent = job.BackupAgent
	}
	if err := validateRerunAgent(resolutionPath, job.Agent, backupAgent, cfg); err != nil {
		return storage.ReenqueueOpts{}, err
	}

	provider := strings.TrimSpace(job.RequestedProvider)
	if model := strings.TrimSpace(job.RequestedModel); model != "" {
		return storage.ReenqueueOpts{Model: model, Provider: provider}, nil
	}

	model := resolution.ModelForSelectedAgent(job.Agent, "")
	return storage.ReenqueueOpts{Model: model, Provider: provider}, nil
}

func resolveRerunModelProvider(job *storage.ReviewJob, cfg *config.Config) (string, string, error) {
	opts, err := resolveRerunOpts(job, cfg, nil, "")
	return opts.Model, opts.Provider, err
}

func validateRerunAgent(repoPath string, agentName string, backupAgent string, cfg *config.Config) error {
	_, err := agent.GetPreferredOrBackupWithConfig(repoPath, agentName, cfg, backupAgent)
	if err != nil {
		if _, ok := errors.AsType[*agent.UnknownAgentError](err); ok {
			return fmt.Errorf("invalid agent: %w", err)
		}
		return fmt.Errorf("no agent available: %w", err)
	}
	return nil
}

func (s *Server) findReusableSessionID(
	ctx context.Context,
	repoPath string, repoID int64, branch, agentName, reviewType, worktreePath, targetSHA string,
) string {
	cfg := s.configWatcher.Config()
	if !config.ResolveReuseReviewSession(repoPath, cfg) || branch == "" || targetSHA == "" {
		return ""
	}

	candidates, err := s.db.FindReusableSessionCandidates(
		repoID,
		branch,
		agentName,
		reviewType,
		worktreePath,
		config.ResolveReuseReviewSessionLookback(repoPath, cfg),
	)
	if err != nil {
		log.Printf("enqueue: lookup reusable session failed for repo=%d branch=%q agent=%q: %v", repoID, branch, agentName, err)
		return ""
	}
	if len(candidates) == 0 {
		return ""
	}

	const maxSessionReuseDistance = 50
	for _, candidate := range candidates {
		candidateSHA := strings.TrimSpace(candidate.ReusableSessionTarget)
		if candidateSHA == "" {
			candidateSHA = reusableSessionTarget(candidate.GitRef)
		}
		if candidateSHA == "" {
			continue
		}

		isAncestor, err := gitrepo.IsAncestor(ctx, repoPath, candidateSHA, targetSHA)
		if err != nil {
			log.Printf("enqueue: validate reusable session failed for job %d (%q -> %q): %v", candidate.ID, candidateSHA, targetSHA, err)
			continue
		}
		if !isAncestor {
			continue
		}
		commitsSinceCandidate, err := git.GetRangeCommits(repoPath, candidateSHA+".."+targetSHA)
		if err != nil {
			log.Printf("enqueue: compute reusable session distance failed for job %d (%q -> %q): %v", candidate.ID, candidateSHA, targetSHA, err)
			continue
		}
		if len(commitsSinceCandidate) > maxSessionReuseDistance {
			continue
		}
		return candidate.SessionID
	}
	return ""
}

func findCompatibleReusableSession(
	ctx context.Context,
	db *storage.DB,
	repoPath, targetSHA string,
	opts storage.EnqueueOpts,
	repoCfg *config.RepoConfig,
	rawRepoCfg map[string]any,
	globalCfg *config.Config,
	experiment *storage.ExperimentAssignmentInput,
	ciPRNumber int,
) (string, *uuid.UUID) {
	if !config.ResolveReuseReviewSessionFromConfig(repoCfg, globalCfg) ||
		opts.Branch == "" || targetSHA == "" ||
		opts.PanelRole == storage.PanelRoleSynthesis ||
		!agent.SupportsSessionResume(opts.Agent) {
		return "", nil
	}
	machineID, err := db.GetMachineID()
	if err != nil || machineID == uuid.Nil() {
		return "", nil
	}
	candidates, err := db.FindCompatibleReusableSessionCandidates(storage.ReusableSessionQuery{
		RepoID:                opts.RepoID,
		Branch:                opts.Branch,
		Source:                opts.Source,
		Agent:                 opts.Agent,
		Model:                 opts.Model,
		Provider:              opts.Provider,
		Reasoning:             opts.Reasoning,
		ReviewType:            opts.ReviewType,
		WorktreePath:          opts.WorktreePath,
		PanelName:             opts.PanelName,
		PanelMemberName:       opts.PanelMemberName,
		PanelMemberConfigJSON: opts.PanelMemberConfigJSON,
		SourceMachineID:       machineID,
		CIPRNumber:            ciPRNumber,
		Experiment:            experiment,
		Limit: config.ResolveReuseReviewSessionLookbackFromConfig(
			repoCfg, rawRepoCfg, globalCfg,
		),
	})
	if err != nil {
		log.Printf("enqueue: lookup compatible reusable session failed for repo=%d agent=%q: %v", opts.RepoID, opts.Agent, err)
		return "", nil
	}
	const maxSessionReuseDistance = 50
	for _, candidate := range candidates {
		candidateSHA := strings.TrimSpace(candidate.ReusableSessionTarget)
		if candidateSHA == "" {
			continue
		}
		isAncestor, err := gitrepo.IsAncestor(ctx, repoPath, candidateSHA, targetSHA)
		if err != nil || !isAncestor {
			continue
		}
		commitsSinceCandidate, err := git.GetRangeCommits(repoPath, candidateSHA+".."+targetSHA)
		if err != nil || len(commitsSinceCandidate) > maxSessionReuseDistance {
			continue
		}
		return candidate.SessionID, candidate.UUID
	}
	return "", nil
}

func reusableSessionTarget(gitRef string) string {
	if gitRef == "" || gitRef == "dirty" {
		return ""
	}
	if strings.Contains(gitRef, "..") {
		parts := strings.SplitN(gitRef, "..", 2)
		return strings.TrimSpace(parts[1])
	}
	return strings.TrimSpace(gitRef)
}

// getMachineID returns the cached machine ID, fetching it on first successful call.
// Retries on each call until successful to handle transient DB errors.
func (s *Server) getMachineID() *uuid.UUID {
	s.machineIDMu.Lock()
	defer s.machineIDMu.Unlock()

	if s.machineID != uuid.Nil() {
		return &s.machineID
	}

	if id, err := s.db.GetMachineID(); err == nil && id != uuid.Nil() {
		s.machineID = id
	}
	if s.machineID == uuid.Nil() {
		return nil
	}
	return &s.machineID
}

func jobLogSafeEnd(f *os.File, fileSize int64) int64 {
	if fileSize == 0 {
		return 0
	}

	// Check if last byte is newline — common case.
	var last [1]byte
	if _, err := f.ReadAt(last[:], fileSize-1); err != nil {
		return fileSize
	}
	if last[0] == '\n' {
		return fileSize
	}

	// Scan backwards in 64KB chunks to find last newline.
	const chunkSize = 64 * 1024
	buf := make([]byte, chunkSize)
	pos := fileSize
	for pos > 0 {
		readStart := max(pos-chunkSize, 0)
		readLen := pos - readStart
		n, err := f.ReadAt(buf[:readLen], readStart)
		if err != nil && err != io.EOF {
			return fileSize
		}
		for i := n - 1; i >= 0; i-- {
			if buf[i] == '\n' {
				return readStart + int64(i) + 1
			}
		}
		pos = readStart
	}

	// Entire file has no newline — serve nothing to avoid
	// a partial line.
	return 0
}

func isValidGitRef(ref string) bool {
	if ref == "" || ref[0] == '-' {
		return false
	}
	for _, r := range ref {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func parseDuration(s string) (time.Duration, error) {
	if len(s) < 2 {
		return 0, fmt.Errorf("duration too short: %s", s)
	}
	unit := s[len(s)-1]
	val, err := strconv.Atoi(s[:len(s)-1])
	if err != nil {
		return 0, fmt.Errorf("invalid duration number: %s", s)
	}
	if val <= 0 {
		return 0, fmt.Errorf("duration must be positive: %s", s)
	}
	switch unit {
	case 'h':
		return time.Duration(val) * time.Hour, nil
	case 'd':
		return time.Duration(val) * 24 * time.Hour, nil
	case 'w':
		return time.Duration(val) * 7 * 24 * time.Hour, nil
	default:
		return 0, fmt.Errorf("unknown duration unit: %c (use h, d, or w)", unit)
	}
}

// buildFixPromptWithInstructions constructs a fix prompt that includes the review
// findings, optional user-provided instructions, and any comments/responses
// (split into tool attempts and user comments for proper framing).
func buildFixPromptWithInstructions(reviewOutput, userInstructions, minSeverity string, responses []storage.Response, reviewedRef string) string {
	toolAttempts, userComments := prompt.SplitResponses(responses)
	p := "# Fix Request\n\n" +
		"An analysis was performed and produced the following findings:\n\n"
	if inst := config.SeverityInstruction(minSeverity); inst != "" {
		p += inst + "\n"
	}
	p += "## Analysis Findings\n\n" +
		reviewOutput + "\n\n"
	p += prompt.FormatToolAttempts(toolAttempts)
	p += prompt.FormatUserComments(userComments)
	p += "## Restoration History\n\n" +
		autofix.RestorationHistoryGuidance + "\n\n" +
		autofix.FormatReviewedRef(reviewedRef)
	if userInstructions != "" {
		p += "## Additional Instructions\n\n" +
			userInstructions + "\n\n"
	}
	p += "## Instructions\n\n" +
		"Please apply the suggested changes from the analysis above. " +
		"Make the necessary edits to address each finding. " +
		"Focus on the highest priority items first.\n\n" +
		"After making changes:\n" +
		"1. Verify the code still compiles/passes linting\n" +
		"2. Run any relevant tests to ensure nothing is broken\n" +
		"3. Stage the changes with git add but do NOT commit — the changes will be captured as a patch\n"
	return p
}

// buildRebasePrompt constructs a prompt for re-applying a stale patch to current HEAD.
func buildRebasePrompt(stalePatch *string) string {
	prompt := "# Rebase Fix Request\n\n" +
		"A previous fix attempt produced a patch that no longer applies cleanly to the current HEAD.\n" +
		"Your task is to achieve the same changes but adapted to the current state of the code.\n\n"
	if stalePatch != nil && *stalePatch != "" {
		prompt += "## Previous Patch (stale)\n\n`````diff\n" + *stalePatch + "\n`````\n\n"
	}
	prompt += "## Instructions\n\n" +
		"1. Review the intent of the previous patch\n" +
		"2. Apply equivalent changes to the current codebase\n" +
		"3. Resolve any conflicts with recent changes\n" +
		"4. Verify the code compiles and tests pass\n" +
		"5. Stage the changes with git add but do NOT commit\n"
	return prompt
}

// formatDuration formats a duration in human-readable form (e.g., "2h 15m")
func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second

	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	if m > 0 {
		return fmt.Sprintf("%dm %ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

const limitNotProvided = -999999

// stripJobPrompts clears the large prompt and diff payloads from listed jobs
// for omit_prompt=true callers. Metadata-only consumers such as the agent hook
// daemon poll job lists on every hook event; shipping full prompts to them
// costs tens of megabytes of encode/decode per request. Queued/running jobs
// keep their prompt — the active set is small and it is the only way to see
// what a not-yet-reviewed job was asked, matching ListJobs' WithoutPrompt.
func stripJobPrompts(jobs []storage.ReviewJob) {
	for i := range jobs {
		if jobs[i].Status != storage.JobStatusQueued && jobs[i].Status != storage.JobStatusRunning {
			jobs[i].Prompt = ""
		}
		jobs[i].DiffContent = nil
	}
}

func (s *Server) humaListJobs(
	ctx context.Context, input *ListJobsInput,
) (*ListJobsOutput, error) {
	// Single job lookup by ID (>= 0 because ID=0 should
	// return empty, not fall through to the list path).
	if input.ID >= 0 {
		job, err := s.db.GetJobByID(input.ID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				resp := &ListJobsOutput{}
				resp.Body.Jobs = []storage.ReviewJob{}
				return resp, nil
			}
			return nil, huma.Error500InternalServerError(
				fmt.Sprintf("database error: %v", err),
			)
		}
		job.Patch = nil
		review, reviewErr := s.db.GetReviewByJobID(job.ID)
		if reviewErr == nil {
			job.Closed = &review.Closed
			if review.Job != nil {
				job.Verdict = review.Job.Verdict
			}
		} else if !errors.Is(reviewErr, sql.ErrNoRows) {
			return nil, huma.Error500InternalServerError(
				fmt.Sprintf("load job review metadata: %v", reviewErr),
			)
		}
		resp := &ListJobsOutput{}
		resp.Body.Jobs = []storage.ReviewJob{*job}
		attachPanelSummaries(s.db, resp.Body.Jobs)
		if input.OmitPrompt == "true" {
			stripJobPrompts(resp.Body.Jobs)
		}
		return resp, nil
	}

	// repo is repeatable: a display name spanning multiple repos sends one
	// ?repo= per path. A single path uses the positional equality filter
	// (fast path); multiple paths use an IN clause via WithRepoPaths so the
	// daemon scopes server-side instead of returning every job.
	var repoPaths []string
	for _, p := range input.Repo {
		if p != "" {
			repoPaths = append(repoPaths, filepath.ToSlash(filepath.Clean(p)))
		}
	}
	var repo string
	var repoPathsFilter []string
	switch len(repoPaths) {
	case 0:
		// no repo filter
	case 1:
		repo = repoPaths[0]
	default:
		repoPathsFilter = repoPaths
	}
	repoPrefix := input.RepoPrefix
	if repoPrefix != "" {
		repoPrefix = filepath.ToSlash(filepath.Clean(repoPrefix))
	}

	const maxLimit = 10000
	limit := 50
	switch {
	case input.Limit == limitNotProvided:
		// Not provided — use default
	case input.Limit < 0:
		limit = 0 // any negative → unlimited (legacy behavior)
	default:
		limit = input.Limit
	}
	if limit > maxLimit {
		limit = maxLimit
	}

	// A panel_run expansion returns the full run (members + synthesis). Without
	// an explicit caller limit, default to unlimited so a run with >=50 rows is
	// not silently truncated. An explicit limit is still honored.
	if input.PanelRun != uuid.Nil() && input.Limit == limitNotProvided {
		limit = 0
	}

	offset := max(input.Offset, 0)
	if limit == 0 {
		offset = 0
	}

	fetchLimit := limit
	if limit > 0 {
		fetchLimit = limit + 1
	}

	var listOpts []storage.ListJobsOption
	if input.OmitPrompt == "true" {
		listOpts = append(listOpts, storage.WithoutPrompt())
	}
	if input.GitRef != "" {
		listOpts = append(
			listOpts, storage.WithGitRef(input.GitRef),
		)
	}
	if input.BranchEmpty == "true" {
		listOpts = append(listOpts, storage.WithEmptyBranch())
	} else if input.Branch != "" {
		if input.BranchIncludeEmpty == "true" {
			listOpts = append(
				listOpts,
				storage.WithBranchOrEmpty(input.Branch),
			)
		} else {
			listOpts = append(
				listOpts, storage.WithBranch(input.Branch),
			)
		}
	}
	if input.Closed == "true" || input.Closed == "false" {
		listOpts = append(
			listOpts,
			storage.WithClosed(input.Closed == "true"),
		)
	}
	if input.JobType != "" {
		listOpts = append(
			listOpts, storage.WithJobType(input.JobType),
		)
	}
	if input.ExcludeJobType != "" {
		listOpts = append(
			listOpts,
			storage.WithExcludeJobType(input.ExcludeJobType),
		)
	}
	if input.HideClassifyJobs == "true" {
		listOpts = append(listOpts, storage.WithHideClassifyJobs())
	}
	if len(repoPathsFilter) > 0 {
		listOpts = append(listOpts, storage.WithRepoPaths(repoPathsFilter))
	}
	if repoPrefix != "" && len(repoPaths) == 0 {
		listOpts = append(
			listOpts, storage.WithRepoPrefix(repoPrefix),
		)
	}
	if input.Cursor != "" && input.Before > 0 {
		return nil, huma.Error400BadRequest("cursor and before are mutually exclusive")
	}
	position, enqueuedAt, cursorErr := s.decodeJobListCursor(input.Cursor)
	if cursorErr != nil {
		return nil, huma.Error400BadRequest(cursorErr.Error())
	}
	if position != nil {
		listOpts = append(
			listOpts, storage.WithBeforePosition(enqueuedAt, position.JobID),
		)
	} else if input.Before > 0 {
		listOpts = append(
			listOpts, storage.WithBeforeCursor(input.Before),
		)
	}

	// Panels: panel_run returns a full run (members + synthesis) for
	// expansion. Otherwise the listing is parent-only — member rows are
	// excluded so list/wait/fix-discovery resolve to the synthesis parent,
	// never an individual reviewer — the same caller-driven exclusion
	// mechanism that fix jobs use via exclude_job_type.
	if input.PanelRun != uuid.Nil() {
		listOpts = append(listOpts, storage.WithPanelRun(input.PanelRun))
	} else {
		listOpts = append(
			listOpts,
			storage.WithExcludePanelRole(storage.PanelRoleMember),
		)
	}

	jobs, err := s.db.ListJobs(
		input.Status, repo, fetchLimit, offset, listOpts...,
	)
	if err != nil {
		return nil, huma.Error500InternalServerError(
			fmt.Sprintf("list jobs: %v", err),
		)
	}

	hasMore := false
	if limit > 0 && len(jobs) > limit {
		hasMore = true
		jobs = jobs[:limit]
	}
	var nextCursor *string
	if hasMore && len(jobs) > 0 {
		encoded, cursorErr := s.encodeJobListCursor(jobs[len(jobs)-1])
		if cursorErr != nil {
			return nil, huma.Error500InternalServerError(
				fmt.Sprintf("encode jobs cursor: %v", cursorErr),
			)
		}
		nextCursor = &encoded
	}

	if input.OmitPrompt == "true" {
		stripJobPrompts(jobs)
	}

	attachPanelSummaries(s.db, jobs)

	// Stats describe the aggregate population for the active scope and ignore
	// pagination. The closed-state filter intentionally applies only to the
	// listing: queue consumers use Stats to report both open and closed totals.
	// FilteredStats below carries the exact closed-filtered counts for browser
	// views that need counts matching the visible rows.
	var statsOpts []storage.ListJobsOption
	if input.GitRef != "" {
		statsOpts = append(statsOpts, storage.WithGitRef(input.GitRef))
	}
	if input.BranchEmpty == "true" {
		statsOpts = append(statsOpts, storage.WithEmptyBranch())
	} else if input.Branch != "" {
		if input.BranchIncludeEmpty == "true" {
			statsOpts = append(
				statsOpts,
				storage.WithBranchOrEmpty(input.Branch),
			)
		} else {
			statsOpts = append(
				statsOpts,
				storage.WithBranch(input.Branch),
			)
		}
	}
	if input.JobType != "" {
		statsOpts = append(
			statsOpts, storage.WithJobType(input.JobType),
		)
	}
	if input.ExcludeJobType != "" {
		statsOpts = append(
			statsOpts,
			storage.WithExcludeJobType(input.ExcludeJobType),
		)
	}
	if input.HideClassifyJobs == "true" {
		statsOpts = append(statsOpts, storage.WithHideClassifyJobs())
	}
	if len(repoPathsFilter) > 0 {
		statsOpts = append(statsOpts, storage.WithRepoPaths(repoPathsFilter))
	}
	if repoPrefix != "" && len(repoPaths) == 0 {
		statsOpts = append(
			statsOpts, storage.WithRepoPrefix(repoPrefix),
		)
	}
	// Stats describe the same parent-only population as the listing, so member
	// rows are excluded here too — a panel run counts as its synthesis parent,
	// not N+1 reviewers — keeping the queue header consistent with the rows.
	statsOpts = append(
		statsOpts,
		storage.WithExcludePanelRole(storage.PanelRoleMember),
	)
	stats, statsErr := s.db.CountJobStats(input.Status, repo, statsOpts...)
	if statsErr != nil {
		log.Printf(
			"Warning: failed to count job stats: %v", statsErr,
		)
	}
	var filteredStats *storage.JobStats
	if input.Closed == "true" || input.Closed == "false" {
		filteredOpts := append(
			append([]storage.ListJobsOption(nil), statsOpts...),
			storage.WithClosed(input.Closed == "true"),
		)
		filtered, filteredErr := s.db.CountJobStats(
			input.Status, repo, filteredOpts...,
		)
		if filteredErr != nil {
			log.Printf(
				"Warning: failed to count closed-filtered job stats: %v",
				filteredErr,
			)
		} else {
			filteredStats = &filtered
		}
	}

	resp := &ListJobsOutput{}
	resp.Body.Jobs = jobs
	resp.Body.HasMore = hasMore
	resp.Body.NextCursor = nextCursor
	resp.Body.Stats = &stats
	resp.Body.FilteredStats = filteredStats
	return resp, nil
}

func (s *Server) humaGetReview(
	ctx context.Context, input *GetReviewInput,
) (*GetReviewOutput, error) {
	var review *storage.Review
	var err error

	if input.JobID >= 0 {
		review, err = s.db.GetReviewByJobID(input.JobID)
	} else if input.SHA != "" {
		review, err = s.db.GetReviewByCommitSHA(input.SHA)
	} else {
		return nil, huma.Error400BadRequest(
			"job_id or sha parameter required",
		)
	}

	if err != nil {
		return nil, huma.Error404NotFound("review not found")
	}

	return &GetReviewOutput{Body: review}, nil
}

const (
	exportReviewsDefaultLimit = 500
	exportReviewsMaxLimit     = 5000
)

func (s *Server) humaExportReviews(
	ctx context.Context, input *ExportReviewsInput,
) (*ExportReviewsOutput, error) {
	format := input.Format
	if format == "" {
		format = "json"
	}
	if format != "json" {
		return nil, huma.Error400BadRequest("unsupported export format")
	}

	profile := input.Profile
	if profile == "" {
		profile = string(storage.ExportProfileContent)
	}
	if profile != string(storage.ExportProfileContent) &&
		profile != string(storage.ExportProfileMetadata) {
		return nil, huma.Error400BadRequest("unsupported export profile")
	}
	if input.Cursor != "" && input.Since != "" {
		return nil, huma.Error400BadRequest("cursor cannot be used with since")
	}

	since, sinceOut, err := parseExportTimeBound(input.Since, false)
	if err != nil {
		return nil, huma.Error400BadRequest("invalid since")
	}
	until, untilOut, err := parseExportTimeBound(input.Until, true)
	if err != nil {
		return nil, huma.Error400BadRequest("invalid until")
	}

	limit := input.Limit
	if limit <= 0 {
		limit = exportReviewsDefaultLimit
	}
	if limit > exportReviewsMaxLimit {
		limit = exportReviewsMaxLimit
	}

	page, err := s.db.ExportReviews(storage.ExportReviewsOptions{
		Profile:    storage.ExportProfile(profile),
		Since:      since,
		Until:      until,
		Cursor:     input.Cursor,
		ClosedOnly: input.ClosedOnly,
		Repo:       input.Repo,
		Project:    input.Project,
		Limit:      limit,
	})
	if err != nil {
		if errors.Is(err, storage.ErrExportCursorDatabaseMismatch) {
			return nil, huma.Error409Conflict(err.Error())
		}
		return nil, huma.Error400BadRequest(err.Error())
	}
	databaseID, err := s.db.GetDatabaseID()
	if err != nil {
		return nil, fmt.Errorf("get database ID: %w", err)
	}

	var nextCursor *string
	if page.NextCursor != nil {
		nextCursor = page.NextCursor
	}
	resp := &ExportReviewsOutput{}
	resp.Body = ExportReviewsDocument{
		SchemaVersion: 1,
		Tool:          "roborev",
		ToolVersion:   version.Version,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		DatabaseID:    databaseID,
		Profile:       profile,
		Window: ExportReviewsWindow{
			Field: "completed_at",
			Since: sinceOut,
			Until: untilOut,
		},
		Truncated:  page.Truncated,
		NextCursor: nextCursor,
		Reviews:    page.Reviews,
	}
	return resp, nil
}

func (s *Server) humaExportCIMetrics(
	ctx context.Context, input *ExportCIMetricsInput,
) (*ExportCIMetricsOutput, error) {
	if input.Format != "" && input.Format != "json" {
		return nil, huma.Error400BadRequest("unsupported export format")
	}
	if input.Cursor != "" && input.Since != "" {
		return nil, huma.Error400BadRequest("cursor cannot be used with since")
	}
	since, sinceOut, err := parseExportTimeBound(input.Since, false)
	if err != nil {
		return nil, huma.Error400BadRequest("invalid since")
	}
	until, untilOut, err := parseExportTimeBound(input.Until, true)
	if err != nil {
		return nil, huma.Error400BadRequest("invalid until")
	}

	page, err := s.db.ExportCIMetrics(storage.ExportCIMetricsOptions{
		Since:  since,
		Until:  until,
		Cursor: input.Cursor,
		Limit:  input.Limit,
		Legacy: input.Legacy,
	})
	if err != nil {
		if errors.Is(err, storage.ErrExportCursorDatabaseMismatch) {
			return nil, huma.Error409Conflict(err.Error())
		}
		return nil, huma.Error400BadRequest(err.Error())
	}
	databaseID, err := s.db.GetDatabaseID()
	if err != nil {
		return nil, fmt.Errorf("get database ID: %w", err)
	}

	resp := &ExportCIMetricsOutput{}
	resp.Body = ExportCIMetricsDocument{
		SchemaVersion: 1,
		Tool:          "roborev",
		ToolVersion:   version.Version,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		DatabaseID:    databaseID,
		Window: ExportReviewsWindow{
			Field: "posted_at",
			Since: sinceOut,
			Until: untilOut,
		},
		Truncated:  page.Truncated,
		NextCursor: page.NextCursor,
		Panels:     page.Panels,
	}
	return resp, nil
}

func (s *Server) humaExportCICosts(
	ctx context.Context, input *ExportCICostInput,
) (*ExportCICostOutput, error) {
	if input.Format != "" && input.Format != "json" {
		return nil, huma.Error400BadRequest("unsupported export format")
	}
	if input.Cursor != "" && (input.Since != "" || input.Until != "") {
		return nil, huma.Error400BadRequest("cursor cannot be used with since or until")
	}
	since, sinceOut, err := parseExportTimeBound(input.Since, false)
	if err != nil {
		return nil, huma.Error400BadRequest("invalid since")
	}
	until, untilOut, err := parseExportTimeBound(input.Until, true)
	if err != nil {
		return nil, huma.Error400BadRequest("invalid until")
	}

	page, err := s.db.ExportCICosts(storage.ExportCICostOptions{
		Since: since, Until: until, Cursor: input.Cursor,
		Limit: input.Limit, Legacy: input.Legacy,
	})
	if err != nil {
		if errors.Is(err, storage.ErrExportCursorDatabaseMismatch) {
			return nil, huma.Error409Conflict(err.Error())
		}
		return nil, huma.Error400BadRequest(err.Error())
	}
	if sinceOut == nil && !page.EffectiveSince.IsZero() {
		value := page.EffectiveSince.UTC().Format(time.RFC3339)
		sinceOut = &value
	}
	if untilOut == nil && !page.EffectiveUntil.IsZero() {
		value := page.EffectiveUntil.UTC().Format(time.RFC3339)
		untilOut = &value
	}
	databaseID, err := s.db.GetDatabaseID()
	if err != nil {
		return nil, fmt.Errorf("get database ID: %w", err)
	}

	resp := &ExportCICostOutput{}
	resp.Body = ExportCICostDocument{
		SchemaVersion: 1,
		Tool:          "roborev",
		ToolVersion:   version.Version,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		DatabaseID:    databaseID,
		Legacy:        input.Legacy,
		Window: ExportReviewsWindow{
			Field: "finished_at",
			Since: sinceOut,
			Until: untilOut,
		},
		Truncated:  page.Truncated,
		NextCursor: page.NextCursor,
		Jobs:       page.Jobs,
	}
	return resp, nil
}

func parseExportTimeBound(raw string, upper bool) (time.Time, *string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil, nil
	}
	if t, err := time.Parse("2006-01-02", raw); err == nil {
		if upper {
			t = t.Add(24 * time.Hour)
		}
		out := t.UTC().Format(time.RFC3339)
		return t, &out, nil
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, nil, err
	}
	t = t.UTC()
	out := t.Format(time.RFC3339)
	return t, &out, nil
}

func (s *Server) humaListComments(
	ctx context.Context, input *ListCommentsInput,
) (*ListCommentsOutput, error) {
	var responses []storage.Response
	var err error

	if input.JobID >= 0 {
		responses, err = s.db.GetCommentsForJob(input.JobID)
		if err != nil {
			return nil, huma.Error500InternalServerError(
				fmt.Sprintf("get responses: %v", err),
			)
		}
	} else if input.CommitID >= 0 {
		responses, err = s.db.GetCommentsForCommit(input.CommitID)
		if err != nil {
			return nil, huma.Error500InternalServerError(
				fmt.Sprintf("get responses: %v", err),
			)
		}
	} else if input.SHA != "" {
		responses, err = s.db.GetCommentsForCommitSHA(input.SHA)
		if err != nil {
			return nil, huma.Error404NotFound("commit not found")
		}
	} else {
		return nil, huma.Error400BadRequest(
			"job_id, commit_id, or sha parameter required",
		)
	}

	resp := &ListCommentsOutput{}
	resp.Body.Responses = responses
	return resp, nil
}

func (s *Server) humaListRepos(
	ctx context.Context, input *ListReposInput,
) (*ListReposOutput, error) {
	prefix := input.Prefix
	if prefix != "" {
		prefix = filepath.ToSlash(filepath.Clean(prefix))
	}

	var repoOpts []storage.ListReposOption
	if prefix != "" {
		repoOpts = append(
			repoOpts, storage.WithRepoPathPrefix(prefix),
		)
	}
	if input.Branch != "" {
		repoOpts = append(
			repoOpts, storage.WithRepoBranch(input.Branch),
		)
	}

	repos, totalCount, err := s.db.ListReposWithReviewCounts(
		repoOpts...,
	)
	if err != nil {
		return nil, huma.Error500InternalServerError(
			fmt.Sprintf("list repos: %v", err),
		)
	}

	resp := &ListReposOutput{}
	resp.Body.Repos = repos
	resp.Body.TotalCount = totalCount
	return resp, nil
}

func (s *Server) humaResolveRepo(
	ctx context.Context, input *ResolveRepoInput,
) (*ResolveRepoOutput, error) {
	path := strings.TrimSpace(input.Path)
	if path == "" {
		return nil, huma.Error400BadRequest("path is required")
	}

	resolved, err := resolveTrackedRepo(ctx, s.db, path, input.Branch)
	if err != nil {
		return nil, huma.Error500InternalServerError(
			err.Error(),
		)
	}
	if !resolved.Tracked {
		return &ResolveRepoOutput{}, nil
	}

	resp := &ResolveRepoOutput{}
	resp.Body.Tracked = true
	resp.Body.Repo = &ResolvedRepo{
		RootPath: resolved.RootPath,
		Identity: resolved.Identity,
		Name:     resolved.Name,
	}
	if !resolved.SnoozedUntil.IsZero() {
		resp.Body.Repo.AgentHookSnoozedUntil = &resolved.SnoozedUntil
	}
	return resp, nil
}

func (s *Server) humaSetAgentHookSnooze(
	_ context.Context, input *AgentHookSnoozeInput,
) (*AgentHookSnoozeOutput, error) {
	req := input.Body
	if strings.TrimSpace(req.RepoPath) == "" ||
		strings.TrimSpace(req.WorktreePath) == "" {
		return nil, huma.Error400BadRequest(
			"repo_path and worktree_path are required",
		)
	}

	resp := &AgentHookSnoozeOutput{}
	if !req.Enabled {
		err := s.db.ClearAgentHookSnooze(
			req.RepoPath, req.WorktreePath, req.Branch,
		)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, huma.Error404NotFound("repository is not tracked")
		}
		if err != nil {
			return nil, huma.Error500InternalServerError(
				fmt.Sprintf("clear agent hook snooze: %v", err),
			)
		}
		return resp, nil
	}

	if !req.SnoozedUntil.After(time.Now()) {
		return nil, huma.Error400BadRequest(
			"snoozed_until must be in the future",
		)
	}
	snooze, err := s.db.SetAgentHookSnooze(
		req.RepoPath, req.WorktreePath, req.Branch, req.SnoozedUntil,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, huma.Error404NotFound("repository is not tracked")
	}
	if err != nil {
		return nil, huma.Error500InternalServerError(
			fmt.Sprintf("set agent hook snooze: %v", err),
		)
	}
	resp.Body.Snoozed = true
	resp.Body.SnoozedUntil = &snooze.SnoozedUntil
	return resp, nil
}

func (s *Server) humaListBranches(
	ctx context.Context, input *ListBranchesInput,
) (*ListBranchesOutput, error) {
	// Filter out empty strings to treat ?repo= as no filter
	var repoPaths []string
	for _, p := range input.Repo {
		if p != "" {
			repoPaths = append(
				repoPaths,
				filepath.ToSlash(filepath.Clean(p)),
			)
		}
	}

	result, err := s.db.ListBranchesWithCounts(repoPaths)
	if err != nil {
		return nil, huma.Error500InternalServerError(
			fmt.Sprintf("list branches: %v", err),
		)
	}

	resp := &ListBranchesOutput{}
	resp.Body.Branches = result.Branches
	resp.Body.TotalCount = result.TotalCount
	resp.Body.NullsRemaining = result.NullsRemaining
	return resp, nil
}

func (s *Server) humaGetStatus(
	ctx context.Context, input *GetStatusInput,
) (*GetStatusOutput, error) {
	queued, running, done, failed, canceled,
		applied, rebased, skipped, err := s.db.GetJobCounts()
	if err != nil {
		return nil, huma.Error500InternalServerError(
			fmt.Sprintf("get counts: %v", err),
		)
	}
	activeSnoozes, err := s.db.ListActiveAgentHookSnoozes(time.Now())
	if err != nil {
		return nil, huma.Error500InternalServerError(
			fmt.Sprintf("list active agent hook snoozes: %v", err),
		)
	}

	configReloadedAt := ""
	if t := s.configWatcher.LastReloadedAt(); !t.IsZero() {
		configReloadedAt = t.Format(time.RFC3339Nano)
	}
	configReloadCounter := s.configWatcher.ReloadCounter()

	queuePaused, err := s.db.IsQueuePaused()
	if err != nil {
		return nil, huma.Error500InternalServerError(
			fmt.Sprintf("get queue paused state: %v", err),
		)
	}

	s.endpointMu.Lock()
	ep := s.endpoint
	s.endpointMu.Unlock()

	resp := &GetStatusOutput{}
	resp.Body = storage.DaemonStatus{
		ActiveSnoozes:       activeSnoozes,
		Version:             version.Version,
		QueuedJobs:          queued,
		RunningJobs:         running,
		CompletedJobs:       done,
		FailedJobs:          failed,
		CanceledJobs:        canceled,
		AppliedJobs:         applied,
		RebasedJobs:         rebased,
		SkippedJobs:         skipped,
		AutoDesign:          s.autoDesignStatusForResponse(),
		ActiveWorkers:       s.workerPool.ActiveWorkers(),
		MaxWorkers:          s.workerPool.MaxWorkers(),
		QueuePaused:         queuePaused,
		Network:             ep.Network,
		Address:             ep.Address,
		Port:                ep.Port(),
		MachineID:           s.getMachineID(),
		ConfigReloadedAt:    configReloadedAt,
		ConfigReloadCounter: configReloadCounter,
		WebCapabilities:     []string{"review-projection-v1", "analytics-v1"},
	}
	updateDraining, updatePolicy, updateExpiresAt := s.updateDrainStatus()
	resp.Body.UpdateDraining = updateDraining
	resp.Body.UpdateDrainPolicy = updatePolicy
	if !updateExpiresAt.IsZero() {
		resp.Body.UpdateDrainExpiresAt = updateExpiresAt.Format(time.RFC3339)
	}
	return resp, nil
}

func (s *Server) humaPauseQueue(
	ctx context.Context, input *QueuePauseInput,
) (*QueuePauseOutput, error) {
	s.shutdownDrainMu.Lock()
	defer s.shutdownDrainMu.Unlock()
	if s.shutdownDraining {
		return nil, huma.Error409Conflict("daemon shutdown in progress")
	}
	if err := s.db.SetQueuePaused(true); err != nil {
		return nil, huma.Error500InternalServerError(
			fmt.Sprintf("pause queue: %v", err),
		)
	}
	resp := &QueuePauseOutput{}
	resp.Body.QueuePaused = true
	return resp, nil
}

func (s *Server) humaUnpauseQueue(
	ctx context.Context, input *QueuePauseInput,
) (*QueuePauseOutput, error) {
	s.shutdownDrainMu.Lock()
	defer s.shutdownDrainMu.Unlock()
	if s.shutdownDraining {
		return nil, huma.Error409Conflict("daemon shutdown in progress")
	}
	if err := s.db.SetQueuePaused(false); err != nil {
		return nil, huma.Error500InternalServerError(
			fmt.Sprintf("unpause queue: %v", err),
		)
	}
	resp := &QueuePauseOutput{}
	resp.Body.QueuePaused = false
	return resp, nil
}

func (s *Server) humaGetSummary(
	ctx context.Context, input *GetSummaryInput,
) (*GetSummaryOutput, error) {
	since := time.Now().Add(-7 * 24 * time.Hour)
	if input.Since != "" {
		d, err := parseDuration(input.Since)
		if err != nil {
			return nil, huma.Error400BadRequest(
				fmt.Sprintf("invalid since value: %s", input.Since),
			)
		}
		since = time.Now().Add(-d)
	}

	opts := storage.SummaryOptions{
		RepoPath: input.Repo,
		Branch:   input.Branch,
		Since:    since,
		AllRepos: input.All == "true",
	}

	summary, err := s.db.GetSummary(opts)
	if err != nil {
		return nil, huma.Error500InternalServerError(
			fmt.Sprintf("get summary: %v", err),
		)
	}

	return &GetSummaryOutput{Body: summary}, nil
}

// costOptionsFromInput maps request params to storage.CostOptions. An empty
// since means all-time (zero Time); a malformed since is an error.
func costOptionsFromInput(input *GetCostInput) (storage.CostOptions, error) {
	opts := storage.CostOptions{
		RepoPaths:   input.Repo,
		Branch:      input.Branch,
		BranchEmpty: input.BranchEmpty == "true",
	}
	if input.Since != "" {
		d, err := parseDuration(input.Since)
		if err != nil {
			return storage.CostOptions{}, err
		}
		opts.Since = time.Now().Add(-d)
	}
	return opts, nil
}

func (s *Server) humaGetCost(
	ctx context.Context, input *GetCostInput,
) (*GetCostOutput, error) {
	opts, err := costOptionsFromInput(input)
	if err != nil {
		return nil, huma.Error400BadRequest(
			fmt.Sprintf("invalid since value: %s", input.Since),
		)
	}

	cost, err := s.db.GetCostAggregate(opts)
	if err != nil {
		return nil, huma.Error500InternalServerError(
			fmt.Sprintf("get cost: %v", err),
		)
	}

	return &GetCostOutput{Body: cost}, nil
}

func (s *Server) humaCancelJob(
	ctx context.Context, input *CancelJobInput,
) (*CancelJobOutput, error) {
	if input.Body.JobID == 0 {
		return nil, huma.Error400BadRequest(
			"job_id is required",
		)
	}

	// Best-effort panel routing: load the target first so we can cascade a
	// synthesis-parent cancel to its members and release the synthesis when a
	// member is canceled directly. A load error leaves job nil; the cancel
	// below still returns the correct 404/500 for the target. A not-found here
	// is expected (the cancel reports it); log only a real lookup failure so a
	// transient DB error that downgrades a panel cancel leaves a trace.
	job, jobErr := s.db.GetJobByID(input.Body.JobID)
	if jobErr != nil && !errors.Is(jobErr, sql.ErrNoRows) {
		log.Printf("cancel job %d: panel routing lookup failed: %v",
			input.Body.JobID, jobErr)
	}
	if remoteBrowserPrincipal(ctx) && jobErr != nil {
		if errors.Is(jobErr, sql.ErrNoRows) {
			return nil, huma.Error404NotFound(
				"job not found or not cancellable",
			)
		}
		return nil, huma.Error500InternalServerError(
			fmt.Sprintf("load job: %v", jobErr),
		)
	}
	if jobErr == nil {
		if err := s.authorizeBrowserJobCancellation(ctx, job); err != nil {
			return nil, err
		}
	}
	if err := s.db.CancelJob(input.Body.JobID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, huma.Error404NotFound(
				"job not found or not cancellable",
			)
		}
		return nil, huma.Error500InternalServerError(
			fmt.Sprintf("cancel job: %v", err),
		)
	}
	s.workerPool.cancelJob(input.Body.JobID, true)

	s.retireCIPanelForCanceledSynthesis(job)

	// Cancel the synthesis parent BEFORE cascading to its members. A running
	// member that observes cancellation can release the synthesis gate; if that
	// raced ahead of the parent's own cancel, a worker could still claim and
	// complete the synthesis despite the user's cancel. Canceling the parent
	// first makes the later MaybeReleasePanelSynthesis a no-op on an
	// already-terminal row.
	canceledMembers := s.cascadeCancelPanelMembers(job, true)
	s.releaseSynthesisIfCanceledMember(job)

	if job == nil {
		job, _ = s.db.GetJobByID(input.Body.JobID)
	}
	s.broadcaster.Broadcast(eventForMutationPrincipal(
		ctx, eventForJob("review.canceled", job, input.Body.JobID),
	))
	for i := range canceledMembers {
		member := &canceledMembers[i]
		s.broadcaster.Broadcast(eventForMutationPrincipal(
			ctx, eventForJob("review.canceled", member, member.ID),
		))
	}

	resp := &CancelJobOutput{}
	resp.Body.Success = true
	return resp, nil
}

func (s *Server) humaRerunJob(
	ctx context.Context, input *RerunJobInput,
) (*RerunJobOutput, error) {
	if input.Body.JobID == 0 {
		return nil, huma.Error400BadRequest(
			"job_id is required",
		)
	}
	job, err := s.db.GetJobByID(input.Body.JobID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, huma.Error404NotFound(
				"job not found or not rerunnable",
			)
		}
		return nil, huma.Error500InternalServerError(
			fmt.Sprintf("load job: %v", err),
		)
	}
	if remoteBrowserPrincipal(ctx) {
		return nil, huma.Error403Forbidden(
			"remote browser sessions cannot rerun jobs",
		)
	}
	requestID := uuid.Nil()
	if input.Body.RequestID != nil {
		requestID = *input.Body.RequestID
		result, found, err := s.db.GetRerunRequest(requestID, input.Body.JobID)
		if err != nil {
			return nil, huma.Error500InternalServerError(
				fmt.Sprintf("load rerun request: %v", err),
			)
		}
		if found {
			resp := &RerunJobOutput{}
			resp.Body.Success = true
			resp.Body.JobID = result.JobID
			resp.Body.RequestID = requestID
			resp.Body.RunUUID = result.PanelRunUUID
			return resp, nil
		}
	}
	if job.Status == storage.JobStatusCanceled && job.WorkerID != "" {
		return nil, huma.Error409Conflict("canceled job is still stopping")
	}
	selectedAgent := strings.TrimSpace(input.Body.Agent)

	// Rerunning a panel synthesis parent spawns a brand-new panel run (fresh
	// members + a re-blocked synthesis) rather than re-queueing the parent in
	// place, so the new run gets fresh member reviews to synthesize.
	// rerunPanelRun enforces the terminal-state guard.
	if job.IsSynthesisJob() {
		if selectedAgent != "" {
			return nil, huma.Error400BadRequest(
				"panel synthesis jobs cannot change agents on rerun",
			)
		}
		return s.rerunPanelRun(job, requestID)
	}
	if job.PanelRole == storage.PanelRoleMember {
		return nil, huma.Error400BadRequest(
			"panel members cannot be rerun directly; rerun the panel synthesis job",
		)
	}

	jobUUID := uuid.Nil()
	if job.UUID != nil {
		jobUUID = *job.UUID
	}
	assignment, err := s.db.GetExperimentAssignmentInputForJobUUID(jobUUID)
	if err != nil {
		return nil, huma.Error500InternalServerError(
			fmt.Sprintf("load experiment assignment: %v", err),
		)
	}
	rerunOpts, err := resolveRerunOpts(
		job, s.configWatcher.Config(), assignment, selectedAgent,
	)
	if err != nil {
		return nil, huma.Error400BadRequest(err.Error())
	}

	resultJobID, replayed, err := s.db.ReenqueueJobWithRequest(
		input.Body.JobID, rerunOpts, requestID,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, huma.Error404NotFound(
				"job not found or not rerunnable",
			)
		}
		return nil, huma.Error500InternalServerError(
			fmt.Sprintf("rerun job: %v", err),
		)
	}
	if !replayed {
		if selectedAgent != "" {
			job.Agent = rerunOpts.Agent
		}
		s.broadcastRerunEnqueued(resultJobID, job.UUID, job)
	}

	resp := &RerunJobOutput{}
	resp.Body.Success = true
	resp.Body.JobID = resultJobID
	resp.Body.RequestID = requestID
	return resp, nil
}

func (s *Server) broadcastRerunEnqueued(
	jobID int64, jobUUID *uuid.UUID, source *storage.ReviewJob,
) {
	s.broadcaster.Broadcast(Event{
		Type:     "job.enqueued",
		TS:       time.Now(),
		JobID:    jobID,
		JobUUID:  jobUUID,
		Repo:     source.RepoPath,
		RepoName: source.RepoName,
		SHA:      source.GitRef,
		Agent:    source.Agent,
	})
}

func (s *Server) humaCloseReview(
	ctx context.Context, input *CloseReviewInput,
) (*CloseReviewOutput, error) {
	if input.Body.JobID == 0 {
		return nil, huma.Error400BadRequest(
			"job_id is required",
		)
	}

	err := s.db.MarkReviewClosedByJobID(
		input.Body.JobID, input.Body.Closed,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, huma.Error404NotFound(
				"review not found for job",
			)
		}
		return nil, huma.Error500InternalServerError(
			fmt.Sprintf("mark closed: %v", err),
		)
	}

	eventType := "review.closed"
	if !input.Body.Closed {
		eventType = "review.reopened"
	}
	evt := Event{
		Type:  eventType,
		TS:    time.Now(),
		JobID: input.Body.JobID,
	}
	if job, err := s.db.GetJobByID(input.Body.JobID); err == nil {
		evt.Repo = job.RepoPath
		evt.RepoName = job.RepoName
		evt.SHA = job.GitRef
		evt.Branch = job.HookBranch()
		evt.Agent = job.Agent
	}
	s.broadcaster.Broadcast(eventForMutationPrincipal(ctx, evt))

	resp := &CloseReviewOutput{}
	resp.Body.Success = true
	return resp, nil
}

func (s *Server) humaAddComment(
	ctx context.Context, input *AddCommentInput,
) (*AddCommentOutput, error) {
	if input.Body.Commenter == "" || input.Body.Comment == "" {
		return nil, huma.Error400BadRequest(
			"commenter and comment are required",
		)
	}

	if input.Body.JobID == 0 && input.Body.SHA == "" {
		return nil, huma.Error400BadRequest(
			"job_id or sha is required",
		)
	}

	var resp *storage.Response
	var commentEvent Event
	var err error
	source := storage.ResponseSourceLocal
	if principal, found := BrowserPrincipalFromContext(ctx); found && !principal.Local {
		source = storage.ResponseSourceRemoteBrowser
	}

	if input.Body.JobID != 0 {
		resp, err = s.db.AddCommentToJobWithSource(
			input.Body.JobID,
			input.Body.Commenter,
			input.Body.Comment,
			source,
		)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, huma.Error404NotFound(
					"job not found",
				)
			}
			return nil, huma.Error500InternalServerError(
				fmt.Sprintf("add comment: %v", err),
			)
		}
		job, jobErr := s.db.GetJobByID(input.Body.JobID)
		if jobErr != nil {
			log.Printf("comment on job %d: load event metadata: %v", input.Body.JobID, jobErr)
		}
		commentEvent = eventForJob("review.commented", job, input.Body.JobID)
	} else {
		commit, commitErr := s.db.GetCommitBySHA(input.Body.SHA)
		if commitErr != nil {
			return nil, huma.Error404NotFound(
				"commit not found",
			)
		}

		resp, err = s.db.AddCommentWithSource(
			commit.ID,
			input.Body.Commenter,
			input.Body.Comment,
			source,
		)
		if err != nil {
			return nil, huma.Error500InternalServerError(
				fmt.Sprintf("add comment: %v", err),
			)
		}
		commentEvent = Event{
			Type: "review.commented",
			TS:   time.Now(),
			SHA:  commit.SHA,
		}
		repo, repoErr := s.db.GetRepoByID(commit.RepoID)
		if repoErr != nil {
			log.Printf("comment on commit %s: load event metadata: %v", commit.SHA, repoErr)
		} else {
			commentEvent.Repo = repo.RootPath
			commentEvent.RepoName = repo.Name
		}
	}
	s.broadcaster.Broadcast(eventForMutationPrincipal(ctx, commentEvent))

	return &AddCommentOutput{Body: resp}, nil
}

func (s *Server) humaBackfillTokens(
	_ context.Context, input *BackfillTokensInput,
) (*BackfillTokensOutput, error) {
	if len(input.Body.Sessions) == 0 {
		return nil, huma.Error400BadRequest("sessions are required")
	}

	sessions := make([]backfill.SessionUsage, 0, len(input.Body.Sessions))
	for _, payload := range input.Body.Sessions {
		payload.SessionID = strings.TrimSpace(payload.SessionID)
		if payload.SessionID == "" {
			return nil, huma.Error400BadRequest("session_id is required")
		}
		usage, err := tokens.UsageFromSessionPayload(payload)
		if err != nil {
			return nil, huma.Error400BadRequest(err.Error())
		}
		sessions = append(sessions, backfill.SessionUsage{
			SessionID: payload.SessionID,
			Usage:     usage,
		})
	}

	summary, err := backfill.ApplyTokenUsage(
		s.db, sessions, input.Body.DryRun,
	)
	if err != nil {
		return nil, huma.Error500InternalServerError(
			fmt.Sprintf("backfill tokens: %v", err),
		)
	}
	return &BackfillTokensOutput{Body: summary}, nil
}

func (s *Server) humaEnqueue(
	ctx context.Context, input *EnqueueInput,
) (*RawJSONOutput, error) {
	req := input.Body
	gitRef := req.GitRef
	if gitRef == "" {
		gitRef = req.CommitSHA
	}

	if req.RepoPath == "" || gitRef == "" {
		return rawJSONOutput(
			http.StatusBadRequest,
			ErrorResponse{Error: "repo_path and git_ref (or commit_sha) are required"},
		)
	}

	if req.ReviewType == "" {
		req.ReviewType = config.ReviewTypeDefault
	}

	metadata := git.OpenEnqueueMetadataReader(ctx, req.RepoPath)
	checkoutRoot, err := metadata.Root()
	if err != nil {
		return rawJSONOutput(
			http.StatusBadRequest,
			ErrorResponse{Error: fmt.Sprintf("not a git repository: %v", err)},
		)
	}

	repoRoot, err := gitrepo.MainRoot(ctx, req.RepoPath)
	if err != nil {
		return rawJSONOutput(
			http.StatusBadRequest,
			ErrorResponse{Error: fmt.Sprintf("not a git repository: %v", err)},
		)
	}

	var worktreePath string
	if filepath.Clean(checkoutRoot) != filepath.Clean(repoRoot) {
		worktreePath = filepath.Clean(checkoutRoot)
	}
	cfg := s.configWatcher.Config()
	repoCfg, err := config.LoadRepoConfig(checkoutRoot)
	if err != nil {
		return rawJSONOutput(
			http.StatusBadRequest,
			ErrorResponse{Error: fmt.Sprintf("resolve workflow config: %v", err)},
		)
	}
	canonical, err := config.ValidateReviewTypesFromConfig(
		[]string{req.ReviewType}, repoCfg, cfg,
	)
	if err != nil {
		return rawJSONOutput(
			http.StatusBadRequest,
			ErrorResponse{Error: err.Error()},
		)
	}
	req.ReviewType = canonical[0]

	currentBranch := metadata.CurrentBranch()
	// A post-commit request's explicit branch names the branch being
	// reviewed, which can differ from the checkout's branch (for example a
	// pre-push flush of another branch). Exclusion policy must follow the
	// reviewed branch there, or a skip decided by the wrong branch silently
	// drops the review. Manual reviews keep the checkout-branch check:
	// excluded_branches applies to automatic reviews only.
	branchToCheck := currentBranch
	if req.Source == "post_commit" && req.Branch != "" {
		branchToCheck = req.Branch
	} else if req.JobType == storage.JobTypeInsights {
		if req.Branch != "" {
			branchToCheck = req.Branch
		} else {
			branchToCheck = ""
		}
	}
	if branchToCheck != "" &&
		isBranchExcluded(checkoutRoot, repoCfg, branchToCheck, req.Source) {
		return rawJSONOutput(http.StatusOK, EnqueueSkippedResponse{
			Skipped: true,
			Reason: fmt.Sprintf(
				"branch %q is excluded from reviews", branchToCheck,
			),
		})
	}

	if req.Branch == "" && req.JobType != storage.JobTypeInsights {
		req.Branch = currentBranch
	}

	repoIdentity := config.ResolveRepoIdentity(repoRoot, nil)
	repo, err := s.db.GetOrCreateRepo(repoRoot, repoIdentity)
	if err != nil {
		if s.errorLog != nil {
			s.errorLog.LogError(
				"server", fmt.Sprintf("get repo: %v", err), 0,
			)
		}
		return rawJSONOutput(
			http.StatusInternalServerError,
			ErrorResponse{Error: fmt.Sprintf("get repo: %v", err)},
		)
	}

	workflow := workflowForJob(req.JobType, req.ReviewType)
	resolutionPath := repoRoot
	if worktreePath != "" {
		resolutionPath = worktreePath
	}

	var normalizedMinSev string
	if strings.TrimSpace(req.MinSeverity) != "" {
		normalizedMinSev, err = config.NormalizeMinSeverity(
			req.MinSeverity,
		)
		if err != nil {
			return rawJSONOutput(
				http.StatusBadRequest,
				ErrorResponse{Error: err.Error()},
			)
		}
	}

	requestedModel := strings.TrimSpace(req.Model)
	requestedProvider := strings.TrimSpace(req.Provider)

	repoCfg, rawRepoCfg, err := config.LoadRepoConfigWithRaw(resolutionPath)
	if err != nil {
		return rawJSONOutput(
			http.StatusBadRequest,
			ErrorResponse{Error: fmt.Sprintf("resolve workflow config: %v", err)},
		)
	}
	// The descriptor is agent-independent, so freeze it before resolving the
	// single-review agent. Selecting a panel ahead of the single-agent
	// availability gate means a panel run is never rejected because the
	// unrelated default/requested single-review agent is unavailable.
	descriptor, early := s.buildTargetDescriptor(ctx, freezeInputs{
		repo:              repo,
		req:               req,
		gitRef:            gitRef,
		checkoutRoot:      checkoutRoot,
		repoRoot:          repoRoot,
		metadata:          metadata,
		worktreePath:      worktreePath,
		normalizedMinSev:  normalizedMinSev,
		requestedModel:    requestedModel,
		requestedProvider: requestedProvider,
	})
	if early != nil {
		return early, nil
	}

	// Detached-HEAD attribution: when the client sent no branch and HEAD has
	// no symbolic ref, infer the branch from the frozen target SHA so the
	// job lands in branch-scoped views. sessionSHA is the SHA the freeze
	// already resolved (single commit, range end, or dirty HEAD); reusing it
	// keeps inference and storage on the same commit. Prompt jobs have no
	// sessionSHA and are never attributed.
	if req.Branch == "" && req.JobType != storage.JobTypeInsights &&
		descriptor.sessionSHA != "" {
		if inferred := git.InferBranchForCommit(
			ctx, checkoutRoot, descriptor.sessionSHA,
		); inferred != "" {
			if isBranchExcluded(
				checkoutRoot, repoCfg, inferred, req.Source,
			) {
				return rawJSONOutput(http.StatusOK, EnqueueSkippedResponse{
					Skipped: true,
					Reason: fmt.Sprintf(
						"branch %q is excluded from reviews", inferred,
					),
				})
			}
			log.Printf(
				"enqueue: inferred branch %q for detached-HEAD target %s",
				inferred, descriptor.sessionSHA,
			)
			descriptor.branch = inferred
			req.Branch = inferred
		}
	}

	var experiment *config.ExperimentAssignment
	if descriptor.prompt == "" {
		selection, selectErr := config.SelectReviewExperiment(config.ExperimentSelectionInput{
			Workflow: config.ExperimentWorkflowReview,
			Subject: config.ExperimentSubject{
				Repository: repo.Identity,
				Branch:     req.Branch,
			},
			Global:  cfg,
			Repo:    repoCfg,
			RawRepo: rawRepoCfg,
		})
		if selectErr != nil {
			return rawJSONOutput(http.StatusBadRequest,
				ErrorResponse{Error: fmt.Sprintf("select review experiment: %v", selectErr)})
		}
		repoCfg = selection.RepoConfig
		if selection.RawRepoConfig != nil {
			rawRepoCfg = selection.RawRepoConfig
		}
		experiment = selection.Assignment
		if experiment != nil {
			descriptor.minSeverity, selectErr = config.ResolveReviewMinSeverityFromConfig(
				normalizedMinSev, repoCfg, cfg,
			)
			if selectErr != nil {
				return rawJSONOutput(http.StatusBadRequest,
					ErrorResponse{Error: fmt.Sprintf("resolve review severity: %v", selectErr)})
			}
		}
	}

	var reasoning string
	if workflow == "fix" {
		reasoning, err = config.ResolveFixReasoningFromConfig(req.Reasoning, repoCfg, cfg)
	} else {
		reasoning, err = config.ResolveReviewReasoningForTypeFromConfig(
			req.Reasoning, repoCfg, cfg, req.ReviewType,
		)
	}
	if err != nil {
		return rawJSONOutput(http.StatusBadRequest, ErrorResponse{Error: err.Error()})
	}

	merged := config.MergeReviewConfigFromConfig(repoCfg, cfg)
	panelName := selectPanelForTarget(descriptor, req, merged)
	if panelName != "" {
		return s.enqueuePanelRun(ctx, panelRunInputs{
			descriptor:     descriptor,
			req:            req,
			panelName:      panelName,
			gitRef:         gitRef,
			resolutionPath: resolutionPath,
			cfg:            cfg,
			repoCfg:        repoCfg,
			rawRepoCfg:     rawRepoCfg,
			experiment:     experiment,
			repo:           repo,
		})
	}

	return s.enqueueSingleAgent(ctx, singleAgentInputs{
		descriptor:     descriptor,
		req:            req,
		repo:           repo,
		gitRef:         gitRef,
		checkoutRoot:   checkoutRoot,
		worktreePath:   worktreePath,
		resolutionPath: resolutionPath,
		cfg:            cfg,
		repoCfg:        repoCfg,
		rawRepoCfg:     rawRepoCfg,
		experiment:     experiment,
		workflow:       workflow,
		reasoning:      reasoning,
		requestedModel: requestedModel,
	})
}

func isBranchExcluded(
	repoPath string, repoCfg *config.RepoConfig, branch, source string,
) bool {
	if config.IsBranchExcluded(repoPath, branch) {
		return true
	}
	if source != storage.JobSourcePostCommit || repoCfg == nil ||
		len(repoCfg.ExcludedBranchPatterns) == 0 {
		return false
	}
	return matchBranch(repoCfg.ExcludedBranchPatterns, branch)
}

// singleAgentInputs groups the inputs threaded from humaEnqueue into the
// single-agent enqueue path. It keeps enqueueSingleAgent within the
// positional-param limit.
type singleAgentInputs struct {
	descriptor     targetDescriptor
	req            EnqueueRequest
	repo           *storage.Repo
	gitRef         string
	checkoutRoot   string
	worktreePath   string
	resolutionPath string
	cfg            *config.Config
	repoCfg        *config.RepoConfig
	rawRepoCfg     map[string]any
	experiment     *config.ExperimentAssignment
	workflow       string
	reasoning      string
	requestedModel string
}

type resolvedSingleAgent struct {
	Agent       string
	Model       string
	BackupAgent string
	BackupModel string
}

// resolveSingleAgent resolves the single-review agent for the no-panel path:
// it applies the workflow config plus the availability gate (with failover
// backup) and returns the chosen agent name and effective model. A non-nil
// early response is a hard return (400 for an unknown agent, 503 when none is
// available, 400 when the workflow config cannot be resolved).
func (s *Server) resolveSingleAgent(
	in singleAgentInputs,
) (resolvedSingleAgent, *RawJSONOutput) {
	resolution, err := agent.ResolveWorkflowConfigFromConfig(
		in.req.Agent, in.repoCfg, in.cfg, in.workflow, in.reasoning,
	)
	if err != nil {
		out, _ := rawJSONOutput(
			http.StatusBadRequest,
			ErrorResponse{Error: fmt.Sprintf("resolve workflow config: %v", err)},
		)
		return resolvedSingleAgent{}, out
	}
	agentName := resolution.PreferredAgent
	resolved, err := agent.GetPreferredOrBackupWithConfigFromConfig(
		in.repoCfg, agentName, in.cfg, resolution.BackupAgent,
	)
	if err != nil {
		if _, ok := errors.AsType[*agent.UnknownAgentError](err); ok {
			out, _ := rawJSONOutput(
				http.StatusBadRequest,
				ErrorResponse{Error: fmt.Sprintf("invalid agent: %v", err)},
			)
			return resolvedSingleAgent{}, out
		}
		out, _ := rawJSONOutput(
			http.StatusServiceUnavailable,
			ErrorResponse{Error: fmt.Sprintf("no review agent available: %v", err)},
		)
		return resolvedSingleAgent{}, out
	}
	if err := agent.ValidateStructuredReviewSelection(
		in.req.ReviewType, resolved,
	); err != nil {
		out, _ := rawJSONOutput(
			http.StatusBadRequest,
			ErrorResponse{Error: fmt.Sprintf("invalid agent: %v", err)},
		)
		return resolvedSingleAgent{}, out
	}
	agentName = resolved.Name()
	if err := agent.ValidateStructuredReviewBackup(
		in.req.ReviewType, resolution, agentName,
	); err != nil {
		out, _ := rawJSONOutput(
			http.StatusBadRequest,
			ErrorResponse{Error: fmt.Sprintf("invalid agent: %v", err)},
		)
		return resolvedSingleAgent{}, out
	}
	backupAgent, backupModel := backupExecutionForSelectedAgent(
		resolution, agentName, in.repoCfg, in.cfg,
	)
	return resolvedSingleAgent{
		Agent:       agentName,
		Model:       resolution.ModelForSelectedAgent(agentName, in.requestedModel),
		BackupAgent: backupAgent,
		BackupModel: backupModel,
	}, nil
}

// enqueueSingleAgent resolves the single-review agent, enqueues one job from the
// frozen descriptor, and runs the shared tail (auto-design dispatch, activity
// log, broadcast). This is the no-panel path; it must stay behaviorally
// identical to the pre-panel handler.
func (s *Server) enqueueSingleAgent(
	ctx context.Context, in singleAgentInputs,
) (*RawJSONOutput, error) {
	execution, early := s.resolveSingleAgent(in)
	if early != nil {
		return early, nil
	}

	o := in.descriptor.baseOpts()
	o.Agent = execution.Agent
	o.Model = execution.Model
	o.Provider = in.descriptor.requestedProvider
	o.Reasoning = in.reasoning
	o.ReviewType = in.req.ReviewType
	if in.experiment != nil {
		o.BackupAgent = execution.BackupAgent
		o.BackupModel = execution.BackupModel
		assignment, assignErr := storageAssignmentForExperiment(
			in.experiment, experimentPlanForJob(o),
		)
		if assignErr != nil {
			return rawJSONOutput(http.StatusInternalServerError,
				ErrorResponse{Error: fmt.Sprintf("fingerprint experiment plan: %v", assignErr)})
		}
		o.Experiment = assignment
	}
	o.SessionID, o.ResumeSourceJobUUID = findCompatibleReusableSession(
		ctx, s.db, in.checkoutRoot, in.descriptor.sessionSHA, o,
		in.repoCfg, in.rawRepoCfg, in.cfg, o.Experiment, 0,
	)

	var job *storage.ReviewJob
	var duplicate bool
	var err error
	if in.req.Source == storage.JobSourcePostCommit {
		job, duplicate, err = s.db.EnqueuePostCommitJob(o)
	} else {
		job, err = s.db.EnqueueJob(o)
	}
	if err != nil {
		return rawJSONOutput(
			http.StatusInternalServerError,
			ErrorResponse{Error: fmt.Sprintf("enqueue job: %v", err)},
		)
	}
	if duplicate {
		return postCommitDuplicateResponse()
	}
	if in.descriptor.commitSubject != "" {
		job.CommitSubject = in.descriptor.commitSubject
	}
	job.RepoPath = in.repo.RootPath
	job.RepoName = in.repo.Name

	s.finishSingleEnqueue(ctx, job, execution.Agent, in)
	return rawJSONOutput(http.StatusCreated, EnqueueCreatedResponse{
		ReviewJob: job,
		UUID:      *job.UUID,
	})
}

func postCommitDuplicateResponse() (*RawJSONOutput, error) {
	return rawJSONOutput(http.StatusOK, EnqueueSkippedResponse{
		Skipped: true,
		Reason:  "a matching post-commit job already exists",
	})
}

// finishSingleEnqueue runs the no-panel post-enqueue side effects: auto-design
// dispatch for default reviews, the activity-log entry, and the SSE broadcast.
func (s *Server) finishSingleEnqueue(
	ctx context.Context, job *storage.ReviewJob,
	agentName string, in singleAgentInputs,
) {
	if job.JobType == storage.JobTypeReview &&
		config.IsDefaultReviewType(in.req.ReviewType) {
		if err := s.maybeDispatchAutoDesign(ctx, job); err != nil {
			log.Printf("auto-design dispatch failed: %v", err)
		}
	}

	s.logEnqueueSideEffects(job, enqueueSideEffectInputs{
		repo:       in.repo,
		gitRef:     in.gitRef,
		agentName:  agentName,
		reviewType: in.req.ReviewType,
	})
}

type enqueueSideEffectInputs struct {
	repo       *storage.Repo
	gitRef     string
	agentName  string
	reviewType string
}

func (s *Server) logEnqueueSideEffects(
	job *storage.ReviewJob, in enqueueSideEffectInputs,
) {
	if s.activityLog != nil {
		s.activityLog.Log(
			"job.enqueued", "server",
			fmt.Sprintf("job %d enqueued for %s", job.ID, job.GitRef),
			map[string]string{
				"job_id":      strconv.FormatInt(job.ID, 10),
				"repo":        in.repo.Name,
				"ref":         in.gitRef,
				"agent":       in.agentName,
				"review_type": in.reviewType,
			},
		)
	}

	s.broadcaster.Broadcast(Event{
		Type:     "job.enqueued",
		TS:       time.Now(),
		JobID:    job.ID,
		Repo:     in.repo.RootPath,
		RepoName: in.repo.Name,
		SHA:      job.GitRef,
		Agent:    in.agentName,
	})
}

func (s *Server) humaBatchJobs(
	ctx context.Context, input *BatchJobsInput,
) (*BatchJobsOutput, error) {
	if len(input.Body.JobIDs) == 0 {
		return nil, huma.Error400BadRequest("job_ids is required")
	}

	const maxBatchSize = 100
	if len(input.Body.JobIDs) > maxBatchSize {
		return nil, huma.Error400BadRequest(
			fmt.Sprintf("too many job IDs (max %d)", maxBatchSize),
		)
	}

	results, err := s.db.GetJobsWithReviewsByIDs(input.Body.JobIDs)
	if err != nil {
		if s.errorLog != nil {
			s.errorLog.LogError(
				"server",
				fmt.Sprintf("batch fetch: %v", err),
				0,
			)
		}
		return nil, huma.Error500InternalServerError(
			fmt.Sprintf("batch fetch: %v", err),
		)
	}

	resp := &BatchJobsOutput{}
	resp.Body.Results = results
	return resp, nil
}

func (s *Server) humaRegisterRepo(
	ctx context.Context, input *RegisterRepoInput,
) (*RegisterRepoOutput, error) {
	if input.Body.RepoPath == "" {
		return nil, huma.Error400BadRequest("repo_path is required")
	}

	repoRoot, err := gitrepo.MainRoot(ctx, input.Body.RepoPath)
	if err != nil {
		return nil, huma.Error400BadRequest(
			fmt.Sprintf("not a git repository: %v", err),
		)
	}

	repoIdentity := config.ResolveRepoIdentity(repoRoot, nil)
	repo, err := s.db.GetOrCreateRepo(repoRoot, repoIdentity)
	if err != nil {
		if s.errorLog != nil {
			s.errorLog.LogError(
				"server",
				fmt.Sprintf("register repo: %v", err),
				0,
			)
		}
		return nil, huma.Error500InternalServerError(
			fmt.Sprintf("register repo: %v", err),
		)
	}

	return &RegisterRepoOutput{Body: repo}, nil
}

func (s *Server) humaUpdateJobBranch(
	ctx context.Context, input *UpdateJobBranchInput,
) (*UpdateJobBranchOutput, error) {
	if input.Body.JobID == 0 {
		return nil, huma.Error400BadRequest("job_id is required")
	}
	if input.Body.Branch == "" {
		return nil, huma.Error400BadRequest("branch is required")
	}

	rowsAffected, err := s.db.UpdateJobBranch(
		input.Body.JobID, input.Body.Branch,
	)
	if err != nil {
		return nil, huma.Error500InternalServerError(
			fmt.Sprintf("update branch: %v", err),
		)
	}

	resp := &UpdateJobBranchOutput{}
	resp.Body.Success = true
	resp.Body.Updated = rowsAffected > 0
	return resp, nil
}

func (s *Server) humaRemap(
	ctx context.Context, input *RemapInput,
) (*RemapOutput, error) {
	if len(input.Body.Mappings) > 1000 {
		return nil, huma.Error400BadRequest(
			fmt.Sprintf("too many mappings (%d, max %d)",
				len(input.Body.Mappings), 1000),
		)
	}
	if input.Body.RepoPath == "" {
		return nil, huma.Error400BadRequest("repo_path is required")
	}

	repoRoot, err := gitrepo.MainRoot(ctx, input.Body.RepoPath)
	if err != nil {
		return nil, huma.Error400BadRequest(
			fmt.Sprintf("not a git repository: %s", input.Body.RepoPath),
		)
	}

	repo, err := s.db.GetRepoByPath(repoRoot)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, huma.Error404NotFound(
			fmt.Sprintf("unknown repo: %s", repoRoot),
		)
	}
	if err != nil {
		return nil, huma.Error500InternalServerError(
			fmt.Sprintf("lookup repo: %v", err),
		)
	}

	timestamps := make([]time.Time, len(input.Body.Mappings))
	for i, m := range input.Body.Mappings {
		ts, err := time.Parse(time.RFC3339, m.Timestamp)
		if err != nil {
			return nil, huma.Error400BadRequest(
				fmt.Sprintf("invalid timestamp %q: %v", m.Timestamp, err),
			)
		}
		timestamps[i] = ts
	}

	var remapped, skipped int
	for i, m := range input.Body.Mappings {
		n, err := s.db.RemapJob(
			repo.ID, m.OldSHA, m.NewSHA, m.PatchID,
			m.Author, m.Subject, timestamps[i],
		)
		if err != nil {
			skipped++
			continue
		}
		remapped += n
		if n == 0 {
			skipped++
		}
	}

	if remapped > 0 {
		// Repo-level batch event: it spans many jobs, so it carries no single
		// Branch. A branch-filtered hook therefore never fires for it.
		s.broadcaster.Broadcast(Event{
			Type: "review.remapped",
			TS:   time.Now(),
			Repo: repo.RootPath,
		})
	}

	return &RemapOutput{Body: RemapResult{
		Remapped: remapped,
		Skipped:  skipped,
	}}, nil
}

func (s *Server) humaFixJob(
	ctx context.Context, input *FixJobInput,
) (*RawJSONOutput, error) {
	req := input.Body
	if req.ParentJobID == 0 {
		return rawJSONOutput(
			http.StatusBadRequest,
			ErrorResponse{Error: "parent_job_id is required"},
		)
	}

	parentJob, err := s.db.GetJobByID(req.ParentJobID)
	if err != nil {
		return rawJSONOutput(
			http.StatusNotFound,
			ErrorResponse{Error: "parent job not found"},
		)
	}
	if parentJob.IsFixJob() {
		return rawJSONOutput(
			http.StatusBadRequest,
			ErrorResponse{Error: "parent job must be a review, not a fix job"},
		)
	}

	fixPrompt := ""
	if req.StaleJobID > 0 {
		staleJob, err := s.db.GetJobByID(req.StaleJobID)
		if err != nil {
			return rawJSONOutput(
				http.StatusNotFound,
				ErrorResponse{Error: "stale job not found"},
			)
		}
		if staleJob.JobType != storage.JobTypeFix {
			return rawJSONOutput(
				http.StatusBadRequest,
				ErrorResponse{Error: "stale job is not a fix job"},
			)
		}
		if staleJob.RepoID != parentJob.RepoID {
			return rawJSONOutput(
				http.StatusBadRequest,
				ErrorResponse{Error: "stale job belongs to a different repo"},
			)
		}
		if staleJob.ParentJobID == nil ||
			*staleJob.ParentJobID != req.ParentJobID {
			return rawJSONOutput(
				http.StatusBadRequest,
				ErrorResponse{Error: "stale job is not linked to the specified parent"},
			)
		}
		switch staleJob.Status {
		case storage.JobStatusDone, storage.JobStatusApplied, storage.JobStatusRebased:
		default:
			return rawJSONOutput(
				http.StatusBadRequest,
				ErrorResponse{Error: "stale job is not in a terminal state"},
			)
		}
		if staleJob.Patch == nil || *staleJob.Patch == "" {
			return rawJSONOutput(
				http.StatusBadRequest,
				ErrorResponse{Error: "stale job has no patch to rebase from"},
			)
		}
		fixPrompt = buildRebasePrompt(staleJob.Patch)
	}

	var fixMinSev string
	if fixPrompt == "" {
		if !parentJob.IsTaskJob() {
			effectivePath := parentJob.RepoPath
			if parentJob.WorktreePath != "" &&
				git.ValidateWorktreeForRepo(
					parentJob.WorktreePath, parentJob.RepoPath,
				) {
				effectivePath = parentJob.WorktreePath
			}
			cfg := s.configWatcher.Config()
			resolved, resolveErr := config.ResolveFixMinSeverity(
				"", effectivePath, cfg,
			)
			if resolveErr != nil {
				return rawJSONOutput(
					http.StatusBadRequest,
					ErrorResponse{Error: fmt.Sprintf(
						"resolve fix min-severity: %v", resolveErr,
					)},
				)
			}
			fixMinSev = resolved
		}

		review, err := s.db.GetReviewByJobID(req.ParentJobID)
		if err != nil || review == nil {
			return rawJSONOutput(
				http.StatusBadRequest,
				ErrorResponse{Error: "parent job has no review to fix"},
			)
		}

		commitID, fallbackSHA := parentJob.LegacyCommentLookupTarget()
		comments, commentsErr := s.db.GetAllCommentsForJob(
			req.ParentJobID, commitID, fallbackSHA,
		)
		if commentsErr != nil {
			log.Printf(
				"fix job for parent %d: failed to fetch comments: %v",
				req.ParentJobID, commentsErr,
			)
		}
		reviewedRef := ""
		if parentJob.IsReviewJob() && !parentJob.IsDirtyJob() {
			reviewedRef = parentJob.GitRef
		}
		fixPrompt = buildFixPromptWithInstructions(
			review.Output, req.Prompt, fixMinSev, comments, reviewedRef,
		)
	}

	cfg := s.configWatcher.Config()
	resolutionPath := parentJob.RepoPath
	worktreePath := validatedWorktreePath(
		parentJob.WorktreePath, parentJob.RepoPath,
	)
	if parentJob.WorktreePath != "" && worktreePath == "" {
		return rawJSONOutput(
			http.StatusBadRequest,
			ErrorResponse{Error: "parent job worktree path is stale or invalid"},
		)
	}
	if worktreePath != "" {
		resolutionPath = worktreePath
	}
	reasoning, err := config.ResolveFixReasoning(
		"", resolutionPath, cfg,
	)
	if err != nil {
		return rawJSONOutput(
			http.StatusBadRequest,
			ErrorResponse{Error: err.Error()},
		)
	}
	if err := config.ValidateRepoConfig(resolutionPath); err != nil {
		return rawJSONOutput(
			http.StatusBadRequest,
			ErrorResponse{Error: fmt.Sprintf("resolve workflow config: %v", err)},
		)
	}
	resolution, err := agent.ResolveWorkflowConfig(
		"", resolutionPath, cfg, "fix", reasoning,
	)
	if err != nil {
		return rawJSONOutput(
			http.StatusBadRequest,
			ErrorResponse{Error: fmt.Sprintf("resolve workflow config: %v", err)},
		)
	}
	agentName := resolution.PreferredAgent
	if resolved, err := agent.GetPreferredOrBackupWithConfig(
		resolutionPath, agentName, cfg, resolution.BackupAgent,
	); err != nil {
		if _, ok := errors.AsType[*agent.UnknownAgentError](err); ok {
			return rawJSONOutput(
				http.StatusBadRequest,
				ErrorResponse{Error: fmt.Sprintf("invalid agent: %v", err)},
			)
		}
		return rawJSONOutput(
			http.StatusServiceUnavailable,
			ErrorResponse{Error: fmt.Sprintf("no agent available: %v", err)},
		)
	} else {
		agentName = resolved.Name()
	}

	model := resolution.ModelForSelectedAgent(agentName, "")

	req.GitRef = strings.TrimSpace(req.GitRef)
	if req.GitRef != "" && !isValidGitRef(req.GitRef) {
		return rawJSONOutput(
			http.StatusBadRequest,
			ErrorResponse{Error: "invalid git_ref"},
		)
	}

	fixGitRef := req.GitRef
	if fixGitRef == "" && !strings.Contains(parentJob.GitRef, "..") {
		fixGitRef = parentJob.GitRef
	}
	if fixGitRef == "" {
		fixGitRef = parentJob.Branch
	}
	if fixGitRef == "" {
		fixGitRef = "HEAD"
		log.Printf(
			"fix job for parent %d: no git ref or branch available, falling back to HEAD",
			req.ParentJobID,
		)
	}

	var commitID int64
	if parentJob.CommitID != nil {
		commitID = *parentJob.CommitID
	}

	job, err := s.db.EnqueueJob(storage.EnqueueOpts{
		RepoID:       parentJob.RepoID,
		CommitID:     commitID,
		GitRef:       fixGitRef,
		Branch:       parentJob.Branch,
		Agent:        agentName,
		Model:        model,
		Reasoning:    reasoning,
		Prompt:       fixPrompt,
		Agentic:      true,
		Label:        fmt.Sprintf("fix #%d", req.ParentJobID),
		JobType:      storage.JobTypeFix,
		ParentJobID:  req.ParentJobID,
		WorktreePath: worktreePath,
		MinSeverity:  fixMinSev,
	})
	if err != nil {
		if s.errorLog != nil {
			s.errorLog.LogError(
				"server",
				fmt.Sprintf("enqueue fix job: %v", err),
				0,
			)
		}
		return rawJSONOutput(
			http.StatusInternalServerError,
			ErrorResponse{Error: fmt.Sprintf("enqueue fix job: %v", err)},
		)
	}
	if commitID > 0 {
		job.CommitSubject = parentJob.CommitSubject
	}

	return rawJSONOutput(http.StatusCreated, job)
}

func (s *Server) humaMarkJobApplied(
	ctx context.Context, input *JobIDInput,
) (*JobStatusOutput, error) {
	if input.Body.JobID == 0 {
		return nil, huma.Error400BadRequest("job_id is required")
	}

	if err := s.db.MarkJobApplied(input.Body.JobID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, huma.Error404NotFound(
				"job not found or not in done state",
			)
		}
		return nil, huma.Error500InternalServerError(
			fmt.Sprintf("mark applied: %v", err),
		)
	}

	resp := &JobStatusOutput{}
	resp.Body.Status = "applied"
	return resp, nil
}

func (s *Server) humaMarkJobRebased(
	ctx context.Context, input *JobIDInput,
) (*JobStatusOutput, error) {
	if input.Body.JobID == 0 {
		return nil, huma.Error400BadRequest("job_id is required")
	}

	if err := s.db.MarkJobRebased(input.Body.JobID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, huma.Error404NotFound(
				"job not found or not in done state",
			)
		}
		return nil, huma.Error500InternalServerError(
			fmt.Sprintf("mark rebased: %v", err),
		)
	}

	resp := &JobStatusOutput{}
	resp.Body.Status = "rebased"
	return resp, nil
}

func (s *Server) humaGetHealth(
	ctx context.Context, input *struct{},
) (*HealthOutput, error) {
	uptime := time.Since(s.startTime)
	uptimeStr := formatDuration(uptime)

	var components []storage.ComponentHealth
	allHealthy := true

	dbHealthy := true
	dbMessage := ""
	if err := s.db.Ping(); err != nil {
		dbHealthy = false
		dbMessage = err.Error()
		allHealthy = false
	}
	components = append(components, storage.ComponentHealth{
		Name:    "database",
		Healthy: dbHealthy,
		Message: dbMessage,
	})

	workersHealthy := true
	workersMessage := ""
	stalledCount, err := s.db.CountStalledJobs(30 * time.Minute)
	if err != nil {
		workersHealthy = false
		workersMessage = fmt.Sprintf(
			"error checking stalled jobs: %v", err,
		)
		allHealthy = false
	} else if stalledCount > 0 {
		workersHealthy = false
		workersMessage = fmt.Sprintf(
			"%d stalled job(s) running > 30 min", stalledCount,
		)
		allHealthy = false
	}
	components = append(components, storage.ComponentHealth{
		Name:    "workers",
		Healthy: workersHealthy,
		Message: workersMessage,
	})

	if s.syncWorker != nil {
		syncHealthy, syncMessage := s.syncWorker.HealthCheck()
		if !syncHealthy {
			allHealthy = false
		}
		components = append(components, storage.ComponentHealth{
			Name:    "sync",
			Healthy: syncHealthy,
			Message: syncMessage,
		})
	}

	var recentErrors []storage.ErrorEntry
	var errorCount24h int
	if s.errorLog != nil {
		for _, e := range s.errorLog.RecentN(10) {
			recentErrors = append(recentErrors, storage.ErrorEntry{
				Timestamp: e.Timestamp,
				Level:     e.Level,
				Component: e.Component,
				Message:   e.Message,
				JobID:     e.JobID,
			})
		}
		errorCount24h = s.errorLog.Count24h()
	}

	return &HealthOutput{Body: storage.HealthStatus{
		Healthy:      allHealthy,
		Uptime:       uptimeStr,
		Version:      version.Version,
		Components:   components,
		RecentErrors: recentErrors,
		ErrorCount:   errorCount24h,
	}}, nil
}

func (s *Server) humaPing(
	ctx context.Context, input *struct{},
) (*PingOutput, error) {
	return &PingOutput{Body: PingInfo{
		OK:      true,
		Service: daemonServiceName,
		Version: version.Version,
		PID:     os.Getpid(),
	}}, nil
}

// humaShutdown requests a graceful daemon shutdown. This is the only
// graceful stop path on Windows, where the daemon cannot receive SIGTERM;
// without it every stop is a hard TerminateProcess that can interrupt
// SQLite WAL writes. The response is written before the server begins
// draining connections, so the client always sees it.
func (s *Server) humaShutdown(
	ctx context.Context, input *struct{},
) (*ShutdownOutput, error) {
	if err := s.beginShutdownDrain(); err != nil {
		return nil, huma.Error500InternalServerError(
			fmt.Sprintf("prepare graceful shutdown: %v", err),
		)
	}
	s.RequestShutdown()
	resp := &ShutdownOutput{}
	resp.Body.Status = "shutting down"
	return resp, nil
}

func (s *Server) beginShutdownDrain() error {
	s.shutdownDrainMu.Lock()
	defer s.shutdownDrainMu.Unlock()
	if s.shutdownDraining {
		return nil
	}
	if s.updateDrain == nil {
		if err := s.db.SetShutdownDraining(true); err != nil {
			return fmt.Errorf("block job claims for shutdown: %w", err)
		}
	}
	s.shutdownDraining = true
	if s.updateDrain != nil {
		if s.updateDrain.timer != nil {
			s.updateDrain.timer.Stop()
		}
		s.updateDrain = nil
	}
	s.workerPool.BeginStop()
	return nil
}

// RequestShutdown signals that the daemon should shut down gracefully.
func (s *Server) RequestShutdown() {
	s.shutdownOnce.Do(func() { close(s.shutdownCh) })
}

// ShutdownRequested returns a channel that is closed when a graceful
// shutdown has been requested via /api/shutdown.
func (s *Server) ShutdownRequested() <-chan struct{} {
	return s.shutdownCh
}

func (s *Server) humaSyncStatus(
	ctx context.Context, input *struct{},
) (*SyncStatusOutput, error) {
	resp := &SyncStatusOutput{}
	if s.syncWorker == nil {
		resp.Body.Enabled = false
		resp.Body.Connected = false
		resp.Body.Message = "sync not enabled"
		return resp, nil
	}

	healthy, message := s.syncWorker.HealthCheck()
	resp.Body.Enabled = true
	resp.Body.Connected = healthy
	resp.Body.Message = message
	return resp, nil
}

func (s *Server) humaActivity(
	ctx context.Context, input *ActivityInput,
) (*ActivityOutput, error) {
	resp := &ActivityOutput{}
	if s.activityLog == nil {
		resp.Body.Entries = []ActivityEntry{}
		return resp, nil
	}

	limit := 50
	if n, err := strconv.Atoi(input.Limit); err == nil && n > 0 {
		limit = n
	}
	if limit > activityLogCapacity {
		limit = activityLogCapacity
	}

	entries := s.activityLog.RecentN(limit)
	if entries == nil {
		entries = []ActivityEntry{}
	}
	resp.Body.Entries = entries
	return resp, nil
}

func (s *Server) humaJobOutput(
	ctx context.Context, input *JobOutputInput,
) (*huma.StreamResponse, error) {
	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		jobID, ok := parseHumaJobID(hctx, input.JobID, "job_id required")
		if !ok {
			return
		}

		job, err := s.db.GetJobByID(jobID)
		if err != nil {
			writeHumaJSON(
				hctx, http.StatusNotFound,
				ErrorResponse{Error: "job not found"},
			)
			return
		}

		if input.Stream != "1" {
			lines := s.workerPool.GetJobOutput(jobID)
			if len(lines) == 0 && jobStatusHasPersistedOutput(job.Status) {
				normalizerAgent := agent.CanonicalName(job.Agent)
				if review, reviewErr := s.db.GetReviewByJobID(jobID); reviewErr == nil && review.Agent != "" {
					normalizerAgent = agent.CanonicalName(review.Agent)
				}
				persisted, err := readNormalizedJobOutputForAttempt(
					jobID, normalizerAgent, job.StartedAt,
				)
				if err == nil {
					lines = persisted
				}
			}
			if lines == nil {
				lines = []OutputLine{}
			}
			writeHumaJSON(hctx, http.StatusOK, JobOutputResponse{
				JobID:   jobID,
				Status:  string(job.Status),
				Lines:   lines,
				HasMore: job.Status == storage.JobStatusRunning,
			})
			return
		}

		hctx.SetHeader("Content-Type", "application/x-ndjson")
		writer := hctx.BodyWriter()
		if job.Status != storage.JobStatusRunning {
			writeHumaNDJSON(writer, map[string]any{
				"type":   "complete",
				"status": string(job.Status),
			})
			return
		}

		hctx.SetHeader("Cache-Control", "no-cache")
		hctx.SetHeader("Connection", "keep-alive")
		flusher, ok := writer.(http.Flusher)
		if !ok {
			writeHumaJSON(
				hctx, http.StatusInternalServerError,
				ErrorResponse{Error: "streaming not supported"},
			)
			return
		}

		initial, ch, cancel := s.workerPool.SubscribeJobOutput(jobID)
		defer cancel()

		for _, line := range initial {
			if !writeHumaNDJSON(writer, map[string]any{
				"type":      "line",
				"ts":        line.Timestamp.Format(time.RFC3339Nano),
				"text":      line.Text,
				"line_type": line.Type,
			}) {
				return
			}
		}
		flusher.Flush()

		for {
			select {
			case <-hctx.Context().Done():
				return
			case line, ok := <-ch:
				if !ok {
					finalStatus := "done"
					if fj, err := s.db.GetJobByID(jobID); err == nil {
						finalStatus = string(fj.Status)
					}
					writeHumaNDJSON(writer, map[string]any{
						"type":   "complete",
						"status": finalStatus,
					})
					flusher.Flush()
					return
				}
				if !writeHumaNDJSON(writer, map[string]any{
					"type":      "line",
					"ts":        line.Timestamp.Format(time.RFC3339Nano),
					"text":      line.Text,
					"line_type": line.Type,
				}) {
					return
				}
				flusher.Flush()
			}
		}
	}}, nil
}

func jobStatusHasPersistedOutput(status storage.JobStatus) bool {
	switch status {
	case storage.JobStatusDone, storage.JobStatusFailed,
		storage.JobStatusCanceled, storage.JobStatusApplied,
		storage.JobStatusRebased, storage.JobStatusSkipped:
		return true
	default:
		return false
	}
}

func (s *Server) humaJobLog(
	ctx context.Context, input *JobLogInput,
) (*huma.StreamResponse, error) {
	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		jobID, ok := parseHumaJobID(hctx, input.JobID, "job_id required")
		if !ok {
			return
		}

		var offset int64
		var err error
		if input.Offset != "" {
			offset, err = strconv.ParseInt(input.Offset, 10, 64)
			if err != nil || offset < 0 {
				writeHumaJSON(
					hctx, http.StatusBadRequest,
					ErrorResponse{Error: "invalid offset"},
				)
				return
			}
		}

		job, err := s.db.GetJobByID(jobID)
		if err != nil {
			writeHumaJSON(
				hctx, http.StatusNotFound,
				ErrorResponse{Error: "job not found"},
			)
			return
		}
		identity, readErr := ResolveJobLogIdentity(job)
		if readErr != nil {
			log.Printf("humaJobLog: read agent metadata for job %d: %v", jobID, readErr)
		}
		logAgent := identity.Agent
		resetOffset := false
		if input.PreviousAgent != "" && input.PreviousAgent != logAgent {
			if !identity.Recorded && job.Status == storage.JobStatusQueued {
				logAgent = input.PreviousAgent
			} else if identity.Source != storage.JobSourceAutoDesign || identity.Recorded {
				offset = 0
				resetOffset = true
			}
		}

		f, err := os.Open(JobLogPath(jobID))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) &&
				job.Status == storage.JobStatusRunning {
				hctx.SetHeader("Content-Type", "application/x-ndjson")
				hctx.SetHeader("X-Job-Status", string(job.Status))
				hctx.SetHeader("X-Job-Agent", logAgent)
				hctx.SetHeader("X-Job-Source", job.Source)
				hctx.SetHeader("X-Log-Offset", "0")
				return
			}
			writeHumaJSON(
				hctx, http.StatusNotFound,
				ErrorResponse{Error: "no log file for this job"},
			)
			return
		}
		defer f.Close()

		fi, err := f.Stat()
		if err != nil {
			writeHumaJSON(
				hctx, http.StatusInternalServerError,
				ErrorResponse{Error: "stat log file"},
			)
			return
		}
		fileSize := fi.Size()
		if offset > fileSize {
			offset = 0
			resetOffset = true
		}

		endPos := fileSize
		if job.Status == storage.JobStatusRunning {
			endPos = jobLogSafeEnd(f, fileSize)
		}
		if offset > endPos {
			offset = endPos
		}

		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			writeHumaJSON(
				hctx, http.StatusInternalServerError,
				ErrorResponse{Error: "seek log file"},
			)
			return
		}

		hctx.SetHeader("Content-Type", "application/x-ndjson")
		hctx.SetHeader("X-Job-Status", string(job.Status))
		hctx.SetHeader("X-Job-Agent", logAgent)
		hctx.SetHeader("X-Job-Source", job.Source)
		hctx.SetHeader("X-Log-Offset", strconv.FormatInt(endPos, 10))
		if resetOffset {
			hctx.SetHeader("X-Log-Reset", "true")
		}

		if n := endPos - offset; n > 0 {
			if _, err := io.CopyN(hctx.BodyWriter(), f, n); err != nil {
				log.Printf(
					"humaJobLog: write error for job %d: %v",
					jobID, err,
				)
			}
		}
	}}, nil
}

func (s *Server) humaJobPatch(
	ctx context.Context, input *JobPatchInput,
) (*huma.StreamResponse, error) {
	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		jobID, ok := parseHumaJobID(
			hctx, input.JobID, "job_id parameter required",
		)
		if !ok {
			return
		}

		job, err := s.db.GetJobByID(jobID)
		if err != nil {
			writeHumaJSON(
				hctx, http.StatusNotFound,
				ErrorResponse{Error: "job not found"},
			)
			return
		}

		if !job.HasViewableOutput() || job.Patch == nil {
			writeHumaJSON(
				hctx, http.StatusNotFound,
				ErrorResponse{Error: "no patch available for this job"},
			)
			return
		}

		hctx.SetHeader("Content-Type", "text/plain")
		hctx.SetStatus(http.StatusOK)
		_, _ = hctx.BodyWriter().Write([]byte(*job.Patch))
	}}, nil
}

func (s *Server) humaSyncNow(
	ctx context.Context, input *SyncNowInput,
) (*huma.StreamResponse, error) {
	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		if s.syncWorker == nil {
			hctx.SetHeader("Content-Type", "text/plain; charset=utf-8")
			hctx.SetStatus(http.StatusNotFound)
			_, _ = hctx.BodyWriter().Write([]byte("Sync not enabled\n"))
			return
		}

		if input.Stream == "1" {
			hctx.SetHeader("Content-Type", "application/x-ndjson")
			hctx.SetHeader("X-Content-Type-Options", "nosniff")
			writer := hctx.BodyWriter()
			flusher, ok := writer.(http.Flusher)
			if !ok {
				writeHumaJSON(
					hctx, http.StatusInternalServerError,
					ErrorResponse{Error: "Streaming not supported"},
				)
				return
			}

			stats, err := s.syncWorker.SyncNowWithProgress(
				func(p storage.SyncProgress) bool {
					if !writeHumaNDJSON(writer, map[string]any{
						"type":        "progress",
						"phase":       p.Phase,
						"batch":       p.BatchNum,
						"batch_jobs":  p.BatchJobs,
						"batch_revs":  p.BatchRevs,
						"batch_resps": p.BatchResps,
						"total_jobs":  p.TotalJobs,
						"total_revs":  p.TotalRevs,
						"total_resps": p.TotalResps,
					}) {
						return false
					}
					flusher.Flush()
					return true
				})
			if err != nil {
				writeHumaNDJSON(writer, map[string]any{
					"type":  "error",
					"error": err.Error(),
				})
				return
			}
			writeHumaNDJSON(writer, syncCompletePayload(stats, true))
			return
		}

		stats, err := s.syncWorker.SyncNow()
		if err != nil {
			hctx.SetHeader("Content-Type", "text/plain; charset=utf-8")
			hctx.SetStatus(http.StatusInternalServerError)
			_, _ = hctx.BodyWriter().Write([]byte(err.Error() + "\n"))
			return
		}

		writeHumaJSON(hctx, http.StatusOK, syncCompletePayload(stats, false))
	}}, nil
}

func (s *Server) humaStreamEvents(
	ctx context.Context, input *StreamEventsInput,
) (*huma.StreamResponse, error) {
	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		hctx.SetHeader("Content-Type", "application/x-ndjson")
		hctx.SetHeader("Cache-Control", "no-cache")
		hctx.SetHeader("Connection", "keep-alive")

		writer := hctx.BodyWriter()
		flusher, ok := writer.(http.Flusher)
		if !ok {
			writeHumaJSON(
				hctx, http.StatusInternalServerError,
				ErrorResponse{Error: "streaming not supported"},
			)
			return
		}

		subID, eventCh := s.broadcaster.Subscribe(input.Repo)
		defer s.broadcaster.Unsubscribe(subID)
		flusher.Flush()

		encoder := json.NewEncoder(writer)
		for {
			select {
			case <-hctx.Context().Done():
				return
			case event, ok := <-eventCh:
				if !ok {
					return
				}
				if err := encoder.Encode(event); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	}}, nil
}

func rawJSONOutput(status int, body any) (*RawJSONOutput, error) {
	return &RawJSONOutput{
		Status: status,
		Body:   body,
	}, nil
}

func parseHumaJobID(ctx huma.Context, value, missingMessage string) (int64, bool) {
	if value == "" {
		writeHumaJSON(ctx, http.StatusBadRequest, ErrorResponse{Error: missingMessage})
		return 0, false
	}
	jobID, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		writeHumaJSON(ctx, http.StatusBadRequest, ErrorResponse{Error: "invalid job_id"})
		return 0, false
	}
	return jobID, true
}

func writeHumaJSON(ctx huma.Context, status int, v any) {
	ctx.SetHeader("Content-Type", "application/json")
	ctx.SetStatus(status)
	if err := json.NewEncoder(ctx.BodyWriter()).Encode(v); err != nil {
		_, _ = io.WriteString(
			ctx.BodyWriter(),
			fmt.Sprintf(`{"error":"failed to write JSON response: %v"}`, err),
		)
	}
}

func writeHumaNDJSON(writer io.Writer, v any) bool {
	line, err := json.Marshal(v)
	if err != nil {
		return false
	}
	if _, err := writer.Write(line); err != nil {
		return false
	}
	if _, err := writer.Write([]byte("\n")); err != nil {
		return false
	}
	return true
}

func syncCompletePayload(stats *storage.SyncStats, includeType bool) map[string]any {
	payload := map[string]any{
		"message": "Sync completed",
		"pushed": map[string]int{
			"jobs":      stats.PushedJobs,
			"reviews":   stats.PushedReviews,
			"responses": stats.PushedResponses,
		},
		"pulled": map[string]int{
			"jobs":      stats.PulledJobs,
			"reviews":   stats.PulledReviews,
			"responses": stats.PulledResponses,
		},
	}
	if includeType {
		payload["type"] = "complete"
	}
	return payload
}
