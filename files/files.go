// Package files walks the download directory and serves what is in it.
//
// It knows nothing about torrents, rendering or authorization: it takes a root
// directory and answers questions about the tree below it. The containment
// rules live here because they are the only thing standing between a
// user-supplied path and the rest of the filesystem.
package files

import (
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Limit caps how many entries the tree walk will visit. It is exported because
// the UI says so when a listing is truncated.
const Limit = 1000

// Node is one entry in the download tree.
type Node struct {
	Name     string
	Size     int64
	Modified time.Time
	Children []*Node
	// IsDir is recorded rather than inferred from len(Children): an empty
	// directory has no children but is still a directory.
	IsDir bool `json:",omitempty"`
	// Truncated marks a tree that hit Limit, so the UI can say so instead of
	// presenting a partial listing as complete.
	Truncated bool `json:",omitempty"`
}

var (
	errFileLimit = errors.New("over file limit")
	// ErrOutsideRoot is returned for any path that does not resolve inside the
	// download directory. Callers must not echo it back with the path attached:
	// that turns every rejected probe into a filesystem-layout oracle.
	ErrOutsideRoot = errors.New("path is outside the download directory")
)

// List walks root and returns the tree below it. A missing or unreadable root
// yields an empty node rather than an error: an unconfigured or not-yet-created
// download directory is a normal state, not a failure.
func List(root string) *Node {
	node := &Node{}
	info, err := os.Stat(root)
	if err != nil {
		return node
	}
	n := 0
	if err := list(root, info, node, &n); err != nil {
		if errors.Is(err, errFileLimit) {
			node.Truncated = true
		} else {
			log.Printf("File listing failed: %s", err)
		}
	}
	return node
}

// visible reports whether an entry belongs in the tree. It is applied to the
// *entries* of a directory, never to the walk root: the download directory is
// the operator's choice, and testing it here made a root named ".torrents" fail
// the whole walk — once per poll tick, logging an error and rendering an empty
// tree.
func visible(info os.FileInfo) bool {
	if !info.IsDir() && !info.Mode().IsRegular() {
		return false
	}
	return !strings.HasPrefix(info.Name(), ".")
}

// list walks path into node. It returns errFileLimit once the budget is spent,
// which aborts the whole walk and marks the result truncated.
func list(path string, info os.FileInfo, node *Node, n *int) error {
	*n++
	if *n > Limit {
		return errFileLimit
	}
	node.Name = info.Name()
	node.Size = info.Size()
	node.Modified = info.ModTime()
	node.IsDir = info.IsDir()
	if !info.IsDir() {
		return nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	node.Size = 0
	for _, e := range entries {
		ei, err := e.Info()
		if err != nil || !visible(ei) {
			continue
		}
		c := &Node{}
		if err := list(filepath.Join(path, e.Name()), ei, c, n); err != nil {
			if errors.Is(err, errFileLimit) {
				return err // propagate: the walk is over
			}
			continue // unreadable child
		}
		node.Size += c.Size
		node.Children = append(node.Children, c)
	}
	return nil
}

// ResolveWithin maps a user-supplied relative path to an absolute path proven to
// live inside root.
//
// A plain strings.HasPrefix check is not enough: it has no separator boundary,
// so "<root>-backup/secret" passes it.
func ResolveWithin(root, rel string) (string, error) {
	if root == "" {
		return "", ErrOutsideRoot
	}
	target := filepath.Join(root, filepath.FromSlash(rel))
	if !isWithin(root, target) {
		return "", ErrOutsideRoot
	}
	// Follow symlinks and re-check: a link inside the download directory can
	// otherwise point anywhere on disk.
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", err
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	if !isWithin(resolvedRoot, resolvedTarget) {
		return "", ErrOutsideRoot
	}
	return resolvedTarget, nil
}

// isWithin reports whether target is strictly inside root.
func isWithin(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	if rel == "." {
		return false // the root itself is not a valid target
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}
