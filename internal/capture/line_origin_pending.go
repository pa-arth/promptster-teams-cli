package capture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/ingest"
	"github.com/pa-arth/promptster-teams-cli/internal/sign"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// Only local job metadata is persisted here. Root paths never enter a receipt,
// and credentials are loaded by the existing signing/outbox path, not stored.
type pendingLineOrigin struct {
	Root       string `json:"root"`
	SHA        string `json:"sha"`
	DeviceID   string `json:"deviceId"`
	ObservedMs int64  `json:"observedMs"`
}

func lineOriginPendingDir() string { return filepath.Join(state.StateDir(), "line-origin-pending-v1") }

// Persist before either watcher advances its cursor. No Git subprocess or trace
// lock runs on the polling goroutine. Atomic publication keeps the worker from
// consuming a half-written request after a crash.
func requestLineOrigin(session Session, root, sha string, nowMs int64) bool {
	if !originSHA.MatchString(sha) {
		return false
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	dir := lineOriginPendingDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return false
	}
	path := filepath.Join(dir, ingest.Sha256Hex(absolute+":"+sha)+".json")
	if _, err := os.Stat(path); err == nil {
		return true
	}
	data, err := json.Marshal(pendingLineOrigin{absolute, sha, session.DeviceID, nowMs})
	if err != nil {
		return false
	}
	f, err := os.CreateTemp(dir, ".pending-*")
	if err != nil {
		return false
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return false
	}
	// Hard-link publishes without overwriting another observer's pending job.
	if err = os.Link(f.Name(), path); err != nil && !os.IsExist(err) {
		return false
	}
	return true
}

// One bounded trace at a time, independent of capture polling. A failed job
// remains on disk across restarts and moves behind other jobs for fair retries.
func drainLineOriginPending() {
	dir := lineOriginPendingDir()
	_ = sign.WithBufferLock(filepath.Join(state.StateDir(), "line-origin-worker")+".lock", func() error {
		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		type candidate struct {
			name     string
			modified time.Time
		}
		jobs := []candidate{}
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
				continue
			}
			if info, err := entry.Info(); err == nil {
				jobs = append(jobs, candidate{entry.Name(), info.ModTime()})
			}
		}
		sort.Slice(jobs, func(i, j int) bool {
			if jobs[i].modified.Equal(jobs[j].modified) {
				return jobs[i].name < jobs[j].name
			}
			return jobs[i].modified.Before(jobs[j].modified)
		})
		if len(jobs) == 0 {
			return nil
		}
		path := filepath.Join(dir, jobs[0].name)
		data, err := os.ReadFile(path) // #nosec G304 -- enumerated fixed private pending directory; parsed local job metadata never uploaded. No externally supplied filename.
		if err != nil {
			return err
		}
		var job pendingLineOrigin
		if json.Unmarshal(data, &job) != nil || !originSHA.MatchString(job.SHA) || !filepath.IsAbs(job.Root) {
			return os.Remove(path)
		}
		if queueLineOrigin(Session{DeviceID: job.DeviceID}, job.Root, job.SHA, job.ObservedMs) {
			return os.Remove(path)
		}
		now := time.Now()
		return os.Chtimes(path, now, now)
	})
}

func runLineOriginWorker(stop <-chan struct{}) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		default:
		}
		drainLineOriginPending()
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
	}
}
