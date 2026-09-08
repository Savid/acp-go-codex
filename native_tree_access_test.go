package codexacp

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/stretchr/testify/require"
)

func TestManagedImageRootsExcludeOpaqueTreesAndSurviveSymlinkSwaps(t *testing.T) {
	root := t.TempDir()
	home, workspace, handoff, scratch := filepath.Join(root, "home"), filepath.Join(root, "workspace"), filepath.Join(root, "handoff"), filepath.Join(root, "scratch")
	data := outputFixture(t, "valid.png")
	for _, dir := range []string{home, workspace, handoff, scratch} {
		require.NoError(t, os.Mkdir(dir, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "image.png"), data, 0o600))
	}
	require.NoError(t, os.WriteFile(filepath.Join(home, "image.png"), outputFixture(t, "valid.jpg"), 0o600))
	alias := filepath.Join(root, "workspace-alias")
	require.NoError(t, os.Symlink(workspace, alias))
	var reclaimErr error
	host := authorityCoverageHost{
		environment: func() map[string]string { return map[string]string{} },
		prepare:     func() error { return nil },
		reclaim:     func() error { return reclaimErr },
	}
	agent := NewAgent(WithHostAuthority(host), WithHome(home), WithScratchDir(scratch), WithInputHandoffRoot(handoff))
	require.NoError(t, agent.prepareManagedImageRoots(home, scratch, []string{root, home, workspace, alias, os.TempDir()}))
	t.Cleanup(agent.closeManagedImageRoots)
	require.NoError(t, agent.options.HostAuthority.PrepareNativeTree(t.Context(), home))
	s := &session{agent: agent, cwd: workspace}

	for _, path := range []string{home, filepath.Join(home, "image.png"), filepath.Join(home, "missing.png")} {
		_, _, err := s.readAllowedImageFile(path)
		requireImageOutputReason(t, err, imageOutputPathDenied)
	}
	got, _, err := s.readAllowedImageFile(filepath.Join(workspace, "image.png"))
	require.NoError(t, err)
	require.Equal(t, data, got)
	_, _, err = s.readAllowedImageFile(filepath.Join(workspace, "missing.png"))
	requireImageOutputReason(t, err, imageOutputMissingFile)
	_, _, err = s.readAllowedImageFile(workspace)
	requireImageOutputReason(t, err, imageOutputPathDenied)

	// Changing the configured root name never changes its retained directory.
	require.NoError(t, os.Remove(alias))
	require.NoError(t, os.Symlink(home, alias))
	s.cwd = alias
	got, _, err = s.readAllowedImageFile(filepath.Join(alias, "image.png"))
	require.NoError(t, err)
	require.Equal(t, data, got)
	s.cwd = workspace

	// Replace an initially valid relative link at the actual confined-open seam.
	link := filepath.Join(workspace, "link.png")
	require.NoError(t, os.Symlink("image.png", link))
	originalOpen := openManagedImageFile
	t.Cleanup(func() { openManagedImageFile = originalOpen })
	openManagedImageFile = func(confined *os.Root, name string) (*os.File, error) {
		require.NoError(t, os.Remove(link))
		require.NoError(t, os.Symlink(filepath.Join(home, "image.png"), link))

		return originalOpen(confined, name)
	}
	_, _, err = s.readAllowedImageFile(link)
	requireImageOutputReason(t, err, imageOutputPathDenied)
	openManagedImageFile = originalOpen

	for _, failure := range []error{ErrNativeTreeBusy, ErrContainmentIncomplete} {
		reclaimErr = failure
		require.ErrorIs(t, agent.options.HostAuthority.ReclaimNativeTree(t.Context(), home), failure)
		_, _, err = s.readAllowedImageFile(filepath.Join(home, "image.png"))
		requireImageOutputReason(t, err, imageOutputPathDenied)
	}

	reclaimErr = nil
	require.NoError(t, agent.options.HostAuthority.ReclaimNativeTree(t.Context(), home))
	authority, ok := agent.options.HostAuthority.(*guardedHostAuthority)
	require.True(t, ok)
	require.NotEmpty(t, authority.trees.readRoots, "handoff can validate before the next runtime starts")
	agent.closeManagedImageRoots()
	require.Empty(t, authority.trees.readRoots, "Agent close releases read handles")
}

