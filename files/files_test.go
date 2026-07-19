package files

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestResolveWithin covers the path-containment fix. The previous check was
// strings.HasPrefix(file, dldir), which has no separator boundary — so a sibling
// directory sharing the root's name prefix was both readable and DELETE-able.
func TestResolveWithin(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "downloads")
	sibling := filepath.Join(base, "downloads-backup")
	for _, d := range []string{root, sibling, filepath.Join(root, "sub")} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p string) {
		if err := os.WriteFile(p, []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "ok.txt"))
	write(filepath.Join(root, "sub", "nested.txt"))
	write(filepath.Join(sibling, "secret.key"))

	allowed := []string{"ok.txt", "sub", "sub/nested.txt"}
	for _, rel := range allowed {
		t.Run("allow/"+rel, func(t *testing.T) {
			got, err := ResolveWithin(root, rel)
			if err != nil {
				t.Fatalf("ResolveWithin(%q) = error %v, want success", rel, err)
			}
			if !isWithin(root, got) {
				t.Fatalf("resolved %q escaped the root", got)
			}
		})
	}

	denied := []struct{ name, rel string }{
		// The regression case: Join+Clean neutralizes "..", landing on a real
		// sibling path that the old prefix check accepted.
		{"sibling prefix escape", "../downloads-backup/secret.key"},
		{"parent traversal", "../../etc/passwd"},
		{"absolute path", "/etc/passwd"},
		{"root itself", ""},
		{"dot", "."},
		{"missing file", "does-not-exist.txt"},
	}
	for _, c := range denied {
		t.Run("deny/"+c.name, func(t *testing.T) {
			if got, err := ResolveWithin(root, c.rel); err == nil {
				t.Fatalf("ResolveWithin(%q) = %q, want error", c.rel, got)
			}
		})
	}
}

// TestResolveWithinRejectsSymlink covers the second half of the fix: os.Stat
// follows links, so containment must be re-checked after resolution.
func TestResolveWithinRejectsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require elevation on windows")
	}
	base := t.TempDir()
	root := filepath.Join(base, "downloads")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{root, outside} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	secret := filepath.Join(outside, "secret.key")
	if err := os.WriteFile(secret, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "escape")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	if got, err := ResolveWithin(root, "escape"); err == nil {
		t.Fatalf("symlink escape allowed: %q", got)
	}
}

// TestListFileLimit checks that hitting the walk budget is reported rather than
// silently producing a partial tree the UI renders as complete.
func TestListFileLimit(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < Limit+50; i++ {
		p := filepath.Join(root, string(rune('a'+i%26))+string(rune('a'+i/26))+".txt")
		if err := os.WriteFile(p, []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	node := &Node{}
	n := 0
	err = list(root, info, node, &n)
	if err == nil {
		t.Fatal("expected the walk to abort once over the limit")
	}
	if n <= Limit {
		t.Fatalf("walked %d entries, expected to exceed %d", n, Limit)
	}

	// The exported path, which is what the UI actually calls. This test drove
	// the unexported walk and stopped at "it returns an error" — so the
	// conversion of that error into Truncated, the flag the UI reads to say a
	// listing is partial rather than presenting it as complete, was untested.
	// A regression there shows a truncated tree as the whole truth.
	got := List(root)
	if !got.Truncated {
		t.Error("List must set Truncated when the walk hits Limit")
	}
	if len(got.Children) == 0 {
		t.Error("a truncated listing must still return what it managed to walk")
	}
}

// TestListNotTruncatedUnderLimit is the other half: Truncated must not be set
// for a tree that fits, or the UI warns about truncation on every listing.
func TestListNotTruncatedUnderLimit(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 5; i++ {
		p := filepath.Join(root, string(rune('a'+i))+".txt")
		if err := os.WriteFile(p, []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	got := List(root)
	if got.Truncated {
		t.Error("Truncated set for a tree well under Limit")
	}
	if len(got.Children) != 5 {
		t.Errorf("children = %d, want 5", len(got.Children))
	}
}

// TestListRootMayBeHidden covers a self-inflicted outage: the dotfile filter was
// applied to the walk root as well as to its entries, so a download directory
// named ".torrents" aborted the whole walk. List did not special-case that
// sentinel, so it logged "File listing failed: skip entry" once per poll tick
// and rendered an empty tree.
func TestListRootMayBeHidden(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".torrents")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "visible.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	node := List(root)
	if len(node.Children) != 1 || node.Children[0].Name != "visible.txt" {
		t.Fatalf("got %d children, want visible.txt under a hidden root", len(node.Children))
	}
}

// TestListSkipsDotfiles documents that hidden entries are filtered, not errors.
func TestListSkipsDotfiles(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".hidden"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "visible.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(root)
	node := &Node{}
	n := 0
	if err := list(root, info, node, &n); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(node.Children) != 1 || node.Children[0].Name != "visible.txt" {
		t.Fatalf("got %d children, want only visible.txt", len(node.Children))
	}
}
