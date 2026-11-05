package action

import (
	"slices"
	"sync"

	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
)

// connectionRegistry owns membership and selection, not session resources. No
// setup, cleanup, status rendering or peer RPC may run while mu is held.
type connectionRegistry struct {
	mu        sync.RWMutex
	list      handler.Connects
	currentID string
}

func (r *connectionRegistry) snapshot() (handler.Connects, string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Clone(r.list), r.currentID
}

func (r *connectionRegistry) find(id string) (handler.Connection, int) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.findLocked(id)
}

func (r *connectionRegistry) findLocked(id string) (handler.Connection, int) {
	for i, c := range r.list {
		if c.GetConnectionID() == id {
			return c, i
		}
	}
	return nil, -1
}

func (r *connectionRegistry) current() handler.Connection {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, _ := r.findLocked(r.currentID)
	return c
}

// filter preserves requested ID order and first-match semantics, including repeats.
func (r *connectionRegistry) filter(ids []string) (handler.Connects, string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var matched handler.Connects
	for _, id := range ids {
		if c, _ := r.findLocked(id); c != nil {
			matched = append(matched, c)
		}
	}
	return matched, r.currentID
}

func (r *connectionRegistry) add(c handler.Connection, selectCurrent bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.list = append(r.list, c)
	if selectCurrent {
		r.currentID = c.GetConnectionID()
	}
}

// removeByID removes all duplicates in order, atomically updating selection.
func (r *connectionRegistry) removeByID(id string) handler.Connects {
	r.mu.Lock()
	defer r.mu.Unlock()
	var removed handler.Connects
	for i := 0; i < len(r.list); {
		if r.list[i].GetConnectionID() == id {
			removed = removed.Append(r.list[i])
			r.list = slices.Delete(r.list, i, i+1)
		} else {
			i++
		}
	}
	r.resetCurrentLocked(id)
	return removed
}

// removeInstance retires only the observed session. Connect replacement retains
// the selected ID while setup runs, matching the existing reconnect semantics.
func (r *connectionRegistry) removeInstance(c handler.Connection) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, current := range r.list {
		if current == c {
			r.list = slices.Delete(r.list, i, i+1)
			return
		}
	}
}

func (r *connectionRegistry) takeAll() handler.Connects {
	r.mu.Lock()
	defer r.mu.Unlock()
	list := r.list
	r.list, r.currentID = nil, ""
	return list
}

func (r *connectionRegistry) use(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, i := r.findLocked(id); i < 0 {
		return false
	}
	r.currentID = id
	return true
}

func (r *connectionRegistry) resetCurrent(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resetCurrentLocked(id)
}

func (r *connectionRegistry) resetCurrentLocked(id string) {
	if r.currentID != id {
		return
	}
	r.currentID = ""
	if len(r.list) > 0 {
		r.currentID = r.list[0].GetConnectionID()
	}
}
