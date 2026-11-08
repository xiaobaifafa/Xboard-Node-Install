package xray

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	_ "unsafe"

	xrayDispatcher "github.com/xtls/xray-core/app/dispatcher"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/session"
	xrayCore "github.com/xtls/xray-core/core"
	featurebandwidth "github.com/xtls/xray-core/features/bandwidth"
	"github.com/xtls/xray-core/features/routing"
	"github.com/xtls/xray-core/transport"
	"golang.org/x/time/rate"

	"github.com/cedar2025/xboard-node/internal/nlog"
)

// Access xray's internal config creator registry so we can replace the
// default dispatcher factory with ours. This runs AFTER xray's init()
// functions because our package imports xray (dependency order guarantee).
//
//go:linkname typeCreatorRegistry github.com/xtls/xray-core/common.typeCreatorRegistry
var typeCreatorRegistry map[reflect.Type]common.ConfigCreator

var origDispatcherFactory common.ConfigCreator

// globalLimitDispatcher is set when the factory creates a LimitDispatcher.
// The Xray kernel reads it to configure limits and get connections.
var globalLimitDispatcher atomic.Pointer[LimitDispatcher]

func init() {
	configType := reflect.TypeOf((*xrayDispatcher.Config)(nil))
	origDispatcherFactory = typeCreatorRegistry[configType]
	typeCreatorRegistry[configType] = limitDispatcherFactory
}

func limitDispatcherFactory(ctx context.Context, config interface{}) (interface{}, error) {
	orig, err := origDispatcherFactory(ctx, config)
	if err != nil {
		return nil, err
	}
	inner, ok := orig.(routing.Dispatcher)
	if !ok {
		return orig, nil
	}
	ld := &LimitDispatcher{
		inner:      orig,
		innerDisp:  inner,
		activeIPs: make(map[string]map[string]int),
	}
	globalLimitDispatcher.Store(ld)
	nlog.Core().Debug("xray: limit dispatcher installed")
	return ld, nil
}

// LimitDispatcher wraps xray's DefaultDispatcher to enforce per-user
// admission checks and track each dispatched session by its authenticated user.
//
// trackLink preserves the original Reader (required by mux/XUDP) and wraps only
// the Writer with a buf.Writer-compatible lifecycle hook. Interrupting that
// writer interrupts its paired Xray pipe without touching other user sessions.
type LimitDispatcher struct {
	inner     interface{}        // original DefaultDispatcher (Feature + Dispatcher)
	innerDisp routing.Dispatcher // same object, typed as Dispatcher

	// activeIPs tracks all users' active client source IPs.
	// email → sourceIP → active dispatched-link refcount.
	// A single map keeps state consistent when device limits change at runtime.
	mu           sync.RWMutex
	activeIPs    map[string]map[string]int
	deviceLimits map[string]int // email → max distinct active client IPs
	speedLimits  map[string]int // email → configured Mbps
	emailToUID   map[string]int // email → panel user ID

	// activeWriters tracks the writable pipe endpoint for each user's sessions.
	// Interrupting these writers closes only that user's Xray pipe sessions.
	connMu       sync.Mutex
	activeWriters map[string]map[*closeTrackingWriter]struct{}

	connCount atomic.Int64 // total active connections tracked by dispatcher
}

// ─── routing.Dispatcher ──────────────────────────────────────────────────────

func (d *LimitDispatcher) Dispatch(ctx context.Context, dest net.Destination) (*transport.Link, error) {
	email, sourceIP, err := d.identifyAndCheck(ctx)
	if err != nil {
		return nil, err
	}

	link, err := d.innerDisp.Dispatch(ctx, dest)
	if err != nil {
		if email != "" {
			d.delConn(email, sourceIP)
		}
		return nil, err
	}

	if email != "" {
		d.trackLink(link, email, sourceIP)
	}
	return link, nil
}

func (d *LimitDispatcher) DispatchLink(ctx context.Context, dest net.Destination, link *transport.Link) error {
	email, sourceIP, err := d.identifyAndCheck(ctx)
	if err != nil {
		return err
	}

	// DispatchLink is the VLESS-style path: Xray's WrapLink applies the
	// download limiter to link.Writer, while this reader wrapper limits client
	// upload. Both use the same per-user token bucket.
	if email != "" {
		reader, err := d.applyUploadSpeedLimit(ctx, email, link.Reader)
		if err != nil {
			d.delConn(email, sourceIP)
			return err
		}
		link.Reader = reader

		// Wrap before delegation because the inner dispatcher may start
		// consuming the link immediately.
		d.trackLink(link, email, sourceIP)
	}
	if err := d.innerDisp.DispatchLink(ctx, dest, link); err != nil {
		if email != "" {
			_ = common.Close(link.Writer)
		}
		return err
	}
	return nil
}

