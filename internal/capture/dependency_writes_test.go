package capture

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func dependencyHook(t *testing.T, step, root, session, generation, command string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"hook_event_name": step, "cwd": root, "conversation_id": session, "generation_id": generation, "command": command})
	captureDependencyHook(raw)
}
func TestDependencyCaptureCommittedBlob(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	root, git, gitOut := gitRepo(t)
	dependencyHook(t, "beforeShellExecution", root, "cursor-s", "g1", "npm install")
	writeCommitFile(t, root, "package-lock.json", "{\"lockfileVersion\": 3}\n")
	dependencyHook(t, "afterShellExecution", root, "cursor-s", "g1", "npm install")
	git("add", ".")
	git("commit", "-m", "dependencies")
	sha := gitOut("rev-parse", "HEAD")
	// A subsequent working-tree edit (and mtime change) cannot erase the evidence
	// for the captured commit or lend it to a different committed blob.
	writeCommitFile(t, root, "package-lock.json", "{\"lockfileVersion\": 99}\n")
	ev, ok := buildCommitAttributionEvent(Session{DeviceID: "dev", TaskRoot: root}, root, sha, siblingLineage{})
	if !ok {
		t.Fatal("no attribution")
	}
	f := filesByPath(t, ev)["package-lock.json"]
	if f["generationKind"] != "dependency" || f["sessionId"] != "cursor-s" || ev.SessionID != "cursor-s" {
		t.Fatalf("missing generated dependency identity: %+v", f)
	}
	if f["lineRanges"].([]interface{})[0].(map[string]interface{})["attribution"] != "likely_ai" {
		t.Fatal("generated lockfile not attributed")
	}
	raw, _ := json.Marshal(ev)
	if strings.Contains(string(raw), "lockfileVersion") || strings.Contains(string(raw), "\"hash\"") {
		t.Fatal("content or hash leaked into event")
	}
	git("add", ".")
	git("commit", "-m", "later edit")
	ev, _ = buildCommitAttributionEvent(Session{DeviceID: "dev", TaskRoot: root}, root, gitOut("rev-parse", "HEAD"), siblingLineage{})
	if filesByPath(t, ev)["package-lock.json"]["generationKind"] != nil {
		t.Fatal("later edit inherited old evidence")
	}
}
func TestDependencyCaptureRejectsAmbiguousOrMissingPairs(t *testing.T) {
	for _, scenario := range []string{"missing-before", "wrong-generation", "overlap", "unsupported-overlap", "unchanged", "deleted", "expired", "symlink"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
			root, _, _ := gitRepo(t)
			writeCommitFile(t, root, "package-lock.json", "old")
			if scenario != "missing-before" {
				dependencyHook(t, "beforeShellExecution", root, "s", "g", "npm install")
			}
			if scenario == "overlap" {
				dependencyHook(t, "beforeShellExecution", root, "s2", "g2", "pnpm install")
			}
			if scenario == "unsupported-overlap" {
				dependencyHook(t, "beforeShellExecution", root, "s2", "g2", "echo hello")
			}
			if scenario != "unchanged" {
				writeCommitFile(t, root, "package-lock.json", "new")
			}
			if scenario == "deleted" {
				os.Remove(filepath.Join(root, "package-lock.json"))
			}
			if scenario == "symlink" {
				os.Remove(filepath.Join(root, "package-lock.json"))
				if err := os.Symlink("outside", filepath.Join(root, "package-lock.json")); err != nil {
					t.Skip(err)
				}
			}
			if scenario == "expired" {
				withDependencyLedger(func(l *dependencyLedger) {
					for k, p := range l.Pending {
						p.Started = time.Now().Add(-time.Hour).UnixMilli()
						l.Pending[k] = p
					}
				})
			}
			gen := "g"
			if scenario == "wrong-generation" {
				gen = "other"
			}
			dependencyHook(t, "afterShellExecution", root, "s", gen, "npm install")
			withDependencyLedger(func(l *dependencyLedger) {
				if len(l.Marks) != 0 {
					t.Fatalf("unexpected evidence: %+v", l.Marks)
				}
			})
		})
	}
}
func TestDependencyCaptureWorkspaceRootAndIsolation(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	root, _, _ := gitRepo(t)
	nested := filepath.Join(root, "packages", "one")
	os.MkdirAll(nested, 0755)
	other, _, _ := gitRepo(t)
	dependencyHook(t, "beforeShellExecution", nested, "s", "g", "pnpm install")
	writeCommitFile(t, root, "pnpm-lock.yaml", "lockfileVersion: 9\n")
	dependencyHook(t, "afterShellExecution", other, "s", "g", "pnpm install")
	withDependencyLedger(func(l *dependencyLedger) {
		if len(l.Marks) != 0 {
			t.Fatal("cross-repository pair matched")
		}
	})
	dependencyHook(t, "afterShellExecution", nested, "s", "g", "pnpm install")
	withDependencyLedger(func(l *dependencyLedger) {
		if len(l.Marks) != 1 {
			t.Fatalf("workspace root lockfile not captured: %+v", l)
		}
	})
}
func TestDependencyInstallRecognition(t *testing.T) {
	for _, cmd := range []string{"npm install", "pnpm add typescript", "yarn install --immutable"} {
		if !dependencyInstall(cmd) {
			t.Errorf("rejected %q", cmd)
		}
	}
	for _, cmd := range []string{"npm run build", "npm install && git commit", "npm install --prefix other", "pnpm -C other install", "npm install -g foo", "cd other; npm i", "yarn install\nnpm i"} {
		if dependencyInstall(cmd) {
			t.Errorf("accepted %q", cmd)
		}
	}
	var response map[string]interface{}
	if json.Unmarshal([]byte(cursorHookStdout), &response) != nil || response["permission"] != "allow" {
		t.Fatal("before hook must always allow execution")
	}
}

