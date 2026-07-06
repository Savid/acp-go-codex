package codexacp

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestSessionInteractionCancellationBranches(t *testing.T) {
	session := &session{}
	release, err := session.acquireTurn(context.Background())
	if err != nil {
		t.Fatalf("acquireTurn returned error: %v", err)
	}
	if _, err := session.acquireTurn(context.Background()); err == nil {
		t.Fatal("acquireTurn ignored prompt backpressure")
	}
	release()

	interactionCtx, finish := session.beginInteraction(context.TODO(), "")
	if interactionCtx.Err() != nil {
		t.Fatal("interaction without parent started canceled")
	}
	finish()
	if interactionCtx.Err() == nil {
		t.Fatal("interaction finish did not cancel context")
	}

	turnParent := context.Background()
	_ = session.beginTurn(turnParent)
	firstCtx, firstFinish := session.beginInteraction(context.Background(), "duplicate")
	secondCtx, secondFinish := session.beginInteraction(context.Background(), "duplicate")
	if firstCtx.Err() == nil {
		t.Fatal("duplicate interaction did not detach first context")
	}
	session.cancelTurn()
	if secondCtx.Err() == nil || !session.wasTurnCancelled() {
		t.Fatal("cancelTurn did not cancel pending interaction")
	}
	firstFinish()
	secondFinish()
	session.finishTurn()

	_ = session.beginTurn(context.Background())
	turnInteraction, turnFinish := session.beginInteraction(context.Background(), "finish-turn")
	session.finishTurn()
	if turnInteraction.Err() == nil {
		t.Fatal("finishTurn did not detach pending interaction")
	}
	turnFinish()
}

func TestSessionSnapshotConcurrentAccountUpdates(t *testing.T) {
	session := &session{
		id:              "s",
		cwd:             "/tmp/project",
		codexThreadID:   "thread",
		model:           "gpt",
		modelProvider:   "openai",
		reasoningEffort: "medium",
		serviceTier:     "flex",
		personality:     "pragmatic",
		accountMeta:     map[string]any{"id": "acct"},
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				session.setAccount(map[string]any{"id": "acct", "planType": "plus"})
			}
		}
	}()
	defer func() {
		close(stop)
		<-done
	}()
	for range 1000 {
		meta := sessionResponseMeta(session.snapshot())
		codexMeta := asType[map[string]any](t, meta[codexMetaKey])
		if _, ok := meta["github.com/savid/acp-go-codex"]; ok {
			t.Fatal("deleted package-path meta was emitted")
		}
		asType[map[string]any](t, codexMeta[codexAccountMetaKey])["id"] = "changed"
		nextMeta := sessionResponseMeta(session.snapshot())
		nextCodexMeta := asType[map[string]any](t, nextMeta[codexMetaKey])
		if asType[map[string]any](t, nextCodexMeta[codexAccountMetaKey])["id"] == "changed" {
			t.Fatal("session account meta aliases response meta")
		}
		_ = codexAuthRequiredError(errors.New("not logged in"), session.accountMetaSnapshot())
	}
}

func TestSessionCloseJoinsClientAndMaterializedErrors(t *testing.T) {
	origRemoveRollout := removeMaterializedRolloutFile
	t.Cleanup(func() { removeMaterializedRolloutFile = origRemoveRollout })
	removeMaterializedRolloutFile = func(string) error {
		return errors.New("remove failed")
	}

	session := &session{
		client:           &errorCodexClient{spyCodexClient: newSpyCodexClient(), closeErr: errors.New("close failed")},
		materializedPath: "/tmp/rollout.jsonl",
	}
	err := session.Close(context.Background())
	if err == nil || !strings.Contains(err.Error(), "close failed") || !strings.Contains(err.Error(), "remove failed") {
		t.Fatalf("joined close error = %v", err)
	}
}