// identifyAndCheck extracts the authenticated user and client source IP.
// Device accounting is based on the client IP, not the destination network:
// UDP proxy traffic still belongs to the same authenticated client device.
func (d *LimitDispatcher) identifyAndCheck(ctx context.Context) (email, sourceIP string, err error) {
	si := session.InboundFromContext(ctx)
	if si == nil || si.User == nil || len(si.User.Email) == 0 {
		return "", "", nil
	}
	email = si.User.Email
	sourceIP = si.Source.Address.IP().String()

	if err := d.ensureSpeedLimiter(ctx, email); err != nil {
		return "", "", err
	}

	if d.checkDeviceLimit(email, sourceIP) {
		nlog.Core().Debug("xray: device limit exceeded", "email", email, "ip", sourceIP)
		return "", "", errors.New("device limit exceeded for " + email)
	}
	return email, sourceIP, nil
}

// ensureSpeedLimiter fails closed if a configured speed cap has not yet
// been installed in Xray's bandwidth manager. This prevents a hot-update
// window from admitting an unthrottled connection.
func (d *LimitDispatcher) ensureSpeedLimiter(ctx context.Context, email string) error {
	d.mu.RLock()
	speedLimited := d.speedLimits[email] > 0
	d.mu.RUnlock()
	if !speedLimited {
		return nil
	}
	feat := xrayCore.MustFromContext(ctx).GetFeature(featurebandwidth.ManagerType())
	if feat == nil {
		return errors.New("xray bandwidth manager unavailable for speed-limited user " + email)
	}
	manager, ok := feat.(featurebandwidth.Manager)
	if !ok || manager.GetUserLimiter(email) == nil {
		return errors.New("xray user rate limiter unavailable for " + email)
	}
	return nil
}

// applyUploadSpeedLimit wraps the inbound reader on DispatchLink paths.
// The default Xray dispatcher only wraps link.Writer here, so without this
// reader wrapper client upload would remain unlimited.
func (d *LimitDispatcher) applyUploadSpeedLimit(ctx context.Context, email string, reader buf.Reader) (buf.Reader, error) {
	d.mu.RLock()
	speedLimited := d.speedLimits[email] > 0
	d.mu.RUnlock()
	if !speedLimited {
		return reader, nil
	}

	feat := xrayCore.MustFromContext(ctx).GetFeature(featurebandwidth.ManagerType())
	if feat == nil {
		return nil, errors.New("xray bandwidth manager unavailable for speed-limited user " + email)
	}
	manager, ok := feat.(featurebandwidth.Manager)
	if !ok {
		return nil, errors.New("xray bandwidth manager has unexpected type")
	}
	lim := manager.GetUserLimiter(email)
	if lim == nil {
		return nil, errors.New("xray user rate limiter unavailable for " + email)
	}
	return &rateLimitReader{Reader: reader, Limiter: lim, Context: ctx}, nil
}

type rateLimitReader struct {
	buf.Reader
	Limiter *rate.Limiter
	Context context.Context
}

func (r *rateLimitReader) ReadMultiBuffer() (buf.MultiBuffer, error) {
	mb, err := r.Reader.ReadMultiBuffer()
	if mb.IsEmpty() || r.Limiter == nil {
		return mb, err
	}
	ctx := r.Context
	if ctx == nil {
		ctx = context.Background()
	}
	if waitErr := r.Limiter.WaitN(ctx, int(mb.Len())); waitErr != nil {
		buf.ReleaseMulti(mb)
		return nil, waitErr
	}
	return mb, err
}

// trackLink records connection lifecycle without mutating xray-core owned
// transport primitives. This keeps mux/XUDP compatible while still allowing
// the dispatcher to release device-limit state when the link closes.
func (d *LimitDispatcher) trackLink(link *transport.Link, email, sourceIP string) {
	d.connCount.Add(1)

	writer := &closeTrackingWriter{Writer: link.Writer}
	writer.onClose = func() {
		d.connMu.Lock()
		if writers := d.activeWriters[email]; writers != nil {
			delete(writers, writer)
			if len(writers) == 0 {
				delete(d.activeWriters, email)
			}
		}
		d.connMu.Unlock()

		d.delConn(email, sourceIP)
		d.connCount.Add(-1)
	}

	d.connMu.Lock()
	if d.activeWriters == nil {
		d.activeWriters = make(map[string]map[*closeTrackingWriter]struct{})
	}
	if d.activeWriters[email] == nil {
		d.activeWriters[email] = make(map[*closeTrackingWriter]struct{})
	}
	d.activeWriters[email][writer] = struct{}{}
	d.connMu.Unlock()

	link.Writer = writer
}

