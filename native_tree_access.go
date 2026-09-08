package codexacp

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/coder/acp-go-sdk"
)

// nativeTreeAccess pins the starting workspace and handoff directory before
// native launch. Both must be disjoint from the home and complete scratch
// allocation domain, so later prepared image directories cannot enter a read
// boundary. Directory identities account for casing and filesystem aliases.
type nativeTreeAccess struct {
	mu        sync.RWMutex
	roots     map[string]struct{}
	readRoots map[string]nativeImageRoot
}

type nativeImageRoot struct {
	handle   *os.Root
	resolved string
}

var openManagedImageFile = func(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|handoffOpenFlags, 0)
}

var errOpaqueNativeImagePath = errors.New("image path has no disjoint native read root")

type nativeTreeAccessContextKey struct{}

func (a *Agent) validatePromptImages(ctx context.Context, blocks []acp.ContentBlock) ([]decodedPromptImage, *promptImageError, error) {
	if authority, ok := a.options.HostAuthority.(*guardedHostAuthority); ok {
		authority.trees.mu.RLock()
		defer authority.trees.mu.RUnlock()

		ctx = context.WithValue(ctx, nativeTreeAccessContextKey{}, &authority.trees)
	}

	return validatePromptImages(ctx, blocks, a.options.ImageLimits, a.options.InputHandoffRoot)
}

func (a *Agent) prepareManagedImageRoots(home, scratch string, workspaces []string) error {
	authority, ok := a.options.HostAuthority.(*guardedHostAuthority)
	if !ok {
		return ErrHostAuthorityUnavailable
	}

	authority.trees.mu.Lock()
	defer authority.trees.mu.Unlock()

	if len(authority.trees.roots) != 0 {
		return errOpaqueNativeImagePath
	}

	domains := make([][]os.FileInfo, 0, 2)

	for _, path := range []string{home, scratch} {
		_, lineage, err := nativeDirectoryLineage(path)
		if err != nil {
			return err
		}

		domains = append(domains, lineage)
	}

	if nativeDirectoryLineageContains(domains[1], domains[0][0]) {
		return unsupportedField("scratchDir")
	}

	authority.trees.closeRoots()

	for _, path := range append(append([]string(nil), workspaces...), a.options.InputHandoffRoot) {
		authority.trees.pin(path, domains)
	}

	return nil
}

func (a *Agent) closeManagedImageRoots() {
	if authority, ok := a.options.HostAuthority.(*guardedHostAuthority); ok {
		authority.trees.mu.Lock()
		defer authority.trees.mu.Unlock()

		authority.trees.closeRoots()
	}
}

func (a *nativeTreeAccess) closeRoots() {
	for path, root := range a.readRoots {
		_ = root.handle.Close()

		delete(a.readRoots, path)
	}
}

func nativeDirectoryLineage(path string) (string, []os.FileInfo, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", nil, err
	}

	absolute, err := filepath.Abs(resolved)
	if err != nil {
		return "", nil, err
	}

	lineage := make([]os.FileInfo, 0)

	for path := absolute; ; path = filepath.Dir(path) {
		info, err := os.Stat(path)
		if err != nil {
			return "", nil, err
		}

		lineage = append(lineage, info)
		if filepath.Dir(path) == path {
			return absolute, lineage, nil
		}
	}
}

func (a *nativeTreeAccess) pin(path string, domains [][]os.FileInfo) {
	if path == "" {
		return
	}

	if _, exists := a.readRoots[path]; exists {
		return
	}

	resolved, lineage, err := nativeDirectoryLineage(path)
	if err != nil || !lineage[0].IsDir() {
		return
	}

	for _, domain := range domains {
		if nativeDirectoryLineagesOverlap(lineage, domain) {
			return
		}
	}

	handle, err := os.OpenRoot(resolved)
	if err != nil {
		return
	}

	if a.readRoots == nil {
		a.readRoots = make(map[string]nativeImageRoot)
	}

	a.readRoots[path] = nativeImageRoot{handle: handle, resolved: resolved}
}

func nativeDirectoryLineagesOverlap(left, right []os.FileInfo) bool {
	return nativeDirectoryLineageContains(left, right[0]) || nativeDirectoryLineageContains(right, left[0])
}

func nativeDirectoryLineageContains(lineage []os.FileInfo, directory os.FileInfo) bool {
	for _, ancestor := range lineage {
		if os.SameFile(ancestor, directory) {
			return true
		}
	}

	return false
}

func nativeImageRelativeName(path, original, resolved string) (string, bool) {
	for _, root := range []string{original, resolved} {
		name, err := filepath.Rel(root, path)
		if err == nil && name != handoffParentName && !strings.HasPrefix(name, handoffParentName+string(filepath.Separator)) {
			return name, true
		}
	}

	return "", false
}

func (a *nativeTreeAccess) open(path string, roots []string) (*os.File, error) {
	for _, requested := range roots {
		if root, ok := a.readRoots[requested]; ok {
			if name, within := nativeImageRelativeName(path, requested, root.resolved); within {
				return openManagedImageFile(root.handle, name)
			}
		}

		for original, parent := range a.readRoots {
			rootName, within := nativeImageRelativeName(requested, original, parent.resolved)
			if !within {
				continue
			}

			name, within := nativeImageRelativeName(path, requested, requested)
			if !within {
				continue
			}

			root, err := parent.handle.OpenRoot(rootName)
			if err != nil {
				return nil, err
			}

			file, err := openManagedImageFile(root, name)
			_ = root.Close()

			return file, err
		}
	}

	return nil, errOpaqueNativeImagePath
}

func openPromptHandoff(ctx context.Context, root, path string) (io.ReadCloser, *handoffVerdict) {
	access, ok := ctx.Value(nativeTreeAccessContextKey{}).(*nativeTreeAccess)
	if !ok {
		return openHandoffImage(root, path)
	}

	file, err := access.open(path, []string{root})
	if err != nil {
		return nil, handoffLocationVerdict(err)
	}

	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()

		return nil, &handoffVerdict{code: imageErrorPathNotAllowed, message: handoffCauseNotRegular}
	}

	return file, nil
}
