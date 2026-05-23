package project

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

type Project struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Path       string  `json:"path"`
	RemoteKey  string  `json:"remote_key,omitempty"`
	SourceTool *string `json:"source_tool,omitempty"`
}

type Detector struct {
	db *sql.DB
}

func NewDetector(db *sql.DB) *Detector {
	return &Detector{db: db}
}

func (d *Detector) Detect(cwd string) (*Project, error) {
	gitRoot, err := gitRoot(cwd)
	if err != nil || gitRoot == "" {
		return nil, nil
	}

	gitRoot, err = filepath.Abs(gitRoot)
	if err != nil {
		return nil, err
	}

	var existing Project
	err = d.db.QueryRow(
		"SELECT id, name, path, source_tool, COALESCE(remote_key, '') FROM projects WHERE path = ?",
		gitRoot,
	).Scan(&existing.ID, &existing.Name, &existing.Path, &existing.SourceTool, &existing.RemoteKey)

	if err == nil {
		if existing.RemoteKey == "" {
			rk := deriveRemoteKey(gitRoot)
			if rk != "" {
				_, _ = d.db.Exec("UPDATE projects SET remote_key = ? WHERE id = ?", rk, existing.ID)
				existing.RemoteKey = rk
			}
		}
		return &existing, nil
	}
	if err != sql.ErrNoRows {
		return nil, err
	}

	name := filepath.Base(gitRoot)
	id := newID()
	rk := deriveRemoteKey(gitRoot)

	_, err = d.db.Exec(
		"INSERT INTO projects (id, name, path, remote_key) VALUES (?, ?, ?, ?)",
		id, name, gitRoot, rk,
	)
	if err != nil {
		return nil, err
	}

	return &Project{ID: id, Name: name, Path: gitRoot, RemoteKey: rk}, nil
}

func (d *Detector) Resolve(id string) (*Project, error) {
	var p Project
	err := d.db.QueryRow(
		"SELECT id, name, path, source_tool, COALESCE(remote_key, '') FROM projects WHERE id = ?",
		id,
	).Scan(&p.ID, &p.Name, &p.Path, &p.SourceTool, &p.RemoteKey)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func gitRoot(dir string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", nil
	}
	return strings.TrimSpace(string(out)), nil
}

func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func getGitRemoteURL(cwd string) string {
	cmd := exec.Command("git", "remote", "get-url", "origin")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// normalizeRemoteURL reduces various git remote URL formats to a canonical form:
// https://github.com/user/repo.git → github.com/user/repo
// git@github.com:user/repo.git     → github.com/user/repo
// ssh://git@github.com/user/repo   → github.com/user/repo
func normalizeRemoteURL(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimRight(s, "/")
	s = strings.TrimSuffix(s, ".git")

	// ssh://git@host/path → host/path
	if strings.HasPrefix(s, "ssh://") {
		s = strings.TrimPrefix(s, "ssh://")
		s = regexp.MustCompile(`^git@`).ReplaceAllString(s, "")
	}

	// git@host:path → host/path
	if strings.Contains(s, "@") && strings.Contains(s, ":") {
		parts := strings.SplitN(s, "@", 2)
		if len(parts) == 2 {
			rest := parts[1]
			idx := strings.Index(rest, ":")
			if idx >= 0 {
				s = rest[:idx] + "/" + rest[idx+1:]
			} else {
				s = rest
			}
		}
	}

	// https:// or http:// host/path → host/path
	if strings.HasPrefix(s, "https://") {
		s = strings.TrimPrefix(s, "https://")
	} else if strings.HasPrefix(s, "http://") {
		s = strings.TrimPrefix(s, "http://")
	}

	s = regexp.MustCompile(`^www\.`).ReplaceAllString(s, "")
	s = strings.TrimRight(s, "/")
	s = strings.ToLower(s)

	return s
}

// deriveRemoteKey returns a stable 16-hex-char SHA-256 prefix from the git remote URL, or "" if no remote.
func deriveRemoteKey(cwd string) string {
	remoteURL := getGitRemoteURL(cwd)
	if remoteURL == "" {
		return ""
	}
	normalized := normalizeRemoteURL(remoteURL)
	if normalized == "" {
		return ""
	}
	hash := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(hash[:8])
}
