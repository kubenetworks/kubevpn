// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

package tracer

import (
	"container/list"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"
)

var (
	tickerInterval = 1 * time.Minute
	logSize        = 9000
)

// bucket is a not thread-safe generic implementation of a dynamic collection of elements
// stored under a value-bound key (like time). Inspired by concentrator.rawBucket.
type bucket[K comparable, T any] struct {
	start, duration uint64
	// index is a map of data's entries by aggregating value to avoid iterating data.
	index map[K]*list.Element
	// data is a list because insertion order may be important to users.
	data *list.List
}

func newBucket[K comparable, T any](btime uint64, bsize int64) *bucket[K, T] {
	return &bucket[K, T]{
		start:    btime,
		duration: uint64(bsize),
		index:    make(map[K]*list.Element),
		data:     list.New(),
	}
}

func (b *bucket[K, T]) add(k K, v T) {
	e := b.data.PushBack(v)
	b.index[k] = e
}

func (b *bucket[K, T]) get(k K) (T, bool) {
	e, ok := b.index[k]
	if !ok {
		// Compiler trick to return any zero value in generic code.
		// https://stackoverflow.com/a/70589302
		var zero T
		return zero, ok
	}
	return e.Value.(T), ok
}

func (b *bucket[K, T]) remove(k K) {
	e, ok := b.index[k]
	if !ok {
		return
	}
	delete(b.index, k)
	_ = b.data.Remove(e)
}

func (b *bucket[K, T]) Len() int {
	return b.data.Len()
}

// abandonedSpanCandidate is a struct to store the minimum required information about
// spans that can be abandoned.
type abandonedSpanCandidate struct {
	Name            string
	TraceID, SpanID uint64
	Start           int64
	Finished        bool
}

func newAbandonedSpanCandidate(s *span, finished bool) *abandonedSpanCandidate {
	// finished is explicit instead of implicit as s.finished may be not set
	// at the moment of calling this method.
	// Also, locking is not required as it's called while the span is already locked or it's
	// being initialized.
	return &abandonedSpanCandidate{
		Name:     s.Name,
		TraceID:  s.TraceID,
		SpanID:   s.SpanID,
		Start:    s.Start,
		Finished: finished,
	}
}

// String takes a span and returns a human-readable string representing that span.
func (s *abandonedSpanCandidate) String() string {
	age := now() - s.Start
	a := fmt.Sprintf("%d sec", age/1e9)
	return fmt.Sprintf("[name: %s, span_id: %d, trace_id: %d, age: %s],", s.Name, s.SpanID, s.TraceID, a)
}

type abandonedSpansDebugger struct {
	// buckets holds all the potentially abandoned tracked spans sharded by the configured interval.
	buckets map[int64]*bucket[uint64, *abandonedSpanCandidate]

	// In takes candidate spans and adds them to the debugger.
	In chan *abandonedSpanCandidate

	// waits for any active goroutines
	wg sync.WaitGroup

	// stop causes the debugger to shut down when closed.
	stop chan struct{}

	// stopped reports whether the debugger is stopped (when non-zero).
	stopped uint32

	// addedSpans and removedSpans are internal counters, mainly for testing
	// purposes
	addedSpans, removedSpans uint32
}

// newAbandonedSpansDebugger creates a new abandonedSpansDebugger debugger
func newAbandonedSpansDebugger() *abandonedSpansDebugger {
	d := &abandonedSpansDebugger{
		buckets: make(map[int64]*bucket[uint64, *abandonedSpanCandidate]),
		In:      make(chan *abandonedSpanCandidate, 10000),
	}
	atomic.SwapUint32(&d.stopped, 1)
	return d
}

// Start periodically finds and reports potentially abandoned spans that are older
// than the given interval. These spans are stored in a bucketed linked list,
// sorted by their `Start` time, where the front of the list contains the oldest spans,
// and the end of the list contains the newest spans.
func (d *abandonedSpansDebugger) Start(interval time.Duration) {
	if atomic.SwapUint32(&d.stopped, 0) == 0 {
		// already running
		log.Warn("(*abandonedSpansDebugger).Start called more than once. This is likely a programming error.")
		return
	}
	d.stop = make(chan struct{})
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		tick := time.NewTicker(tickerInterval)
		defer tick.Stop()
		d.runConsumer(tick, &interval)
	}()
}

