package storage

import (
	"database/sql"
	"path/filepath"
	"time"
)

// GetOrCreateRepo finds or creates a repo by its root path
func (db *DB) GetOrCreateRepo(rootPath string) (*Repo, error) {
	// Normalize path
	absPath, err := filepath.Abs(rootPath)
	if err != nil {
		return nil, err
	}

	// Try to find existing
	var repo Repo
	var createdAt string
	err = db.QueryRow(`SELECT id, root_path, name, created_at FROM repos WHERE root_path = ?`, absPath).
		Scan(&repo.ID, &repo.RootPath, &repo.Name, &createdAt)
	if err == nil {
		repo.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		return &repo, nil
	}
	if err != sql.ErrNoRows {
		return nil, err
	}

	// Create new
	name := filepath.Base(absPath)
	result, err := db.Exec(`INSERT INTO repos (root_path, name) VALUES (?, ?)`, absPath, name)
	if err != nil {
		return nil, err
	}

	id, _ := result.LastInsertId()
	return &Repo{
		ID:        id,
		RootPath:  absPath,
		Name:      name,
		CreatedAt: time.Now(),
	}, nil
}

// GetRepoByPath returns a repo by its path
func (db *DB) GetRepoByPath(rootPath string) (*Repo, error) {
	absPath, err := filepath.Abs(rootPath)
	if err != nil {
		return nil, err
	}

	var repo Repo
	var createdAt string
	err = db.QueryRow(`SELECT id, root_path, name, created_at FROM repos WHERE root_path = ?`, absPath).
		Scan(&repo.ID, &repo.RootPath, &repo.Name, &createdAt)
	if err != nil {
		return nil, err
	}
	repo.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	return &repo, nil
}

// RepoWithCount represents a repo with its total job count
type RepoWithCount struct {
	Name     string `json:"name"`
	RootPath string `json:"root_path"`
	Count    int    `json:"count"`
}

// ListReposWithReviewCounts returns all repos with their total job counts
func (db *DB) ListReposWithReviewCounts() ([]RepoWithCount, int, error) {
	// Query repos with their job counts (includes queued/running, not just completed reviews)
	rows, err := db.Query(`
		SELECT r.name, r.root_path, COUNT(rj.id) as job_count
		FROM repos r
		LEFT JOIN review_jobs rj ON rj.repo_id = r.id
		GROUP BY r.id, r.name, r.root_path
		ORDER BY r.name
	`)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var repos []RepoWithCount
	totalCount := 0
	for rows.Next() {
		var rc RepoWithCount
		if err := rows.Scan(&rc.Name, &rc.RootPath, &rc.Count); err != nil {
			return nil, 0, err
		}
		repos = append(repos, rc)
		totalCount += rc.Count
	}
	return repos, totalCount, rows.Err()
}
