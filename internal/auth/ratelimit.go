package auth

import (
	"strings"
	"sync"
	"time"
)

const (
	userAttempts  = 5
	ipAttempts    = 20
	attemptWindow = 15 * time.Minute
	blockFor      = 15 * time.Minute

	// pruneAbove is the map size above which Allow drops idle entries.
	pruneAbove = 1024
	// maxUserKey is the longest user name an account can have; longer names, which match no account, are counted
	// under their first maxUserKey bytes, so that a sign-in cannot store a name of any length.
	maxUserKey = 64
)

// Limiter limits sign-in attempts per user and per source IP. Allow counts an
// attempt as a failure the moment it lets it through, so concurrent requests
// cannot pass before earlier ones have failed; Succeed takes the user's
// attempts back. It is safe for concurrent use.
type Limiter struct {
	mu    sync.Mutex
	users map[string]*counter
	ips   map[string]*counter
}

// counter holds the recent attempts of one user or IP and its block.
type counter struct {
	attempts     []time.Time
	blockedUntil time.Time
}

// SignInLimiter returns a limiter that blocks a user for 15 minutes after 5
// unsuccessful attempts within 15 minutes, and a source IP for 15 minutes
// after 20 attempts within 15 minutes.
func SignInLimiter() *Limiter {
	return &Limiter{users: map[string]*counter{}, ips: map[string]*counter{}}
}

// Allow reports whether user may try to sign in from ip at now and, when so,
// counts the attempt for both, starting a block when it reaches a limit. When
// not, it returns how long the longer of the two blocks still lasts.
func (l *Limiter) Allow(user, ip string, now time.Time) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	u, i := counterFor(l.users, limiterKey(user), now), counterFor(l.ips, ip, now)
	retryAfter = max(u.remaining(now), i.remaining(now))
	if retryAfter > 0 {
		return false, retryAfter
	}
	u.add(now, userAttempts)
	i.add(now, ipAttempts)
	return true, 0
}

// Succeed clears the attempts and any block of user. Call it only after both
// the password and the TOTP code of the attempt were accepted. The attempts of
// the source IP stay.
func (l *Limiter) Succeed(user string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.users, limiterKey(user))
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
func counterFor(counters map[string]*counter, key string, now time.Time) *counter {
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
