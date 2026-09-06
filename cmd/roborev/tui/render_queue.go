package tui

import (
	"fmt"
	"image/color"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/streamfmt"
	"go.kenn.io/roborev/internal/tokens"
	"go.kenn.io/roborev/internal/version"
)

func (m model) getVisibleJobs() []storage.ReviewJob {
	if len(m.activeRepoFilter) == 0 && m.activeBranchFilter == "" && !m.hideClosed {
		return m.jobs
	}
	var visible []storage.ReviewJob
	for _, job := range m.jobs {
		if m.isJobVisible(job) {
			visible = append(visible, job)
		}
	}
	return visible
}

// visibleQueueRows is the flattened, filtered render/nav source: parents from
// getVisibleJobs (filters already applied) expanded into member rows per the
// expanded set and side-fetched members. With expandedPanels/panelMembers empty
// this is the filtered job list 1:1 (all depth 0).
func (m model) visibleQueueRows() []queueRow {
	return flattenQueueRows(m.getVisibleJobs(), m.expandedPanels, m.panelMembers)
}

// anyPanelRow reports whether any visible row is an expandable panel parent.
// It gates every panel-only render affordance (tree slot, disclosure column,
// banding, panel cell) so a panel-free page renders byte-identical to before.
func anyPanelRow(rows []queueRow) bool {
	for i := range rows {
		if rows[i].hasChildren {
			return true
		}
	}
	return false
}

// queueColorEnabled reports whether the queue should render the unicode tree
// glyphs (vs the ASCII fallbacks). It uses the same color-mode resolver as
// markdown rendering (NO_COLOR / ROBOREV_COLOR_MODE=none -> false).
func queueColorEnabled() bool {
	mode := strings.ToLower(os.Getenv("ROBOREV_COLOR_MODE"))
	if mode == "none" || termenv.EnvNoColor() {
		return false
	}
	if mode == "dark" || mode == "light" {
		return true
	}
	return streamfmt.ResolveColorProfile() != termenv.Ascii
}

// queueTreeSlot returns the leading tree marker for a row, with a trailing
// separator space: the disclosure glyph for a depth-0 row or the ├─/└─
// connector for a depth-1 member. Only prefixed when the page has panels.
func queueTreeSlot(r queueRow, color bool) string {
	if r.depth == 1 {
		return childConnector(r.lastChild, color) + " "
	}
	return disclosureGlyph(r.hasChildren, r.expanded, color) + " "
}

// decorateRefCell prefixes the ref cell with the tree slot and, for a panel
// parent, appends the live/terminal panel status cell. The summary text is
// sanitized so it cannot inject terminal escapes into the single-line cell.
func decorateRefCell(ref string, r queueRow, color bool) string {
	ref = queueTreeSlot(r, color) + ref
	if r.depth == 0 && r.hasChildren {
		if cell := stripControlChars(panelStatusCell(r.job)); cell != "" {
			ref = ref + "  " + cell
		}
	}
	return ref
}

// withExpandHint returns the help rows with a "space — expand" entry appended
// to the last row. Used only when the selected row is a panel parent.
func withExpandHint(rows [][]helpItem) [][]helpItem {
	if len(rows) == 0 {
		return [][]helpItem{{{"space", "expand"}}}
	}
	out := make([][]helpItem, len(rows))
	copy(out, rows)
	last := len(out) - 1
	out[last] = append(append([]helpItem(nil), out[last]...), helpItem{"space", "expand"})
	return out
}

func (m model) queueHelpRows() [][]helpItem {
	row1 := []helpItem{
		{"x", "cancel"},
		{"r", "rerun"},
		{"R", "rerun new agent"},
		{"l", "log"},
		{"p", "prompt"},
		{"c", "comment"},
		{"y", "copy"},
		{"m", "commit"},
	}
	if m.tasksWorkflowEnabled() {
		row1 = append(row1, helpItem{"F", "fix"})
	}
	row1 = append(row1, helpItem{"o", "options"})
	row2 := []helpItem{
		{"↑/↓", "nav"}, {"↵", "review"}, {"a", "close"},
	}
	if !m.lockedRepoFilter || !m.lockedBranchFilter {
		row2 = append(row2, helpItem{"f", "filter"})
	}
	row2 = append(row2, helpItem{"h", "hide"})
	if m.shouldShowClassifyJobs() {
		row2 = append(row2, helpItem{"s", "hide classify"})
	} else {
		row2 = append(row2, helpItem{"s", "show classify"})
	}
	row2 = append(row2, helpItem{"D", "focus"})
	pauseLabel := "pause"
	if m.status.QueuePaused {
		pauseLabel = "resume"
	}
	row2 = append(row2, helpItem{"P", pauseLabel})
	if m.tasksWorkflowEnabled() {
		row2 = append(row2, helpItem{"T", "tasks"})
	}
	row2 = append(row2, helpItem{"?", "help"})
	if !m.noQuit {
		row2 = append(row2, helpItem{"q", "quit"})
	}
	return [][]helpItem{row1, row2}
}

func (m model) renderRerunAgentView() string {
	var b strings.Builder

	fmt.Fprintf(&b, "%s\x1b[K\n\x1b[K\n",
		titleStyle.Render(fmt.Sprintf("Rerun job #%d with agent", m.rerunAgentJobID)))
	visibleRows := max(m.height-5, 0)
	start := 0
	if m.rerunAgentSelected >= visibleRows && visibleRows > 0 {
		start = m.rerunAgentSelected - visibleRows + 1
	}
	end := min(start+visibleRows, len(m.rerunAgentOptions))
	for i := start; i < end; i++ {
		line := "  " + sanitizeForDisplay(m.rerunAgentOptions[i])
		if i == m.rerunAgentSelected {
			line = selectedStyle.Render("> " + sanitizeForDisplay(m.rerunAgentOptions[i]))
		}
		b.WriteString(line)
		b.WriteString("\x1b[K\n")
	}
	for i := end - start; i < visibleRows; i++ {
		b.WriteString("\x1b[K\n")
	}
	b.WriteString(renderHelpTable([][]helpItem{{
		{"Up/Down", "navigate"}, {"Enter", "rerun"}, {"Esc", "cancel"},
	}}, m.width))
	b.WriteString("\x1b[K\x1b[J")
	return b.String()
}

// selectedRowHasChildren reports whether the currently selected visible row is
// an expandable panel parent. Shared by queueHelpLines (height reservation) and
// renderQueueView (the expand-hint gate) so the reserved help height can never
// drift from the help actually drawn.
func (m model) selectedRowHasChildren() bool {
	rows := m.visibleQueueRows()
	idx := visibleSelectedRowIndex(rows, m.selectedJobID)
	return idx >= 0 && rows[idx].hasChildren
}

func (m model) queueHelpLines() int {
	rows := m.queueHelpRows()
	if m.selectedRowHasChildren() {
		rows = withExpandHint(rows)
	}
	return len(reflowHelpRows(rows, m.width))
}

