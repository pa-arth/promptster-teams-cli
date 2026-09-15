package capture

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/sign"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

const dependencyHashLimit = 16 << 20
const dependencyPendingTTL = 30 * time.Minute

type dependencyPending struct {
	Root      string            `json:"root"`
	Session   string            `json:"session"`
	Started   int64             `json:"started"`
	Ambiguous bool              `json:"ambiguous"`
	Before    map[string]string `json:"before"`
}
type dependencyMark struct {
	Root    string `json:"root"`
	Path    string `json:"path"`
	Hash    string `json:"hash"`
	Session string `json:"session"`
	At      int64  `json:"at"`
}
type dependencyLedger struct {
	Pending map[string]dependencyPending `json:"pending"`
	Marks   map[string]dependencyMark    `json:"marks"`
}

func dependencyLedgerPath() string { return filepath.Join(state.StateDir(), "dependency-writes.json") }

// Local-only hashes. No command text, dependency contents, or hashes are emitted.
func withDependencyLedger(fn func(*dependencyLedger)) {
	path := dependencyLedgerPath()
	_ = os.MkdirAll(filepath.Dir(path), 0700)
	_ = sign.WithBufferLock(path+".lock", func() error {
		l := dependencyLedger{}
		if raw, err := os.ReadFile(path); err == nil {
			if json.Unmarshal(raw, &l) != nil {
				return nil
			}
		}
		if l.Pending == nil {
			l.Pending = map[string]dependencyPending{}
		}
		if l.Marks == nil {
			l.Marks = map[string]dependencyMark{}
		}
		now := time.Now().UnixMilli()
		for key, p := range l.Pending {
			if now-p.Started > dependencyPendingTTL.Milliseconds() {
				delete(l.Pending, key)
			}
		}
		for key, m := range l.Marks {
			if now-m.At > aiPathsTTL.Milliseconds() {
				delete(l.Marks, key)
			}
		}
		fn(&l)
		raw, err := json.Marshal(l)
		if err != nil {
			return err
		}
		if err = os.WriteFile(path+".tmp", raw, 0600); err != nil {
			return err
		}
		return os.Rename(path+".tmp", path)
	})
}
func dependencyDigest(r io.Reader) (string, bool) {
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(r, dependencyHashLimit+1))
	if err != nil || n > dependencyHashLimit {
		return "", false
	}
	return hex.EncodeToString(h.Sum(nil)), true
}
func dependencyFileHash(path string) (string, bool) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return "", true
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > dependencyHashLimit {
		return "", false
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	return dependencyDigest(f)
}

// Deliberately bounded to direct invocations. Chained shell commands, global
// installs, and directory overrides need an execution-level working-directory
// signal before we can know which lockfiles they could have changed.
func dependencyInstall(command string) bool {
	if strings.ContainsAny(command, ";&|<>`$\n\r()") {
		return false
	}
	args := strings.Fields(command)
	if len(args) < 2 {
		return false
	}
	switch args[0] {
	case "npm", "pnpm", "yarn":
	default:
		return false
	}
	switch args[1] {
	case "install", "i", "ci", "add", "update", "upgrade", "remove", "uninstall":
	default:
		return false
	}
	for _, a := range args[2:] {
		for _, flag := range []string{"--prefix", "--cwd", "--dir", "--global", "-g", "-C", "--filter", "--workspace", "-w"} {
			if a == flag || strings.HasPrefix(a, flag+"=") || (len(flag) == 2 && strings.HasPrefix(a, flag)) {
				return false
			}
		}
	}
	return true
}
func dependencyPaths(cwd, root string) []string {
	var paths []string
	for dir := cwd; ; dir = filepath.Dir(dir) {
		for _, name := range []string{"package-lock.json", "npm-shrinkwrap.json", "pnpm-lock.yaml", "yarn.lock"} {
			rel, err := filepath.Rel(root, filepath.Join(dir, name))
			if err == nil {
				paths = append(paths, filepath.ToSlash(rel))
			}
		}
		if dir == root || filepath.Dir(dir) == dir {
			break
		}
	}
	return paths
}

