// Package discovery finds additional local user homes that appear to contain
// supported AI-tool state. It reads directory metadata only: discovery never
// opens transcripts, configuration, databases, or credentials.
package discovery

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
)

type Home struct {
	Path               string
	Products           []string
	PromptsterEnrolled bool
}

// AdditionalHomes returns candidate homes other than the invoking user's.
// Permission failures are ordinary: another user's home may be intentionally
// private, in which case its name can be listed by the parent while its product
// markers cannot be inspected. Such a directory is not reported as a finding.
func AdditionalHomes() []Home {
	current, _ := os.UserHomeDir()
	roots := platformHomeRoots(current)
	return scanHomeRoots(roots, current)
}

func platformHomeRoots(current string) []string {
	roots := []string{}
	switch runtime.GOOS {
	case "darwin":
		roots = append(roots, "/Users")
	case "linux":
		roots = append(roots, "/home")
	}
	// Custom enterprise layouts need not live under the platform default. The
	// current home's parent is a bounded, unsurprising second place to inspect.
	if current != "" {
		roots = append(roots, filepath.Dir(filepath.Clean(current)))
	}
	return roots
}

func scanHomeRoots(roots []string, current string) []Home {
	current = filepath.Clean(current)
	seen := map[string]bool{}
	var found []Home
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				continue
			}
			home := filepath.Clean(filepath.Join(root, entry.Name()))
			if home == current || seen[home] {
				continue
			}
			seen[home] = true
			products := productsInHome(home)
			if len(products) == 0 {
				continue
			}
			found = append(found, Home{
				Path:               home,
				Products:           products,
				PromptsterEnrolled: isDir(filepath.Join(home, ".promptster-teams")),
			})
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Path < found[j].Path })
	return found
}

func productsInHome(home string) []string {
	markers := []struct {
		name  string
		paths []string
	}{
		{"Claude", []string{filepath.Join(home, ".claude")}},
		{"Codex", []string{filepath.Join(home, ".codex")}},
		{"Cursor", []string{
			filepath.Join(home, ".cursor"),
			filepath.Join(home, "Library", "Application Support", "Cursor"),
			filepath.Join(home, ".config", "Cursor"),
		}},
	}
	products := []string{}
	for _, marker := range markers {
		for _, path := range marker.paths {
			if isDir(path) {
				products = append(products, marker.name)
				break
			}
		}
	}
	return products
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
