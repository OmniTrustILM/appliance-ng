package auth

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSignInLimiter(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)

	t.Run("user and source IP", func(t *testing.T) {
		l := SignInLimiter()
		for i := range 5 {
			if ok, _ := l.Allow("admin", "10.0.0.1", now.Add(time.Duration(i)*time.Minute)); !ok {
				t.Fatalf("attempt %d refused", i+1)
			}
		}
		at := now.Add(5 * time.Minute)
		ok, retry := l.Allow("admin", "10.0.0.1", at)
		if ok || retry != 14*time.Minute {
			t.Fatalf("after 5 attempts Allow = %v, %s; want false, 14m (15 min from the fifth attempt)", ok, retry)
		}
		if ok, _ := l.Allow("other", "10.0.0.1", at); !ok {
			t.Fatal("another user is blocked")
		}
		if ok, _ := l.Allow("admin", "10.0.0.1", now.Add(19*time.Minute)); !ok {
			t.Fatal("still blocked after 15 min")
		}
	})

	t.Run("a block from one address does not lock the user out from another", func(t *testing.T) {
		l := SignInLimiter()
		for range 5 {
			l.Allow("admin", "10.0.0.1", now)
		}
		if ok, _ := l.Allow("admin", "10.0.0.1", now.Add(time.Minute)); ok {
			t.Fatal("the blocked address may try again")
		}
		if ok, retry := l.Allow("admin", "10.0.0.2", now.Add(time.Minute)); !ok {
			t.Fatalf("another address is refused for %s", retry)
		}
	})

	t.Run("failures from any source space the next attempts", func(t *testing.T) {
		l := SignInLimiter()
		for i := range 5 {
			l.Allow("admin", fmt.Sprint("10.0.1.", i), now)
		}
		ok, retry := l.Allow("admin", "10.0.1.0", now.Add(time.Second))
		if ok || retry != userDelay-time.Second {
			t.Fatalf("Allow = %v, %s; want false, %s", ok, retry, userDelay-time.Second)
		}
		if ok, _ := l.Allow("admin", "10.0.1.0", now.Add(userDelay)); !ok {
			t.Fatal("refused after the delay")
		}
	})

	t.Run("attempts outside the window do not count", func(t *testing.T) {
		l := SignInLimiter()
		for i := range 4 {
			l.Allow("admin", "10.0.0.1", now.Add(time.Duration(i)*time.Minute))
		}
		for i, at := range []time.Duration{16 * time.Minute, 17 * time.Minute} {
			if ok, _ := l.Allow("admin", "10.0.0.1", now.Add(at)); !ok {
				t.Fatalf("attempt %d at %s refused although older attempts left the window", i+1, at)
			}
		}
	})

	t.Run("success resets the user", func(t *testing.T) {
		l := SignInLimiter()
		for range 4 {
			l.Allow("admin", "10.0.0.1", now)
		}
		l.Allow("admin", "10.0.0.2", now)
		l.Succeed("admin", "10.0.0.1")
		for i := range 4 {
			if ok, _ := l.Allow("admin", "10.0.0.1", now); !ok {
				t.Fatalf("attempt %d after a success refused", i+1)
			}
		}
	})

	t.Run("source IP", func(t *testing.T) {
		l := SignInLimiter()
		for i := range 20 {
			if ok, _ := l.Allow(fmt.Sprint("user", i), "10.0.0.1", now); !ok {
				t.Fatalf("attempt %d refused", i+1)
			}
		}
		ok, retry := l.Allow("fresh", "10.0.0.1", now.Add(time.Minute))
		if ok || retry != 14*time.Minute {
			t.Fatalf("after 20 attempts from one IP Allow = %v, %s; want false, 14m", ok, retry)
		}
		if ok, _ := l.Allow("fresh", "10.0.0.9", now); !ok {
			t.Fatal("another IP is blocked")
		}
		if ok, _ := l.Allow("fresh", "10.0.0.1", now.Add(15*time.Minute)); !ok {
			t.Fatal("IP still blocked after 15 min")
		}
	})

	t.Run("the longer block wins", func(t *testing.T) {
		l := SignInLimiter()
		for range 5 {
			l.Allow("admin", "10.0.0.1", now)
		}
		for i := range 15 {
			l.Allow(fmt.Sprint("user", i), "10.0.0.1", now.Add(5*time.Minute))
		}
		_, retry := l.Allow("admin", "10.0.0.1", now.Add(6*time.Minute))
		if retry != 14*time.Minute {
			t.Fatalf("retry %s, want 14m (the address's block outlasts the user's)", retry)
		}
	})
}

// allowConcurrently calls Allow n times at once and returns how many passed.
func allowConcurrently(l *Limiter, n int, user func(int) string, ip string, now time.Time) int {
	var passed atomic.Int32
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			if ok, _ := l.Allow(user(i), ip, now); ok {
				passed.Add(1)
			}
		})
	}
	wg.Wait()
	return int(passed.Load())
}

