package git

import (
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// CommitInfo holds metadata about a commit
type CommitInfo struct {
	SHA       string
	Author    string
	Subject   string
	Timestamp time.Time
}

// GetCommitInfo retrieves commit metadata
func GetCommitInfo(repoPath, sha string) (*CommitInfo, error) {
	// Format: %H|%an|%s|%aI
	cmd := exec.Command("git", "log", "-1", "--format=%H|%an|%s|%aI", sha)
	cmd.Dir = repoPath

	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git log: %w", err)
	}

	parts := strings.SplitN(strings.TrimSpace(string(out)), "|", 4)
	if len(parts) < 4 {
		return nil, fmt.Errorf("unexpected git log output: %s", out)
	}

	ts, err := time.Parse(time.RFC3339, parts[3])
	if err != nil {
		ts = time.Now() // Fallback
	}

	return &CommitInfo{
		SHA:       parts[0],
		Author:    parts[1],
		Subject:   parts[2],
		Timestamp: ts,
	}, nil
}

// GetDiff returns the full diff for a commit
func GetDiff(repoPath, sha string) (string, error) {
	cmd := exec.Command("git", "show", sha, "--format=")
	cmd.Dir = repoPath

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git show: %w", err)
	}

	return string(out), nil
}

// GetFilesChanged returns the list of files changed in a commit
func GetFilesChanged(repoPath, sha string) ([]string, error) {
	cmd := exec.Command("git", "diff-tree", "--no-commit-id", "--name-only", "-r", sha)
	cmd.Dir = repoPath

	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git diff-tree: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var files []string
	for _, line := range lines {
		if line != "" {
			files = append(files, line)
		}
	}

	return files, nil
}

// GetStat returns the stat summary for a commit
func GetStat(repoPath, sha string) (string, error) {
	cmd := exec.Command("git", "show", "--stat", sha, "--format=")
	cmd.Dir = repoPath

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git show --stat: %w", err)
	}

	return string(out), nil
}

// ResolveSHA resolves a ref (like HEAD) to a full SHA
func ResolveSHA(repoPath, ref string) (string, error) {
	cmd := exec.Command("git", "rev-parse", ref)
	cmd.Dir = repoPath

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse: %w", err)
	}

	return strings.TrimSpace(string(out)), nil
}

// GetRepoRoot returns the root directory of the git repository
func GetRepoRoot(path string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = path

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse --show-toplevel: %w", err)
	}

	return strings.TrimSpace(string(out)), nil
}

// ReadFile reads a file at a specific commit
func ReadFile(repoPath, sha, filePath string) ([]byte, error) {
	cmd := exec.Command("git", "show", fmt.Sprintf("%s:%s", sha, filePath))
	cmd.Dir = repoPath

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git show %s:%s: %s", sha, filePath, stderr.String())
	}

	return stdout.Bytes(), nil
}

// GetParentCommits returns the N commits before the given commit (not including it)
// Returns commits in reverse chronological order (most recent parent first)
func GetParentCommits(repoPath, sha string, count int) ([]string, error) {
	// Use git log to get parent commits, skipping the commit itself
	cmd := exec.Command("git", "log", "--format=%H", "-n", fmt.Sprintf("%d", count), "--skip=1", sha)
	cmd.Dir = repoPath

	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git log: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var commits []string
	for _, line := range lines {
		if line != "" {
			commits = append(commits, line)
		}
	}

	return commits, nil
}

// IsRange returns true if the ref is a range (contains "..")
func IsRange(ref string) bool {
	return strings.Contains(ref, "..")
}

// ParseRange splits a range ref into start and end
func ParseRange(ref string) (start, end string, ok bool) {
	parts := strings.SplitN(ref, "..", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// GetRangeCommits returns all commits in a range (oldest first)
func GetRangeCommits(repoPath, rangeRef string) ([]string, error) {
	cmd := exec.Command("git", "log", "--format=%H", "--reverse", rangeRef)
	cmd.Dir = repoPath

	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git log range: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var commits []string
	for _, line := range lines {
		if line != "" {
			commits = append(commits, line)
		}
	}

	return commits, nil
}

// GetRangeDiff returns the combined diff for a range
func GetRangeDiff(repoPath, rangeRef string) (string, error) {
	cmd := exec.Command("git", "diff", rangeRef)
	cmd.Dir = repoPath

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git diff range: %w", err)
	}

	return string(out), nil
}

// GetRangeStart returns the start commit (first parent before range) for context lookup
func GetRangeStart(repoPath, rangeRef string) (string, error) {
	start, _, ok := ParseRange(rangeRef)
	if !ok {
		return "", fmt.Errorf("invalid range: %s", rangeRef)
	}

	// Resolve the start ref
	return ResolveSHA(repoPath, start)
}

// GetHooksPath returns the path to the hooks directory, respecting core.hooksPath
func GetHooksPath(repoPath string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--git-path", "hooks")
	cmd.Dir = repoPath

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse --git-path hooks: %w", err)
	}

	hooksPath := strings.TrimSpace(string(out))

	// If the path is relative, make it absolute relative to repoPath
	if !filepath.IsAbs(hooksPath) {
		hooksPath = filepath.Join(repoPath, hooksPath)
	}

	return hooksPath, nil
}
