//go:build windows

package queue

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"
)

// staleAfter is how old a lock file must be before it is presumed left over
// from a process that crashed. Windows has no flock that the kernel releases
// on exit, so a lock is a file created exclusively.
const staleAfter = 30 * time.Minute

func acquire(ctx context.Context, path string, wait bool) (func(), error) {
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
			_ = f.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		if st, serr := os.Stat(path); serr == nil && time.Since(st.ModTime()) > staleAfter {
			_ = os.Remove(path)
			continue
		}
		if !wait {
			return nil, ErrLocked
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}
