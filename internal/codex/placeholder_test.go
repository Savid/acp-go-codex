package codex

import (
	"context"
	"errors"
	"testing"
)

func TestPlaceholderClientRunTurn(t *testing.T) {
	client := NewPlaceholderClient(Options{DefaultModel: "gpt-5.5"})
	ctx := context.Background()

	thread, err := client.StartThread(ctx, ThreadStartRequest{Cwd: "/tmp/project"})
	if err != nil {
		t.Fatalf("StartThread returned error: %v", err)
	}
	if thread.ID == "" {
		t.Fatal("StartThread returned empty thread id")
	}
	if thread.Model != "gpt-5.5" {
		t.Fatalf("thread model = %q, want default model", thread.Model)
	}

	events, err := client.RunTurn(ctx, TurnStartRequest{ThreadID: thread.ID, Prompt: []UserInput{{"type": "text", "text": "hello"}}})
	if err != nil {
		t.Fatalf("RunTurn returned error: %v", err)
	}

	var got []Event
	for event := range events {
		got = append(got, event)
	}

	if len(got) != 4 {
		t.Fatalf("event count = %d, want 4", len(got))
	}
	if got[0].Kind != EventPlanUpdated {
		t.Fatalf("first event kind = %q, want %q", got[0].Kind, EventPlanUpdated)
	}
	if got[2].Kind != EventAgentMessageDelta || got[2].Text == "" {
		t.Fatalf("agent message event = %#v", got[2])
	}
	if got[3].Kind != EventCompleted || got[3].StopReason != StopReasonEndTurn {
		t.Fatalf("completion event = %#v", got[3])
	}
}

func TestPlaceholderClientUnknownThread(t *testing.T) {
	client := NewPlaceholderClient(Options{})

	_, err := client.RunTurn(context.Background(), TurnStartRequest{ThreadID: "missing"})
	if !errors.Is(err, ErrThreadNotFound) {
		t.Fatalf("RunTurn error = %v, want ErrThreadNotFound", err)
	}
}

func TestPlaceholderClientLifecycleMethods(t *testing.T) {
	client := NewPlaceholderClient(Options{DefaultModel: "gpt-default", Env: map[string]string{"A": "B"}})
	ctx := context.Background()

	resumed, err := client.ResumeThread(ctx, ThreadResumeRequest{ThreadID: "thread-a", Cwd: "/repo"})
	if err != nil {
		t.Fatalf("ResumeThread returned error: %v", err)
	}
	forked, err := client.ForkThread(ctx, ThreadForkRequest{ThreadID: resumed.ID, Cwd: "/fork"})
	if err != nil {
		t.Fatalf("ForkThread returned error: %v", err)
	}
	threads, err := client.ListThreads(ctx, ThreadListRequest{Cwd: "/repo"})
	if err != nil || len(threads) != 1 {
		t.Fatalf("ListThreads len=%d err=%v", len(threads), err)
	}
	history, err := client.ReadThread(ctx, ThreadReadRequest{ThreadID: resumed.ID})
	if err != nil || history.Thread.ID != resumed.ID {
		t.Fatalf("ReadThread = %#v err=%v", history, err)
	}
	turns, err := client.ListTurns(ctx, ThreadTurnsListRequest{ThreadID: resumed.ID})
	if err != nil || len(turns.Turns) != 1 {
		t.Fatalf("ListTurns = %#v err=%v", turns, err)
	}
	if err := client.SteerTurn(ctx, TurnSteerRequest{ThreadID: resumed.ID}); err != nil {
		t.Fatalf("SteerTurn returned error: %v", err)
	}
	if err := client.CancelTurn(ctx, resumed.ID, "turn"); err != nil {
		t.Fatalf("CancelTurn returned error: %v", err)
	}
	if _, err := client.CompactThread(ctx, ThreadCompactRequest{ThreadID: resumed.ID}); err != nil {
		t.Fatalf("CompactThread returned error: %v", err)
	}
	if _, err := client.StartReview(ctx, ReviewStartRequest{ThreadID: resumed.ID, Target: map[string]any{"type": "custom"}}); err != nil {
		t.Fatalf("StartReview returned error: %v", err)
	}
	if modes, err := client.CollaborationModeList(ctx); err != nil || len(modes.Modes) != 2 {
		t.Fatalf("CollaborationModeList = %#v err=%v", modes, err)
	}
	if status, err := client.MCPServerStatusList(ctx, resumed.ID); err != nil || status.Raw == nil {
		t.Fatalf("MCPServerStatusList = %#v err=%v", status, err)
	}
	if err := client.UnsubscribeThread(ctx, resumed.ID); err != nil {
		t.Fatalf("UnsubscribeThread returned error: %v", err)
	}
	if err := client.DeleteThread(ctx, ThreadDeleteRequest{ThreadID: resumed.ID}); err != nil {
		t.Fatalf("DeleteThread returned error: %v", err)
	}
	if err := client.DeleteThread(ctx, ThreadDeleteRequest{}); err != nil {
		t.Fatalf("DeleteThread empty thread returned error: %v", err)
	}
	if _, err := client.ReadThread(ctx, ThreadReadRequest{ThreadID: resumed.ID}); !errors.Is(err, ErrThreadNotFound) {
		t.Fatalf("ReadThread after DeleteThread error = %v, want ErrThreadNotFound", err)
	}
	if err := client.DeleteThread(ctx, ThreadDeleteRequest{ThreadID: resumed.ID}); !errors.Is(err, ErrThreadNotFound) {
		t.Fatalf("DeleteThread missing error = %v, want ErrThreadNotFound", err)
	}
	if forked.ID == resumed.ID {
		t.Fatalf("fork did not create a new thread: %q", forked.ID)
	}
}

