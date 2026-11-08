package xray

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cedar2025/xboard-node/internal/model"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/transport"
	"golang.org/x/time/rate"
)

func newTestDispatcher() *LimitDispatcher {
	return &LimitDispatcher{
		activeIPs: make(map[string]map[string]int),
	}
}

type nopReader struct{}

func (nopReader) ReadMultiBuffer() (buf.MultiBuffer, error) { return nil, nil }

type sequenceBufferReader struct {
	reads int
}

func (r *sequenceBufferReader) ReadMultiBuffer() (buf.MultiBuffer, error) {
	if r.reads >= 2 {
		return nil, io.EOF
	}
	b := buf.NewWithSize(1)
	_, _ = b.Write([]byte{byte('a' + r.reads)})
	r.reads++
	return buf.MultiBuffer{b}, nil
}

func TestRateLimitReaderThrottlesInboundPayload(t *testing.T) {
	reader := &rateLimitReader{
		Reader:  &sequenceBufferReader{},
		Limiter: rate.NewLimiter(rate.Limit(5), 1),
		Context: context.Background(),
	}

	first, err := reader.ReadMultiBuffer()
	if err != nil {
		t.Fatalf("first ReadMultiBuffer() error = %v", err)
	}
	buf.ReleaseMulti(first)

	start := time.Now()
	second, err := reader.ReadMultiBuffer()
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("second ReadMultiBuffer() error = %v", err)
	}
	buf.ReleaseMulti(second)
	if elapsed < 150*time.Millisecond {
		t.Fatalf("second read waited only %v; expected token-bucket throttling", elapsed)
	}
}

func TestLimitDispatcher_DeviceLimitCheck(t *testing.T) {
	ld := newTestDispatcher()

	users := []model.UserSpec{
		{ID: 1, UUID: "uuid-1", DeviceLimit: 2, SpeedLimit: 0},
		{ID: 2, UUID: "uuid-2", DeviceLimit: 0, SpeedLimit: 10},
	}

	emailToUID := make(map[string]int)
	deviceLimits := make(map[string]int)
	speedLimits := make(map[string]int)
	for _, u := range users {
		email := userEmail(u.ID)
		emailToUID[email] = u.ID
		if u.DeviceLimit > 0 {
			deviceLimits[email] = u.DeviceLimit
		}
		if u.SpeedLimit > 0 {
			speedLimits[email] = u.SpeedLimit
		}
	}
	ld.UpdateLimits(emailToUID, deviceLimits, speedLimits)

	email1 := userEmail(1)

	// First IP should be allowed
	if ld.checkDeviceLimit(email1, "1.1.1.1") {
		t.Error("first IP should be allowed")
	}

	// Second IP should be allowed (limit=2)
	if ld.checkDeviceLimit(email1, "2.2.2.2") {
		t.Error("second IP should be allowed")
	}

	// Third unique IP should be rejected
	if !ld.checkDeviceLimit(email1, "3.3.3.3") {
		t.Error("third IP should be rejected (limit=2)")
	}

	// Same IP as first should be allowed (already connected)
	if ld.checkDeviceLimit(email1, "1.1.1.1") {
		t.Error("same IP should always be allowed")
	}

	// User 2 has no device limit — should always be allowed
	email2 := userEmail(2)
	for i := 0; i < 10; i++ {
		ip := "10.0.0." + string(rune('0'+i))
		if ld.checkDeviceLimit(email2, ip) {
			t.Errorf("user with no device limit should always be allowed (ip=%s)", ip)
		}
	}
}

func TestLimitDispatcher_DelConn(t *testing.T) {
	ld := newTestDispatcher()

	email := userEmail(1)
	deviceLimits := map[string]int{email: 2}
	ld.UpdateLimits(map[string]int{email: 1}, deviceLimits, nil)

	// Add 2 IPs
	ld.checkDeviceLimit(email, "1.1.1.1")
	ld.checkDeviceLimit(email, "2.2.2.2")

	// Third should be rejected
	if !ld.checkDeviceLimit(email, "3.3.3.3") {
		t.Error("third IP should be rejected")
	}

	// Remove first IP
	ld.delConn(email, "1.1.1.1")

	// Now third IP should be allowed
	if ld.checkDeviceLimit(email, "3.3.3.3") {
		t.Error("after deleting one IP, new IP should be allowed")
	}
}

func TestLimitDispatcher_GetConnectionState(t *testing.T) {
	ld := newTestDispatcher()

	email1 := userEmail(1)
	email2 := userEmail(2)
	ld.UpdateLimits(map[string]int{email1: 1, email2: 2}, nil, nil)
	ld.mu.Lock()
	ld.activeIPs[email1] = map[string]int{"1.1.1.1": 2, "2.2.2.2": 1}
	ld.activeIPs[email2] = map[string]int{"3.3.3.3": 1}
	ld.mu.Unlock()
	ld.connCount.Store(5)

	aliveIPs, connCount := ld.GetConnectionState()
	if connCount != 5 {
		t.Errorf("expected connCount=5, got %d", connCount)
	}
	if len(aliveIPs[1]) != 2 {
		t.Errorf("user 1 IPs: got %d, want 2", len(aliveIPs[1]))
	}
	if len(aliveIPs[2]) != 1 {
		t.Errorf("user 2 IPs: got %d, want 1", len(aliveIPs[2]))
	}
}

