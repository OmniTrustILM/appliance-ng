package auth

import (
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

// Limiter limits sign-in attempts per user and source IP, per source IP and,
// with a short delay only, per user. Blocks never span addresses, so that no
// peer can lock a user out of the other addresses. Allow counts an attempt as
// a failure the moment it lets it through, so concurrent requests cannot pass
// before earlier ones have failed; Succeed takes the attempts back. It is safe
// for concurrent use.
type Limiter struct {
	mu    sync.Mutex
	pairs map[pairKey]*counter
	ips   map[string]*counter
	users map[string]*counter
}

// pairKey identifies the attempts of one user from one source IP.
type pairKey struct {
	user, ip string
}

// counter holds the recent attempts of one key and its block.
type counter struct {
	attempts     []time.Time
	blockedUntil time.Time
}

// SignInLimiter returns a limiter that blocks a user at a source IP for 15
// minutes after 5 unsuccessful attempts from that IP within 15 minutes, and a
// source IP for 15 minutes after 20 attempts within 15 minutes. Once a user
// has 5 unsuccessful attempts within 15 minutes from any source, a source that
// has failed for the user must wait 2 seconds after the user's last attempt.
func SignInLimiter() *Limiter {
	return &Limiter{pairs: map[pairKey]*counter{}, ips: map[string]*counter{}, users: map[string]*counter{}}
}

// Allow reports whether user may try to sign in from ip at now and, when so,
// counts the attempt, starting a block when it reaches a limit. When not, it
// returns how long the longest wait still lasts.
func (l *Limiter) Allow(user, ip string, now time.Time) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	name := limiterKey(user)
	p, i, u := counterFor(l.pairs, pairKey{name, ip}, now), counterFor(l.ips, ip, now), counterFor(l.users, name, now)
	retryAfter = max(p.remaining(now), i.remaining(now))
	if p.failing(now) {
		retryAfter = max(retryAfter, u.delay(now))
	}
	if retryAfter > 0 {
		return false, retryAfter
	}
	p.add(now, pairAttempts)
	i.add(now, ipAttempts)
	u.record(now)
	return true, 0
}

// Succeed clears the attempts and any block of user, both from ip and from any
// source. Call it only after both the password and the TOTP code of the
// attempt were accepted. The attempts of the source IP stay.
func (l *Limiter) Succeed(user, ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	name := limiterKey(user)
	delete(l.pairs, pairKey{name, ip})
	delete(l.users, name)
}

// limiterKey is the counter key of user.
func limiterKey(user string) string {
	if len(user) > maxUserKey {
		return strings.Clone(user[:maxUserKey])
	}
	return user
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

// failing reports whether c holds an attempt within the window at now.
func (c *counter) failing(now time.Time) bool {
	c.drop(now)
	return len(c.attempts) > 0
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