// CloseUserConnections interrupts every tracked Xray pipe writer for one user.
// The snapshot is taken under lock; Interrupt is called after releasing it
// because the close callback removes the writer from activeWriters.
func (d *LimitDispatcher) CloseUserConnections(email string) int {
	d.connMu.Lock()
	writers := make([]*closeTrackingWriter, 0, len(d.activeWriters[email]))
	for writer := range d.activeWriters[email] {
		writers = append(writers, writer)
	}
	d.connMu.Unlock()

	nlog.Core().Info("xray: force-close requested",
		"email", email,
		"matched_connections", len(writers),
	)

	interrupted := 0
	for _, writer := range writers {
		writer.Interrupt()
		interrupted++
	}

	nlog.Core().Info("xray: force-close interrupt dispatched",
		"email", email,
		"interrupted_connections", interrupted,
	)
	return interrupted
}

// ─── features.Feature (delegated) ───────────────────────────────────────────

func (d *LimitDispatcher) Type() interface{} { return routing.DispatcherType() }

func (d *LimitDispatcher) Start() error {
	if s, ok := d.inner.(interface{ Start() error }); ok {
		return s.Start()
	}
	return nil
}

func (d *LimitDispatcher) Close() error {
	if c, ok := d.inner.(interface{ Close() error }); ok {
		return c.Close()
	}
	return nil
}

// ─── Limit management (called by Xray kernel) ──────────────────────────────

func (d *LimitDispatcher) UpdateLimits(emailToUID map[string]int, deviceLimits, speedLimits map[string]int) {
	d.mu.Lock()
	d.emailToUID = emailToUID
	d.deviceLimits = deviceLimits
	d.speedLimits = speedLimits
	d.mu.Unlock()

}

func (d *LimitDispatcher) ResetConns() {
	d.mu.Lock()
	d.activeIPs = make(map[string]map[string]int)
	d.mu.Unlock()

	d.connMu.Lock()
	d.activeWriters = make(map[string]map[*closeTrackingWriter]struct{})
	d.connMu.Unlock()

	d.connCount.Store(0)
}

// GetConnectionState returns dispatcher-tracked alive client IPs and connection count.
// Traffic bytes are intentionally left to xray's built-in stats pipeline.
func (d *LimitDispatcher) GetConnectionState() (aliveIPs map[int]map[string]bool, connCount int) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	aliveIPs = make(map[int]map[string]bool)
	for email, refsByIP := range d.activeIPs {
		uid := d.emailToUID[email]
		if uid == 0 {
			continue
		}
		for ip, refs := range refsByIP {
			if refs <= 0 {
				continue
			}
			if aliveIPs[uid] == nil {
				aliveIPs[uid] = make(map[string]bool)
			}
			aliveIPs[uid][ip] = true
		}
	}
	connCount = int(d.connCount.Load())
	return
}

// ─── Internal helpers ───────────────────────────────────────────────────────

// checkDeviceLimit atomically tracks active client IPs and rejects a new IP
// once the configured per-user limit is reached. It applies to every target
// network; the destination may be UDP even though the authenticated client
// connection itself is still a device.
func (d *LimitDispatcher) checkDeviceLimit(email, sourceIP string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	ips := d.activeIPs[email]
	if ips == nil {
		ips = make(map[string]int)
		d.activeIPs[email] = ips
	}

	// Existing IPs are allowed and only increment their active-link refcount.
	if ips[sourceIP] > 0 {
		ips[sourceIP]++
		return false
	}

	limit := d.deviceLimits[email]
	if limit > 0 && len(ips) >= limit {
		return true
	}

	ips[sourceIP]++
	return false
}

// delConn decrements the active-link refcount for a client IP.
func (d *LimitDispatcher) delConn(email, sourceIP string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if ips, ok := d.activeIPs[email]; ok {
		ips[sourceIP]--
		if ips[sourceIP] <= 0 {
			delete(ips, sourceIP)
		}
		if len(ips) == 0 {
			delete(d.activeIPs, email)
		}
	}
}

type closeTrackingWriter struct {
	buf.Writer
	onClose func()
	closed  atomic.Bool
}

func (w *closeTrackingWriter) Close() error {
	if w.closed.CompareAndSwap(false, true) {
		w.onClose()
	}
	return common.Close(w.Writer)
}

func (w *closeTrackingWriter) Interrupt() {
	if w.closed.CompareAndSwap(false, true) {
		w.onClose()
	}
	common.Interrupt(w.Writer)
}
