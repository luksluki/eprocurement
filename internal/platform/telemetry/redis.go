package telemetry

import (
	"context"

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// redisTracer names the instrumentation scope of the go-redis hook.
var redisTracer = otel.Tracer("github.com/luksluki/eprocurement/internal/platform/telemetry/redis")

// RedisHook implements go-redis's Hook interface: one client span per
// command ("redis GET", "redis SET", ...) and one per pipeline batch —
// the "spans around cache" half of PRD §18. The worker is the only
// service dialing Redis directly (export lock + heartbeat, #49), so it is
// also the only pool that installs the hook. No command ARGUMENTS are
// recorded — keys and values stay out of the telemetry pipeline.
type RedisHook struct{}

var _ redis.Hook = RedisHook{}

// DialHook is pass-through: connection setup is boot-time, not a command.
func (RedisHook) DialHook(next redis.DialHook) redis.DialHook { return next }

// ProcessHook wraps each single command in a span.
func (RedisHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		ctx, span := redisTracer.Start(ctx, "redis "+cmd.FullName(),
			trace.WithSpanKind(trace.SpanKindClient),
			trace.WithAttributes(attribute.String("db.system", "redis")))
		defer span.End()
		err := next(ctx, cmd)
		if err != nil && !isRedisNoopErr(err) {
			span.SetStatus(codes.Error, err.Error())
		}
		return err
	}
}

// ProcessPipelineHook wraps one batch per pipeline.
func (RedisHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		ctx, span := redisTracer.Start(ctx, "redis pipeline",
			trace.WithSpanKind(trace.SpanKindClient),
			trace.WithAttributes(
				attribute.Int("db.redis.cmd_count", len(cmds)),
				attribute.String("db.system", "redis"),
			))
		defer span.End()
		err := next(ctx, cmds)
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
		}
		return err
	}
}

// isRedisNoopErr keeps redis.Nil (the documented GET miss) an OK span.
func isRedisNoopErr(err error) bool { return err == redis.Nil }
