package proxy

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// State is what the proxy knows about pool members, kept across reloads:
// whether each is up, and how many calls it has (for least-calls).
type State struct {
	mu    sync.Mutex
	down  map[string]bool
	calls map[string]int
	owner map[string]string // Call-ID -> member key, until BYE
}

func NewState() *State {
	return &State{down: map[string]bool{}, calls: map[string]int{}, owner: map[string]string{}}
}

func (s *State) Up(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.down[key]
}

func (s *State) setUp(key string, up bool) (changed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed = s.down[key] == up
	s.down[key] = !up
	return changed
}

func (s *State) Calls(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[key]
}

// CallStarted counts an answered call against a member.
func (s *State) CallStarted(callID, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, seen := s.owner[callID]; seen {
		return
	}
	s.owner[callID] = key
	s.calls[key]++
}

// CallEnded releases the member a call was counted against.
func (s *State) CallEnded(callID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if key, ok := s.owner[callID]; ok {
		delete(s.owner, callID)
		if s.calls[key] > 0 {
			s.calls[key]--
		}
	}
}

// prober sends OPTIONS to every pool member at its pool's interval and marks
// members up or down. A member starts up; one that fails a probe is skipped
// until a probe succeeds again.
type prober struct {
	client *sipgo.Client
	laddr  sip.Addr
	state  *State
	log    *slog.Logger
}

func (pr *prober) run(ctx context.Context, rt *Runtime) {
	seen := map[string]bool{}
	for _, pool := range rt.Pools {
		for _, m := range pool.Members {
			if seen[m.Key] {
				continue
			}
			seen[m.Key] = true
			go pr.probeLoop(ctx, pool, m)
		}
	}
}

func (pr *prober) probeLoop(ctx context.Context, pool *Pool, m *Member) {
	t := time.NewTicker(pool.OptionsInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			up := pr.probe(ctx, pool, m)
			if pr.state.setUp(m.Key, up) {
				pr.log.Info("member health changed", "member", m.Key, "up", up)
			}
		}
	}
}

func (pr *prober) probe(ctx context.Context, pool *Pool, m *Member) bool {
	uri := m.URI
	if pool.OptionsURI != "" {
		var u sip.Uri
		if err := sip.ParseUri(Expand(pool.OptionsURI, nil, m), &u); err == nil {
			uri = u
		}
	}
	req := sip.NewRequest(sip.OPTIONS, uri)
	req.SetDestination(m.Key)
	laddr := pr.laddr
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	res, err := pr.client.Do(ctx, req, sipgo.ClientRequestBuild, func(_ *sipgo.Client, r *sip.Request) error {
		r.Laddr = laddr
		return nil
	})
	if err != nil {
		return false
	}
	if len(pool.Accept) == 0 {
		return res.StatusCode >= 200
	}
	return pool.Accept[int(res.StatusCode)]
}