func (d *abandonedSpansDebugger) runConsumer(tick *time.Ticker, interval *time.Duration) {
	for {
		select {
		case <-tick.C:
			d.log(interval)
		case s := <-d.In:
			if s.Finished {
				d.remove(s, *interval)
			} else {
				d.add(s, *interval)
			}
		case <-d.stop:
			return
		}
	}
}

func (d *abandonedSpansDebugger) Stop() {
	if d == nil {
		return
	}
	if atomic.SwapUint32(&d.stopped, 1) > 0 {
		return
	}
	close(d.stop)
	d.wg.Wait()
	d.log(nil)
}

func (d *abandonedSpansDebugger) add(s *abandonedSpanCandidate, interval time.Duration) {
	// Locking was considered in this method and remove method, but it's not required as long
	// as these methods are called from the single goroutine responsible for debugging
	// the abandoned spans.
	bucketSize := interval.Nanoseconds()
	btime := alignTs(s.Start, bucketSize)
	b, ok := d.buckets[btime]
	if !ok {
		b = newBucket[uint64, *abandonedSpanCandidate](uint64(btime), bucketSize)
		d.buckets[btime] = b
	}

	b.add(s.SpanID, s)
	atomic.AddUint32(&d.addedSpans, 1)
}

func (d *abandonedSpansDebugger) remove(s *abandonedSpanCandidate, interval time.Duration) {
	bucketSize := interval.Nanoseconds()
	btime := alignTs(s.Start, bucketSize)
	b, ok := d.buckets[btime]
	if !ok {
		return
	}
	// If a matching bucket exists, attempt to find the element containing
	// the finished span, then remove that element from the bucket.
	// If a bucket becomes empty, also remove that bucket from the
	// abandoned spans list.
	b.remove(s.SpanID)
	atomic.AddUint32(&d.removedSpans, 1)
	if b.Len() > 0 {
		return
	}
	delete(d.buckets, btime)
}

// log returns a string containing potentially abandoned spans. If `interval` is
// `nil`, it will print all unfinished spans. If `interval` holds a time.Duration, it will
// only print spans that are older than `interval`. It will also truncate the log message to
// `logSize` bytes to prevent overloading the logger.
func (d *abandonedSpansDebugger) log(interval *time.Duration) {
	var (
		sb        strings.Builder
		spanCount = 0
		truncated = false
		curTime   = now()
	)

	if len(d.buckets) == 0 {
		return
	}

	// maps are iterated in random order, and to guarantee that is iterated in
	// creation order, it's required to sort first the buckets' keys.
	keys := make([]int64, 0, len(d.buckets))
	for k := range d.buckets {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return keys[i] < keys[j]
	})
	for _, k := range keys {
		if truncated {
			break
		}

		// Since spans are bucketed by time, finding a bucket that is newer
		// than the allowed time interval means that all spans in this bucket
		// and future buckets will be younger than `interval`, and thus aren't
		// worth checking.
		b := d.buckets[k]
		if interval != nil && curTime-int64(b.start) < interval.Nanoseconds() {
			break
		}

		msg, nSpans := formatAbandonedSpans(b, interval, curTime)
		spanCount += nSpans
		space := logSize - len(sb.String())
		if len(msg) > space {
			msg = msg[0:space]
			truncated = true
		}
		sb.WriteString(msg)
	}

	if spanCount == 0 {
		return
	}

	log.Warn("%d abandoned spans:", spanCount)
	if truncated {
		log.Warn("Too many abandoned spans. Truncating message.")
		sb.WriteString("...")
	}
	log.Warn(sb.String())
}

// formatAbandonedSpans takes a bucket and returns a human-readable string representing
// the contents of it. If `interval` is not nil, it will check if the bucket might
// contain spans older than the user configured timeout. If it does, it will filter for
// older spans. If not, it will print all spans without checking their duration.
func formatAbandonedSpans(b *bucket[uint64, *abandonedSpanCandidate], interval *time.Duration, curTime int64) (string, int) {
	var (
		sb        strings.Builder
		spanCount int
	)
	for e := b.data.Front(); e != nil; e = e.Next() {
		s := e.Value.(*abandonedSpanCandidate)
		// If `interval` is not nil, it will check if the span is older than the
		// user configured timeout, and discard it if it is not.
		if interval != nil && curTime-s.Start < interval.Nanoseconds() {
			continue
		}
		spanCount++
		msg := s.String()
		sb.WriteString(msg)
	}
	return sb.String(), spanCount
}
