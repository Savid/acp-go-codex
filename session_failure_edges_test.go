package codexacp

import (
	"context"
	"errors"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

func TestMapTurnFailureBranches(t *testing.T) {
	failSession := &session{agent: NewAgent(), id: "map"}

	unknown := failSession.mapTurnFailure(errors.Join(codex.ErrThreadNotFound, errors.New("drift")))

	var reqErr *acp.RequestError
	if !errors.As(unknown, &reqErr) || reqErr.Code != -32602 {
		t.Fatalf("thread-not-found mapped to %v, want -32602 unknown session", unknown)
	}

	auth := failSession.mapTurnFailure(errors.New("unauthorized: 401"))
	if !errors.As(auth, &reqErr) || reqErr.Code != -32000 {
		t.Fatalf("auth failure mapped to %v, want -32000", auth)
	}

	provider := failSession.mapTurnFailure(errors.New("weird provider glitch"))
	data := turnFailureData(t, provider)
	if data[jsonFieldCause] != codex.CauseProvider {
		t.Fatalf("generic failure cause = %v, want provider", data[jsonFieldCause])
	}
	if failSession.clientDead {
		t.Fatal("provider failure must not mark the client dead")
	}
}

func TestEnsureLiveClientRelaunchFailures(t *testing.T) {
	ctx := context.Background()

	factoryErr := errors.New("relaunch factory failed")
	newClientFails := &session{
		agent:         NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return nil, factoryErr })),
		id:            "relaunch-factory",
		codexThreadID: "thread",
		clientDead:    true,
	}
	if err := newClientFails.ensureLiveClient(ctx); !errors.Is(err, factoryErr) {
		t.Fatalf("ensureLiveClient factory error = %v", err)
	}
	if !newClientFails.clientDead {
		t.Fatal("failed relaunch must leave client dead")
	}

	resumeErr := errors.New("resume rejected")
	resumeClient := &errorCodexClient{spyCodexClient: newSpyCodexClient(), resumeErr: resumeErr}
	resumeFails := &session{
		agent:         NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return resumeClient, nil })),
		id:            "relaunch-resume",
		codexThreadID: "thread",
		clientDead:    true,
	}
	if err := resumeFails.ensureLiveClient(ctx); !errors.Is(err, resumeErr) {
		t.Fatalf("ensureLiveClient resume error = %v", err)
	}

	// A prompt on a dead session whose relaunch fails surfaces the transport
	// failure and keeps the session addressable.
	if _, err := resumeFails.Prompt(ctx, TextPromptRequest("relaunch-resume", "hi")); !isTurnFailure(err, codex.CauseTransport) {
		t.Fatalf("prompt after failed relaunch = %v, want transport failure", err)
	}
}

func TestRecordRawEmitFailure(t *testing.T) {
	recordSession := &session{agent: NewAgent(), id: "record"}
	recordSession.recordRawEmitFailure(context.Background(), nil)
	if recordSession.rawEmitFailures != 0 {
		t.Fatal("nil raw emit error advanced the counter")
	}

	recordSession.recordRawEmitFailure(context.Background(), errors.New("emit failed"))
	if recordSession.rawEmitFailures != 1 {
		t.Fatalf("raw emit failure counter = %d, want 1", recordSession.rawEmitFailures)
	}
}

func TestCodexThreadACPErrorBranches(t *testing.T) {
	if err := codexThreadACPError(errors.New("unauthorized"), nil); err == nil {
		t.Fatal("auth error returned nil")
	} else {
		var reqErr *acp.RequestError
		if !errors.As(err, &reqErr) || reqErr.Code != -32000 {
			t.Fatalf("auth error mapped to %v, want -32000", err)
		}
	}

	if err := codexThreadACPError(errors.Join(codex.ErrThreadNotFound, errors.New("gone")), nil); err == nil {
		t.Fatal("thread-not-found returned nil")
	} else {
		var reqErr *acp.RequestError
		if !errors.As(err, &reqErr) || reqErr.Code != -32602 {
			t.Fatalf("thread-not-found mapped to %v, want -32602", err)
		}
	}

	passthrough := errors.New("some other error")
	if err := codexThreadACPError(passthrough, nil); !errors.Is(err, passthrough) {
		t.Fatalf("passthrough error = %v", err)
	}
}

func TestDurableRolloutEntriesSkipsMirroredRows(t *testing.T) {
	skipSession := &session{mirroredRows: 1}
	entries, next := skipSession.durableRolloutEntries([]rolloutMirrorRow{
		{index: 0, entry: SessionStoreEntry(`{"type":"already"}`)},
		{index: 1, entry: SessionStoreEntry(`{"type":"new"}`)},
	})
	if len(entries) != 1 || next != 2 {
		t.Fatalf("durable entries=%d next=%d, want 1 and 2", len(entries), next)
	}
}
