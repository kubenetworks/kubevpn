package upgrade

import (
	"io"
	"sync"
	"sync/atomic"
)

type progress struct {
	sync.Mutex
	updates    chan<- Update
	lastUpdate *Update
}

type Update struct {
	Total    int64
	Complete int64
	Error    error
}

func (p *progress) total(delta int64) {
	atomic.AddInt64(&p.lastUpdate.Total, delta)
}

func (p *progress) complete(delta int64) {
	p.Lock()
	defer p.Unlock()
	p.updates <- Update{
		Total:    p.lastUpdate.Total,
		Complete: atomic.AddInt64(&p.lastUpdate.Complete, delta),
	}
}

func (p *progress) err(err error) error {
	if err != nil && p.updates != nil {
		p.updates <- Update{Error: err}
	}
	return err
}

type progressReader struct {
	rc io.ReadCloser

	count    *int64 // number of bytes this reader has read, to support resetting on retry.
	progress *progress
}

func (r *progressReader) Read(b []byte) (int, error) {
	n, err := r.rc.Read(b)
	if err != nil {
		return n, err
	}
	atomic.AddInt64(r.count, int64(n))
	// TODO: warn/debug log if sending takes too long, or if sending is blocked while context is canceled.
	r.progress.complete(int64(n))
	return n, nil
}

func (r *progressReader) Close() error { return r.rc.Close() }