// queueCompact returns true when chrome should be hidden
// (status line, table header, scroll indicator, flash, help footer).
// Triggered automatically for short terminals or manually via distraction-free mode.
func (m model) queueCompact() bool {
	return m.height < 15 || m.distractionFree
}

func (m model) queueVisibleRows() int {
	if m.queueCompact() {
		// compact: title(1) only
		return max(m.height-1, 1)
	}
	// title(1) + status(2) + header(1) + separator(1) + scroll(1) + flash(1) + help(dynamic)
	reserved := 7 + m.queueHelpLines()
	visibleRows := max(m.height-reserved, 3)
	return visibleRows
}

func (m model) canPaginate() bool {
	return m.hasMore && !m.loadingMore && !m.loadingJobs &&
		m.activeBranchFilter != branchNone
}

// visibleSelectedRowIndex returns the index of selectedJobID within the
// flattened visible rows, or -1 when the id is 0 or not present. This is the
// render/windowing cursor; selectedJobID is the authoritative selection identity
// (kept in sync with selectedIdx by the nav handlers).
func visibleSelectedRowIndex(rows []queueRow, selectedJobID int64) int {
	if selectedJobID == 0 {
		return -1
	}
	for i := range rows {
		if rows[i].job.ID == selectedJobID {
			return i
		}
	}
	return -1
}

func (m model) getVisibleSelectedIdx() int {
	if m.selectedIdx < 0 {
		return -1
	}
	if len(m.activeRepoFilter) == 0 && m.activeBranchFilter == "" && !m.hideClosed {
		return m.selectedIdx
	}
	count := 0
	for i, job := range m.jobs {
		if m.isJobVisible(job) {
			if i == m.selectedIdx {
				return count
			}
			count++
		}
	}
	return -1
}

// Queue table column indices.
const (
	colSel               = iota // "> " selection indicator
	colJobID                    // Job ID
	colRef                      // Git ref (short SHA or range)
	colBranch                   // Branch name
	colRepo                     // Repository display name
	colAgent                    // Agent name
	colReviewType               // Review type (default, design, security, or custom)
	colQueued                   // Enqueue timestamp
	colElapsed                  // Elapsed time
	colStatus                   // Job status
	colPF                       // Pass/Fail verdict
	colHandled                  // Done status
	colSessionID                // Session ID
	colRequestedModel           // Explicitly requested model
	colRequestedProvider        // Explicitly requested provider
	colCost                     // Cost estimate (USD)
	colReasoning                // Recorded reasoning effort
	colCount                    // total number of columns
)

// queueWindowStart returns the [start,end) slice of the flattened rows to show,
// centering selIdx within a viewport of visibleRows lines. selIdx<0 (no/offscreen
// selection) anchors at the top. Shared by renderQueueView and the mouse handler
// so the render window and click-hit-testing can never drift apart.
func queueWindowStart(total, selIdx, visibleRows int) (start, end int) {
	if total <= visibleRows {
		return 0, total
	}
	start = max(selIdx-visibleRows/2, 0)
	end = start + visibleRows
	if end > total {
		end = total
		start = max(end-visibleRows, 0)
	}
	return start, end
}

func (m model) queueFullRowCells(r queueRow, hasAnyPanel, treeColor bool) []string {
	job := *r.job
	cells := m.jobCells(job)
	fullRow := make([]string, colCount)
	fullRow[colSel] = "  "
	fullRow[colJobID] = fmt.Sprintf("%d", job.ID)
	copy(fullRow[colRef:], cells)
	if hasAnyPanel {
		fullRow[colRef] = decorateRefCell(fullRow[colRef], r, treeColor)
	}
	return fullRow
}

// costSegmentText returns the bare approximate-cost text (no separator) and
// whether to show it. Coverage is dropped when complete. The segment hides while
// the stored cost predates the active filter generation (m.costSeq != m.fetchSeq),
// so a filter change cannot briefly show the prior scope's spend before the
// refreshed cost arrives.
func (m model) costSegmentText() (string, bool) {
	if m.cost == nil || m.cost.JobsTotal == 0 || m.costSeq != m.fetchSeq {
		return "", false
	}
	if m.cost.Complete {
		return fmt.Sprintf("~$%.2f", m.cost.TotalUSD), true
	}
	return fmt.Sprintf("~$%.2f (%d/%d)", m.cost.TotalUSD, m.cost.JobsWithCost, m.cost.JobsTotal), true
}

func (m model) activeSnooze(now time.Time) *storage.AgentHookSnooze {
	if len(m.activeRepoFilter) != 1 ||
		m.activeRepoFilter[0] != m.cwdRepoRoot ||
		m.activeBranchFilter == "" ||
		m.activeBranchFilter != m.cwdBranch ||
		m.cwdWorktreePath == "" {
		return nil
	}
	for i := range m.status.ActiveSnoozes {
		snooze := &m.status.ActiveSnoozes[i]
		if snooze.RepoPath == m.cwdRepoRoot &&
			snooze.WorktreePath == m.cwdWorktreePath &&
			snooze.Branch == m.cwdBranch &&
			snooze.SnoozedUntil.After(now) {
			return snooze
		}
	}
	return nil
}

// statusSeg is one segment of the queue status line. Segments with a lower prio
// are dropped first when the line does not fit the terminal width.
type statusSeg struct {
	rendered string // already styled; display width measured with xansi
	prio     int
}

// fitStatusSegments joins rendered segments in slice order with sep, dropping
// the lowest-prio segment until the result fits within width. The single
// highest-prio segment is always kept; callers truncate as a final guard.
func fitStatusSegments(segs []statusSeg, sep string, width int) string {
	keep := make([]bool, len(segs))
	for i := range keep {
		keep[i] = true
	}
	join := func() string {
		parts := make([]string, 0, len(segs))
		for i, s := range segs {
			if keep[i] {
				parts = append(parts, s.rendered)
			}
		}
		return strings.Join(parts, sep)
	}
	for width > 0 && xansi.StringWidth(join()) > width {
		lowest, kept := -1, 0
		for i := range segs {
			if !keep[i] {
				continue
			}
			kept++
			if lowest == -1 || segs[i].prio < segs[lowest].prio {
				lowest = i
			}
		}
		if kept <= 1 {
			break
		}
		keep[lowest] = false
	}
	return join()
}

