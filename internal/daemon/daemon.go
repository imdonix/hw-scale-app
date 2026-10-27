package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/tamasmagyar/sscale/internal/app"
	"github.com/tamasmagyar/sscale/internal/bluez"
	"github.com/tamasmagyar/sscale/internal/config"
	"github.com/tamasmagyar/sscale/internal/live"
	"github.com/tamasmagyar/sscale/internal/store"
)

// Presence tracks advertisements of the target scale.
type Presence struct {
	mu       sync.Mutex
	lastSeen time.Time
	rssi     int16
	name     string
	seen     chan struct{}
	unwatch  []func()
}

// WatchPresence starts tracking adverts of dev.
func WatchPresence(c *bluez.Client, dev *bluez.Device) *Presence {
	p := &Presence{seen: make(chan struct{}, 1)}
	p.unwatch = append(p.unwatch,
		c.WatchAdded(func(path dbus.ObjectPath, ifaces map[string]map[string]dbus.Variant) {
			if path != dev.Path {
				return
			}
			if props, ok := ifaces["org.bluez.Device1"]; ok {
				info := bluez.ParseDeviceProps(path, props)
				p.mark(info.RSSI, info.HasRSSI, info.Name)
			}
		}),
		c.WatchProps(dev.Path, func(iface string, changed map[string]dbus.Variant) {
			if iface != "org.bluez.Device1" {
				return
			}
			for _, k := range []string{"RSSI", "ManufacturerData", "ServiceData", "TxPower"} {
				if _, ok := changed[k]; ok {
					info := bluez.ParseDeviceProps(dev.Path, changed)
					p.mark(info.RSSI, info.HasRSSI, info.Name)
					return
				}
			}
		}),
	)
	return p
}

func (p *Presence) mark(rssi int16, hasRSSI bool, name string) {
	p.mu.Lock()
	p.lastSeen = time.Now()
	if hasRSSI {
		p.rssi = rssi
	}
	if name != "" {
		p.name = name
	}
	p.mu.Unlock()
	select {
	case p.seen <- struct{}{}:
	default:
	}
}

// Touch records that the scale was certainly awake now.
func (p *Presence) Touch() { p.touchAt(time.Now()) }

func (p *Presence) touchAt(t time.Time) {
	p.mu.Lock()
	if t.After(p.lastSeen) {
		p.lastSeen = t
	}
	p.mu.Unlock()
}

// Seen fires (coalesced) whenever an advert of the scale is observed.
func (p *Presence) Seen() <-chan struct{} { return p.seen }

// Last returns the last time the scale was seen and its RSSI/name.
func (p *Presence) Last() (time.Time, int16, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastSeen, p.rssi, p.name
}

// Close stops tracking.
func (p *Presence) Close() {
	for _, u := range p.unwatch {
		u()
	}
}

// Daemon runs the scan -> connect -> record loop.
type Daemon struct {
	Cfg   *config.Config
	Store *store.Store
	App   *app.App
	Live  *live.Hub // may be nil
	Log   *slog.Logger
}

type loopState struct {
	armed         bool
	cooldownStart time.Time
	nextAttempt   time.Time
	failures      int
	awake         bool
}

// Run blocks until ctx is cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	cfg := d.Cfg
	_ = d.Store.Update(func(s *store.State) {
		s.Daemon.PID = os.Getpid()
		s.Daemon.StartedAt = time.Now()
		s.Daemon.Heartbeat = time.Now()
		s.Daemon.Phase = "starting"
		s.Scale.Address = cfg.Address
		s.Scale.Awake = false
	})
	defer d.Store.Update(func(s *store.State) {
		s.Daemon.Phase = "stopped"
		s.Daemon.PID = 0
		s.Scale.Awake = false
	})

	d.Log.Info("sscale daemon started", "scale", cfg.Address, "model", cfg.Model, "data", d.Store.Dir)
	backoff := 5 * time.Second
	for ctx.Err() == nil {
		err := d.runBus(ctx)
		if ctx.Err() != nil {
			break
		}
		d.Log.Error("bluetooth unavailable, retrying", "err", err, "retry_in", backoff)
		d.setPhase("waiting for bluetooth")
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
	d.Log.Info("sscale daemon stopped")
	return nil
}