func TestSignInLimiterConcurrentAttempts(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	sameUser := func(int) string { return "admin" }
	if got := allowConcurrently(SignInLimiter(), 10, sameUser, "10.0.0.1", now); got != 5 {
		t.Fatalf("%d of 10 concurrent attempts for one user passed, want 5", got)
	}
	otherUsers := func(i int) string { return fmt.Sprint("user", i) }
	if got := allowConcurrently(SignInLimiter(), 30, otherUsers, "10.0.0.1", now); got != 20 {
		t.Fatalf("%d of 30 concurrent attempts from one IP passed, want 20", got)
	}
}

func TestSignInLimiterForgetsIdleEntries(t *testing.T) {
	l := SignInLimiter()
	now := time.Unix(1_800_000_000, 0)
	for i := range 2 * pruneAbove {
		l.Allow(fmt.Sprint("user", i), fmt.Sprint("ip", i), now)
	}
	l.Allow("late", "10.0.0.2", now.Add(time.Hour))
	l.mu.Lock()
	users, pairs, ips := len(l.users), len(l.pairs), len(l.ips)
	l.mu.Unlock()
	if users > pruneAbove || pairs > pruneAbove || ips > pruneAbove {
		t.Fatalf("kept %d users, %d user and IP pairs and %d IPs; idle entries are not pruned", users, pairs, ips)
	}
}

func TestSignInLimiterKeepsOnlyTheStartOfLongUserNames(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	l := SignInLimiter()
	long := strings.Repeat("a", maxUserKey)
	for i := range 5 {
		l.Allow(fmt.Sprintf("%s-%d-%s", long, i, strings.Repeat("x", 16<<10)), fmt.Sprintf("10.0.0.%d", i), now)
	}
	if len(l.users) != 1 {
		t.Fatalf("%d user counters, want 1 for names that share their first %d bytes", len(l.users), maxUserKey)
	}
	for key := range l.users {
		if len(key) != maxUserKey {
			t.Errorf("user key of %d bytes, want %d", len(key), maxUserKey)
		}
	}
	if ok, _ := l.Allow("admin", "10.0.0.9", now); !ok {
		t.Error("a short user name is blocked by the long ones")
	}
}

func TestLimiterIPv6Slash64(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	l := SignInLimiter()
	for i := range 5 {
		if ok, _ := l.Allow("admin", fmt.Sprintf("2001:db8:1:2::%x", i+1), now); !ok {
			t.Fatalf("attempt %d refused", i+1)
		}
	}
	if ok, _ := l.Allow("admin", "2001:db8:1:2:ffff:ffff:ffff:ffff", now.Add(time.Minute)); ok {
		t.Error("another address of the same /64 escapes the block of the user and source")
	}
	if ok, retry := l.Allow("admin", "2001:db8:1:3::1", now.Add(time.Minute)); !ok {
		t.Errorf("an address of another /64 is refused for %s", retry)
	}
	l.Succeed("admin", "2001:db8:1:2::abcd")
	if ok, _ := l.Allow("admin", "2001:db8:1:2::1", now.Add(time.Minute)); !ok {
		t.Error("a success from the same /64 does not clear the block")
	}

	l = SignInLimiter()
	for i := range ipAttempts {
		l.Allow(fmt.Sprint("user", i), fmt.Sprintf("2001:db8:5:6:%x::1", i), now)
	}
	if ok, _ := l.Allow("fresh", "2001:db8:5:6::9", now); ok {
		t.Error("rotating within a /64 escapes the per-source limit")
	}
	if ok, _ := l.Allow("fresh", "::ffff:10.0.0.1", now); !ok {
		t.Error("an IPv4-mapped address is counted with an IPv6 /64")
	}
}

func TestPerUserDelaySurvivesRotation(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	l := SignInLimiter()
	for i := range userAttempts {
		if ok, _ := l.Allow("admin", fmt.Sprint("10.0.2.", i), now); !ok {
			t.Fatalf("attempt %d refused", i+1)
		}
	}
	ok, retry := l.Allow("admin", "10.0.2.99", now.Add(time.Second))
	if ok || retry != userDelay-time.Second {
		t.Fatalf("a new address after %d failures: Allow = %v, %s; want false, %s", userAttempts, ok, retry, userDelay-time.Second)
	}
	if ok, _ := l.Allow("admin", "10.0.2.100", now.Add(userDelay)); !ok {
		t.Fatal("refused after the delay")
	}
	if ok, _ := l.Allow("admin", "10.0.2.101", now.Add(userDelay+time.Second)); ok {
		t.Error("a failure from a new address resets the delay")
	}
	if ok, _ := l.Allow("other", "10.0.2.102", now.Add(userDelay+time.Second)); !ok {
		t.Error("the delay of one user holds back another")
	}
}
