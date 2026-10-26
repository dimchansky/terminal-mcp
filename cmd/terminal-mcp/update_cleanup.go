//go:build linux || darwin

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type cleanupArtifact struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

type cleanupResult struct {
	Mode        string            `json:"mode"`
	Files       []cleanupArtifact `json:"files"`
	TotalBytes  int64             `json:"total_bytes"`
	SkippedJobs []string          `json:"skipped_jobs,omitempty"`
}

// cleanupUpdateArtifacts only considers the three binary payload names in
// terminally successful updater jobs. It deliberately leaves worker/preflight
// logs, manifests, statuses, failed/ambiguous jobs, manually staged candidates,
// user backups, source builds, and the one durable rollback executable intact.
func cleanupUpdateArtifacts(root string) (cleanupResult, error) {
	result := cleanupResult{Files: []cleanupArtifact{}}
	entries, err := os.ReadDir(root)
	if err != nil {
		return result, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !validUpdateID(entry.Name()) {
			continue
		}
		jobDir := filepath.Join(root, entry.Name())
		info, err := os.Lstat(jobDir)
		if err != nil {
			return result, err
		}
		if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
			return result, fmt.Errorf("update job directory must be private: %s", jobDir)
		}
		var status updateStatus
		if err := readJSON(filepath.Join(jobDir, "status.json"), &status); err != nil {
			result.SkippedJobs = append(result.SkippedJobs, entry.Name())
			continue // Protect ambiguous/incomplete or manually altered jobs.
		}
		if status.ID != entry.Name() || status.Status != "applied" && status.Status != "rolled_back" {
			result.SkippedJobs = append(result.SkippedJobs, entry.Name())
			continue
		}
		for _, filename := range []string{"candidate", "previous", "worker"} {
			path := filepath.Join(jobDir, filename)
			info, err := os.Lstat(path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return result, err
			}
			if !info.Mode().IsRegular() {
				return result, fmt.Errorf("refusing to clean nonregular update artifact: %s", path)
			}
			result.Files = append(result.Files, cleanupArtifact{Path: path, Bytes: info.Size()})
			result.TotalBytes += info.Size()
		}
	}
	slices.SortFunc(result.Files, func(a, b cleanupArtifact) int {
		return strings.Compare(a.Path, b.Path)
	})
	slices.Sort(result.SkippedJobs)
	return result, nil
}

func validUpdateID(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, b := range s {
		if b < '0' || b > '9' && b < 'a' || b > 'f' {
			return false
		}
	}
	return true
}

func cleanupCommand(args []string) error {
	flags := flag.NewFlagSet("terminal-mcp cleanup", flag.ContinueOnError)
	var state string
	var apply, dryRun bool
	flags.StringVar(&state, "state-dir", "", "private update directory (defaults to the installed user's)")
	flags.BoolVar(&dryRun, "dry-run", false, "show candidates without deleting (default)")
	flags.BoolVar(&apply, "apply", false, "remove only verified, successful/rolled-back update payloads")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || dryRun && apply {
		return errors.New("usage: terminal-mcp cleanup [--dry-run | --apply] [--state-dir=...]")
	}
	root, err := updateDir(state)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		return json.NewEncoder(os.Stdout).Encode(cleanupResult{
			Mode: "dry_run", Files: []cleanupArtifact{},
		})
	} else if err != nil {
		return err
	}
	if err := secureUpdateDir(root); err != nil {
		return err
	}
	unlock, err := lockUpdateRoot(root)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := os.Lstat(filepath.Join(root, "pending")); err == nil {
		return errors.New("an update is in progress; cleanup refused")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	result, err := cleanupUpdateArtifacts(root)
	if err != nil {
		return err
	}
	if apply {
		result.Mode = "applied"
		for _, item := range result.Files {
			// The control lock excludes new updates. All paths came from
			// known job IDs and exact update artifact basenames, with
			// nonregular files rejected before the first unlink.
			if err := os.Remove(item.Path); err != nil {
				return fmt.Errorf("partial cleanup after %s: %w", item.Path, err)
			}
		}
	} else {
		result.Mode = "dry_run"
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}