// Called synchronously by live Cursor hooks, never by transcript replay.
// generation+command+cwd pairs the hooks; any overlapping shell invocation in
// this checkout invalidates the pending sample, including identical invocations.
func captureDependencyHook(raw []byte) bool {
	var p struct {
		Step       string `json:"hook_event_name"`
		Session    string `json:"conversation_id"`
		Generation string `json:"generation_id"`
		Command    string `json:"command"`
		Cwd        string `json:"cwd"`
	}
	if json.Unmarshal(raw, &p) != nil || (p.Step != "beforeShellExecution" && p.Step != "afterShellExecution") {
		return false
	}
	before := p.Step == "beforeShellExecution"
	if p.Session == "" || p.Generation == "" || !filepath.IsAbs(p.Cwd) {
		return before
	}
	cwd, err := filepath.EvalSymlinks(p.Cwd)
	if err != nil {
		return before
	}
	root, ok := gitRootOf(cwd)
	if !ok {
		return before
	}
	keyBytes := sha256.Sum256([]byte(p.Session + "\x00" + p.Generation + "\x00" + cwd + "\x00" + p.Command))
	key := hex.EncodeToString(keyBytes[:])
	withDependencyLedger(func(l *dependencyLedger) {
		if before {
			ambiguous := false
			for k, pending := range l.Pending {
				if pending.Root == root {
					pending.Ambiguous = true
					l.Pending[k] = pending
					ambiguous = true
				}
			}
			if len(l.Pending) >= 128 {
				return
			}
			pending := dependencyPending{Root: root, Session: p.Session, Started: time.Now().UnixMilli(), Ambiguous: ambiguous, Before: map[string]string{}}
			if dependencyInstall(p.Command) {
				for _, path := range dependencyPaths(cwd, root) {
					hash, valid := dependencyFileHash(filepath.Join(root, path))
					if valid {
						pending.Before[path] = hash
					}
				}
			}
			l.Pending[key] = pending
			return
		}
		pending, exists := l.Pending[key]
		if !exists {
			return
		}
		delete(l.Pending, key)
		if pending.Ambiguous {
			return
		}
		for path, old := range pending.Before {
			hash, valid := dependencyFileHash(filepath.Join(root, path))
			if !valid || hash == "" || hash == old || len(l.Marks) >= 2048 {
				continue
			}
			markKey := root + "\x00" + path + "\x00" + hash
			// Multiple sessions producing identical bytes are not uniquely attributable.
			if prior, exists := l.Marks[markKey]; exists && prior.Session != p.Session {
				prior.Session = ""
				l.Marks[markKey] = prior
				continue
			}
			l.Marks[markKey] = dependencyMark{Root: root, Path: path, Hash: hash, Session: p.Session, At: time.Now().UnixMilli()}
		}
	})
	return before
}

func committedDependencyHash(root, sha, path string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", root, "show", sha+":"+path)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return "", false
	}
	if cmd.Start() != nil {
		return "", false
	}
	hash, ok := dependencyDigest(pipe)
	if !ok {
		_ = cmd.Process.Kill()
	}
	if cmd.Wait() != nil {
		return "", false
	}
	return hash, ok
}

// Match the immutable committed blob, not the working tree or its mtime.
// A later checkout cannot erase this evidence; a later edit cannot inherit it.
func applyDependencyAttribution(root, sha string, files []attrFile) {
	root = resolvePath(root)
	marks := readDependencyMarks(root)
	if len(marks) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	raw, err := exec.CommandContext(ctx, "git", "-C", root, "show", "-s", "--format=%ct", sha).Output()
	if err != nil {
		return
	}
	commitTime, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return
	}
	for i := range files {
		var candidates []dependencyMark
		for _, m := range marks {
			if m.Path == files[i].Path && commitTime >= m.At/1000 {
				candidates = append(candidates, m)
			}
		}
		if len(candidates) == 0 {
			continue
		}
		hash, ok := committedDependencyHash(root, sha, files[i].Path)
		if !ok {
			continue
		}
		for _, m := range candidates {
			if m.Hash == hash {
				files[i].SessionID = m.Session
				files[i].GenerationKind = "dependency"
				for j := range files[i].LineRanges {
					files[i].LineRanges[j].Attribution = attributionLikelyAI
				}
				break
			}
		}
	}
}

// Atomic ledger replacement makes reads safe without a lock or a disk write.
func readDependencyMarks(root string) []dependencyMark {
	raw, err := os.ReadFile(dependencyLedgerPath())
	if err != nil {
		return nil
	}
	var ledger dependencyLedger
	if json.Unmarshal(raw, &ledger) != nil {
		return nil
	}
	var marks []dependencyMark
	for _, m := range ledger.Marks {
		if (root == "" || m.Root == root) && m.Session != "" && time.Now().UnixMilli()-m.At <= aiPathsTTL.Milliseconds() {
			marks = append(marks, m)
		}
	}
	return marks
}
