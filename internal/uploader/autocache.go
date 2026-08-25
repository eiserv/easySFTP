package uploader

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/eiserv/easySFTP/internal/autocache"
	"github.com/eiserv/easySFTP/internal/config"
	"github.com/eiserv/easySFTP/internal/metrics"
)

// autoCache is the run's side of internal/autocache: it reads the file once
// before the network is touched, offers the record to the policy when the link
// probe has confirmed it, and writes back what this run learned.
//
// Everything here is best effort by construction. The cache exists to save a
// run from planning against a guess; a cache that could fail a deploy would be
// a bad trade at any hit rate, so every path below degrades to "carry on
// without it" and says so once (issue #212).
type autoCache struct {
	path  string
	store *autocache.Store

	// target is the fingerprint this run reads and writes under. It is
	// derived from the connection, never from the deployment: what a record
	// describes is a path to a server.
	target string

	// candidate survives the pre-connection gates and is waiting for the link
	// probe to confirm or reject it; decision is the answer, once there is
	// one.
	candidate autocache.Candidate
	decision  autocache.Decision
}

// enabled reports whether this run has a cache at all.
func (c *autoCache) enabled() bool { return c != nil && c.path != "" }

// targetIdentity is what a record is keyed by: where this run connects and as
// whom, plus the jump host when there is one, because a different bastion is a
// different path even to the same server. It is hashed before it is stored;
// see autocache.Record.Target.
func targetIdentity(cfg *config.Config) string {
	id := fmt.Sprintf("%s:%d:%s", cfg.Server, cfg.Port, cfg.Username)
	if p := cfg.Proxy; p != nil {
		id += fmt.Sprintf(" via %s:%d:%s", p.Server, p.Port, p.Username)
	}
	return id
}

// openAutoCache loads the cache file, if the configuration named one. A run
// without advanced.auto_cache gets a value that answers false to enabled() and
// does nothing from then on.
//
// A file that is missing is the normal first run. A file that cannot be read
// or understood costs one warning: the user asked for a cache and is entitled
// to know it is not working, and the run continues exactly as it would have
// without one.
func openAutoCache(cfg *config.Config, log Logger) *autoCache {
	if cfg.AutoCachePath == "" {
		return &autoCache{}
	}
	c := &autoCache{path: cfg.AutoCachePath, target: autocache.Fingerprint(targetIdentity(cfg))}
	store, err := autocache.Load(cfg.AutoCachePath)
	c.store = store
	switch {
	case errors.Is(err, autocache.ErrEmpty):
		if cfg.Debug() {
			log.Infof("auto tuning: no cache at %s yet; this run will write one", cfg.AutoCachePath)
		}
	case err != nil:
		log.Warningf("could not use the auto-tuning cache at %s (%v); planning from easySFTP's own assumptions and rewriting it at the end of this run", cfg.AutoCachePath, err)
	}
	return c
}

// lookup runs the gates that need no connection: the format, the policy
// generation, the age, the reuse budget and the workload anchor. The workload
// is the whole run's plan, which is also what a write-back stores as the
// anchor: a lookup and an anchor have to be the same kind of number or the
// comparison means nothing.
func (c *autoCache) lookup(w autocache.Workload) {
	if !c.enabled() {
		return
	}
	c.candidate, c.decision = c.store.Lookup(c.target, w, time.Now())
}

// confirm is the second half of the lookup and the validation issue #212 asks
// for: the round-trip time this run has just measured, against the one the
// record was written with. It is as cheap as validation gets, because the
// probe runs anyway.
//
// It returns the decision so the caller can hand it to the policy. A run
// without a cache, or one that already missed, gets a decision that restores
// nothing.
func (c *autoCache) confirm(rtt time.Duration, log Logger, debug bool) autocache.Decision {
	if !c.enabled() {
		return c.decision
	}
	if c.decision.Reason == "" {
		// Nothing rejected the record before the connection, so this is the
		// gate it still has to pass. A record that already missed keeps the
		// sentence it missed with, which is the one worth logging.
		c.decision = c.candidate.Validate(rtt)
	}
	switch {
	case c.decision.Restores():
		log.Infof("auto tuning: reusing what an earlier run measured against this server (%s), checked against the link this run measured (%s)",
			restored(c.decision), c.decision.Reason)
	case debug && c.decision.Hit:
		log.Infof("auto tuning: the cached record for this server matches (%s) but carries nothing to reuse", c.decision.Reason)
	case debug:
		log.Infof("auto tuning: not reusing the cached settings (%s)", c.decision.Reason)
	}
	return c.decision
}

// restored describes what a hit actually hands the policy, for the log line.
func restored(d autocache.Decision) string {
	parts := make([]string, 0, 3)
	if d.StreamBytesPerSecond > 0 {
		parts = append(parts, fmt.Sprintf("%s/s per connection", HumanSize(int64(d.StreamBytesPerSecond))))
	}
	if d.ConnectionCeiling > 0 {
		parts = append(parts, fmt.Sprintf("at most %d connection(s)", d.ConnectionCeiling))
	}
	if d.Reused > 0 {
		parts = append(parts, fmt.Sprintf("reused %d time(s) so far", d.Reused))
	}
	return strings.Join(parts, ", ")
}

// save writes back what this run learned. It is called once, at the end of the
// run, whether or not the run succeeded: a refused connection and a measured
// throughput are both worth remembering, and a deploy that failed halfway
// still met the same server over the same path.
func (c *autoCache) save(obs autocache.Observation, log Logger, debug bool) {
	if !c.enabled() {
		return
	}
	obs.Target = c.target
	obs.Reused = c.decision.Hit
	if !c.store.Update(obs, time.Now()) {
		if debug {
			log.Infof("auto tuning: the cache at %s already says everything this run learned", c.path)
		}
		return
	}
	if err := autocache.Save(c.path, c.store); err != nil {
		log.Warningf("could not write the auto-tuning cache at %s (%v); the next run plans from easySFTP's own assumptions", c.path, err)
		return
	}
	if debug {
		log.Infof("auto tuning: recorded this run's measurements in %s", c.path)
	}
}

// report writes what the cache did into the run counters, so a benchmark (and
// a bug report) can tell a run that planned from a measurement apart from one
// that planned from the prior. A run without a cache reports nothing at all,
// which is how every other optional feature behaves here.
func (c *autoCache) report() {
	if !c.enabled() {
		return
	}
	hit := int64(0)
	if c.decision.Restores() {
		hit = 1
	}
	metrics.Set("auto_cache_hit", hit)
	metrics.Set("auto_cache_stream_bytes_per_second", int64(c.decision.StreamBytesPerSecond))
	metrics.Set("auto_cache_connection_ceiling", int64(c.decision.ConnectionCeiling))
	metrics.Set("auto_cache_reused", int64(c.decision.Reused))
}