// renderQueueStatusLine builds the width-adaptive status line (line 2 of the
// queue header). Segments are dropped lowest-priority-first until the line fits
// m.width: Completed and Closed go before Workers, while Open and approximate
// cost are kept longest. Version info lives in the title bar, so it never
// appears here.
func (m model) renderQueueStatusLine(done, closed, open int) string {
	width := m.width
	if width <= 0 {
		width = 100
	}
	sep := statusStyle.Render(" | ")

	segs := []statusSeg{
		{rendered: statusStyle.Render(fmt.Sprintf("Workers: %d/%d", m.status.ActiveWorkers, m.status.MaxWorkers)), prio: 30},
		{rendered: statusStyle.Render(fmt.Sprintf("Completed: %d", done)), prio: 10},
		{rendered: statusStyle.Render(fmt.Sprintf("Closed: %d", closed)), prio: 20},
		{rendered: statusStyle.Render(fmt.Sprintf("Open: %d", open)), prio: 90},
	}
	if text, ok := m.costSegmentText(); ok {
		segs = append(segs, statusSeg{rendered: statusStyle.Render(text), prio: 80})
	}

	line := fitStatusSegments(segs, sep, width)
	if xansi.StringWidth(line) > width {
		line = xansi.Truncate(line, width, "")
	}
	return line
}

// titleFilters renders the active repo/branch filter chips (in stack order) in
// the title style, each with a leading space, or "" when no filter is active.
func (m model) titleFilters() string {
	var chips strings.Builder
	for _, filterType := range m.filterStack {
		switch filterType {
		case filterTypeRepo:
			if len(m.activeRepoFilter) > 0 {
				fmt.Fprintf(&chips, " [f: %s]", m.repoFilterDisplayName())
			}
		case filterTypeBranch:
			if m.activeBranchFilter != "" {
				fmt.Fprintf(&chips, " [b: %s]", m.activeBranchFilter)
			}
		}
	}
	if chips.Len() == 0 {
		return ""
	}
	return titleStyle.Render(chips.String())
}

// fitTitleLeft fits the left side of the title (app + filters + the
// hiding-closed flag) to width. On overflow it drops the hiding-closed flag
// first, then truncates with an ellipsis so the leftmost, most important
// filters (repo before branch) survive longest.
func fitTitleLeft(app, filters, hideClosed string, width int) string {
	if full := app + filters + hideClosed; xansi.StringWidth(full) <= width {
		return full
	}
	if withFilters := app + filters; xansi.StringWidth(withFilters) <= width {
		return withFilters
	}
	return xansi.Truncate(app+filters, width, "…")
}

// renderQueueTitle builds line 1 of the queue header. The app name and active
// filter chips take the prominent left space; the client version is reference
// info, right-aligned and dimmed, and is the first thing dropped when the line
// is tight (then the hiding-closed flag, then filter text truncates). On a
// daemon/client version mismatch the daemon version is shown in red inside the
// parentheses ("roborev (<client>, Daemon: <daemon>)") in place of the
// right-aligned version, so the warning is prominent and self-explanatory. A
// paused queue adds a yellow [PAUSED] marker to the app name.
func (m model) renderQueueTitle() string {
	width := m.width
	if width <= 0 {
		width = 100
	}

	var app string
	if m.versionMismatch {
		daemonVersion := m.daemonVersion
		if daemonVersion == "" {
			daemonVersion = "?"
		}
		app = titleStyle.Render("roborev ("+version.Version+", ") +
			errorStyle.Render("Daemon: "+daemonVersion) +
			titleStyle.Render(")")
	} else {
		app = titleStyle.Render("roborev")
	}
	// A paused queue is a global daemon state the operator just chose. Mark it
	// in the title's leftmost, never-dropped span so it stays visible under
	// filters and in compact mode, where the status line is hidden.
	if m.status.QueuePaused {
		app += " " + warningFlashStyle.Render("[PAUSED]")
	}
	if snooze := m.activeSnooze(time.Now()); snooze != nil {
		label := "[SNOOZED until " +
			snooze.SnoozedUntil.Local().Format("Jan 02 15:04") + "]"
		app += " " + warningFlashStyle.Render(label)
	}

	filters := m.titleFilters()
	hideClosed := ""
	if m.hideClosed {
		hideClosed = titleStyle.Render(" [hiding closed]")
	}

	// Matching: right-align the dimmed version when there is room for it (with
	// at least two spaces of separation); otherwise it drops out entirely.
	if !m.versionMismatch {
		right := statusStyle.Render(version.Version)
		full := app + filters + hideClosed
		if gap := width - xansi.StringWidth(full) - xansi.StringWidth(right); gap >= 2 {
			return full + strings.Repeat(" ", gap) + right
		}
	}
	return fitTitleLeft(app, filters, hideClosed, width)
}

