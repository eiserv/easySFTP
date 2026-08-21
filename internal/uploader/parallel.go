package uploader

import (
	"context"

	"golang.org/x/sync/errgroup"
)

// runBounded runs fn for indexes [0, count), with at most limit calls active.
// The first error cancels work that has not started; calls already in flight
// finish, which is why their callers must account for partial progress.
func runBounded(ctx context.Context, limit, count int, fn func(context.Context, int) error) error {
	if limit < 1 {
		limit = 1
	}
	g, groupCtx := errgroup.WithContext(ctx)
	g.SetLimit(limit)
	for i := 0; i < count; i++ {
		i := i
		g.Go(func() error {
			if err := groupCtx.Err(); err != nil {
				return err
			}
			return fn(groupCtx, i)
		})
	}
	return g.Wait()
}
