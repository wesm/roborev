package tui

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
	"uuid"

	tea "charm.land/bubbletea/v2"
	gansi "charm.land/glamour/v2/ansi"
	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/mattn/go-runewidth"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/streamfmt"
	roborevclient "go.kenn.io/roborev/pkg/client"
)

// Tick intervals for local redraws and fallback polling.
const (
	displayTickInterval  = 1 * time.Second  // Repaint only (elapsed counters, flash expiry)
	tickIntervalFallback = 15 * time.Second // Fallback poll; SSE handles real-time updates
)

// TUI styles using AdaptiveColor for light/dark terminal support.
// Light colors are chosen for dark-on-light terminals; Dark colors for light-on-dark.
var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(adaptiveColor("125", "205")) // Magenta/Pink

	statusStyle = lipgloss.NewStyle().
			Foreground(adaptiveColor("242", "246")) // Gray

	selectedStyle = lipgloss.NewStyle().
			Background(adaptiveColor("153", "24")) // Light blue background

	queuedStyle   = lipgloss.NewStyle().Foreground(adaptiveColor("136", "226")) // Yellow/Gold
	runningStyle  = lipgloss.NewStyle().Foreground(adaptiveColor("25", "33"))   // Blue
	doneStyle     = lipgloss.NewStyle().Foreground(adaptiveColor("27", "39"))   // Blue (completed)
	failedStyle   = lipgloss.NewStyle().Foreground(adaptiveColor("166", "208")) // Orange (job error)
	canceledStyle = lipgloss.NewStyle().Foreground(adaptiveColor("243", "245")) // Gray

	passStyle   = lipgloss.NewStyle().Foreground(adaptiveColor("28", "46"))   // Green
	failStyle   = lipgloss.NewStyle().Foreground(adaptiveColor("124", "196")) // Red (review found issues)
	closedStyle = lipgloss.NewStyle().Foreground(adaptiveColor("30", "51"))   // Cyan

	helpStyle = lipgloss.NewStyle().
			Foreground(adaptiveColor("242", "246")) // Gray

	helpKeyStyle = lipgloss.NewStyle().
			Foreground(adaptiveColor("242", "246")) // Gray (matches status/scroll text)
	helpDescStyle = lipgloss.NewStyle().
			Foreground(adaptiveColor("248", "240")) // Dimmer gray for descriptions

	errorStyle = lipgloss.NewStyle().
			Foreground(adaptiveColor("124", "196")).Bold(true) // Red

	flashStyle = lipgloss.NewStyle().
			Foreground(adaptiveColor("28", "46")) // Green

	warningFlashStyle = lipgloss.NewStyle().
				Foreground(adaptiveColor("136", "226")).Bold(true) // Yellow/Gold

	updateStyle = lipgloss.NewStyle().
			Foreground(adaptiveColor("136", "226")).Bold(true) // Yellow/Gold
)

// reflowHelpRows redistributes items across rows so that when rendered
// as an aligned table (columns sized to the widest cell), the result
// fits within width. Each cell's visible width is key + space + desc,
// and non-first columns add 2 chars (▕ border + padding). If width is
// <= 0, rows are returned unchanged.
func reflowHelpRows(rows [][]helpItem, width int) [][]helpItem {
	if width <= 0 {
		return rows
	}

	// cellWidth returns the visible width of a help item (key + space + desc,
	// or just key when desc is empty).
	cellWidth := func(item helpItem) int {
		w := runewidth.StringWidth(item.key)
		if item.desc != "" {
			w += 1 + runewidth.StringWidth(item.desc)
		}
		return w
	}

	// Find the max items in any single input row.
	maxItemsPerRow := 0
	for _, row := range rows {
		if len(row) > maxItemsPerRow {
			maxItemsPerRow = len(row)
		}
	}

	// Try ncols from max down to 1. For each candidate, chunk every
	// input row into sub-rows of at most ncols items, compute aligned
	// column widths, and check if the total fits within width.
	for ncols := maxItemsPerRow; ncols >= 1; ncols-- {
		var candidate [][]helpItem
		for _, row := range rows {
			for i := 0; i < len(row); i += ncols {
				end := min(i+ncols, len(row))
				candidate = append(candidate, row[i:end])
			}
		}

		// Compute max column widths across all candidate rows.
		colW := make([]int, ncols)
		for _, crow := range candidate {
			for c, item := range crow {
				if w := cellWidth(item); w > colW[c] {
					colW[c] = w
				}
			}
		}

		// Total rendered width.
		total := 0
		for c, w := range colW {
			total += w
			if c > 0 {
				total += 2 // ▕ + padding
			}
		}

		if total <= width {
			return candidate
		}
	}

	// Fallback: one item per row.
	var result [][]helpItem
	for _, row := range rows {
		for _, item := range row {
			result = append(result, []helpItem{item})
		}
	}
	return result
}

// renderHelpTable renders helpItem entries as an aligned table.
// Keys and descriptions are two-tone gray, separated by a thin ▕ border
// that is hidden for column 0 and trailing empty cells.
func renderHelpTable(rows [][]helpItem, width int) string {
	rows = reflowHelpRows(rows, width)
	if len(rows) == 0 {
		return ""
	}

	borderColor := adaptiveColor("248", "242")
	cellStyle := lipgloss.NewStyle()
	// PaddingLeft gaps the ▕ from cell text.
	cellWithBorder := lipgloss.NewStyle().
		PaddingLeft(1).
		Border(lipgloss.Border{Left: "▕"}, false, false, false, true).
		BorderForeground(borderColor)

	// Pad rows to the same number of columns so the table aligns.
	maxCols := 0
	for _, row := range rows {
		if len(row) > maxCols {
			maxCols = len(row)
		}
	}

	// Compute minimum visible width per column.
	colMinW := make([]int, maxCols)
	for _, row := range rows {
		for c, item := range row {
			w := runewidth.StringWidth(item.key)
			if item.desc != "" {
				w += 1 + runewidth.StringWidth(item.desc)
			}
			if w > colMinW[c] {
				colMinW[c] = w
			}
		}
	}

	// Track which cells have content for conditional borders.
	empty := make([][]bool, len(rows))

	t := table.New().
		BorderTop(false).
		BorderBottom(false).
		BorderLeft(false).
		BorderRight(false).
		BorderColumn(false).
		BorderRow(false).
		StyleFunc(func(row, col int) lipgloss.Style {
			minW := 0
			if col < len(colMinW) {
				minW = colMinW[col]
			}
			if col == 0 || (row < len(empty) && col < len(empty[row]) && empty[row][col]) {
				return cellStyle.Width(minW)
			}
			return cellWithBorder.Width(minW + 2) // +2 for ▕ border + padding
		}).
		Wrap(false)

	for ri, row := range rows {
		styled := make([]string, maxCols)
		empty[ri] = make([]bool, maxCols)
		for i, item := range row {
			if item.desc != "" {
				styled[i] = helpKeyStyle.Render(item.key) + " " + helpDescStyle.Render(item.desc)
			} else {
				styled[i] = helpKeyStyle.Render(item.key)
			}
		}
		for i := len(row); i < maxCols; i++ {
			empty[ri][i] = true
		}
		t = t.Row(styled...)
	}

	return t.Render()
}

// fullSHAPattern matches a 40-character hex git SHA (not ranges or branch names)
var fullSHAPattern = regexp.MustCompile(`(?i)^[0-9a-f]{40}$`)

