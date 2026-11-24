package auth

import (
	"net/netip"
	"strings"
	"sync"
	"time"
)

const (
	pairAttempts  = 5
	ipAttempts    = 20
	attemptWindow = 15 * time.Minute
	blockFor      = 15 * time.Minute
	userAttempts  = 5
	userDelay     = 2 * time.Second

	// pruneAbove is the map size above which Allow drops idle entries.
	pruneAbove = 1024
	// maxUserKey is the longest user name an account can have; longer names, which match no account, are counted
	// under their first maxUserKey bytes, so that a sign-in cannot store a name of any length.
	maxUserKey = 64
)

// Limiter limits sign-in attempts per user and source, per source and, with a
// short delay only, per user. A source is an IPv4 address or an IPv6 /64, the
// smallest network a host usually gets. Blocks never span sources, so that no
// peer can lock a user out of the other sources; the delay does, so that
// rotating sources does not escape it. Allow counts an attempt as a failure
// the moment it lets it through, so concurrent requests cannot pass before
// earlier ones have failed; Succeed takes the attempts back. It is safe for
// concurrent use.
type Limiter struct {
	mu    sync.Mutex
	pairs map[pairKey]*counter
	ips   map[string]*counter
	users map[string]*counter
}

// pairKey identifies the attempts of one user from one source.
type pairKey struct {
	user, ip string
}

// counter holds the recent attempts of one key and its block.
type counter struct {
	attempts     []time.Time
	blockedUntil time.Time
}

// SignInLimiter returns a limiter with the limits of the constants above: a block of a user at a source, a block of
// a source, and a delay on every attempt of a user who failed often from any sources.
func SignInLimiter() *Limiter {
	return &Limiter{pairs: map[pairKey]*counter{}, ips: map[string]*counter{}, users: map[string]*counter{}}
}

// Allow reports whether user may try to sign in from ip at now and, when so,
// counts the attempt, starting a block when it reaches a limit. When not, it
// returns how long the longest wait still lasts.
func (l *Limiter) Allow(user, ip string, now time.Time) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	name, source := limiterKey(user), sourceKey(ip)
	p, i, u := counterFor(l.pairs, pairKey{name, source}, now), counterFor(l.ips, source, now), counterFor(l.users, name, now)
	retryAfter = max(p.remaining(now), i.remaining(now), u.delay(now))
	if retryAfter > 0 {
		return false, retryAfter
	}
	p.add(now, pairAttempts)
	i.add(now, ipAttempts)
	u.record(now)
	return true, 0
}

// Succeed clears the attempts and any block of user, both from the source of ip
// and from any source. Call it only after both the password and the TOTP code
// of the attempt were accepted. The attempts of the source stay.
func (l *Limiter) Succeed(user, ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	name := limiterKey(user)
	delete(l.pairs, pairKey{name, sourceKey(ip)})
	delete(l.users, name)
}

// limiterKey is the counter key of user.
func limiterKey(user string) string {
	if len(user) > maxUserKey {
		return strings.Clone(user[:maxUserKey])
	}
	return user
}

// sourceKey is the counter key of the source of ip: the IPv4 address, also when
// mapped into IPv6, or the IPv6 /64. Anything else is used as it is.
func sourceKey(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	addr = addr.Unmap().WithZone("")
	if addr.Is4() {
		return addr.String()
	}
	prefix, _ := addr.Prefix(64) // never fails for an IPv6 address without a zone
	return prefix.String()
}

// counterFor returns the counter of key, creating it, and drops idle entries
// when the map has grown large.
func counterFor[K comparable](counters map[K]*counter, key K, now time.Time) *counter {
	if len(counters) > pruneAbove {
		for k, c := range counters {
			if c.idle(now) {
				delete(counters, k)
			}
		}
	}
	c := counters[key]
	if c == nil {
		c = &counter{}
		counters[key] = c
	}
	return c
}

// remaining is how long the block of c still lasts at now.
func (c *counter) remaining(now time.Time) time.Duration {
	return max(c.blockedUntil.Sub(now), 0)
}

// add records an attempt at now and starts a block when the attempts within
// the window reach limit.
func (c *counter) add(now time.Time, limit int) {
	c.drop(now)
	c.attempts = append(c.attempts, now)
	if len(c.attempts) >= limit {
		c.blockedUntil = now.Add(blockFor)
		c.attempts = nil
	}
}

// record notes an attempt at now, keeping only the last userAttempts.
func (c *counter) record(now time.Time) {
	c.drop(now)
	c.attempts = append(c.attempts, now)
	if len(c.attempts) > userAttempts {
		c.attempts = c.attempts[len(c.attempts)-userAttempts:]
	}
}

// delay is how long an attempt must still wait at now once userAttempts
// attempts fall within the window: userDelay after the latest.
func (c *counter) delay(now time.Time) time.Duration {
	c.drop(now)
	if len(c.attempts) < userAttempts {
		return 0
	}
	return max(c.attempts[len(c.attempts)-1].Add(userDelay).Sub(now), 0)
}

// drop forgets the attempts older than the window.
func (c *counter) drop(now time.Time) {
	keep := c.attempts[:0]
	for _, t := range c.attempts {
		if now.Sub(t) < attemptWindow {
			keep = append(keep, t)
		}
	}
	c.attempts = keep
}

// idle reports whether c holds neither a block nor an attempt within the window.
func (c *counter) idle(now time.Time) bool {
	c.drop(now)
	return len(c.attempts) == 0 && !now.Before(c.blockedUntil)
}
