package limiter

import (
	"sync"
	"sync/atomic"

	"github.com/cedar2025/xboard-node/internal/model"
	"golang.org/x/time/rate"
)

const (
	unlimitedBurst = int(^uint(0) >> 1)
	unlimitedRate  = rate.Limit(1e30)
)

// SpeedTracker maintains one shared token bucket per speed-limited user.
// Existing buckets are updated in place so active Xray sessions immediately
// receive positive-to-positive rate changes. When a user changes from
// unlimited to limited, the Xray kernel reconnects existing sessions because
// those sessions may not have been wrapped by a limiter at creation time.
type SpeedTracker struct {
	limiter *Limiter
	mu      sync.RWMutex
	buckets map[int]*rate.Limiter // userID → shared limiter, limited users only
	uuidMap map[string]int        // UUID → userID

	hasLimits atomic.Bool
	logFunc   SpeedTrackerLogCallback
}

// SpeedTrackerLogCallback is called when bucket updates occur.
type SpeedTrackerLogCallback func(msg string)

// NewSpeedTracker creates a bucket manager for per-user bandwidth throttling.
func NewSpeedTracker(l *Limiter) *SpeedTracker {
	return &SpeedTracker{
		limiter: l,
		buckets: make(map[int]*rate.Limiter),
		uuidMap: make(map[string]int),
	}
}

// SetLogCallback sets the logging callback.
func (t *SpeedTracker) SetLogCallback(f SpeedTrackerLogCallback) {
	t.logFunc = f
}

// UpdateBuckets updates UUID mappings and mutates existing limiters in place.
// A limiter being removed is first changed to a very high finite rate because existing
// Xray RateLimitWriter/Reader wrappers may still hold its pointer.
func (t *SpeedTracker) UpdateBuckets() {
	currentUsers := make([]model.UserSpec, 0, 32)
	t.limiter.mu.RLock()
	for _, u := range t.limiter.users {
		currentUsers = append(currentUsers, u)
	}
	t.limiter.mu.RUnlock()

	t.mu.Lock()
	newUUIDMap := make(map[string]int, len(currentUsers))
	activeIDs := make(map[int]struct{}, len(currentUsers))

	for _, user := range currentUsers {
		activeIDs[user.ID] = struct{}{}
		if user.UUID != "" {
			newUUIDMap[user.UUID] = user.ID
		}

		if user.SpeedLimit <= 0 {
			if lim := t.buckets[user.ID]; lim != nil {
				lim.SetLimit(unlimitedRate)
				lim.SetBurst(unlimitedBurst)
				delete(t.buckets, user.ID)
			}
			continue
		}

		bytesPerSec := int(user.SpeedLimit) * 1_000_000 / 8
		burst := bytesPerSec
		if burst < 64*1024 {
			burst = 64 * 1024
		}
		lim := t.buckets[user.ID]
		if lim == nil {
			// Start with a full burst, matching rate.Limiter's normal initial
			// token balance instead of making the first request wait to refill.
			t.buckets[user.ID] = rate.NewLimiter(rate.Limit(bytesPerSec), burst)
			continue
		}
		lim.SetLimit(rate.Limit(bytesPerSec))
		lim.SetBurst(burst)
	}

	for id, lim := range t.buckets {
		if _, ok := activeIDs[id]; !ok {
			lim.SetLimit(unlimitedRate)
			lim.SetBurst(unlimitedBurst)
			delete(t.buckets, id)
		}
	}

	t.uuidMap = newUUIDMap
	t.hasLimits.Store(len(t.buckets) > 0)
	t.mu.Unlock()

	if t.logFunc != nil {
		t.logFunc("buckets updated")
	}
}

// GetLimiter returns the shared limiter for a speed-limited user, or nil when
// no limit applies. UUID mappings and buckets are refreshed by UpdateBuckets.
func (t *SpeedTracker) GetLimiter(user string) *rate.Limiter {
	t.mu.RLock()
	defer t.mu.RUnlock()

	uid, exists := t.uuidMap[user]
	if !exists {
		return nil
	}
	return t.buckets[uid]
}

// HasLimits reports whether any user currently has a positive speed limit.
func (t *SpeedTracker) HasLimits() bool {
	return t.hasLimits.Load()
}

// LimitedUserCount returns the number of users with active speed limits.
func (t *SpeedTracker) LimitedUserCount() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.buckets)
}