// ansiEscapePattern matches ANSI escape sequences (colors, cursor movement, etc.)
// Handles CSI sequences (\x1b[...X) and OSC sequences terminated by BEL (\x07) or ST (\x1b\\)
var ansiEscapePattern = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]|\x1b\]([^\x07\x1b]|\x1b[^\\])*(\x07|\x1b\\)`)

// sanitizeForDisplay strips ANSI escape sequences and control characters from text
// to prevent terminal injection when displaying untrusted content (e.g., commit messages).
func sanitizeForDisplay(s string) string {
	// Strip ANSI escape sequences
	s = ansiEscapePattern.ReplaceAllString(s, "")
	// Strip control characters except newline (\n) and tab (\t)
	var result strings.Builder
	result.Grow(len(s))
	for _, r := range s {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			result.WriteRune(r)
		}
	}
	return result.String()
}

type model struct {
	endpoint             daemon.DaemonEndpoint
	daemonVersion        string
	client               *http.Client
	api                  *roborevclient.Client
	glamourStyle         gansi.StyleConfig // detected once at init
	jobs                 []storage.ReviewJob
	jobStats             storage.JobStats       // aggregate done/closed/open from server
	cost                 *storage.CostAggregate // approx agent spend for the active filter scope; nil = hidden
	costSeq              int                    // fetchSeq of the stored cost; segment hides until it matches m.fetchSeq
	status               storage.DaemonStatus
	selectedIdx          int
	selectedJobID        int64                             // Track selected job by ID to maintain position on refresh
	expandedPanels       map[uuid.UUID]bool                // panel_run_uuid -> expanded
	panelMembers         map[uuid.UUID][]storage.ReviewJob // panel_run_uuid -> side-fetched members
	currentView          viewKind
	rerunAgentJobID      int64
	rerunAgentOptions    []string
	rerunAgentSelected   int
	currentReview        *storage.Review
	currentResponses     []storage.Response // Responses for current review (fetched with review)
	currentBranch        string             // Cached branch name for current review (computed on load)
	reviewScroll         int
	promptScroll         int
	promptFromQueue      bool // true if prompt view was entered from queue (not review)
	width                int
	height               int
	err                  error
	updateAvailable      string // Latest version if update available, empty if up to date
	updateIsDevBuild     bool   // True if running a dev build
	versionMismatch      bool   // True if daemon version doesn't match TUI version
	releaseNotes         []storage.ReleaseNote
	releaseNotesErr      error
	releaseNotesStale    bool
	releaseNotesLoading  bool
	releaseNotesScroll   int
	releaseNotesFromView viewKind

	// CLI-locked filters: set via --repo/--branch flags, cannot be cleared by the user
	lockedRepoFilter   bool // true if repo filter was set via --repo flag
	lockedBranchFilter bool // true if branch filter was set via --branch flag

	// Pagination state
	hasMore        bool     // true if there are more jobs to load
	loadingMore    bool     // true if currently loading more jobs (pagination)
	loadingJobs    bool     // true if currently loading jobs (full refresh)
	loadingStatus  bool     // true if currently loading daemon status
	loadingFixJobs bool     // true if currently loading fix jobs
	statusStale    bool     // true if a state change occurred while status fetch was in flight
	fixJobsStale   bool     // true if a state change occurred while fix-jobs fetch was in flight
	fetchGen       uint64   // monotonic generation; incremented on reconnect to discard stale responses
	heightDetected bool     // true after first WindowSizeMsg (real terminal height known)
	fetchSeq       int      // incremented on filter changes; stale fetch responses are discarded
	paginateNav    viewKind // non-zero: auto-navigate in this view after pagination loads

	// Unified tree filter modal state
	filterTree        []treeFilterNode  // Tree of repos (each may have branch children)
	filterFlatList    []flatFilterEntry // Flattened visible rows for navigation
	filterSelectedIdx int               // Currently highlighted row in flat list
	filterSearch      string            // Search/filter text typed by user
	filterSearchSeq   int               // Incremented on search text changes; gates stale fetchFailed
	filterBranchMode  bool              // True when opened via 'b' key (auto-expand repo to branches)

	// Comment modal state
	commentText     string   // The response text being typed
	commentJobID    int64    // Job ID we're responding to
	commentCommit   string   // Short commit SHA for display
	commentFromView viewKind // View to return to after comment modal closes

	// Active filter (applied to queue view)
	activeRepoFilter   []string // Empty = show all, otherwise repo root_paths to filter by
	autoRepoFilter     bool     // true when activeRepoFilter came from auto_filter_repo
	activeBranchFilter string   // Empty = show all, otherwise branch name to filter by
	filterStack        []string // Order of applied filters: "repo", "branch" - for escape to pop in order
	hideClosed         bool     // When true, hide jobs with closed reviews
	// globalCfg is cached at startup and consulted at fetch time so that
	// show_classify_jobs can be resolved against whichever repo is the
	// currently active single-repo filter (rather than baked in once
	// from the cwd at launch).
	globalCfg *config.Config
	// classifyOverride is a session-level toggle that overrides the
	// config-derived show_classify_jobs value. nil = follow config;
	// non-nil = use the override regardless of config.
	classifyOverride *bool
	// classifyEffective caches the resolved show-classify-jobs value
	// so render and fetch paths don't hit disk via LoadRepoConfig on
	// every paint or event. Recomputed by recomputeClassifyEffective
	// whenever classifyOverride or activeRepoFilter changes.
	classifyEffective bool

	// Display name cache (keyed by repo path)
	displayNames map[string]string

	// Repo name lookup (display name → root paths), populated from
	// /api/repos at init. Used by control socket set-filter to resolve
	// display names to the root paths that the daemon API expects.
	repoNames      map[string][]string
	repoIdentities map[string][]string

	// Branch name cache (keyed by job ID) - caches derived branches to avoid repeated git calls
	branchNames map[int64]string

	// Track if branch backfill has run this session (one-time migration)
	branchBackfillDone bool

	// Repo, worktree, and branch detected from cwd at launch.
	cwdRepoRoot     string
	cwdRepoIdentity string
	cwdWorktreePath string
	cwdBranch       string

	// Pending closed state changes (prevents flash during refresh race)
	// Each pending entry stores the requested state and a sequence number to
	// distinguish between multiple requests for the same state (e.g., true→false→true)
	pendingClosed map[int64]pendingState // job ID -> pending state

	// Flash message (temporary status message shown briefly)
	flashMessage   string
	flashExpiresAt time.Time
	flashView      viewKind // View where flash was triggered (only show in same view)
	flashWarning   bool

	// Track config reload notifications
	lastConfigReloadCounter uint64                 // Last known ConfigReloadCounter from daemon status
	statusFetchedOnce       bool                   // True after first successful status fetch (for flash logic)
	pendingReviewClosed     map[int64]pendingState // review ID -> pending state (for reviews without jobs)
	closedSeq               uint64                 // monotonic counter for request sequencing

	// Daemon reconnection state
	consecutiveErrors int  // Count of consecutive connection failures
	reconnecting      bool // True if currently attempting reconnection

	// Commit message view state
	commitMsgContent  string   // Formatted commit message(s) content
	commitMsgScroll   int      // Scroll position in commit message view
	commitMsgJobID    int64    // Job ID for the commit message being viewed
	commitMsgFromView viewKind // View to return to after closing commit message view

	// Help view state
	helpFromView viewKind // View to return to after closing help
	helpScroll   int      // Scroll position in help view

	// Log view state
	logJobID          int64                // Job being viewed
	logLines          []logLine            // Buffer of output lines
	logScroll         int                  // Scroll position
	logStreaming      bool                 // True if job is still running
	logFromView       viewKind             // View to return to
	logReviewAnchored bool                 // Opened from a review-rooted context
	logFollow         bool                 // True if auto-scrolling to bottom (follow mode)
	logOffset         int64                // Byte offset for next incremental fetch
	logFmtr           *streamfmt.Formatter // Persistent formatter across polls
	logAgent          string               // Agent protocol identity retained across formatter rebuilds
	logSource         string               // Job source retained for legacy mixed-log compatibility
	logLoading        bool                 // True while a fetch is in-flight
	logFetchSeq       uint64               // Monotonic seq to drop stale responses

	// Command-line header expansion is independent by view. Prompt defaults to
	// expanded so long invocations remain readable; Log retains its compact
	// collapsed default.
	promptCmdExpanded bool
	logCmdExpanded    bool

	// Glamour markdown render cache (pointer so View's value receiver can update it)
	mdCache *markdownCache

	distractionFree   bool // hide status line, headers, footer, scroll indicator
	clipboard         ClipboardWriter
	tasksEnabled      bool          // Enables advanced tasks workflow in the TUI
	mouseEnabled      bool          // Enables mouse capture and mouse-driven interactions in the TUI
	noQuit            bool          // Suppress keyboard quit (for managed TUI instances)
	controlSocket     string        // Socket path for runtime metadata updates (empty if disabled)
	ready             chan struct{} // Closed on first Update; signals event loop is running
	sseCh             chan struct{} // Signals from SSE goroutine; nil when external IO disabled
	sseStop           chan struct{} // Close to stop SSE goroutine; nil when external IO disabled
	ssePendingRefresh bool          // True when an SSE event arrived during an in-flight fetch

	// Review view navigation
	reviewFromView viewKind // View to return to when exiting review (queue or tasks)

	// Fix task state
	fixJobs              []storage.ReviewJob // Fix jobs for tasks view
	fixSelectedIdx       int                 // Selected index in tasks view
	fixPromptText        string              // Editable fix prompt text
	fixPromptJobID       int64               // Parent job ID for fix prompt modal
	fixShowHelp          bool                // Show help overlay in tasks view
	patchText            string              // Current patch text for patch viewer
	patchScroll          int                 // Scroll offset in patch viewer
	patchJobID           int64               // Job ID of the patch being viewed
	savePatchInputActive bool                // Whether the save-filename input is visible
	savePatchInput       string              // Current text in the save-filename input

	// Inline fix panel (review view)
	reviewFixPanelOpen    bool // true when fix panel is visible in review view
	reviewFixPanelFocused bool // true when keyboard focus is on the fix panel
	reviewFixPanelPending bool // true when 'F' from queue; panel opens on review load
	// fixPromptFollowRetried bounds handleReviewFollowErrMsg's retry for a
	// pending queue-'F' request: a follow fetch that supersedes 'F's own
	// ordinary fetch and then FAILS is the one terminal outcome
	// acceptReview's success-only consume never sees, so without a retry
	// the pending flag could be left armed with nothing in flight to serve
	// it. Set true when the first such failure retries; checked on the
	// next one to clear the flag with a flash instead of retrying forever.
	// Reset wherever reviewFixPanelPending is (re)armed or disarmed, so it
	// is only ever meaningful during the one pending request it was set
	// for.
	fixPromptFollowRetried bool

	// fixPromptSeq is the reviewFetchSeq value stamped by the dispatch
	// that most recently armed reviewFixPanelPending (handlers_review.go's
	// queue-'F' path) -- the pending request's own per-arm IDENTITY, not
	// just its job. Comparing jobID alone is unsound when the SAME job is
	// armed twice: navigate away, navigate back, press 'F' again, and an
	// older in-flight request from before the navigation can finally fail
	// carrying the same jobID -- clearing on jobID alone would wipe out
	// the fresh arm whose own fetch is still healthily in flight. So the
	// stale-rejection branches refuse a clear unless the message is at
	// least as new as the armed request (msg.fetchSeq >= fixPromptSeq).
	//
	// Not needed on the "this message IS the current one" paths
	// (acceptReview's consume, handleReviewFollowErrMsg's retry-or-clear
	// block): reviewFetchSeq only increases, so nothing armed there can
	// exceed the current message's own fetchSeq. Reset alongside
	// fixPromptJobID wherever that is, so it is only ever meaningful
	// during the one pending request it was stamped for. The seq check is
	// a conjunct alongside the jobID gate, not a replacement --
	// defense-in-depth for dispatchReviewFetch's invariant that it always
	// sets selectedJobID together with the armed fields.
	fixPromptSeq uint64

	worktreeConfirmJobID  int64  // Job ID pending worktree-apply confirmation
	worktreeConfirmBranch string // Branch name for worktree confirmation prompt

	// Column options modal
	colOptionsIdx        int            // Cursor in modal
	colOptionsList       []columnOption // Items in modal (columns + borders toggle)
	colOptionsDirty      bool           // True if options changed since modal opened
	colBordersOn         bool           // Column borders enabled
	hiddenColumns        map[int]bool   // Set of hidden column IDs
	columnOrder          []int          // Ordered toggleable queue columns
	taskColumnOrder      []int          // Ordered task columns
	colOptionsReturnView viewKind       // Return-to view from column options

	// Column width caches (avoid full-scan on every render)
	queueColCache *colWidthCache // cached per-column max widths for queue
	queueColGen   int            // bumped when queue data/filters/columns change
	taskColCache  *colWidthCache // cached per-column max widths for tasks
	taskColGen    int            // bumped when fixJobs/columns change

	// Split layout state (see layout.go). layout/focus are only consulted
	// when layout == layoutSplit; stacked mode behaves exactly as before.
	layout          layoutMode
	focus           focusPane
	layoutLocked    bool       // true after a manual L toggle
	preferredLayout layoutMode // the layout the user chose with L

	// detailFollowGen is a generation guard for in-flight review fetches:
	// responses are stamped at dispatch, and the response handlers
	// (handleReviewMsg/handleReviewFollowErrMsg/handleReviewErrMsg) reject
	// a stale stamp unless reviewIntentRescuable (handlers_msg.go) rescues
	// it. Selection-scoped only: invalidating a superseded review ATTEMPT
	// (a rerun) is jobAttemptGen's job, below.
	//
	// Contract for any code that bumps this field. Every bumper is one of:
	//
	//   - ABANDONMENT: the selection left the job an in-flight dispatch
	//     was for, and that dispatch must never be served. The bumper must
	//     also resolve BOTH pending intents (pendingReviewOpen*, the
	//     pending fix panel) in the same handler -- the gen-mismatch
	//     rejection paths deliberately resolve nothing -- and each disarm
	//     must be keyed on the INTENT's own jobID, never on
	//     m.selectedJobID or other "what's on screen" state, even when the
	//     bump itself is gated more narrowly. An intent is scoped to a
	//     job, not a screen.
	//   - INCIDENTAL REFRESH: bookkeeping for a same-job bootstrap or
	//     debounce restart. Nothing was abandoned, so it must NOT disarm
	//     anything.
	//
	// Violating either side makes reviewIntentRescuable serve an abandoned
	// intent (opening content the user never asked for) or strand a live
	// one.
	//
	// Current bumpers: followSelectionChange's stacked branch (layout.go;
	// abandonment), scheduleDetailFollow (layout.go; abandonment when
	// reached via followSelectionChange, which has already disarmed --
	// incidental via maybeBootstrapDetail; the caller determines which),
	// handleJobsMsg's normalization epilogue (handlers_msg.go;
	// abandonment), and resetQueueForFilterChange (filter.go;
	// abandonment).
	detailFollowGen uint64

	// jobAttemptGen counts confirmed reruns per job (absent == 0). Every
	// review fetch is stamped at dispatch with the value for its job, and
	// the response handlers reject a mismatched stamp: the response
	// belongs to a superseded attempt -- for ANY job, not just the
	// selected one, which is what the single global detailFollowGen above
	// cannot express.
	//
	// Contract:
	//
	//  1. The only bumper is handleRerunResultMsg's success path. Any new
	//     bumper must be another "this job's current attempt is dead"
	//     event -- never a repaint, debounce, layout or selection event --
	//     and must first establish the event really superseded THIS job's
	//     attempt: rerunning a panel-synthesis parent spawns a fresh run
	//     with NEW job IDs and leaves this row untouched
	//     (rerunResultMsg.spawnsNewRun), so it must not bump.
	//  2. A bump is always abandonment for that job: the bumper must also
	//     resolve both pending intents for it, keyed on msg.jobID and
	//     unconditional on the current selection.
	//  3. Stamping happens at dispatch, never later: fetchReview (fetch.go)
	//     is the only constructor of reviewMsg/review*ErrMsg and reads the
	//     counter at command-creation time; fetchReviewForPrompt stamps
	//     promptMsg the same way. Every ASYNCHRONOUS, message-borne write
	//     of review content must carry this stamp. Synchronous writers
	//     that build currentReview from m.jobs in place read current state
	//     and have no staleness to identify, so they need not.
	//  4. The check is unconditional and unrescuable: a mismatch is
	//     rejected after the jobID gate and before the detailFollowGen
	//     gate, so an attempt-stale response can never reach
	//     reviewIntentRescuable. Nothing needs disarming on rejection --
	//     the bumper already disarmed (clause 2), and intents armed after
	//     the bump carry the post-bump stamp.
	//
	// Never pruned: entries come only from explicit operator reruns (16
	// bytes each), and deleting one would reset the counter to zero --
	// exactly the stamp a pre-rerun in-flight request carries, silently
	// re-opening the hole this field closes. Shared by reference across
	// model value copies (like pendingClosed): a bump is a fact about the
	// world that an older model copy must not be able to un-say.
	jobAttemptGen map[int64]uint64

	// panelRerunInFlight holds panel-synthesis parents whose rerun request
	// is dispatched but unresolved. Duplicate-dispatch suppression only --
	// never a correctness gate -- consulted by nothing but the two rerun
	// dispatchers. Ordinary reruns need no such set: their optimistic
	// re-queue flips the row to queued and the dispatchers only act on
	// terminal rows. A synthesis parent's row stays terminal (the daemon
	// enqueues a separate run and leaves it alone), so nothing else stops
	// a fast second 'r' or duplicate control-socket rerun from spawning a
	// second full panel run.
	//
	// Added at dispatch (handleRerunKey/handleCtrlRerunJob, only when the
	// snapshot says spawnsNewRun); removed by handleRerunResultMsg as its
	// FIRST statement, before every early return. rerunJob (actions.go)
	// has exactly one return statement and it is a rerunResultMsg, so
	// every add is matched by exactly one delete and an entry cannot leak
	// (a leaked entry would block that job's rerun for the session).
	//
	// Deliberately not a durable fix: another TUI instance or the CLI can
	// still dispatch a duplicate; rejecting that belongs on the daemon
	// side. Shared by reference across model value copies, like
	// jobAttemptGen: an in-flight fact an older model copy must not
	// un-say.
	panelRerunInFlight map[int64]bool

	// Live log tail for a running job in the split detail pane.
	// Independent of the full-screen log view's log* fields above so the
	// two can't stomp on each other's offset/formatter/seq state.
	paneLogJobID     int64                // job whose log is being tailed
	paneLogLines     []logLine            // buffered rendered lines (capped, see paneLogMaxLines)
	paneLogOffset    int64                // byte offset for next incremental fetch
	paneLogFmtr      *streamfmt.Formatter // persistent formatter across polls
	paneLogAgent     string               // agent protocol identity retained across formatter rebuilds
	paneLogSource    string               // job source retained for legacy mixed-log compatibility
	paneLogSeq       uint64               // monotonic seq; drops stale responses
	paneLogStreaming bool                 // true while the tailed job is still running
	paneLogPaused    bool                 // tail invalidated while a transient view hid the pane; resume on return

	// splitDetailErr records a failure that struck the split detail pane
	// while it awaited content in the background (a follow fetchReview
	// failure, or a live-log fetch failure while a job is running) so
	// renderDetailPane can surface it instead of leaving "Loading
	// review..." stuck forever. Cleared on every
	// scheduleDetailFollow (a fresh follow supersedes any earlier failure).
	splitDetailErr error

	// paneReviewSeenNonTerminalJob is a state-machine signal for
	// splitReconcileDetail's Done/Failed idempotency checks, precision-
	// independent unlike comparing FinishedAt or rendered error text: a
	// rerun MUST pass through queued/running before completing again, and
	// splitReconcileDetail observes the selected job on every jobs
	// refresh, so "was the selected job seen queued/running since the
	// currently-loaded review was fetched" proves a fresh attempt occurred
	// regardless of whether its timestamp/output happens to collide with
	// the previous attempt's. Set to the job's ID when splitReconcileDetail
	// observes the selected job queued/running while currentReview is
	// already loaded for it; 0 means no observation is outstanding.
	//
	// Scoped to a JOB ID, not a bare bool: the signal is a fact about one
	// specific job's loaded review, and a global flag bled across
	// selection changes -- observing job X's rerun and then selecting a
	// FAILED job Y left the flag set after Y's failure review was
	// synthesized (handleDetailFollowTick's Failed branch), so
	// selectedReviewLoaded wrongly treated Y's fresh review as stale and
	// blocked detail focus until the next jobs refresh. Keyed by job, an
	// observation for X is inert while Y is displayed and still correct
	// when the user returns to X.
	//
	// Means "a fresh attempt of THIS job has been observed and is not yet
	// reflected in the loaded review" -- so it must be cleared at
	// ACCEPTANCE for that job (when currentReview is actually replaced
	// with fresh content: the fetch landing in acceptReview, or the local
	// Failed-branch rebuilds in splitReconcileDetail and
	// handleDetailFollowTick), not at the DECISION to fetch/rebuild. A
	// Done-branch rebuild dispatches an async fetchReviewFollow that can
	// itself fail (handleReviewFollowErrMsg) -- clearing at dispatch time
	// would silently swallow the "fresh attempt observed" fact if that
	// fetch errors, leaving a stale review and a stuck splitDetailErr with
	// nothing left to retry it (the fallback FinishedAt/text comparison is
	// blind to same-second/same-text reruns, which is the whole reason
	// this signal exists). See splitReconcileDetail's doc comment for the
	// full reasoning, including the narrow case this alone doesn't cover.
	paneReviewSeenNonTerminalJob int64

	// fixPromptOrigin is the view the user was on when 'F' ARMED
	// reviewFixPanelPending (handlers_review.go) -- the origin of the
	// pending fix REQUEST, not of whatever response ends up serving it.
	// The consume must compare against the request's origin: any
	// dispatcher can produce the accepting response (e.g. a reconcile
	// follow fetch dispatched while the user types in the comment editor
	// carries dispatchedFrom == viewKindComment), so deciding the view
	// switch on the response's origin can yank the user out of an
	// unrelated view they explicitly opened.
	//
	// viewKind starts at iota, so this field's zero value happens to be
	// viewQueue -- but nothing rests on that: the field is written at the
	// one site that arms reviewFixPanelPending and cleared wherever the
	// flag is disarmed, so it is only read while holding a value written
	// on purpose. A renumbering of the viewKind constants would still need
	// to revisit this field.
	fixPromptOrigin viewKind

	// pendingReviewOpenJobID/pendingReviewOpenOrigin generalize
	// fixPromptJobID/fixPromptOrigin's "some other dispatcher's response
	// may serve this request" pattern to the plain "open the review view"
	// intent that has no fix panel attached. An ordinary fetch dispatched
	// by explicit navigation can be superseded by a follow fetch for the
	// same job (splitReconcileDetail runs on every jobs refresh); a follow
	// response never switches views by design, so without a recorded
	// intent the explicit navigation would silently do nothing whenever
	// the follow's response landed first.
	//
	// dispatchReviewFetch arms these fields at every ordinary dispatch
	// (jobID + m.currentView, matching what fetchReview stamps as
	// dispatchedFrom), and acceptReview consumes them on the shared
	// acceptance path regardless of msg.follow -- whichever dispatcher's
	// response lands the job's content first performs the transition, via
	// openReviewViewFrom's guard (a no-op if the user has since navigated
	// elsewhere). jobID 0 means nothing pending. Cleared where consumed
	// (acceptReview); disarmed proactively on a genuine selection change
	// (followSelectionChange and the inline sites listed in
	// disarmPendingReviewOpen's doc) and on a confirmed rerun of this
	// field's own job (handleRerunResultMsg, keyed on the rerun's jobID,
	// not on whether it is selected); or superseded by a fresh arm, which
	// overwrites all three fields.
	//
	// pendingReviewOpenSeq is this intent's per-arm IDENTITY -- the
	// reviewFetchSeq value stamped by the dispatch that (re-)armed it,
	// the same role fixPromptSeq plays for the fix panel (see that
	// field's doc): a "clear because stale" check must additionally
	// require msg.fetchSeq >= pendingReviewOpenSeq, so an old message
	// sharing a jobID with a just re-armed request cannot erase the fresh
	// arm. Not needed on acceptReview's consume, where the message is
	// provably current. The stale-rejection branches gate primarily on
	// msg.jobID != m.selectedJobID -- a gen-mismatch-only rejection (job
	// unchanged) clears nothing, since an incidental gen bump does not
	// mean the request was abandoned.
	pendingReviewOpenJobID  int64
	pendingReviewOpenOrigin viewKind
	pendingReviewOpenSeq    uint64

	// pendingReviewOpenRetried mirrors fixPromptFollowRetried for the plain
	// open-review intent: a follow fetch FAILING while this intent is armed
	// resolves the freshest dispatch for the job, but the intent's own
	// ORIGINATING ordinary dispatch may still be in flight -- and even when
	// it later SUCCEEDS, its response arrives fetchSeq-superseded and (with
	// no content loaded, precisely because the follow failed) cannot serve
	// the open. Clearing the intent at the follow failure would therefore
	// swallow a deliberate navigation whose own request succeeded. Instead
	// handleReviewFollowErrMsg retries once (dispatching a fresh follow,
	// whose success serves the intent via acceptReview's consume) and only
	// clears with a warning flash on the second failure. Reset at every arm
	// (dispatchReviewFetch), so each fresh intent gets its own single
	// retry; a stale true while nothing is armed is unread.
	pendingReviewOpenRetried bool

	// promptFetchSeq is the prompt path's own request identity, deliberately
	// separate from the shared review fetch epoch (see handlePromptMsg for
	// why the prompt path cannot join reviewFetchSeq). Bumped by
	// dispatchPromptFetch at every prompt fetch and stamped onto the
	// outgoing promptMsg; handlePromptMsg drops a response whose stamp no
	// longer matches. Also bumped by followSelectionChange's abandonment
	// path (both layouts), so a request abandoned by navigating away is
	// still dropped when the user returns to the same job before the
	// response lands -- without the bump, the jobID gate alone would
	// re-accept it and pop the prompt view open with no fresh keypress.
	promptFetchSeq uint64

	// failedCommentsSeq is the request identity for the synthesized-review
	// comments side channel (failedCommentsMsg), the same shape as
	// promptFetchSeq: bumped by dispatchFailedCommentsFetch at every
	// dispatch and stamped onto the outgoing request; the handler drops a
	// response whose stamp is stale. Without it, synthesized reviews all
	// sharing ID 0 lets an OLDER fetch's response land after a newer one
	// (navigate away and back re-dispatches) or after the post-success
	// local append (wiping the just-submitted comment until the next
	// refresh). The post-success path re-dispatches through the same
	// bump, so its refetch is always the newest request.
	failedCommentsSeq uint64

	// reviewFetchSeq is the single monotonically increasing epoch stamped
	// onto every fetch that produces a reviewMsg, follow or not:
	// fetchReview is the only constructor of that message and
	// dispatchReviewFetch/dispatchReviewFollow are its only entry points,
	// so coverage is by construction. One non-fetch bumper exists:
	// acceptSynthesizedFailure (layout.go) advances the epoch when the
	// synchronous failed-job rebuild replaces currentReview, ordering that
	// acceptance like an instant dispatch-plus-landing so older in-flight
	// responses cannot overwrite it. The handlers accept a response into
	// currentReview/currentResponses/currentBranch only when its stamp is
	// still current -- the newest dispatch wins, globally, whichever call
	// site issued either request. Without this total order an older
	// ordinary response could land after a newer comment refresh and
	// overwrite currentResponses (a just-submitted comment disappearing
	// until something refetched).
	//
	// One review-content path is deliberately NOT covered:
	// fetchReviewForPrompt produces a promptMsg, and handlePromptMsg
	// writes currentReview with no epoch, gated on jobID == selectedJobID
	// and the per-job attempt counter. Joining the epoch in either
	// direction would silently swallow the user's explicit 'p' whenever
	// another review fetch races it, because the prompt view has no
	// pending-intent field to re-serve the request -- see
	// handlePromptMsg's doc comment for the derivation.
	//
	// The three guards compose, none substitutes for another:
	// detailFollowGen rejects responses for a selection the model moved
	// past, jobAttemptGen rejects responses for a superseded job attempt,
	// and this epoch orders the responses that are still for the current
	// job/attempt.
	reviewFetchSeq uint64

	// reconcileFetchJobID is the job ID of the outstanding review fetch
	// dispatched by splitReconcileDetail's Done branch (0 = none). It is
	// purely a duplicate-dispatch SUPPRESSION signal, consulted only by
	// that branch's own guard: correctness is already guaranteed by the
	// epoch above, but reconcile re-runs on EVERY jobs refresh while
	// paneReviewSeenNonTerminal awaits acceptance, so without it a fetch
	// slower than the refresh interval would be superseded by its own
	// successor before it could ever land -- the pane would never converge
	// (a liveness, not a safety, problem), on top of piling up duplicate
	// in-flight requests against a slow daemon. The other dispatchers each
	// fire on a discrete one-shot event (a debounce tick, a tail
	// completing, a comment landing, a keypress) and so neither set nor
	// consult it.
	//
	// Released only by the response to the dispatch that set it, identified
	// by reconcileFetchSeq below.
	reconcileFetchJobID int64

	// reconcileFetchSeq is the fetch epoch stamped on the outstanding
	// reconcile dispatch -- the per-dispatch identity that makes the
	// suppression slot above releasable by exactly ONE arriving message.
	// Releasing on jobID alone would defeat suppression's purpose: an
	// older response for the job could clear a newer reconcile slot, the
	// next refresh would dispatch again and supersede the still-
	// outstanding request, and with fetches slower than the refresh
	// interval the pane would never converge. A real epoch value is never
	// 0 (pre-incremented before first use) and reconcileFetchSeq is 0
	// exactly when nothing is outstanding, so a 0-stamped message can
	// never accidentally match.
	//
	// Why the slot can never be left stuck (invariant: no state may be
	// left set with no arriving response able to clear it):
	//  1. Both fields are assigned non-zero in exactly one place --
	//     splitReconcileDetail's Done branch, immediately around the
	//     dispatch, with reconcileFetchSeq set to the epoch that dispatch
	//     stamped on its request.
	//  2. Every dispatch resolves to exactly one message -- a reviewMsg or
	//     a reviewFollowErrMsg (fetchReviewFollow converts the only other
	//     possibility, errMsg, into the latter).
	//  3. Both handlers call releaseReconcileFetch(msg.fetchSeq) as their
	//     FIRST statement, before the jobID/gen/epoch gates and before
	//     every early return -- a response rejected for DISPLAY still
	//     releases, and a superseded reconcile response is exactly the
	//     case that must.
	//  4. The epoch is monotonic and each value is handed to exactly one
	//     dispatch, so the release is unique: no foreign release, no
	//     missed release.
	//  5. scheduleDetailFollow (selection change) and handleRerunResultMsg
	//     (slot belongs to the reran job) additionally clear both fields
	//     so a fresh dispatch need not wait for a doomed response. Not
	//     load-bearing given (4).
	reconcileFetchSeq uint64
}

// isConnectionError checks if an error indicates a network/connection failure
// (as opposed to an application-level error like 404 or invalid response).
// Only connection errors should trigger reconnection attempts.
func isConnectionError(err error) bool {
	if err == nil {
		return false
	}
	// Check for url.Error (wraps network errors from http.Client)
	if _, ok := errors.AsType[*neturl.Error](err); ok {
		return true
	}
	// Check for net.Error (timeout, connection refused, etc.)
	var netErr net.Error
	return errors.As(err, &netErr)
}

// newClipboard returns the appropriate ClipboardWriter for the current environment.
// When running over SSH (detected via SSH_TTY, SSH_CLIENT, or SSH_CONNECTION env vars),
// it returns an osc52Clipboard that writes clipboard data through OSC52 terminal escape
// sequences — this works without X11 forwarding and is supported by all modern terminals.
// Otherwise, it falls back to realClipboard which uses the local system clipboard tools.
func newClipboard() ClipboardWriter {
	if os.Getenv("SSH_TTY") != "" || os.Getenv("SSH_CLIENT") != "" || os.Getenv("SSH_CONNECTION") != "" {
		return &osc52Clipboard{output: os.Stderr}
	}
	return &realClipboard{}
}

func detectCwdRepoContext(
	ctx context.Context, path string,
) (repoRoot, repoIdentity, worktreePath, branch string) {
	worktreeRoot, err := gitrepo.Root(ctx, path)
	if err != nil || worktreeRoot == "" {
		return "", "", "", ""
	}
	worktreePath = filepath.ToSlash(filepath.Clean(worktreeRoot))
	mainRoot, err := gitrepo.MainRoot(ctx, worktreeRoot)
	if err != nil || mainRoot == "" {
		return "", "", "", ""
	}
	repoRoot = filepath.ToSlash(filepath.Clean(mainRoot))
	return repoRoot, config.ResolveRepoIdentity(repoRoot, nil), worktreePath,
		gitrepo.CurrentBranch(ctx, worktreeRoot)
}

func newModel(ep daemon.DaemonEndpoint, opts ...option) model {
	var opt options
	for _, o := range opts {
		o(&opt)
	}
	ctx := opt.ctx
	if ctx == nil {
		ctx = context.Background()
	}

	daemonVersion := "?"
	hideClosed := false
	autoFilterRepo := false
	autoFilterBranch := false
	mouseEnabled := true
	tabWidth := 2
	columnBorders := false
	tasksEnabled := false
	hiddenCols := parseHiddenColumns(nil)
	colOrder := parseColumnOrder(nil)
	taskColOrder := parseTaskColumnOrder(nil)
	var cwdRepoRoot, cwdRepoIdentity, cwdWorktreePath, cwdBranch string
	var globalCfg *config.Config

	if !opt.disableExternalIO {
		// Read daemon version from runtime file
		if info, err := daemon.GetAnyRunningDaemon(); err == nil && info.Version != "" {
			daemonVersion = info.Version
		}

		// Load preferences from config
		if cfg, err := config.LoadGlobal(); err == nil {
			globalCfg = cfg
			hideClosed = cfg.HideClosedByDefault
			autoFilterRepo = cfg.AutoFilterRepo
			autoFilterBranch = cfg.AutoFilterBranch
			mouseEnabled = cfg.MouseEnabled
			if cfg.TabWidth > 0 {
				tabWidth = cfg.TabWidth
			}
			columnBorders = cfg.ColumnBorders
			tasksEnabled = cfg.Advanced.TasksEnabled

			if migrateColumnConfig(cfg) {
				if err := config.SaveGlobal(cfg); err != nil {
					log.Printf("warning: failed to save migrated config: %v", err)
				}
			}

			hiddenCols = parseHiddenColumns(cfg.HiddenColumns)
			colOrder = parseColumnOrder(cfg.ColumnOrder)
			taskColOrder = parseTaskColumnOrder(cfg.TaskColumnOrder)
		}

		cwdRepoRoot, cwdRepoIdentity, cwdWorktreePath, cwdBranch = detectCwdRepoContext(ctx, ".")
	}

	var sseCh chan struct{}
	var sseStop chan struct{}
	if !opt.disableExternalIO {
		sseCh = make(chan struct{}, 1)
		sseStop = make(chan struct{})
		go startSSESubscription(ep, sseCh, sseStop)
	}

	// Test overrides for auto-filter simulation
	if opt.autoFilterRepo {
		autoFilterRepo = true
		cwdRepoRoot = opt.cwdRepoRoot
		cwdRepoIdentity = opt.cwdRepoIdentity
	}
	if opt.autoFilterBranch {
		autoFilterBranch = true
		cwdBranch = opt.cwdBranch
	}

	// Determine active filters: CLI flags take priority over auto-filter config
	var activeRepoFilter []string
	var filterStack []string
	var autoRepoFilterActive bool
	var lockedRepo, lockedBranch bool

	if opt.repoFilter != "" {
		activeRepoFilter = []string{
			filepath.ToSlash(filepath.Clean(opt.repoFilter)),
		}
		filterStack = append(filterStack, filterTypeRepo)
		lockedRepo = true
	} else if autoFilterRepo && cwdRepoRoot != "" {
		activeRepoFilter = []string{cwdRepoRoot}
		filterStack = append(filterStack, filterTypeRepo)
		autoRepoFilterActive = true
	}

	var activeBranchFilter string
	if opt.branchFilter != "" {
		activeBranchFilter = opt.branchFilter
		filterStack = append(filterStack, filterTypeBranch)
		lockedBranch = true
	} else if autoFilterBranch && cwdBranch != "" {
		activeBranchFilter = cwdBranch
		filterStack = append(filterStack, filterTypeBranch)
	}

	httpClient := ep.HTTPClient(10 * time.Second)
	m := model{
		endpoint:            ep,
		daemonVersion:       daemonVersion,
		client:              httpClient,
		api:                 newDaemonAPI(ep, httpClient),
		glamourStyle:        streamfmt.GlamourStyle(),
		jobs:                []storage.ReviewJob{},
		currentView:         viewQueue,
		width:               80, // sensible defaults until we get WindowSizeMsg
		height:              24,
		loadingJobs:         true, // Init() calls fetchJobs, so mark as loading
		loadingStatus:       true, // Init() calls fetchStatus, so mark as loading
		hideClosed:          hideClosed,
		globalCfg:           globalCfg,
		activeRepoFilter:    activeRepoFilter,
		autoRepoFilter:      autoRepoFilterActive,
		activeBranchFilter:  activeBranchFilter,
		filterStack:         filterStack,
		lockedRepoFilter:    lockedRepo,
		lockedBranchFilter:  lockedBranch,
		cwdRepoRoot:         cwdRepoRoot,
		cwdRepoIdentity:     cwdRepoIdentity,
		cwdWorktreePath:     cwdWorktreePath,
		cwdBranch:           cwdBranch,
		displayNames:        make(map[string]string),      // Cache display names to avoid disk reads on render
		branchNames:         make(map[int64]string),       // Cache derived branch names to avoid git calls on render
		pendingClosed:       make(map[int64]pendingState), // Track pending closed changes (by job ID)
		pendingReviewClosed: make(map[int64]pendingState), // Track pending closed changes (by review ID)
		jobAttemptGen:       make(map[int64]uint64),       // Per-job confirmed-rerun counter (see the field's contract)
		panelRerunInFlight:  make(map[int64]bool),         // Panel reruns awaiting their result (suppression only)
		clipboard:           newClipboard(),
		mdCache:             newMarkdownCache(tabWidth),
		tasksEnabled:        tasksEnabled,
		mouseEnabled:        mouseEnabled,
		noQuit:              opt.noQuit,
		ready:               make(chan struct{}),
		sseCh:               sseCh,
		sseStop:             sseStop,
		colBordersOn:        columnBorders,
		hiddenColumns:       hiddenCols,
		columnOrder:         colOrder,
		taskColumnOrder:     taskColOrder,
		queueColCache:       &colWidthCache{gen: -1},
		taskColCache:        &colWidthCache{gen: -1},
		expandedPanels:      map[uuid.UUID]bool{},
		panelMembers:        map[uuid.UUID][]storage.ReviewJob{},
		promptCmdExpanded:   true,
	}
	// Seed the cached classify-visibility decision once so render and
	// fetch can read m.classifyEffective without hitting disk.
	m.recomputeClassifyEffective()
	return m
}

func (m model) Init() tea.Cmd {
	cmds := []tea.Cmd{
		tea.RequestWindowSize,
		m.displayTick(),
		m.tick(),
		m.fetchJobs(),
		m.fetchStatus(),
		m.fetchRepoNames(),
		m.checkForUpdate(),
	}
	if m.sseCh != nil {
		cmds = append(cmds, waitForSSE(m.sseCh, m.sseStop))
	}
	return tea.Batch(cmds...)
}

func (m model) tasksWorkflowEnabled() bool {
	return m.tasksEnabled
}

func (m model) tasksDisabledMessage() string {
	return "Tasks workflow disabled. Set advanced.tasks_enabled=true in global config to enable it."
}

// shouldShowClassifyJobs returns the cached effective value computed
// by recomputeClassifyEffective. Hot path: callable from render and
// fetch without filesystem I/O.
func (m model) shouldShowClassifyJobs() bool {
	return m.classifyEffective
}

// recomputeClassifyEffective resolves whether the queue should include
// auto-design-router classifier rows and skipped design rows, and
// caches the result in m.classifyEffective.
//
// Precedence: a session-level toggle (the 's' key) takes priority over
// config. When unset, falls back to the per-repo override if the queue
// is filtered to a single repo, otherwise the global setting. This
// avoids the surprise of one repo's override leaking into a multi-repo
// view while still letting the user flip the toggle on the fly.
//
// Call this from any handler that mutates classifyOverride or
// activeRepoFilter. It only loads repo config from disk under the
// single-repo-filter branch, so amortizing it to filter/toggle changes
// keeps the render and fetch paths I/O-free.
func (m *model) recomputeClassifyEffective() {
	switch {
	case m.classifyOverride != nil:
		m.classifyEffective = *m.classifyOverride
	case len(m.activeRepoFilter) == 1:
		m.classifyEffective = config.ResolveShowClassifyJobs(
			m.activeRepoFilter[0], m.globalCfg,
		)
	case m.globalCfg != nil:
		m.classifyEffective = m.globalCfg.ShowClassifyJobs
	default:
		m.classifyEffective = false
	}
}

// getDisplayName returns the display name for a repo, using the cache.
// Falls back to loading from config on cache miss, then to the provided default name.
func (m *model) getDisplayName(repoPath, defaultName string) string {
	if repoPath == "" {
		return defaultName
	}
	if displayName, ok := m.displayNames[repoPath]; ok {
		if displayName != "" {
			return displayName
		}
		return defaultName
	}
	// Cache miss - load from config (handles reviews for repos not in jobs list)
	displayName := config.GetDisplayName(repoPath)
	m.displayNames[repoPath] = displayName
	if displayName != "" {
		return displayName
	}
	return defaultName
}

// updateDisplayNameCache refreshes display names for the given repo paths.
// Called on each jobs fetch to pick up config changes without restart.
func (m *model) updateDisplayNameCache(jobs []storage.ReviewJob) {
	for _, repoPath := range uniqueJobRepoPaths(jobs) {
		// Always refresh to pick up config changes
		m.displayNames[repoPath] = config.GetDisplayName(repoPath)
	}
}

func uniqueJobRepoPaths(jobs []storage.ReviewJob) []string {
	seen := make(map[string]struct{})
	paths := make([]string, 0)
	for _, job := range jobs {
		if job.RepoPath == "" {
			continue
		}
		if _, ok := seen[job.RepoPath]; ok {
			continue
		}
		seen[job.RepoPath] = struct{}{}
		paths = append(paths, job.RepoPath)
	}
	return paths
}

// getBranchForJob returns the branch name for a job, falling back to git lookup
// if the stored branch is empty and the repo is available locally.
// Results are cached to avoid repeated git calls on render.
func (m *model) getBranchForJob(job storage.ReviewJob) string {
	// Use stored branch if available
	if job.Branch != "" {
		return job.Branch
	}

	// Check cache for previously derived branch (if cache exists)
	if m.branchNames != nil {
		if cached, ok := m.branchNames[job.ID]; ok {
			return cached
		}
	}

	// For task jobs (run, analyze, custom) or dirty jobs, no branch makes sense
	if job.IsTaskJob() || job.IsDirtyJob() {
		return ""
	}

	// Fall back to git lookup if repo path exists locally and we have a SHA
	// Only try if repo path is set and is not from a remote machine
	if job.RepoPath == "" || (m.status.MachineID != nil && job.SourceMachineID != nil && *job.SourceMachineID != *m.status.MachineID) {
		// Don't cache - repo might become available later
		return ""
	}

	// Check if repo exists locally before attempting git lookup
	// Return early on any error (not exists, permission denied, I/O failure)
	// to avoid caching incorrect results
	if _, err := os.Stat(job.RepoPath); err != nil {
		return ""
	}

	// For ranges (SHA..SHA), use the end SHA
	sha := job.GitRef
	if idx := strings.Index(sha, ".."); idx != -1 {
		sha = sha[idx+2:]
	}

	branch := git.GetBranchName(job.RepoPath, sha)
	if branch == "" {
		// The commit isn't reachable from any local branch - typically a
		// commit made on top of a detached HEAD (e.g. mid git-bisect).
		// Surface that instead of leaving the queue cell blank.
		branch = detachedBranchLabel(job)
	}
	// Cache result (including "" when neither lookup nor the detached
	// placeholder apply, e.g. task/dirty/CI jobs).
	// We only skip caching above when repo doesn't exist yet
	if m.branchNames != nil {
		m.branchNames[job.ID] = branch
	}
	return branch
}

// mouseDisabledView returns true for views where mouse capture should be
// released to allow native terminal text selection (copy/paste).
func mouseDisabledView(v viewKind) bool {
	switch v {
	case viewLog, viewReview, viewKindPrompt, viewPatch, viewCommitMsg, viewReleaseNotes:
		return true
	}
	return false
}

func mouseCaptureEnabled(v viewKind, mouseEnabled bool) bool {
	return mouseEnabled && !mouseDisabledView(v)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Signal that the event loop is running (once). The control
	// listener waits on this before accepting connections.
	if m.ready != nil {
		select {
		case <-m.ready:
		default:
			close(m.ready)
		}
	}

	var result tea.Model
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		result, cmd = m.handleKeyMsg(msg)
	case tea.MouseMsg:
		if !m.mouseEnabled {
			return m, nil
		}
		result, cmd = m.handleMouseMsg(msg)
	case tea.WindowSizeMsg:
		result, cmd = m.handleWindowSizeMsg(msg)
	case displayTickMsg:
		return m, m.displayTick()
	case tickMsg:
		result, cmd = m.handleTickMsg(msg)
	case logTickMsg:
		result, cmd = m.handleLogTickMsg(msg)
	case jobsMsg:
		result, cmd = m.handleJobsMsg(msg)
	case statusMsg:
		result, cmd = m.handleStatusMsg(msg)
	case costMsg:
		result, cmd = m.handleCostMsg(msg)
	case updateCheckMsg:
		result, cmd = m.handleUpdateCheckMsg(msg)
	case releaseNotesMsg:
		m.releaseNotes = msg.releases
		m.releaseNotesStale = msg.stale
		m.releaseNotesLoading = false
		m.releaseNotesErr = nil
		result = m
	case releaseNotesErrMsg:
		m.releaseNotesLoading = false
		m.releaseNotesErr = msg.err
		result = m
	case reviewMsg:
		result, cmd = m.handleReviewMsg(msg)
	case reviewErrMsg:
		result, cmd = m.handleReviewErrMsg(msg)
	case reviewFollowErrMsg:
		result, cmd = m.handleReviewFollowErrMsg(msg)
	case failedCommentsMsg:
		result, cmd = m.handleFailedCommentsMsg(msg)
	case detailFollowTickMsg:
		result, cmd = m.handleDetailFollowTick(msg)
	case promptMsg:
		result, cmd = m.handlePromptMsg(msg)
	case logOutputMsg:
		result, cmd = m.handleLogOutputMsg(msg)
	case paneLogOutputMsg:
		result, cmd = m.handlePaneLogOutputMsg(msg)
	case paneLogTickMsg:
		result, cmd = m.handlePaneLogTickMsg(msg)
	case closedMsg:
		result, cmd = m.handleClosedToggleMsg(msg)
	case closedResultMsg:
		result, cmd = m.handleClosedResultMsg(msg)
	case cancelResultMsg:
		result, cmd = m.handleCancelResultMsg(msg)
	case rerunResultMsg:
		result, cmd = m.handleRerunResultMsg(msg)
	case pauseResultMsg:
		result, cmd = m.handlePauseResultMsg(msg)
	case repoNamesMsg:
		result, cmd = m.handleRepoNamesMsg(msg)
	case reposMsg:
		result, cmd = m.handleReposMsg(msg)
	case repoBranchesMsg:
		result, cmd = m.handleRepoBranchesMsg(msg)
	case branchesMsg:
		result, cmd = m.handleBranchesMsg(msg)
	case commentResultMsg:
		result, cmd = m.handleCommentResultMsg(msg)
	case clipboardResultMsg:
		result, cmd = m.handleClipboardResultMsg(msg)
	case commitMsgMsg:
		result, cmd = m.handleCommitMsgMsg(msg)
	case jobsErrMsg:
		result, cmd = m.handleJobsErrMsg(msg)
	case paginationErrMsg:
		result, cmd = m.handlePaginationErrMsg(msg)
	case statusErrMsg:
		result, cmd = m.handleStatusErrMsg(msg)
	case errMsg:
		result, cmd = m.handleErrMsg(msg)
	case reconnectMsg:
		result, cmd = m.handleReconnectMsg(msg)
	case sseEventMsg:
		result, cmd = m.handleSSEEventMsg()
	case fixJobsMsg:
		result, cmd = m.handleFixJobsMsg(msg)
	case fixTriggerResultMsg:
		result, cmd = m.handleFixTriggerResultMsg(msg)
	case patchMsg:
		result, cmd = m.handlePatchResultMsg(msg)
	case panelMembersMsg:
		result, cmd = m.handlePanelMembersMsg(msg)
	case applyPatchResultMsg:
		result, cmd = m.handleApplyPatchResultMsg(msg)
	case savePatchResultMsg:
		result, cmd = m.handleSavePatchResultMsg(msg)
	case configSaveErrMsg:
		m.colOptionsDirty = true
		m.setFlash(
			"Config save failed: "+msg.err.Error(),
			5*time.Second, m.currentView,
		)
		result = m
	case controlSocketReadyMsg:
		m.controlSocket = msg.socketPath
		result = m
	case controlQueryMsg:
		result, cmd = m.handleControlQuery(msg)
	case controlMutationMsg:
		result, cmd = m.handleControlMutation(msg)
	default:
		// Unknown message types cannot change the view, so no mouse
		// toggle is needed. If a new message type is added that can
		// change currentView, add a case above.
		return m, nil
	}

	if rm, ok := result.(model); ok {
		rm = rm.normalizeSplitState()
		// Resume a tail paused by a resize that arrived while a transient
		// view covered the split panes. splitReconcileDetail handles
		// whatever the selected job's state is by now: restart the tail if
		// it is still running, or fetch its review if it finished while
		// the pane was hidden. Gated on the paused flag so this never
		// preempts the ordinary debounced follow during navigation.
		if rm.paneLogPaused && rm.splitActive() {
			rm.paneLogPaused = false
			var resumeCmd tea.Cmd
			rm, resumeCmd = rm.splitReconcileDetail()
			if resumeCmd != nil {
				cmd = tea.Batch(cmd, resumeCmd)
			}
		}
		result = rm
	}
	return result, cmd
}

func (m model) viewContent() string {
	if m.currentView == viewRerunAgent {
		return m.renderRerunAgentView()
	}
	if m.currentView == viewReleaseNotes {
		return m.renderReleaseNotesView()
	}
	if m.currentView == viewKindComment {
		return m.renderRespondView()
	}
	if m.currentView == viewFilter {
		return m.renderFilterView()
	}
	if m.currentView == viewCommitMsg {
		return m.renderCommitMsgView()
	}
	if m.currentView == viewHelp {
		return m.renderHelpView()
	}
	if m.currentView == viewLog {
		return m.renderLogView()
	}
	if m.currentView == viewTasks {
		return m.renderTasksView()
	}
	if m.currentView == viewKindWorktreeConfirm {
		return m.renderWorktreeConfirmView()
	}
	if m.currentView == viewPatch {
		return m.renderPatchView()
	}
	if m.currentView == viewColumnOptions {
		return m.renderColumnOptionsView()
	}
	if m.splitActive() {
		return m.renderSplit()
	}
	if m.currentView == viewKindPrompt && m.currentReview != nil {
		return m.renderPromptView()
	}
	if m.currentView == viewReview && m.currentReview != nil {
		return m.renderReviewView()
	}
	return m.renderQueueView()
}

func (m model) View() tea.View {
	v := tea.NewView(m.viewContent())
	v.AltScreen = true
	if mouseCaptureEnabled(m.currentView, m.mouseEnabled) {
		v.MouseMode = tea.MouseModeCellMotion
	}
	return v
}

// helpLines builds the help content lines from shortcut definitions.

// helpMaxScroll returns the maximum scroll offset for the help view.

// Config holds resolved parameters for running the TUI.
type Config struct {
	Context       context.Context
	Endpoint      daemon.DaemonEndpoint
	RepoFilter    string
	BranchFilter  string
	ControlSocket string // Unix socket path for external control (default: auto)
	NoQuit        bool   // Suppress keyboard quit (for managed TUI instances)
}

func programOptionsForModel(m model) []tea.ProgramOption {
	return nil
}

// Run starts the interactive TUI.
func Run(cfg Config) error {
	// Clean up sockets from dead TUI processes before starting
	CleanupStaleTUIRuntimes()

	var opts []option
	if cfg.Context != nil {
		opts = append(opts, withContext(cfg.Context))
	}
	if cfg.RepoFilter != "" {
		opts = append(opts, withRepoFilter(cfg.RepoFilter))
	}
	if cfg.BranchFilter != "" {
		opts = append(opts, withBranchFilter(cfg.BranchFilter))
	}
	if cfg.NoQuit {
		opts = append(opts, withNoQuit())
	}
	// Resolve socket path before creating the model so the
	// model knows its socket path for runtime metadata updates.
	socketPath := cfg.ControlSocket
	if socketPath == "" {
		socketPath = defaultControlSocketPath()
		// Only tighten directory permissions for the default
		// managed directory. Custom --control-socket paths may
		// point anywhere (e.g. /tmp, repo root) and mutating
		// their parent would be destructive.
		if err := ensureSocketDir(
			filepath.Dir(socketPath),
		); err != nil {
			log.Printf("warning: control socket disabled: %v", err)
			socketPath = ""
		}
	}

	m := newModel(cfg.Endpoint, opts...)
	p := tea.NewProgram(
		m,
		programOptionsForModel(m)...,
	)

	// Start the control listener after the event loop is running
	// so that p.Send (used by control handlers) never blocks. The
	// model closes m.ready on its first Update call. Skip if
	// socketPath is empty (ensureSocketDir failed).
	// cleanupDone is closed after socket/runtime cleanup finishes
	// so Run() can wait for it before returning, preventing stale
	// files if the process exits immediately after p.Run().
	// programDone is closed after p.Run() returns so the goroutine
	// can unblock if the program exits before m.ready fires.
	cleanupDone := make(chan struct{})
	programDone := make(chan struct{})
	if socketPath != "" {
		go func() {
			defer close(cleanupDone)

			// Wait for either the event loop to start or the
			// program to exit (e.g. terminal init failure).
			select {
			case <-m.ready:
			case <-programDone:
				return
			}

			cleanup, err := startControlListener(socketPath, p)
			if err != nil {
				log.Printf(
					"warning: control socket disabled: %v", err,
				)
				return
			}

			// Notify the model that the control socket is
			// active so it can update runtime metadata on
			// reconnect. This is deferred until after the
			// listener succeeds to avoid advertising a
			// socket that failed to bind.
			p.Send(controlSocketReadyMsg{
				socketPath: socketPath,
			})

			rtInfo := buildTUIRuntimeInfo(
				socketPath, cfg.Endpoint.ConfigAddr(),
			)
			if err := WriteTUIRuntime(rtInfo); err != nil {
				log.Printf(
					"warning: failed to write TUI runtime info: %v",
					err,
				)
			}

			// Block until the program exits, then clean up.
			p.Wait()
			cleanup()
			RemoveTUIRuntime()
		}()
	} else {
		close(cleanupDone)
	}

	finalModel, err := p.Run()
	// Stop SSE subscription goroutine. Use the final model (not the
	// initial m) because reconnect may have replaced sseStop — closing
	// the original would double-close.
	if fm, ok := finalModel.(model); ok && fm.sseStop != nil {
		close(fm.sseStop)
	}
	close(programDone)
	<-cleanupDone
	return err
}

// renderTasksView renders the background fix tasks list.

// fetchPatch fetches the patch for a fix job from the daemon.

// renderWorktreeConfirmView renders the worktree creation confirmation modal.