func (m model) renderQueueView() string {
	var b strings.Builder
	compact := m.queueCompact()

	b.WriteString(m.renderQueueTitle())
	b.WriteString("\x1b[K\n") // Clear to end of line

	if !compact {
		// Status line - use server-side aggregate counts for paginated views
		// (including multi-repo display names, scoped via an IN clause). Only
		// the "(none)" branch sentinel still loads all jobs to count locally.
		var done, closed, open int
		if m.activeBranchFilter == branchNone {
			// Client-side filtered views load all jobs, so count locally
			for _, job := range m.jobs {
				if len(m.activeRepoFilter) > 0 && !m.repoMatchesFilter(job.RepoPath) {
					continue
				}
				if m.activeBranchFilter == branchNone && job.Branch != "" {
					continue
				}
				if job.Status == storage.JobStatusDone {
					done++
					if job.Closed != nil {
						if *job.Closed {
							closed++
						} else {
							open++
						}
					}
				}
			}
		} else {
			done = m.jobStats.Done
			closed = m.jobStats.Closed
			open = m.jobStats.Open
		}
		b.WriteString(m.renderQueueStatusLine(done, closed, open))
		b.WriteString("\x1b[K\n") // Clear status line

		// Update notification on line 3 (above the table)
		if m.updateAvailable != "" {
			var updateMsg string
			if m.updateIsDevBuild {
				updateMsg = fmt.Sprintf("Dev build - latest release: %s - run 'roborev update --force'", m.updateAvailable)
			} else {
				updateMsg = fmt.Sprintf("Update available: %s - press u for notes or run 'roborev update'", m.updateAvailable)
			}
			b.WriteString(updateStyle.Render(updateMsg))
		}
		b.WriteString("\x1b[K\n") // Clear line 3
	}

	rows := m.visibleQueueRows()
	visibleSelectedIdx := visibleSelectedRowIndex(rows, m.selectedJobID)
	hasAnyPanel := anyPanelRow(rows)
	treeColor := queueColorEnabled()
	// The expand hint is cursor-contextual: it shows only when the selected row
	// is itself a panel parent, not merely because the page has panels. This is
	// the same predicate as selectedRowHasChildren (over the same rows and
	// selectedJobID), which queueHelpLines uses to reserve the help height; the
	// two must stay in lockstep so the reservation matches what we draw below.
	selectedHasChildren := visibleSelectedIdx >= 0 && rows[visibleSelectedIdx].hasChildren

	visibleRows := m.queueVisibleRows()

	// Track scroll indicator state for later
	var scrollInfo string
	start := 0
	end := 0

	if len(rows) == 0 {
		if m.loadingJobs || m.loadingMore {
			b.WriteString("Loading...")
			b.WriteString("\x1b[K\n")
		} else if len(m.activeRepoFilter) > 0 || m.hideClosed {
			b.WriteString("No jobs matching filters")
			b.WriteString("\x1b[K\n")
		} else {
			b.WriteString("No jobs in queue")
			b.WriteString("\x1b[K\n")
		}
		// Pad empty queue to fill visibleRows (minus 1 for the message we just wrote)
		// Also need header lines (2) to match non-empty case (skip in compact)
		linesWritten := 1
		padTarget := visibleRows
		if !compact {
			padTarget += 2 // +2 for header lines we skipped
		}
		for linesWritten < padTarget {
			b.WriteString("\x1b[K\n")
			linesWritten++
		}
	} else {
		// Determine which jobs to show, keeping selected item visible.
		start, end = queueWindowStart(len(rows), visibleSelectedIdx, visibleRows)

		// Determine visible columns (respects hidden columns).
		visCols := m.visibleColumns()

		// Compute per-column max content widths, using cache when data hasn't changed.
		contentWidth := m.queueContentWidths(rows, visCols, hasAnyPanel, treeColor)
		// At narrower widths, drop lower-priority columns before the identifying
		// Ref, Branch, and Repo columns are reduced to unusable fragments.
		visCols = m.queuePaneColumns(m.width, contentWidth)

		tableLines := m.renderQueueTable(rows, m.width, visibleRows, visCols, contentWidth)
		for _, line := range tableLines {
			b.WriteString(line)
			b.WriteString("\x1b[K\n")
		}

		// Pad with clear-to-end-of-line sequences to prevent ghost text
		headerLines := 0
		if !compact {
			headerLines = 2 // header + separator
		}
		jobLinesWritten := len(tableLines) - headerLines
		for jobLinesWritten < visibleRows {
			b.WriteString("\x1b[K\n")
			jobLinesWritten++
		}

		// Build scroll indicator if needed
		if len(rows) > visibleRows || m.hasMore || m.loadingMore {
			if m.loadingMore {
				scrollInfo = fmt.Sprintf("[showing %d-%d of %d] Loading more...", start+1, end, len(rows))
			} else if m.hasMore && m.activeBranchFilter != branchNone {
				scrollInfo = fmt.Sprintf("[showing %d-%d of %d+] scroll down to load more", start+1, end, len(rows))
			} else if len(rows) > visibleRows {
				scrollInfo = fmt.Sprintf("[showing %d-%d of %d]", start+1, end, len(rows))
			}
		}
	}

	if !compact {
		// Always emit scroll indicator line (blank if no scroll info) to maintain consistent height
		if scrollInfo != "" {
			b.WriteString(statusStyle.Render(scrollInfo))
		}
		b.WriteString("\x1b[K\n") // Clear scroll indicator line

		// Status line: flash message (temporary)
		if flash := m.renderFlash(viewQueue); flash != "" {
			b.WriteString(flash)
		}
		b.WriteString("\x1b[K\n") // Clear to end of line

		// Help. The expand hint is appended only when the selected row is a
		// panel parent (cursor-contextual), so a panel-free page is unchanged.
		helpRows := m.queueHelpRows()
		if selectedHasChildren {
			helpRows = withExpandHint(helpRows)
		}
		b.WriteString(renderHelpTable(helpRows, m.width))
	}

	output := b.String()
	if compact {
		// Trim trailing newline to avoid layout overflow (compact has no
		// help footer to consume the final line).
		output = strings.TrimSuffix(output, "\n")
	}
	output += "\x1b[K" // Clear to end of line (no newline at end)
	output += "\x1b[J" // Clear to end of screen to prevent artifacts

	return output
}

// queueContentWidths computes each visible column's max content width across
// rows, using m.queueColCache (keyed on m.queueColGen) to skip recomputation
// when nothing has changed since the last render. Callers should always pass
// the full m.visibleColumns() set as visCols (never a pane-narrowed subset)
// so the cached map stays a superset that any pane-specific column subset
// can safely index into.
func (m model) queueContentWidths(rows []queueRow, visCols []int, hasAnyPanel, treeColor bool) map[int]int {
	allHeaders := [colCount]string{"", "JobID", "Ref", "Branch", "Repo", "Agent", "Review Type", "Queued", "Elapsed", "Status", "P/F", "Closed", "Session", "Req Model", "Req Provider", "Cost", "Reasoning"}
	var contentWidth map[int]int
	if m.queueColCache.gen == m.queueColGen {
		contentWidth = m.queueColCache.contentWidths
	} else {
		contentWidth = make(map[int]int, len(visCols))
		for _, c := range visCols {
			contentWidth[c] = lipgloss.Width(allHeaders[c])
		}
		for i := range rows {
			fullRow := m.queueFullRowCells(rows[i], hasAnyPanel, treeColor)
			for _, c := range visCols {
				contentWidth[c] = max(contentWidth[c], lipgloss.Width(fullRow[c]))
			}
		}
		m.queueColCache.gen = m.queueColGen
		m.queueColCache.contentWidths = contentWidth
	}
	return contentWidth
}