func TestLimitDispatcher_ResetConns(t *testing.T) {
	ld := newTestDispatcher()

	ld.mu.Lock()
	ld.activeIPs["user@1"] = map[string]int{"1.1.1.1": 1}
	ld.mu.Unlock()
	ld.connCount.Store(3)

	ld.ResetConns()

	ld.mu.RLock()
	ipCount := len(ld.activeIPs)
	ld.mu.RUnlock()
	if ipCount != 0 {
		t.Error("activeIPs should be empty after reset")
	}

	if ld.connCount.Load() != 0 {
		t.Error("connCount should be 0 after reset")
	}
}

func TestLimitDispatcher_UnlimitedUserIPTracking(t *testing.T) {
	ld := newTestDispatcher()
	email := userEmail(1)
	ld.UpdateLimits(map[string]int{email: 1}, nil, nil)

	for i := 0; i < 10; i++ {
		ip := "10.0.0." + string(rune('0'+i))
		if ld.checkDeviceLimit(email, ip) {
			t.Errorf("unlimited user should always be allowed (ip=%s)", ip)
		}
	}

	ld.mu.RLock()
	defer ld.mu.RUnlock()
	if got := len(ld.activeIPs[email]); got != 10 {
		t.Fatalf("tracked active IPs = %d, want 10", got)
	}
}

func TestLimitDispatcher_DeviceLimitConcurrentAdmissions(t *testing.T) {
	ld := newTestDispatcher()
	email := userEmail(1)
	ld.UpdateLimits(map[string]int{email: 1}, map[string]int{email: 2}, nil)

	var wg sync.WaitGroup
	var accepted atomic.Int64
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ip := "192.0.2." + string(rune('A'+i))
			if !ld.checkDeviceLimit(email, ip) {
				accepted.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if got := accepted.Load(); got != 2 {
		t.Fatalf("concurrent accepted IPs = %d, want exactly 2", got)
	}
	ld.mu.RLock()
	defer ld.mu.RUnlock()
	if got := len(ld.activeIPs[email]); got != 2 {
		t.Fatalf("active IP count = %d, want 2", got)
	}
}

type interruptTestWriter struct {
	buf.Writer
	interrupted atomic.Bool
}

func (w *interruptTestWriter) Interrupt() {
	w.interrupted.Store(true)
}

func TestLimitDispatcher_CloseUserConnectionsOnlyTargetsOneUser(t *testing.T) {
	ld := newTestDispatcher()
	user1 := userEmail(1)
	user2 := userEmail(2)

	writer1 := &interruptTestWriter{Writer: buf.Discard}
	writer2 := &interruptTestWriter{Writer: buf.Discard}
	link1 := &transport.Link{Reader: nopReader{}, Writer: writer1}
	link2 := &transport.Link{Reader: nopReader{}, Writer: writer2}

	ld.trackLink(link1, user1, "1.1.1.1")
	ld.trackLink(link2, user2, "2.2.2.2")
	ld.CloseUserConnections(user1)

	if !writer1.interrupted.Load() {
		t.Fatal("target user's writer was not interrupted")
	}
	if writer2.interrupted.Load() {
		t.Fatal("another user's writer was interrupted")
	}
	if got := ld.connCount.Load(); got != 1 {
		t.Fatalf("connCount = %d, want 1 after closing one user's session", got)
	}

	ld.CloseUserConnections(user1)
	if writer2.interrupted.Load() {
		t.Fatal("repeated close affected another user's writer")
	}
}

func TestLimitDispatcher_TrackLinkPreservesReader(t *testing.T) {
	ld := newTestDispatcher()
	email := userEmail(1)
	ld.UpdateLimits(map[string]int{email: 1}, map[string]int{email: 1}, nil)

	origReader := nopReader{}
	origWriter := &closeTrackingWriter{Writer: buf.Discard, onClose: func() {}}
	link := &transport.Link{Reader: origReader, Writer: origWriter}

	ld.trackLink(link, email, "1.1.1.1")

	if link.Reader != origReader {
		t.Fatal("trackLink must not replace link.Reader")
	}
	if link.Writer == origWriter {
		t.Fatal("trackLink should wrap link.Writer for lifecycle callbacks")
	}
}

func TestLimitDispatcher_CloseTrackingWriterReleasesConn(t *testing.T) {
	ld := newTestDispatcher()
	email := userEmail(1)
	ld.UpdateLimits(map[string]int{email: 1}, map[string]int{email: 1}, nil)
	if ld.checkDeviceLimit(email, "1.1.1.1") {
		t.Fatal("first connection should be allowed")
	}

	link := &transport.Link{Reader: nopReader{}, Writer: buf.Discard}
	ld.trackLink(link, email, "1.1.1.1")

	if got := ld.connCount.Load(); got != 1 {
		t.Fatalf("expected connCount=1 after tracking, got %d", got)
	}
	cw, ok := link.Writer.(*closeTrackingWriter)
	if !ok {
		t.Fatal("expected closeTrackingWriter wrapper")
	}
	if err := cw.Close(); err != nil {
		t.Fatalf("closeTrackingWriter.Close() error = %v", err)
	}
	if got := ld.connCount.Load(); got != 0 {
		t.Fatalf("expected connCount=0 after close, got %d", got)
	}
	if ld.checkDeviceLimit(email, "2.2.2.2") {
		t.Fatal("device slot should be released after writer close")
	}
}
