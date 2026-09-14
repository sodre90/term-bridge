// Package relay is the home-server rendezvous: it accepts Mac agents'
// outbound yamux tunnels and reverse-proxies authenticated app requests over
// them, one yamux stream per request, one tunnel slot per tenant. It owns
// tenant/device auth, pairing, and FCM push.
package relay

import (
	"sync"

	"github.com/hashicorp/yamux"

	"github.com/sodre90/term-bridge/internal/metrics"
)

// Registry holds one active agent tunnel session per tenant. A new session
// for a tenant replaces and closes that tenant's prior session; it never
// touches other tenants' sessions.
type Registry struct {
	mu    sync.Mutex
	sess  map[string]*yamux.Session
	stops map[string]func()
}

func NewRegistry() *Registry {
	return &Registry{
		sess:  map[string]*yamux.Session{},
		stops: map[string]func(){},
	}
}

// Set installs sess as tenantID's current session, closing and stopping any
// prior session for that same tenant. stop may be nil. Other tenants are
// untouched.
func (r *Registry) Set(tenantID string, sess *yamux.Session, stop func()) {
	r.mu.Lock()
	oldSess, oldStop := r.sess[tenantID], r.stops[tenantID]
	r.sess[tenantID] = sess
	if stop != nil {
		r.stops[tenantID] = stop
	} else {
		delete(r.stops, tenantID)
	}
	metrics.TunnelsActive.Set(int64(len(r.sess)))
	r.mu.Unlock()

	if oldStop != nil {
		oldStop()
	}
	if oldSess != nil {
		_ = oldSess.Close()
	}
}

// Get returns tenantID's active session, or nil when none is connected, it
// has closed, or the tenant is unknown.
func (r *Registry) Get(tenantID string) *yamux.Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	sess := r.sess[tenantID]
	if sess != nil && sess.IsClosed() {
		return nil
	}
	return sess
}

// Clear removes tenantID's session if sess is still the one on record.
func (r *Registry) Clear(tenantID string, sess *yamux.Session) {
	r.mu.Lock()
	var stop func()
	if r.sess[tenantID] == sess {
		stop = r.stops[tenantID]
		delete(r.sess, tenantID)
		delete(r.stops, tenantID)
		metrics.TunnelsActive.Set(int64(len(r.sess)))
	}
	r.mu.Unlock()
	if stop != nil {
		stop()
	}
}
