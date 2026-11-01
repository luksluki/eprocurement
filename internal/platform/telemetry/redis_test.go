package telemetry

import (
	"context"
	"errors"
	"testing"

	"github.com/redis/go-redis/v9"
)

// AC #5 (cache spans): each command and each pipeline runs under a span
// that joins the caller's trace; errors propagate unchanged.
func TestRedisHookSpansCommands(t *testing.T) {
	parent, span := testTracer.Start(context.Background(), "export run")
	defer span.End()
	want := spanContextOf(parent).TraceID().String()

	hook := RedisHook{}
	var captured context.Context
	next := hook.ProcessHook(func(ctx context.Context, cmd redis.Cmder) error {
		captured = ctx
		return nil
	})
	cmd := redis.NewCmd(context.Background(), "GET", "export:heartbeat:t-1")
	if err := next(parent, cmd); err != nil {
		t.Fatalf("process: %v", err)
	}
	if got := spanContextOf(captured).TraceID().String(); got != want {
		t.Fatalf("redis span trace id = %q, want the caller's %q", got, want)
	}

	// Failure path: the hook is transparent to command errors.
	sentinel := errors.New("redis down")
	errNext := hook.ProcessHook(func(ctx context.Context, cmd redis.Cmder) error { return sentinel })
	if err := errNext(parent, cmd); !errors.Is(err, sentinel) {
		t.Fatalf("command error swallowed: %v", err)
	}
}

func TestRedisHookSpansPipelines(t *testing.T) {
	parent, span := testTracer.Start(context.Background(), "export run")
	defer span.End()

	hook := RedisHook{}
	var captured context.Context
	next := hook.ProcessPipelineHook(func(ctx context.Context, cmds []redis.Cmder) error {
		captured = ctx
		return nil
	})
	cmds := []redis.Cmder{
		redis.NewCmd(context.Background(), "SET", "k", "v"),
		redis.NewCmd(context.Background(), "GET", "k"),
	}
	if err := next(parent, cmds); err != nil {
		t.Fatalf("pipeline: %v", err)
	}
	if !spanContextOf(captured).IsValid() {
		t.Fatal("pipeline ran without a span context")
	}
}