// renderQueueTable renders the queue table (header row, separator when
// borders are enabled, and the windowed data rows) sized to width, and
// returns the rendered lines with no "\x1b[K" escapes — callers append their
// own clear-to-end-of-line sequences as they write each line out. Windowing
// is computed internally via queueWindowStart/visibleSelectedRowIndex,
// exactly as the inline code in renderQueueView did before extraction.
func (m model) renderQueueTable(rows []queueRow, width, visibleRows int, visCols []int, contentWidth map[int]int) []string {
	compact := m.queueCompact()
	hasAnyPanel := anyPanelRow(rows)
	treeColor := queueColorEnabled()
	allHeaders := [colCount]string{"", "JobID", "Ref", "Branch", "Repo", "Agent", "Review Type", "Queued", "Elapsed", "Status", "P/F", "Closed", "Session", "Req Model", "Req Provider", "Cost", "Reasoning"}

	visibleSelectedIdx := visibleSelectedRowIndex(rows, m.selectedJobID)
	start, end := queueWindowStart(len(rows), visibleSelectedIdx, visibleRows)

	// Compute column widths: fixed columns get their natural size,
	// flexible columns (Ref, Branch, Repo) absorb excess space.
	bordersOn := m.colBordersOn
	borderColor := adaptiveColor("248", "242")

	// Spacing per column: non-first, non-sel columns get 1 char of spacing
	// (either PaddingRight or border ▕ + PaddingLeft = 2 chars)
	spacing := func(tableCol int, logCol int) int {
		if logCol == colSel || tableCol == 0 {
			return 0
		}
		if bordersOn {
			return 2 // ▕ + PaddingLeft(1)
		}
		return 1 // PaddingRight(1)
	}

	// Fixed-width columns: exact sizes (content + padding, not counting inter-column spacing)
	fixedWidth := map[int]int{
		colSel:               2,
		colJobID:             max(contentWidth[colJobID], 5),
		colStatus:            max(contentWidth[colStatus], 6), // "Status" header = 6, auto-sizes to content
		colQueued:            12,
		colElapsed:           8,
		colPF:                3,                                                    // "P/F" header = 3
		colHandled:           max(contentWidth[colHandled], 6),                     // "Closed" header = 6
		colAgent:             min(max(contentWidth[colAgent], 5), 12),              // "Agent" header = 5, cap at 12
		colReviewType:        min(max(contentWidth[colReviewType], 11), 20),        // "Review Type" header = 11, cap at 20
		colSessionID:         min(max(contentWidth[colSessionID], 7), 12),          // "Session" header = 7, cap at 12
		colRequestedModel:    min(max(contentWidth[colRequestedModel], 9), 24),     // "Req Model" header = 9
		colRequestedProvider: min(max(contentWidth[colRequestedProvider], 12), 24), // "Req Provider" header = 12
		colReasoning:         min(max(contentWidth[colReasoning], 9), 16),
		colCost:              max(contentWidth[colCost], 4), // "Cost" header = 4
	}

	// Flexible columns absorb excess space
	flexCols := []int{colRef, colBranch, colRepo}

	// Compute total fixed consumption
	totalFixed := 0
	for ti, c := range visCols {
		sp := spacing(ti, c)
		if fw, ok := fixedWidth[c]; ok {
			totalFixed += fw + sp
		} else {
			totalFixed += sp // spacing is always consumed
		}
	}

	remaining := width - totalFixed
	// Distribute remaining space among flex columns.
	// colWidths stores content-only width; StyleFunc adds spacing via
	// s.Width(w + spacing(col, logicalCol)) so the total column width
	// on screen = content width + inter-column spacing.
	colWidths := make(map[int]int, len(visCols))
	maps.Copy(colWidths, fixedWidth)

	// Build visible-only flex list once. A flex column only absorbs excess
	// space when it's both user-visible (not in hiddenColumns) and actually
	// present in visCols — callers such as renderQueuePaneBody may pass a
	// pane-narrowed visCols that drops colBranch/colRepo while they remain
	// user-visible; without the visCols membership check, remaining width
	// would be split across those phantom (unrendered) columns and starve
	// the ones actually on screen.
	inVisCols := make(map[int]bool, len(visCols))
	for _, c := range visCols {
		inVisCols[c] = true
	}
	var visFlex []int
	for _, c := range flexCols {
		if !m.hiddenColumns[c] && inVisCols[c] {
			visFlex = append(visFlex, c)
		}
	}

	if len(visFlex) > 0 && remaining > 0 {
		// Two-phase distribution: first guarantee each flex
		// column at least min(contentWidth, equalShare), then
		// distribute surplus proportionally to remaining
		// content headroom. This prevents a single wide column
		// from starving narrower ones.
		equalShare := remaining / len(visFlex)

		// Phase 1: allocate floors.
		distributed := 0
		for _, c := range visFlex {
			floor := min(contentWidth[c], equalShare)
			colWidths[c] = max(floor, 1)
			distributed += colWidths[c]
		}

		// Drain overshoot from max(...,1) inflation when
		// remaining < len(visFlex).
		if distributed > remaining {
			drainFlexOverflow(visFlex, colWidths, distributed-remaining)
			distributed = remaining
		}

		// Compute headroom from actual allocated widths.
		totalHeadroom := 0
		headroom := make(map[int]int, len(visFlex))
		for _, c := range visFlex {
			h := contentWidth[c] - colWidths[c]
			if h > 0 {
				headroom[c] = h
				totalHeadroom += h
			}
		}

		// Phase 2: distribute surplus proportionally to
		// content headroom (columns already at content width
		// have zero headroom and get nothing extra).
		surplus := remaining - distributed
		if surplus > 0 && totalHeadroom > 0 {
			phase2 := 0
			for i, c := range visFlex {
				var extra int
				if i == len(visFlex)-1 {
					extra = surplus - phase2
				} else {
					extra = surplus * headroom[c] / totalHeadroom
				}
				colWidths[c] += extra
				phase2 += extra
			}
		} else if surplus > 0 {
			// All columns at content width — distribute
			// remaining space equally.
			for i, c := range visFlex {
				extra := surplus / (len(visFlex) - i)
				colWidths[c] += extra
				surplus -= extra
			}
		}
	} else if len(visFlex) > 0 {
		// No remaining space: give flex columns 1 char each to
		// avoid overflow at very narrow terminal widths.
		for _, c := range visFlex {
			colWidths[c] = 1
		}
	}

	// Build visible rows for the window
	windowRows := rows[start:end]
	tableRows := make([][]string, 0, end-start)
	for i := range windowRows {
		sel := "  "
		if start+i == visibleSelectedIdx {
			sel = "> "
		}
		fullRow := m.queueFullRowCells(windowRows[i], hasAnyPanel, treeColor)
		fullRow[colSel] = sel

		row := make([]string, len(visCols))
		for vi, c := range visCols {
			row[vi] = fullRow[c]
		}
		tableRows = append(tableRows, row)
	}

	// Compute the selected row index within the visible window
	selectedWindowIdx := visibleSelectedIdx - start

	// Find the last visible table column index (for padding logic)
	lastVisCol := len(visCols) - 1

	// Group banding: a panel parent and its members share one zebra band so
	// the nesting reads as a group. Only computed (and applied) when the page
	// has panels, so a panel-free page keeps its original un-banded bytes.
	var bands []bool
	if hasAnyPanel {
		bands = groupBanding(rows)
	}

	t := table.New().
		BorderTop(false).
		BorderBottom(false).
		BorderLeft(false).
		BorderRight(false).
		BorderColumn(false).
		BorderRow(false).
		BorderHeader(!compact).
		Border(lipgloss.Border{
			Top:    "─",
			Bottom: "─",
			Middle: "─",
		}).
		Width(width).
		Wrap(false).
		StyleFunc(func(row, col int) lipgloss.Style {
			s := lipgloss.NewStyle()

			// Map table col index to logical column
			logicalCol := colSel
			if col >= 0 && col < len(visCols) {
				logicalCol = visCols[col]
			}

			// Inter-column spacing: non-sel, non-first columns get border or padding
			if logicalCol != colSel && col > 0 {
				if bordersOn {
					s = s.Border(lipgloss.Border{Left: "▕"}, false, false, false, true).
						BorderForeground(borderColor).PaddingLeft(1)
				} else if col < lastVisCol {
					s = s.PaddingRight(1)
				}
			}

			// Set explicit width for all columns (includes spacing)
			w := colWidths[logicalCol]
			if w > 0 {
				s = s.Width(w + spacing(col, logicalCol))
			}

			// Right-align elapsed column
			if logicalCol == colElapsed {
				s = s.Align(lipgloss.Right)
			}

			// Header row styling
			if row == table.HeaderRow {
				return s.Foreground(adaptiveColor("242", "246"))
			}

			// Selection highlighting — uniform background, no per-cell coloring
			if row == selectedWindowIdx {
				bg := adaptiveColor("153", "24")
				s = s.Background(bg)
				if bordersOn {
					s = s.BorderBackground(bg)
				}
				return s
			}

			// Group banding for non-selected rows: every other panel group
			// gets a subtle background so a parent and its members read as
			// one block. Foreground per-cell coloring is applied on top.
			if absIdx := start + row; bands != nil && absIdx < len(bands) && bands[absIdx] {
				bg := adaptiveColor("254", "236") // subtle zebra band
				s = s.Background(bg)
				if bordersOn {
					s = s.BorderBackground(bg)
				}
			}

			// Per-cell coloring for non-selected rows
			if row >= 0 && row < len(windowRows) {
				job := windowRows[row].job
				switch logicalCol {
				case colStatus:
					if c := statusColor(job.Status); c != nil {
						s = s.Foreground(c)
					}
				case colPF:
					if c := verdictColor(job.Verdict); c != nil {
						s = s.Foreground(c)
					}
				case colHandled:
					if job.Closed != nil {
						if *job.Closed {
							s = s.Foreground(closedStyle.GetForeground())
						} else {
							s = s.Foreground(queuedStyle.GetForeground())
						}
					}
				}
			}
			return s
		})

	// Always set headers — lipgloss table drops the last data row
	// when Headers() is not called.
	headers := make([]string, len(visCols))
	if !compact {
		for vi, c := range visCols {
			headers[vi] = allHeaders[c]
		}
	}
	t = t.Headers(headers...)
	t = t.Rows(tableRows...)

	tableStr := t.Render()

	// In compact mode, strip the empty header line we added as a
	// workaround (it renders as a row of spaces).
	if compact {
		if idx := strings.Index(tableStr, "\n"); idx >= 0 {
			tableStr = tableStr[idx+1:]
		}
	}

	return strings.Split(tableStr, "\n")
}

