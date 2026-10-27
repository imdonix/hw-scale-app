package scale

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/tamasmagyar/sscale/internal/bluez"
)

// Transport is the GATT link to the scale. It is implemented over BlueZ and
// by a simulated scale in tests.
type Transport interface {
	// Has reports whether the characteristic exists.
	Has(uuid string) bool
	// Write writes one frame to the characteristic.
	Write(ctx context.Context, uuid string, frame []byte) error
	// Subscribe enables notifications on every notifying characteristic and
	// delivers them to fn (which must not block).
	Subscribe(ctx context.Context, fn func(uuid string, data []byte)) error
	// Disconnected is closed when the link drops.
	Disconnected() <-chan struct{}
	// Close disconnects.
	Close()
}

type bluezTransport struct {
	dev   *bluez.Device
	chars map[string]*bluez.Characteristic
	unsub []func()
	disc  chan struct{}
	once  sync.Once
}

// Dial connects to the scale over BlueZ and resolves its characteristics.
func Dial(ctx context.Context, adapter *bluez.Adapter, address string) (Transport, error) {
	t := &bluezTransport{
		dev:   adapter.Device(address),
		chars: map[string]*bluez.Characteristic{},
		disc:  make(chan struct{}),
	}
	if err := t.dev.Connect(ctx); err != nil {
		dctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = t.dev.Disconnect(dctx) // cancel a pending connection attempt
		cancel()
		return nil, err
	}
	t.unsub = append(t.unsub, t.dev.WatchConnected(func(c bool) {
		if !c {
			t.once.Do(func() { close(t.disc) })
		}
	}))
	chars, err := t.dev.Characteristics(ctx)
	if err != nil {
		t.Close()
		return nil, err
	}
	for _, ch := range chars {
		if _, known := charNames[ch.UUID]; !known {
			continue
		}
		if _, dup := t.chars[ch.UUID]; !dup {
			t.chars[ch.UUID] = ch
		}
	}
	if t.chars[chrRequestAuth] == nil || t.chars[chrRealtimeWeight] == nil {
		var all []string
		for _, ch := range chars {
			all = append(all, ch.UUID)
		}
		t.Close()
		return nil, fmt.Errorf("device does not look like a Huawei Hagrid scale (characteristics: %s)", strings.Join(all, ", "))
	}
	return t, nil
}

func (t *bluezTransport) Has(uuid string) bool { return t.chars[uuid] != nil }

func (t *bluezTransport) Write(ctx context.Context, uuid string, frame []byte) error {
	ch := t.chars[uuid]
	if ch == nil {
		return fmt.Errorf("characteristic %s not present", nameOf(uuid))
	}
	return ch.Write(ctx, frame)
}

func (t *bluezTransport) Subscribe(ctx context.Context, fn func(string, []byte)) error {
	for uuid, ch := range t.chars {
		if !ch.Has("notify") && !ch.Has("indicate") {
			continue
		}
		u := uuid
		unsub, err := ch.Subscribe(ctx, func(b []byte) { fn(u, b) })
		if err != nil {
			if t.Has(u) && (u == chrRequestAuth || u == chrAuthToken || u == chrSendWorkKey) {
				return err
			}
			continue // optional characteristic
		}
		t.unsub = append(t.unsub, unsub)
	}
	return nil
}

func (t *bluezTransport) Disconnected() <-chan struct{} { return t.disc }

func (t *bluezTransport) Close() {
	for _, u := range t.unsub {
		u()
	}
	t.unsub = nil
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = t.dev.Disconnect(ctx)
}
