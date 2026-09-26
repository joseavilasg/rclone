package accounting

import (
	"context"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/stretchr/testify/assert"
)

// splitTestEntry is a minimal fs.DirEntry to size transfers with.
type splitTestEntry struct {
	remote string
	size   int64
}

func (e splitTestEntry) Remote() string                        { return e.remote }
func (e splitTestEntry) ModTime(ctx context.Context) time.Time { return time.Now() }
func (e splitTestEntry) Size() int64                           { return e.size }
func (e splitTestEntry) Fs() fs.Info                           { return nil }
func (e splitTestEntry) String() string                        { return e.remote }

// splitFs opts a transfer into split progress; plainFs never does.
type splitFs struct{ fs.Fs }

func (splitFs) SplitUploadProgress() bool { return true }

// TestSplitProgressMath pins the 50/50 accounting: source reads fill the
// first half of the bar, reported destination uploads fill the second, and
// reports never push progress past the transfer size.
func TestSplitProgressMath(t *testing.T) {
	ctx := context.Background()
	stats := NewStats(ctx)

	newSplitTransfer := func(size int64) *Transfer {
		tr := newTransfer(stats, splitTestEntry{remote: "f", size: size}, nil, nil)
		tr.mu.Lock()
		tr.split = true
		tr.mu.Unlock()
		return tr
	}

	t.Run("reads fill the first half", func(t *testing.T) {
		tr := newSplitTransfer(1000)
		acc := tr.Account(ctx, nil)
		acc.AccountReadN(400)
		b, s := acc.progress()
		assert.Equal(t, int64(200), b)
		assert.Equal(t, int64(1000), s)
		assert.Equal(t, int64(200), stats.GetBytes())
	})

	t.Run("uploads fill the second half", func(t *testing.T) {
		tr := newSplitTransfer(1000)
		acc := tr.Account(ctx, nil)
		acc.AccountReadN(1000)
		tr.addUploadBytes(1000)
		b, _ := acc.progress()
		assert.Equal(t, int64(1000), b)
	})

	t.Run("uploads never exceed the size", func(t *testing.T) {
		tr := newSplitTransfer(1000)
		acc := tr.Account(ctx, nil)
		acc.AccountReadN(1000)
		tr.addUploadBytes(100000)
		b, _ := acc.progress()
		assert.Equal(t, int64(1000), b)
	})

	t.Run("non-split transfers ignore upload reports", func(t *testing.T) {
		tr := newTransfer(stats, splitTestEntry{remote: "g", size: 1000}, nil, nil)
		acc := tr.Account(ctx, nil)
		acc.AccountReadN(400)
		tr.addUploadBytes(400)
		b, _ := acc.progress()
		assert.Equal(t, int64(400), b)
	})

	t.Run("reports without an account are dropped", func(t *testing.T) {
		tr := newSplitTransfer(1000)
		tr.addUploadBytes(100) // no Account yet: must not panic
		b, s := tr.acc.progress()
		assert.Equal(t, int64(0), b)
		assert.Equal(t, int64(0), s)
	})
}

// TestEnableSplitUpload pins the opt-in wiring: a destination implementing
// fs.UploadProgressSplitter arms the transfer and carries the reporter in
// the returned context; anything else leaves both untouched.
func TestEnableSplitUpload(t *testing.T) {
	ctx := context.Background()
	stats := NewStats(ctx)
	newAccounted := func(dst fs.Fs) (*Transfer, context.Context) {
		tr := newTransfer(stats, splitTestEntry{remote: "h", size: 1000}, nil, dst)
		tr.Account(ctx, nil)
		return tr, tr.EnableSplitUpload(ctx, dst)
	}

	t.Run("split destination reports through the context", func(t *testing.T) {
		tr, splitCtx := newAccounted(splitFs{nil})
		AddUploadProgress(splitCtx, 200)
		b, _ := tr.acc.progress()
		assert.Equal(t, int64(100), b)
	})

	t.Run("plain destination ignores reports", func(t *testing.T) {
		tr, plainCtx := newAccounted(struct{ fs.Fs }{})
		AddUploadProgress(plainCtx, 200)
		b, _ := tr.acc.progress()
		assert.Equal(t, int64(0), b)
	})

	t.Run("bare context is a nil-safe no-op", func(t *testing.T) {
		AddUploadProgress(ctx, 200)
		AddUploadProgress(ctx, 0)
		AddUploadProgress(ctx, -5)
	})
}