func (d *Daemon) setPhase(phase string) {
	_ = d.Store.Update(func(s *store.State) {
		s.Daemon.Phase = phase
		s.Daemon.Heartbeat = time.Now()
	})
}

// runBus runs the loop on one D-Bus connection; it returns on bus errors.
func (d *Daemon) runBus(ctx context.Context) error {
	cfg := d.Cfg
	bus, err := bluez.Dial()
	if err != nil {
		return err
	}
	defer bus.Close()
	adapter, err := bus.Adapter(ctx, cfg.Adapter)
	if err != nil {
		return err
	}
	if err := adapter.EnsurePowered(ctx); err != nil {
		return err
	}
	_ = d.Store.Update(func(s *store.State) { s.Daemon.Adapter = adapter.Name })

	dev := adapter.Device(cfg.Address)
	pres := WatchPresence(bus, dev)
	defer pres.Close()
	runner := &Runner{Cfg: cfg, Store: d.Store, App: d.App, Live: d.Live, Log: d.Log}

	// A stale connection (e.g. after a crash) would keep the scale awake.
	if info, err := dev.Info(ctx); err == nil && info.Connected {
		d.Log.Info("scale already connected, disconnecting stale link")
		_ = dev.Disconnect(ctx)
	}

	ls := initialLoopState(d.Store.Snapshot(), cfg, time.Now())
	if !ls.armed {
		pres.touchAt(ls.cooldownStart)
		d.Log.Info("resuming cooldown from the previous session; waiting for the scale to sleep")
	}
	awakeTimeout := cfg.ScanWindow + 15*time.Second
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	lastBeat := time.Time{}

	for ctx.Err() == nil {
		if err := adapter.EnsurePowered(ctx); err != nil {
			return err
		}
		// Forget the cached device so the next advert is reported as new;
		// this keeps presence detection reliable even when RSSI is static.
		if info, err := dev.Info(ctx); err == nil && !info.Connected && !info.Paired {
			_ = adapter.RemoveDevice(ctx, dev.Path)
		}
		if err := adapter.StartDiscovery(ctx); err != nil {
			return err
		}
		d.setPhase(d.phaseName(ls))
		windowEnd := time.After(cfg.ScanWindow)

	window:
		for {
			select {
			case <-ctx.Done():
				break window
			case <-windowEnd:
				break window
			case <-pres.Seen():
				d.onSeen(ls, pres)
				if ls.armed && time.Now().After(ls.nextAttempt) {
					_ = adapter.StopDiscovery(ctx)
					d.session(ctx, runner, adapter, ls, pres)
					break window // restart discovery with a fresh window
				}
			case <-tick.C:
				d.onTick(ls, pres, awakeTimeout)
				if time.Since(lastBeat) >= 10*time.Second {
					lastBeat = time.Now()
					d.setPhase(d.phaseName(ls))
				}
			}
		}
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = adapter.StopDiscovery(sctx)
		cancel()
		if cfg.ScanPause > 0 && ctx.Err() == nil {
			d.setPhase("idle (scan pause)")
			pause := time.After(cfg.ScanPause)
		wait:
			for {
				select {
				case <-ctx.Done():
					break wait
				case <-pause:
					break wait
				case <-tick.C:
					d.onTick(ls, pres, awakeTimeout)
				}
			}
		}
	}
	return nil
}

// initialLoopState restores the post-session cooldown after a restart, so a
// restarted daemon does not reconnect to a scale it just read (which would
// keep it awake).
func initialLoopState(st store.State, cfg *config.Config, now time.Time) *loopState {
	ls := &loopState{armed: true}
	if s := st.LastSession; s != nil && s.Result == "ok" && now.Sub(s.End) < cfg.MaxCooldown {
		ls.armed = false
		ls.cooldownStart = s.End
	}
	return ls
}