// jobCells returns plain text cell values for a job row.
// Order: ref, branch, repo, agent, review type, queued, elapsed, status, pf,
// handled, session, requested model, requested provider, cost.
func (m model) jobCells(job storage.ReviewJob) []string {
	ref := shortJobRef(job)
	if !config.IsDefaultReviewType(job.ReviewType) {
		ref = ref + " [" + job.ReviewType + "]"
	}

	branch := m.getBranchForJob(job)

	repo := m.getDisplayName(job.RepoPath, job.RepoName)
	if m.status.MachineID != nil && job.SourceMachineID != nil && *job.SourceMachineID != *m.status.MachineID {
		repo += " [R]"
	}

	agentName := job.Agent
	if agentName == "claude-code" {
		agentName = "claude"
	}
	reviewType := displayReviewType(job.ReviewType, job.PanelRole)

	enqueued := job.EnqueuedAt.Local().Format("Jan 02 15:04")

	elapsed := m.jobElapsedCell(job)

	status := statusLabel(job)

	verdict := "-"
	if job.Verdict != nil {
		verdict = *job.Verdict
	}

	handled := ""
	if job.PanelRole != storage.PanelRoleMember && job.Closed != nil {
		if *job.Closed {
			handled = "yes"
		} else {
			handled = "no"
		}
	}

	sessionID := stripControlChars(job.SessionID)
	if runes := []rune(sessionID); len(runes) > 12 {
		sessionID = string(runes[:12])
	}

	requestedModel := stripControlChars(job.RequestedModel)
	requestedProvider := stripControlChars(job.RequestedProvider)

	cost := m.jobCostCell(job)

	return []string{ref, branch, repo, agentName, reviewType, enqueued, elapsed, status, verdict, handled, sessionID, requestedModel, requestedProvider, cost, displayReasoning(job.Reasoning)}
}

// displayReviewType returns the canonical label shown in the TUI. Synthesis
// rows identify the panel, while legacy aliases and older ordinary jobs with
// no stored value are standard reviews.
func displayReviewType(reviewType, panelRole string) string {
	if panelRole == storage.PanelRoleSynthesis {
		return "panel"
	}
	if config.IsDefaultReviewType(reviewType) {
		return config.ReviewTypeDefault
	}
	return stripControlChars(reviewType)
}

func (m model) jobElapsedCell(job storage.ReviewJob) string {
	startedAt, ok := m.jobElapsedStart(job)
	if !ok {
		return ""
	}
	if job.FinishedAt != nil {
		return job.FinishedAt.Sub(startedAt).Round(time.Second).String()
	}
	return time.Since(startedAt).Round(time.Second).String()
}

func (m model) jobElapsedStart(job storage.ReviewJob) (time.Time, bool) {
	startedAt := time.Time{}
	hasStartedAt := false
	if job.StartedAt != nil {
		startedAt = *job.StartedAt
		hasStartedAt = true
	}

	if !job.IsSynthesisJob() || job.PanelRunUUID == nil {
		return startedAt, hasStartedAt
	}

	if job.PanelSummary != nil && job.PanelSummary.FirstStartedAt != nil {
		startedAt = *job.PanelSummary.FirstStartedAt
		hasStartedAt = true
	}
	for _, member := range m.panelMembers[*job.PanelRunUUID] {
		if member.StartedAt == nil {
			continue
		}
		if !hasStartedAt || member.StartedAt.Before(startedAt) {
			startedAt = *member.StartedAt
			hasStartedAt = true
		}
	}
	return startedAt, hasStartedAt
}

// jobCostCell renders the stored priced estimate for a row. For a panel parent,
// it adds known member costs as a lower-bound aggregate; unpriced members do not
// suppress costs from members that did report pricing.
func (m model) jobCostCell(job storage.ReviewJob) string {
	total := 0.0
	hasCost := false
	if tu := tokens.ParseJSON(job.TokenUsage); tu != nil && tu.HasCost {
		total += tu.CostUSD
		hasCost = true
	}

	if !job.IsSynthesisJob() || job.PanelRunUUID == nil {
		if !hasCost {
			return ""
		}
		return tokens.Usage{CostUSD: total, HasCost: true}.FormatCost()
	}

	members := m.panelMembers[*job.PanelRunUUID]
	if len(members) == 0 {
		if job.PanelSummary != nil &&
			(job.PanelSummary.MembersCostComplete || job.PanelSummary.MembersWithCost > 0) {
			return tokens.Usage{CostUSD: total + job.PanelSummary.MembersCostUSD, HasCost: true}.FormatCost()
		}
		if !hasCost {
			return ""
		}
		return tokens.Usage{CostUSD: total, HasCost: true}.FormatCost()
	}

	memberTotal := 0.0
	for _, member := range members {
		tu := tokens.ParseJSON(member.TokenUsage)
		if tu == nil || !tu.HasCost {
			if job.PanelSummary != nil && job.PanelSummary.MembersCostComplete {
				return tokens.Usage{CostUSD: total + job.PanelSummary.MembersCostUSD, HasCost: true}.FormatCost()
			}
			continue
		}
		memberTotal += tu.CostUSD
		hasCost = true
	}

	if !hasCost {
		return ""
	}
	return tokens.Usage{CostUSD: total + memberTotal, HasCost: true}.FormatCost()
}

