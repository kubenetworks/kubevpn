package regctl

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/docker/go-units"
	"github.com/regclient/regclient/types"

	"github.com/wencaiwulue/kubevpn/v2/pkg/util/regctl/ascii"
)

type ImageProgress struct {
	mu       sync.Mutex
	Start    time.Time
	Entries  map[string]*ImageProgressEntry
	AsciiOut *ascii.Lines
	Bar      *ascii.ProgressBar
	changed  bool
}

type ImageProgressEntry struct {
	kind        types.CallbackKind
	instance    string
	state       types.CallbackState
	start, last time.Time
	cur, total  int64
	bps         []float64
}

func (ip *ImageProgress) Callback(kind types.CallbackKind, instance string, state types.CallbackState, cur, total int64) {
	// track kind/instance
	ip.mu.Lock()
	defer ip.mu.Unlock()
	ip.changed = true
	now := time.Now()
	if e, ok := ip.Entries[kind.String()+":"+instance]; ok {
		e.state = state
		diff := now.Sub(e.last)
		bps := float64(cur-e.cur) / diff.Seconds()
		e.state = state
		e.last = now
		e.cur = cur
		e.total = total
		if len(e.bps) >= 10 {
			e.bps = append(e.bps[1:], bps)
		} else {
			e.bps = append(e.bps, bps)
		}
	} else {
		ip.Entries[kind.String()+":"+instance] = &ImageProgressEntry{
			kind:     kind,
			instance: instance,
			state:    state,
			start:    now,
			last:     now,
			cur:      cur,
			total:    total,
			bps:      []float64{},
		}
	}
}

func (ip *ImageProgress) Display(final bool) {
	ip.mu.Lock()
	defer ip.mu.Unlock()
	if !ip.changed && !final {
		return // skip since no changes since last display and not the final display
	}
	var manifestTotal, manifestFinished, sum, skipped, queued int64
	// sort entry keys by start time
	keys := make([]string, 0, len(ip.Entries))
	for k := range ip.Entries {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(a, b int) bool {
		if ip.Entries[keys[a]].state != ip.Entries[keys[b]].state {
			return ip.Entries[keys[a]].state > ip.Entries[keys[b]].state
		} else if ip.Entries[keys[a]].state != types.CallbackActive {
			return ip.Entries[keys[a]].last.Before(ip.Entries[keys[b]].last)
		} else {
			return ip.Entries[keys[a]].cur > ip.Entries[keys[b]].cur
		}
	})
	startCount, startLimit := 0, 2
	finishedCount, finishedLimit := 0, 2
	// hide old finished entries
	for i := len(keys) - 1; i >= 0; i-- {
		e := ip.Entries[keys[i]]
		if e.kind != types.CallbackManifest && e.state == types.CallbackFinished {
			finishedCount++
			if finishedCount > finishedLimit {
				e.state = types.CallbackArchived
			}
		}
	}
	for _, k := range keys {
		e := ip.Entries[k]
		switch e.kind {
		case types.CallbackManifest:
			manifestTotal++
			if e.state == types.CallbackFinished || e.state == types.CallbackSkipped {
				manifestFinished++
			}
		default:
			// show progress bars
			if !final && (e.state == types.CallbackActive || (e.state == types.CallbackStarted && startCount < startLimit) || e.state == types.CallbackFinished) {
				if e.state == types.CallbackStarted {
					startCount++
				}
				pre := e.instance + " "
				if len(pre) > 15 {
					pre = pre[:14] + " "
				}
				pct := float64(e.cur) / float64(e.total)
				post := fmt.Sprintf(" %4.2f%% %s/%s", pct*100, units.HumanSize(float64(e.cur)), units.HumanSize(float64(e.total)))
				ip.AsciiOut.Add(ip.Bar.Generate(pct, pre, post))
			}
			// track stats
			if e.state == types.CallbackSkipped {
				skipped += e.total
			} else if e.total > 0 {
				sum += e.cur
				queued += e.total - e.cur
			}
		}
	}
	// show stats summary
	ip.AsciiOut.Add([]byte(fmt.Sprintf("Manifests: %d/%d | Blobs: %s copied, %s skipped",
		manifestFinished, manifestTotal,
		units.HumanSize(float64(sum)),
		units.HumanSize(float64(skipped)))))
	if queued > 0 {
		ip.AsciiOut.Add([]byte(fmt.Sprintf(", %s queued",
			units.HumanSize(float64(queued)))))
	}
	ip.AsciiOut.Add([]byte(fmt.Sprintf(" | Elapsed: %ds\n", int64(time.Since(ip.Start).Seconds()))))
	ip.AsciiOut.Flush()
	if !final {
		ip.AsciiOut.Return()
	}
}