func (d *Daemon) phaseName(ls *loopState) string {
	if ls.armed {
		return "scanning"
	}
	return "cooldown (waiting for scale to sleep)"
}

func (d *Daemon) onSeen(ls *loopState, pres *Presence) {
	last, rssi, name := pres.Last()
	if !ls.awake {
		ls.awake = true
		d.Log.Info("scale is awake", "rssi", rssi)
	}
	_ = d.Store.Update(func(s *store.State) {
		t := last
		s.Scale.LastSeen = &t
		s.Scale.RSSI = int(rssi)
		s.Scale.Awake = true
		if name != "" {
			s.Scale.Name = name
		}
	})
}

func (d *Daemon) onTick(ls *loopState, pres *Presence, awakeTimeout time.Duration) {
	last, _, _ := pres.Last()
	if ls.awake && time.Since(last) > awakeTimeout {
		ls.awake = false
		d.Log.Info("scale went to sleep")
		_ = d.Store.Update(func(s *store.State) { s.Scale.Awake = false })
	}
	if !ls.armed {
		quiet := time.Since(last) >= d.Cfg.QuietPeriod
		expired := time.Since(ls.cooldownStart) >= d.Cfg.MaxCooldown
		if quiet || expired {
			ls.armed = true
			d.Log.Debug("ready for next weighing", "quiet", quiet, "cooldown_expired", expired)
		}
	}
}

func (d *Daemon) session(ctx context.Context, runner *Runner, adapter *bluez.Adapter, ls *loopState, pres *Presence) {
	start := time.Now()
	d.setPhase("connected")
	out, err := runner.Run(ctx, adapter, Mode{Measure: true})
	end := time.Now()
	pres.Touch() // it was awake until now
	sess := &store.Session{Start: start, End: end, Records: len(out.New), Result: "ok"}

	switch {
	case err == nil:
		ls.failures = 0
		ls.armed = false
		ls.cooldownStart = end
		ls.nextAttempt = time.Time{}
		if len(out.New) == 0 {
			d.Log.Info("session finished, no new measurements")
		} else {
			d.Log.Info("session finished", "new_measurements", len(out.New))
		}
	case ctx.Err() != nil:
		sess.Result, sess.Error = "cancelled", err.Error()
	default:
		ls.failures++
		delay := retryDelay(ls.failures)
		if IsAuthError(err) {
			delay = 10 * time.Minute
		}
		if out.Connected && len(out.New) > 0 {
			// We got data but the session ended badly (e.g. scale went to sleep).
			ls.armed = false
			ls.cooldownStart = end
		}
		ls.nextAttempt = end.Add(delay)
		sess.Result, sess.Error = "error", err.Error()
		d.Log.Warn("session failed", "err", err, "retry_after", delay.Round(time.Second))
	}
	_ = d.Store.Update(func(s *store.State) {
		s.Sessions++
		s.LastSession = sess
		if sess.Result == "ok" {
			t := end
			s.LastSuccess = &t
		}
		t := end
		s.Scale.LastSeen = &t
	})
}

func retryDelay(failures int) time.Duration {
	steps := []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute}
	return steps[min(failures-1, len(steps)-1)]
}

// WaitForScale scans until the scale advertises or timeout elapses.
func WaitForScale(ctx context.Context, bus *bluez.Client, adapter *bluez.Adapter, dev *bluez.Device, timeout time.Duration) error {
	pres := WatchPresence(bus, dev)
	defer pres.Close()
	if info, err := dev.Info(ctx); err == nil && !info.Connected && !info.Paired {
		_ = adapter.RemoveDevice(ctx, dev.Path)
	}
	if err := adapter.StartDiscovery(ctx); err != nil {
		return err
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = adapter.StopDiscovery(sctx)
		cancel()
	}()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-pres.Seen():
		return nil
	case <-t.C:
		return fmt.Errorf("scale %s not seen within %s - step on the scale to wake it up", dev.Address, timeout)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ErrDaemonRunning is returned by one-shot commands while the daemon runs.
var ErrDaemonRunning = errors.New("the sscale daemon is running; stop it first (sscale stop) or let it capture the measurement")
