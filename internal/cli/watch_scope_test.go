package cli

import "testing"

// TestWatchScopeDisplay pins the `status` watch row. The row is the only place
// an engineer can confirm a second tree is actually covered, so it must name
// every registered root — and must NOT pad the line with roots the daemon's own
// root already contains, which would read as two scopes where there is one.
func TestWatchScopeDisplay(t *testing.T) {
	tests := []struct {
		name       string
		root       string
		registered []string
		want       string
	}{
		{
			name: "no registered roots leaves the row alone",
			root: "/Users/dev/work",
			want: "/Users/dev/work",
		},
		{
			name:       "a second tree is named",
			root:       "/Users/dev/work",
			registered: []string{"/Users/dev/work", "/Volumes/side/tree"},
			want:       "/Users/dev/work (+ /Volumes/side/tree)",
		},
		{
			name:       "roots already inside the daemon root are folded away",
			root:       "/Users/dev",
			registered: []string{"/Users/dev", "/Users/dev/work", "/Users/dev/work/repo"},
			want:       "/Users/dev",
		},
		{
			name:       "several second trees are all named",
			root:       "/Users/dev/a",
			registered: []string{"/Users/dev/a", "/Users/dev/b", "/Users/dev/c"},
			want:       "/Users/dev/a (+ /Users/dev/b, /Users/dev/c)",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := watchScopeDisplay(tc.root, tc.registered); got != tc.want {
				t.Errorf("want %q; got %q", tc.want, got)
			}
		})
	}
}
