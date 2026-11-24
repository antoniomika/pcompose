package run

import (
	"context"
	"io"
	"sync"

	"github.com/picosh/pico/pkg/pssh"
)

// Keep returns a session whose commands run until ctx ends, even if the
// client leaves first.
func Keep(s Session, ctx context.Context) Session {
	return &kept{Session: s, ctx: ctx}
}

type kept struct {
	Session
	ctx context.Context
}

func (k *kept) Context() context.Context { return k.ctx }

func (k *kept) Write(p []byte) (int, error) {
	_, _ = k.Session.Write(p)
	return len(p), nil
}

func (k *kept) Stderr() io.ReadWriter { return &keptStderr{k.Session.Stderr()} }

type keptStderr struct{ io.ReadWriter }

func (k *keptStderr) Write(p []byte) (int, error) {
	_, _ = k.ReadWriter.Write(p)
	return len(p), nil
}

// Detached is a Session with no client. It has no input, and writes its
// output and errors to one writer.
type Detached struct {
	ctx context.Context

	mu sync.Mutex
	w  io.Writer
}

// NewDetached returns a session that writes to w.
func NewDetached(ctx context.Context, w io.Writer) *Detached {
	return &Detached{ctx: ctx, w: w}
}

func (d *Detached) Read([]byte) (int, error) { return 0, io.EOF }

func (d *Detached) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.w.Write(p)
}

func (d *Detached) Stderr() io.ReadWriter { return d }

func (d *Detached) Context() context.Context { return d.ctx }

func (d *Detached) Pty() (*pssh.Pty, <-chan pssh.Window, bool) { return nil, nil, false }

// Locks holds one lock per key. The zero value is ready to use.
type Locks struct{ m sync.Map }

// Lock holds key until unlock is called, waiting while another caller holds
// it, or until ctx ends. It calls waiting once if it waits.
func (l *Locks) Lock(ctx context.Context, key string, waiting func()) (unlock func(), err error) {
	v, _ := l.m.LoadOrStore(key, make(chan struct{}, 1))
	held := v.(chan struct{})

	select {
	case held <- struct{}{}:
	default:
		waiting()

		select {
		case held <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	return func() { <-held }, nil
}