func TestDependencyCaptureRealNpmInstall(t *testing.T) {
	npm, err := exec.LookPath("npm")
	if err != nil {
		t.Skip("npm not installed")
	}
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	root, git, gitOut := gitRepo(t)
	writeCommitFile(t, root, "package.json", `{"name":"dependency-capture-fixture","version":"1.0.0","private":true}`)
	command := "npm install --package-lock-only --ignore-scripts --offline"
	dependencyHook(t, "beforeShellExecution", root, "cursor-npm", "generation", command)
	cmd := exec.Command(npm, "install", "--package-lock-only", "--ignore-scripts", "--offline")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "npm_config_cache="+t.TempDir(), "npm_config_audit=false", "npm_config_fund=false")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("offline install: %v: %s", err, output)
	}
	dependencyHook(t, "afterShellExecution", root, "cursor-npm", "generation", command)
	git("add", ".")
	git("commit", "-m", "generated dependencies")
	ev, ok := buildCommitAttributionEvent(Session{DeviceID: "dev", TaskRoot: root}, root, gitOut("rev-parse", "HEAD"), siblingLineage{})
	if !ok || filesByPath(t, ev)["package-lock.json"]["generationKind"] != "dependency" {
		t.Fatal("real npm lockfile was not classified")
	}
}

func TestDependencyCaptureDiscoversGeneratedOnlyRepository(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	root, _, _ := gitRepo(t)
	dependencyHook(t, "beforeShellExecution", root, "s", "g", "npm install")
	writeCommitFile(t, root, "package-lock.json", "generated")
	dependencyHook(t, "afterShellExecution", root, "s", "g", "npm install")
	found := false
	for _, r := range discoverAiRepoRoots(root) {
		if r == resolvePath(root) {
			found = true
		}
	}
	if !found {
		t.Fatal("generated-only repository is not discoverable")
	}
	if len(readAiTouchedPaths(gitWatchRootKey(root))) != 0 {
		t.Fatal("generated evidence must not seed generic path attribution")
	}
	withDependencyLedger(func(l *dependencyLedger) {
		for k, m := range l.Marks {
			m.At = time.Now().Add(-8 * 24 * time.Hour).UnixMilli()
			l.Marks[k] = m
		}
	})
	if len(readDependencyMarks(resolvePath(root))) != 0 {
		t.Fatal("expired generated evidence remained readable")
	}
}

func TestDependencyCaptureFullLedgerStillInvalidatesAmbiguousMark(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	root, _, _ := gitRepo(t)
	dependencyHook(t, "beforeShellExecution", root, "first", "g1", "npm install")
	writeCommitFile(t, root, "package-lock.json", "result")
	dependencyHook(t, "afterShellExecution", root, "first", "g1", "npm install")
	withDependencyLedger(func(l *dependencyLedger) {
		for i := 0; len(l.Marks) < 2048; i++ {
			l.Marks["filler"+strconv.Itoa(i)] = dependencyMark{Root: "elsewhere", Session: "filler", At: time.Now().UnixMilli()}
		}
	})
	writeCommitFile(t, root, "package-lock.json", "old")
	dependencyHook(t, "beforeShellExecution", root, "second", "g2", "npm install")
	writeCommitFile(t, root, "package-lock.json", "result")
	dependencyHook(t, "afterShellExecution", root, "second", "g2", "npm install")
	if len(readDependencyMarks(resolvePath(root))) != 0 {
		t.Fatal("full ledger retained attribution to the first session")
	}
	withDependencyLedger(func(l *dependencyLedger) {
		if len(l.Marks) != 2048 {
			t.Fatal("ledger cap changed")
		}
	})
}