// statusLabel returns a capitalized display label for the job status.
func statusLabel(job storage.ReviewJob) string {
	switch job.Status {
	case storage.JobStatusQueued:
		return "Queued"
	case storage.JobStatusRunning:
		return "Running"
	case storage.JobStatusFailed:
		return "Error"
	case storage.JobStatusCanceled:
		return "Canceled"
	case storage.JobStatusDone, storage.JobStatusApplied,
		storage.JobStatusRebased:
		return "Done"
	case storage.JobStatusSkipped:
		if job.SkipReason != "" {
			return "skipped: " + truncateReason(job.SkipReason, 40)
		}
		return "skipped"
	default:
		return string(job.Status)
	}
}

// truncateReason returns reason truncated to maxLen runes, with an ellipsis
// appended when it had to be cut.
func truncateReason(reason string, maxLen int) string {
	if maxLen <= 0 {
		return reason
	}
	runes := []rune(reason)
	if len(runes) <= maxLen {
		return reason
	}
	if maxLen <= 1 {
		return string(runes[:maxLen])
	}
	return string(runes[:maxLen-1]) + "…"
}

// statusColor returns the foreground color for the Status column.
func statusColor(
	status storage.JobStatus,
) color.Color {
	switch status {
	case storage.JobStatusQueued:
		return queuedStyle.GetForeground()
	case storage.JobStatusRunning:
		return runningStyle.GetForeground()
	case storage.JobStatusDone, storage.JobStatusApplied,
		storage.JobStatusRebased:
		return doneStyle.GetForeground()
	case storage.JobStatusFailed:
		return failedStyle.GetForeground()
	case storage.JobStatusCanceled:
		return canceledStyle.GetForeground()
	case storage.JobStatusSkipped:
		return canceledStyle.GetForeground()
	default:
		return nil
	}
}

// verdictColor returns the foreground color for the P/F column.
// Returns nil when no color should be applied (nil verdict).
func verdictColor(
	verdict *string,
) color.Color {
	if verdict == nil {
		return nil
	}
	if *verdict == "P" {
		return passStyle.GetForeground()
	}
	return failStyle.GetForeground()
}

// commandLineForJob computes the representative agent command line from job parameters.
// Returns empty string if the agent is not available.
func commandLineForJob(job *storage.ReviewJob) string {
	if job == nil {
		return ""
	}
	// Prefer the actual command line saved by the daemon worker.
	if job.CommandLine != "" {
		return stripControlChars(job.CommandLine)
	}
	// Fallback: reconstruct locally (may not reflect daemon config).
	a, err := agent.Get(job.Agent)
	if err != nil {
		return ""
	}
	reasoning := strings.ToLower(strings.TrimSpace(job.Reasoning))
	if reasoning == "" {
		reasoning = "thorough"
	}
	cmd := a.WithReasoning(agent.ParseReasoningLevel(reasoning)).WithAgentic(job.Agentic).WithModel(job.Model).CommandLine()
	return stripControlChars(cmd)
}

