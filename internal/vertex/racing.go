package vertex

import (
	"context"
	"fmt"

	"github.com/bsfdsagfadg/vertex/internal/config"
	"github.com/bsfdsagfadg/vertex/internal/nodes"
)

func RunParallel[T any](ctx context.Context, cfg config.ConfigProvider, run func(context.Context, string) (T, error)) (T, error) {
	return RunRace(ctx, cfg, run)
}

func StreamParallel(ctx context.Context, cfg config.ConfigProvider,
	op func(context.Context, string) <-chan StreamChunk,
	yield func(StreamChunk) bool,
) {
	streamCtx, streamCancel := context.WithCancel(ctx)
	defer streamCancel()

	wrappedOp := func(ctx context.Context, uri string) (<-chan StreamChunk, error) {
		ch := op(ctx, uri)
		var first StreamChunk
		var ok bool
		select {
		case first, ok = <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if !ok {
			return nil, fmt.Errorf("stream: %s closed immediately", nodes.GetNodeName(uri))
		}
		if first.Err != nil {
			return nil, first.Err
		}
		rest := make(chan StreamChunk, 64)
		rest <- first
		go func() {
			defer close(rest)
			for {
				select {
				case <-ctx.Done():
					return
				case chunk, open := <-ch:
					if !open {
						return
					}
					select {
					case rest <- chunk:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
		return rest, nil
	}

	winnerCh, err := RunRace(streamCtx, cfg, wrappedOp, WithNoCancelOnSuccess())
	if err != nil {
		vertexErr, ok := err.(*VertexError)
		if ok {
			yield(StreamChunk{Err: vertexErr})
		} else {
			yield(StreamChunk{Err: NewInternalError(err.Error())})
		}
		return
	}
	for {
		select {
		case <-streamCtx.Done():
			return
		case chunk, open := <-winnerCh:
			if !open || !yield(chunk) {
				return
			}
		}
	}
}