func TestManagedHandoffKeepsPregatesAndConfinesReads(t *testing.T) {
	root := t.TempDir()
	home, handoff := filepath.Join(root, "home"), filepath.Join(root, "handoff")
	data := outputFixture(t, "valid.png")
	inside := handoffFixture(t, home, "image.png", data)
	safe := handoffFixture(t, handoff, "image.png", data)
	agent := NewAgent(WithHostAuthority(&unavailableAuthority{}), WithInputHandoffRoot(handoff))
	require.NoError(t, agent.prepareManagedImageRoots(home, t.TempDir(), []string{root}))
	t.Cleanup(agent.closeManagedImageRoots)
	require.NoError(t, agent.options.HostAuthority.PrepareNativeTree(t.Context(), home))

	images, refusal, err := agent.validatePromptImages(t.Context(), []acp.ContentBlock{safe})
	require.NoError(t, err)
	require.Nil(t, refusal)
	require.Len(t, images, 1)
	link := filepath.Join(handoff, "link.png")
	require.NoError(t, os.Symlink(filepath.Join(home, "image.png"), link))
	for _, block := range []acp.ContentBlock{
		inside, handoffBlock(link, mimeImagePNG, handoffEnvelopeFor(data)),
		handoffBlock(handoff, mimeImagePNG, handoffEnvelopeFor(data)),
	} {
		_, refusal, err = agent.validatePromptImages(t.Context(), []acp.ContentBlock{block})
		require.NoError(t, err)
		require.NotNil(t, refusal)
		require.Equal(t, imageErrorPathNotAllowed, refusal.code)
	}

	// A broad configured root intersecting the home cannot authorize a read.
	agent.options.InputHandoffRoot = root
	_, refusal, err = agent.validatePromptImages(t.Context(), []acp.ContentBlock{safe})
	require.NoError(t, err)
	require.NotNil(t, refusal)
	require.Equal(t, imageErrorPathNotAllowed, refusal.code)
	_, refusal, err = agent.validatePromptImages(t.Context(), []acp.ContentBlock{
		handoffBlock(filepath.Join(home, "image.png"), "application/pdf", handoffEnvelopeFor(data)),
	})
	require.NoError(t, err)
	require.NotNil(t, refusal)
	require.Equal(t, imageErrorInvalidMediaType, refusal.code, "MIME refusal precedes any root lookup")
}

func TestManagedImageRootsPinAtEstablishmentAndDeriveOnlyInsideSafeRoots(t *testing.T) {
	home, scratch, workspace, handoff := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	data := outputFixture(t, "valid.png")
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "image.png"), data, 0o600))
	agent := NewAgent(WithHome(home), WithScratchDir(scratch), WithInputHandoffRoot(handoff),
		WithHostAuthority(&unavailableAuthority{}),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return newSpyCodexClient(), nil }),
	)
	response, err := agent.NewSession(t.Context(), NewSessionRequest(workspace))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, agent.Close()) })
	s, err := agent.session(response.SessionId)
	require.NoError(t, err)
	_, _, err = s.readAllowedImageFile(filepath.Join(workspace, "image.png"))
	require.NoError(t, err)

	authority, ok := agent.options.HostAuthority.(*guardedHostAuthority)
	require.True(t, ok)
	initialHandles := len(authority.trees.readRoots)
	for i := range 65 {
		child := filepath.Join(workspace, strconv.Itoa(i))
		require.NoError(t, os.Mkdir(child, 0o700))
		file := filepath.Join(child, "image.png")
		require.NoError(t, os.WriteFile(file, data, 0o600))
		derived := &session{agent: agent, cwd: child}
		got, _, readErr := derived.readAllowedImageFile(file)
		require.NoError(t, readErr)
		require.Equal(t, data, got)
		_, _, readErr = derived.readAllowedImageFile(filepath.Join(workspace, "image.png"))
		requireImageOutputReason(t, readErr, imageOutputPathDenied)
	}
	require.Len(t, authority.trees.readRoots, initialHandles, "derived reads release their temporary directory handles")
	unknown := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(unknown, "image.png"), data, 0o600))
	_, _, err = (&session{agent: agent, cwd: unknown}).readAllowedImageFile(filepath.Join(unknown, "image.png"))
	requireImageOutputReason(t, err, imageOutputPathDenied)

	// Every later image tree remains inside the reserved scratch domain.
	imageTree := filepath.Join(scratch, "image-tree")
	require.NoError(t, os.Mkdir(imageTree, 0o700))
	require.NoError(t, agent.options.HostAuthority.PrepareNativeTree(t.Context(), imageTree))
	_, _, err = s.readAllowedImageFile(filepath.Join(workspace, "image.png"))
	require.NoError(t, err)
	require.ErrorIs(t, agent.options.HostAuthority.PrepareNativeTree(t.Context(), imageTree), errOpaqueNativeImagePath)
	require.NoError(t, agent.options.HostAuthority.ReclaimNativeTree(t.Context(), imageTree))
	require.Len(t, authority.trees.readRoots, initialHandles, "the home still belongs to the active generation")
}