// stripControlChars removes all control characters including C0 (\x00-\x1f),
// DEL (\x7f), and C1 (\x80-\x9f) from a string to prevent terminal escape
// injection and line/tab spoofing in single-line display contexts.
func stripControlChars(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// migrateColumnConfig resets stale column_order and hidden_columns
// entries so users pick up the current default layout. Returns true
// if the config was modified and should be saved.
func migrateColumnConfig(cfg *config.Config) bool {
	dirty := false
	// Pre-rename config: "addressed" → reset
	if slices.Contains(cfg.ColumnOrder, "addressed") {
		cfg.ColumnOrder = nil
		dirty = true
	}
	if slices.Contains(cfg.HiddenColumns, "addressed") {
		cfg.HiddenColumns = nil
		dirty = true
	}
	// Old default orders → reset
	oldDefaults := [][]string{
		// status before queued (pre-rename)
		{
			"ref", "branch", "repo", "agent",
			"status", "queued", "elapsed", "closed",
		},
		// combined status without pf column
		{
			"ref", "branch", "repo", "agent",
			"queued", "elapsed", "status", "closed",
		},
	}
	for _, old := range oldDefaults {
		if slices.Equal(cfg.ColumnOrder, old) {
			cfg.ColumnOrder = nil
			dirty = true
			break
		}
	}

	// Version 1: backfill only the columns introduced in d2d671f6.
	// Do not touch session_id — it was already a default-hidden
	// column, so its absence from the list is a deliberate choice.
	if cfg.ColumnConfigVersion < 1 &&
		len(cfg.HiddenColumns) > 0 &&
		cfg.HiddenColumns[0] != config.HiddenColumnsNoneSentinel {
		v1NewColumns := []int{colRequestedModel, colRequestedProvider}
		for _, col := range v1NewColumns {
			name := columnConfigNames[col]
			if !slices.Contains(cfg.HiddenColumns, name) {
				cfg.HiddenColumns = append(
					cfg.HiddenColumns, name,
				)
			}
		}
		cfg.ColumnConfigVersion = 1
		dirty = true
	}

	return dirty
}

// toggleableColumns is the ordered list of columns the user can show/hide.
// colSel and colJobID are always visible and not included here.
var toggleableColumns = []int{colRef, colBranch, colRepo, colAgent, colReasoning, colReviewType, colQueued, colElapsed, colStatus, colPF, colHandled, colCost, colSessionID, colRequestedModel, colRequestedProvider}

// columnNames maps column constants to display names.
var columnNames = map[int]string{
	colRef:               "Ref",
	colBranch:            "Branch",
	colRepo:              "Repo",
	colAgent:             "Agent",
	colReviewType:        "Review Type",
	colStatus:            "Status",
	colQueued:            "Queued",
	colElapsed:           "Elapsed",
	colPF:                "P/F",
	colHandled:           "Closed",
	colSessionID:         "Session",
	colRequestedModel:    "Req Model",
	colRequestedProvider: "Req Provider",
	colCost:              "Cost",
	colReasoning:         "Reasoning",
}

// columnConfigNames maps column constants to config file names (lowercase).
var columnConfigNames = map[int]string{
	colRef:               "ref",
	colBranch:            "branch",
	colRepo:              "repo",
	colAgent:             "agent",
	colReviewType:        "review_type",
	colStatus:            "status",
	colQueued:            "queued",
	colElapsed:           "elapsed",
	colPF:                "pf",
	colHandled:           "closed",
	colSessionID:         "session_id",
	colRequestedModel:    "requested_model",
	colRequestedProvider: "requested_provider",
	colCost:              "cost",
	colReasoning:         "reasoning",
}

// drainFlexOverflow reduces flex column widths to absorb overflow,
// shrinking the widest column first, repeating until overflow is zero
// or all columns are at minimum width 1.
func drainFlexOverflow(
	cols []int, widths map[int]int, overflow int,
) {
	for overflow > 0 {
		widest := -1
		for _, c := range cols {
			if widths[c] > 1 && (widest < 0 || widths[c] > widths[widest]) {
				widest = c
			}
		}
		if widest < 0 {
			break
		}
		reduce := min(overflow, widths[widest]-1)
		widths[widest] -= reduce
		overflow -= reduce
	}
}

// lookupDisplayName returns the display name for a column constant from the given map.
func lookupDisplayName(col int, displayNames map[int]string) string {
	if name, ok := displayNames[col]; ok {
		return name
	}
	return "?"
}

// columnDisplayName returns the display name for a queue column constant.
func columnDisplayName(col int) string {
	return lookupDisplayName(col, columnNames)
}

// defaultHiddenColumns lists columns that are hidden by default.
// Users can enable them via the column options modal.
var defaultHiddenColumns = map[int]bool{
	colSessionID:         true,
	colRequestedModel:    true,
	colRequestedProvider: true,
}

// parseHiddenColumns converts config hidden_columns strings to column ID set.
// When names is empty (no user config), defaultHiddenColumns are applied.
// When names contains only the sentinel "_", the user explicitly has nothing hidden.
// Otherwise, only the listed columns are hidden.
func parseHiddenColumns(names []string) map[int]bool {
	result := map[int]bool{}
	if len(names) == 0 {
		maps.Copy(result, defaultHiddenColumns)
		return result
	}
	if len(names) == 1 && names[0] == config.HiddenColumnsNoneSentinel {
		return result
	}
	lookup := map[string]int{}
	for id, name := range columnConfigNames {
		lookup[name] = id
	}
	for _, n := range names {
		if id, ok := lookup[strings.ToLower(n)]; ok {
			result[id] = true
		}
	}
	return result
}

// hiddenColumnsToNames converts a hidden column ID set to config names.
// When nothing is hidden, returns the sentinel ["_"] to distinguish
// from an unconfigured (nil) slice.
func hiddenColumnsToNames(hidden map[int]bool) []string {
	var names []string
	// Maintain stable order
	for _, col := range toggleableColumns {
		if hidden[col] {
			names = append(names, columnConfigNames[col])
		}
	}
	if len(names) == 0 {
		return []string{config.HiddenColumnsNoneSentinel}
	}
	return names
}

// resolveColumnOrder converts config names to ordered column IDs using the given
// configNames map. Any columns from defaults not in names are appended at the end.
func resolveColumnOrder(names []string, configNames map[int]string, defaults []int) []int {
	if len(names) == 0 {
		result := make([]int, len(defaults))
		copy(result, defaults)
		return result
	}
	lookup := map[string]int{}
	for id, name := range configNames {
		lookup[name] = id
	}
	seen := map[int]bool{}
	var order []int
	for _, n := range names {
		if id, ok := lookup[strings.ToLower(n)]; ok && !seen[id] {
			order = append(order, id)
			seen[id] = true
		}
	}
	for _, col := range defaults {
		if !seen[col] {
			order = append(order, col)
		}
	}
	return order
}

// serializeColumnOrder converts ordered column IDs to config names.
func serializeColumnOrder(order []int, configNames map[int]string) []string {
	names := make([]string, 0, len(order))
	for _, col := range order {
		if name, ok := configNames[col]; ok {
			names = append(names, name)
		}
	}
	return names
}

// parseColumnOrder converts config names to ordered queue column IDs.
func parseColumnOrder(names []string) []int {
	return resolveColumnOrder(names, columnConfigNames, toggleableColumns)
}

// columnOrderToNames converts ordered queue column IDs to config names.
func columnOrderToNames(order []int) []string {
	return serializeColumnOrder(order, columnConfigNames)
}

// visibleColumns returns the ordered list of column indices to display,
// always including colSel and colJobID, plus any non-hidden toggleable columns.
func (m model) visibleColumns() []int {
	cols := []int{colSel, colJobID}
	for _, c := range m.columnOrder {
		if !m.hiddenColumns[c] {
			cols = append(cols, c)
		}
	}
	return cols
}

// saveColumnOptions persists table preferences and advanced task workflow state.
// Column order is only saved when it differs from the built-in default,
// so future default changes take effect for users who haven't customized.
func (m model) saveColumnOptions() tea.Cmd {
	hidden := hiddenColumnsToNames(m.hiddenColumns)
	borders := m.colBordersOn
	mouseEnabled := m.mouseEnabled
	tasksEnabled := m.tasksWorkflowEnabled()
	var colOrd []string
	if !slices.Equal(m.columnOrder, toggleableColumns) {
		colOrd = columnOrderToNames(m.columnOrder)
	}
	var taskColOrd []string
	if !slices.Equal(m.taskColumnOrder, taskToggleableColumns) {
		taskColOrd = taskColumnOrderToNames(m.taskColumnOrder)
	}
	return func() tea.Msg {
		cfg, err := config.LoadGlobal()
		if err != nil {
			return configSaveErrMsg{err: fmt.Errorf("load config: %w", err)}
		}
		cfg.HiddenColumns = hidden
		cfg.ColumnBorders = borders
		cfg.MouseEnabled = mouseEnabled
		cfg.ColumnOrder = colOrd
		cfg.TaskColumnOrder = taskColOrd
		cfg.Advanced.TasksEnabled = tasksEnabled
		cfg.ColumnConfigVersion = 1
		if err := config.SaveGlobal(cfg); err != nil {
			return configSaveErrMsg{err: fmt.Errorf("save config: %w", err)}
		}
		return nil
	}
}

// renderColumnOptionsView renders the column toggle modal.
func (m model) renderColumnOptionsView() string {
	var b strings.Builder

	b.WriteString(titleStyle.Render("Table Options"))
	b.WriteString("\n\n")

	for i, opt := range m.colOptionsList {
		check := "[ ]"
		if opt.enabled {
			check = "[x]"
		}
		prefix := "  "
		line := fmt.Sprintf("%s %s", check, opt.name)
		if i == m.colOptionsIdx {
			prefix = "> "
			line = selectedStyle.Render(line)
		}
		// Separator before settings/toggles
		if opt.id == colOptionBorders && i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(prefix)
		b.WriteString(line)
		b.WriteString("\x1b[K\n")
	}

	b.WriteString("\n")
	helpRows := [][]helpItem{
		{{"↑/↓", "navigate"}, {"j/k", "reorder"}, {"space", "toggle"}, {"esc", "close"}},
	}
	b.WriteString(renderHelpTable(helpRows, m.width))

	return b.String()
}