func TestPlaceholderClientModelAccountAndClose(t *testing.T) {
	client := NewPlaceholderClient(Options{DefaultModel: "gpt-default", Env: map[string]string{"A": "B"}})
	ctx := context.Background()

	if models, err := client.ModelList(ctx); err != nil || models[0].ID != "gpt-default" {
		t.Fatalf("ModelList = %#v err=%v", models, err)
	}
	if account, err := client.AccountRead(ctx); err != nil || account.Raw == nil {
		t.Fatalf("AccountRead = %#v err=%v", account, err)
	}
	if err := client.LoginWithChatGPTTokens(ctx, ChatGPTAuthTokens{AccessToken: "a"}); err != nil {
		t.Fatalf("LoginWithChatGPTTokens returned error: %v", err)
	}
	if err := client.Logout(ctx); err != nil {
		t.Fatalf("Logout returned error: %v", err)
	}
	zeroClient := &PlaceholderClient{threads: map[string]Thread{}}
	if _, err := zeroClient.StartThread(ctx, ThreadStartRequest{Cwd: "/zero"}); err != nil {
		t.Fatalf("zero-thread StartThread returned error: %v", err)
	}
	zeroResume := &PlaceholderClient{threads: map[string]Thread{}}
	if _, err := zeroResume.ResumeThread(ctx, ThreadResumeRequest{ThreadID: "resume-zero"}); err != nil {
		t.Fatalf("zero-thread ResumeThread returned error: %v", err)
	}
	if err := client.Close(ctx); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if _, err := client.StartThread(ctx, ThreadStartRequest{}); err == nil {
		t.Fatal("StartThread after close succeeded")
	}
}

func TestPlaceholderClientErrorBranches(t *testing.T) {
	client := NewPlaceholderClient(Options{})
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.StartThread(canceled, ThreadStartRequest{}); err == nil {
		t.Fatal("StartThread with canceled context succeeded")
	}
	if _, err := client.ResumeThread(canceled, ThreadResumeRequest{}); err == nil {
		t.Fatal("ResumeThread with canceled context succeeded")
	}
	if _, err := client.ForkThread(canceled, ThreadForkRequest{}); err == nil {
		t.Fatal("ForkThread with canceled context succeeded")
	}
	if _, err := client.ListThreads(canceled, ThreadListRequest{}); err == nil {
		t.Fatal("ListThreads with canceled context succeeded")
	}
	if _, err := client.ReadThread(canceled, ThreadReadRequest{}); err == nil {
		t.Fatal("ReadThread with canceled context succeeded")
	}
	if _, err := client.ListTurns(canceled, ThreadTurnsListRequest{}); err == nil {
		t.Fatal("ListTurns with canceled context succeeded")
	}
	if _, err := client.RunTurn(canceled, TurnStartRequest{}); err == nil {
		t.Fatal("RunTurn with canceled context succeeded")
	}
	if err := client.SteerTurn(canceled, TurnSteerRequest{}); err == nil {
		t.Fatal("SteerTurn with canceled context succeeded")
	}
	if _, err := client.CompactThread(canceled, ThreadCompactRequest{}); err == nil {
		t.Fatal("CompactThread with canceled context succeeded")
	}
	if _, err := client.StartReview(canceled, ReviewStartRequest{}); err == nil {
		t.Fatal("StartReview with canceled context succeeded")
	}

	ctx := context.Background()
	if _, err := client.ForkThread(ctx, ThreadForkRequest{ThreadID: "missing"}); !errors.Is(err, ErrThreadNotFound) {
		t.Fatalf("ForkThread missing error = %v", err)
	}
	if _, err := client.ReadThread(ctx, ThreadReadRequest{ThreadID: "missing"}); !errors.Is(err, ErrThreadNotFound) {
		t.Fatalf("ReadThread missing error = %v", err)
	}
	if _, err := client.ListTurns(ctx, ThreadTurnsListRequest{ThreadID: "missing"}); !errors.Is(err, ErrThreadNotFound) {
		t.Fatalf("ListTurns missing error = %v", err)
	}
	if err := client.SteerTurn(ctx, TurnSteerRequest{ThreadID: "missing"}); !errors.Is(err, ErrThreadNotFound) {
		t.Fatalf("SteerTurn missing error = %v", err)
	}
	if _, err := client.CompactThread(ctx, ThreadCompactRequest{ThreadID: "missing"}); !errors.Is(err, ErrThreadNotFound) {
		t.Fatalf("CompactThread missing error = %v", err)
	}
	if _, err := client.StartReview(ctx, ReviewStartRequest{ThreadID: "missing"}); !errors.Is(err, ErrThreadNotFound) {
		t.Fatalf("StartReview missing error = %v", err)
	}

	thread, err := client.StartThread(ctx, ThreadStartRequest{})
	if err != nil {
		t.Fatalf("StartThread returned error: %v", err)
	}
	runCtx, runCancel := context.WithCancel(ctx)
	events, err := client.RunTurn(runCtx, TurnStartRequest{ThreadID: thread.ID})
	if err != nil {
		t.Fatalf("RunTurn returned error: %v", err)
	}
	runCancel()
	for range events {
	}
	if err := client.Close(ctx); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if _, err := client.ResumeThread(ctx, ThreadResumeRequest{}); err == nil {
		t.Fatal("ResumeThread after close succeeded")
	}
	if _, err := client.RunTurn(ctx, TurnStartRequest{ThreadID: thread.ID}); err == nil {
		t.Fatal("RunTurn after close succeeded")
	}
}