func TestManagedImageRootsRetainFailedPrepareWithoutInspectingLaterPaths(t *testing.T) {
	home, scratch, workspace := t.TempDir(), t.TempDir(), t.TempDir()
	host := authorityCoverageHost{
		environment: func() map[string]string { return map[string]string{} },
		prepare:     func() error { return ErrHostAuthorityUnavailable },
	}
	agent := NewAgent(WithHostAuthority(host))
	t.Cleanup(agent.closeManagedImageRoots)
	require.NoError(t, agent.prepareManagedImageRoots(home, scratch, []string{workspace, workspace, "", filepath.Join(workspace, "missing")}))
	authority, ok := agent.options.HostAuthority.(*guardedHostAuthority)
	require.True(t, ok)
	require.Len(t, authority.trees.readRoots, 1)
	require.ErrorIs(t, authority.PrepareNativeTree(t.Context(), home), ErrHostAuthorityUnavailable)
	require.ErrorIs(t, agent.prepareManagedImageRoots(home, scratch, []string{workspace}), errOpaqueNativeImagePath)
	require.ErrorIs(t, authority.PrepareNativeTree(t.Context(), home), errOpaqueNativeImagePath)

	// A later path with no filesystem entry must still reach the host untouched.
	missing := filepath.Join(scratch, "missing")
	require.ErrorIs(t, authority.PrepareNativeTree(t.Context(), missing), ErrHostAuthorityUnavailable)
	require.Contains(t, authority.trees.roots, missing, "failed preparation also leaves the tree opaque")
}

func TestManagedScratchCannotBecomePartOfThePreparedHome(t *testing.T) {
	home := t.TempDir()
	child := filepath.Join(home, "scratch")
	require.NoError(t, os.Mkdir(child, 0o700))
	alias := filepath.Join(t.TempDir(), "home-alias")
	require.NoError(t, os.Symlink(home, alias))
	for _, scratch := range []string{home, child, alias} {
		agent := NewAgent(WithHostAuthority(&unavailableAuthority{}), WithHome(home), WithScratchDir(scratch))
		_, err := agent.prepareRuntimeHome(t.Context())
		require.Equal(t, unsupportedField("scratchDir"), err)
	}
	missing := NewAgent(WithHostAuthority(&unavailableAuthority{}), WithHome(home), WithScratchDir(filepath.Join(home, "missing")))
	_, err := missing.prepareRuntimeHome(t.Context())
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = NewAgent(WithHome(home), WithScratchDir(home)).prepareRuntimeHome(t.Context())
	require.NoError(t, err)
}

func TestManagedImageRootsFollowKnownSessionsAcrossRuntimeReplacement(t *testing.T) {
	data := outputFixture(t, "valid.png")
	agent := NewAgent(WithHome(t.TempDir()), WithScratchDir(t.TempDir()),
		WithHostAuthority(&unavailableAuthority{}),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return newRuntimeRecordingClient(), nil }),
	)
	t.Cleanup(func() { require.NoError(t, agent.Close()) })

	// Authentication and deletion can start the shared runtime before a session.
	first, err := agent.sharedRuntime(t.Context())
	require.NoError(t, err)
	sessions := make([]*session, 0, 2)
	for range 2 {
		workspace := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(workspace, "image.png"), data, 0o600))
		response, startErr := agent.NewSession(t.Context(), NewSessionRequest(workspace))
		require.NoError(t, startErr)
		active, lookupErr := agent.session(response.SessionId)
		require.NoError(t, lookupErr)
		_, _, readErr := active.readAllowedImageFile(filepath.Join(workspace, "image.png"))
		requireImageOutputReason(t, readErr, imageOutputPathDenied)
		sessions = append(sessions, active)
	}

	agent.markRuntimeDead(first)
	_, err = agent.sharedRuntime(t.Context(), sessions[0].cwd)
	require.NoError(t, err)
	for _, active := range sessions {
		got, _, readErr := active.readAllowedImageFile(filepath.Join(active.cwd, "image.png"))
		require.NoError(t, readErr)
		require.Equal(t, data, got)
	}
	_, _, err = sessions[0].readAllowedImageFile(filepath.Join(sessions[1].cwd, "image.png"))
	requireImageOutputReason(t, err, imageOutputPathDenied)
}

func TestManagedHandoffValidationPrecedesRecoveryAfterReclaim(t *testing.T) {
	handoff := t.TempDir()
	block := handoffFixture(t, handoff, "image.png", outputFixture(t, "valid.png"))
	launches := 0
	agent := NewAgent(WithHome(t.TempDir()), WithScratchDir(t.TempDir()), WithInputHandoffRoot(handoff),
		WithHostAuthority(&unavailableAuthority{}),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
			launches++

			return newRuntimeRecordingClient(), nil
		}),
	)
	agent.setAgentClient(newRecordingAgentClient())
	t.Cleanup(func() { require.NoError(t, agent.Close()) })
	response, err := agent.NewSession(t.Context(), NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
	active, err := agent.session(response.SessionId)
	require.NoError(t, err)
	require.NoError(t, agent.retireRuntimeGeneration(t.Context(), active.client))
	_, _, err = active.preparePromptInput(t.Context(), []acp.ContentBlock{block})
	// Explicit retirement also closes the lifecycle. Handoff validation must
	// reach that existing recovery refusal instead of inventing a path refusal.
	var requestErr *acp.RequestError
	require.ErrorAs(t, err, &requestErr)
	require.Equal(t, -32603, requestErr.Code)
	require.Equal(t, 2, launches, "valid handoff reaches runtime recovery after reclaim")
}
