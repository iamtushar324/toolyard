package coderuns

import "sync"

// notifier tells this process's waiters that a run changed. A waiter
// subscribes before it reads the run, so a change committed after that
// read always closes its channel.
type notifier struct {
	mu    sync.Mutex
	chans map[string]chan struct{}
}

func newNotifier() *notifier { return &notifier{chans: map[string]chan struct{}{}} }

// subscribe returns the channel the next notice for id closes.
func (n *notifier) subscribe(id string) chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	ch, ok := n.chans[id]
	if !ok {
		ch = make(chan struct{})
		n.chans[id] = ch
	}
	return ch
}

// notify wakes every waiter of id.
func (n *notifier) notify(id string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if ch, ok := n.chans[id]; ok {
		close(ch)
		delete(n.chans, id)
	}
}

// forget drops ch when it is still id's channel: the waiter saw the run
// final, so no notice will come for it.
func (n *notifier) forget(id string, ch chan struct{}) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if cur, ok := n.chans[id]; ok && cur == ch {
		delete(n.chans, id)
	}
}
