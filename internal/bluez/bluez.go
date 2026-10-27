// Package bluez is a small BlueZ D-Bus client covering what sscale needs:
// LE discovery, connecting, GATT writes and notifications.
package bluez

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	service      = "org.bluez"
	ifAdapter    = "org.bluez.Adapter1"
	ifDevice     = "org.bluez.Device1"
	ifGattSvc    = "org.bluez.GattService1"
	ifGattChar   = "org.bluez.GattCharacteristic1"
	ifProps      = "org.freedesktop.DBus.Properties"
	ifObjManager = "org.freedesktop.DBus.ObjectManager"
)

// Re-exported D-Bus types so callers need not import godbus.
type (
	ObjectPath = dbus.ObjectPath
	Variant    = dbus.Variant
)

// PropsHandler receives PropertiesChanged signals for a path.
type PropsHandler func(iface string, changed map[string]dbus.Variant)

// AddedHandler receives InterfacesAdded signals.
type AddedHandler func(path dbus.ObjectPath, ifaces map[string]map[string]dbus.Variant)

// Client wraps a system bus connection to BlueZ.
type Client struct {
	conn *dbus.Conn

	mu      sync.Mutex
	nextID  int
	props   map[dbus.ObjectPath]map[int]PropsHandler
	added   map[int]AddedHandler
	removed map[int]func(dbus.ObjectPath, []string)
	sigs    chan *dbus.Signal
	done    chan struct{}
}

// Dial connects to the system bus and starts the signal dispatcher.
func Dial() (*Client, error) {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return nil, fmt.Errorf("connect to system D-Bus: %w", err)
	}
	c := &Client{
		conn:    conn,
		props:   map[dbus.ObjectPath]map[int]PropsHandler{},
		added:   map[int]AddedHandler{},
		removed: map[int]func(dbus.ObjectPath, []string){},
		sigs:    make(chan *dbus.Signal, 1024),
		done:    make(chan struct{}),
	}
	opts := [][]dbus.MatchOption{
		{dbus.WithMatchSender(service), dbus.WithMatchInterface(ifProps), dbus.WithMatchMember("PropertiesChanged"), dbus.WithMatchPathNamespace("/org/bluez")},
		{dbus.WithMatchSender(service), dbus.WithMatchInterface(ifObjManager), dbus.WithMatchMember("InterfacesAdded")},
		{dbus.WithMatchSender(service), dbus.WithMatchInterface(ifObjManager), dbus.WithMatchMember("InterfacesRemoved")},
	}
	for _, o := range opts {
		if err := conn.AddMatchSignal(o...); err != nil {
			conn.Close()
			return nil, fmt.Errorf("add D-Bus match: %w", err)
		}
	}
	conn.Signal(c.sigs)
	go c.dispatch()
	return c, nil
}

// Close closes the bus connection.
func (c *Client) Close() error {
	err := c.conn.Close()
	<-c.done
	return err
}

func (c *Client) dispatch() {
	defer close(c.done)
	for sig := range c.sigs {
		switch sig.Name {
		case ifProps + ".PropertiesChanged":
			if len(sig.Body) < 2 {
				continue
			}
			iface, _ := sig.Body[0].(string)
			changed, _ := sig.Body[1].(map[string]dbus.Variant)
			c.mu.Lock()
			hs := make([]PropsHandler, 0, len(c.props[sig.Path]))
			for _, h := range c.props[sig.Path] {
				hs = append(hs, h)
			}
			c.mu.Unlock()
			for _, h := range hs {
				h(iface, changed)
			}
		case ifObjManager + ".InterfacesAdded":
			if len(sig.Body) < 2 {
				continue
			}
			path, _ := sig.Body[0].(dbus.ObjectPath)
			ifaces, _ := sig.Body[1].(map[string]map[string]dbus.Variant)
			c.mu.Lock()
			hs := make([]AddedHandler, 0, len(c.added))
			for _, h := range c.added {
				hs = append(hs, h)
			}
			c.mu.Unlock()
			for _, h := range hs {
				h(path, ifaces)
			}
		case ifObjManager + ".InterfacesRemoved":
			if len(sig.Body) < 2 {
				continue
			}
			path, _ := sig.Body[0].(dbus.ObjectPath)
			ifaces, _ := sig.Body[1].([]string)
			c.mu.Lock()
			hs := make([]func(dbus.ObjectPath, []string), 0, len(c.removed))
			for _, h := range c.removed {
				hs = append(hs, h)
			}
			c.mu.Unlock()
			for _, h := range hs {
				h(path, ifaces)
			}
		}
	}
}

// WatchProps registers h for PropertiesChanged on path. Call the returned
// func to unregister.
func (c *Client) WatchProps(path dbus.ObjectPath, h PropsHandler) func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.nextID
	c.nextID++
	if c.props[path] == nil {
		c.props[path] = map[int]PropsHandler{}
	}
	c.props[path][id] = h
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		delete(c.props[path], id)
		if len(c.props[path]) == 0 {
			delete(c.props, path)
		}
	}
}

// WatchAdded registers h for InterfacesAdded.
func (c *Client) WatchAdded(h AddedHandler) func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.nextID
	c.nextID++
	c.added[id] = h
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		delete(c.added, id)
	}
}

// WatchRemoved registers h for InterfacesRemoved.
func (c *Client) WatchRemoved(h func(dbus.ObjectPath, []string)) func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.nextID
	c.nextID++
	c.removed[id] = h
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		delete(c.removed, id)
	}
}

// ManagedObjects returns BlueZ's object tree.
func (c *Client) ManagedObjects(ctx context.Context) (map[dbus.ObjectPath]map[string]map[string]dbus.Variant, error) {
	var out map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	err := c.conn.Object(service, "/").CallWithContext(ctx, ifObjManager+".GetManagedObjects", 0).Store(&out)
	if err != nil {
		return nil, fmt.Errorf("GetManagedObjects: %w", err)
	}
	return out, nil
}

func (c *Client) call(ctx context.Context, path dbus.ObjectPath, method string, args ...any) *dbus.Call {
	return c.conn.Object(service, path).CallWithContext(ctx, method, 0, args...)
}

func (c *Client) getProp(ctx context.Context, path dbus.ObjectPath, iface, name string) (dbus.Variant, error) {
	var v dbus.Variant
	err := c.call(ctx, path, ifProps+".Get", iface, name).Store(&v)
	return v, err
}

func (c *Client) setProp(ctx context.Context, path dbus.ObjectPath, iface, name string, v any) error {
	return c.call(ctx, path, ifProps+".Set", iface, name, dbus.MakeVariant(v)).Err
}

func (c *Client) getAll(ctx context.Context, path dbus.ObjectPath, iface string) (map[string]dbus.Variant, error) {
	var out map[string]dbus.Variant
	err := c.call(ctx, path, ifProps+".GetAll", iface).Store(&out)
	return out, err
}

// ---------------------------------------------------------------- Adapter

// Adapter is a local Bluetooth controller.
type Adapter struct {
	c    *Client
	Path dbus.ObjectPath
	Name string
}

// ErrNoAdapter is returned when no Bluetooth controller is present.
var ErrNoAdapter = errors.New("no Bluetooth adapter found")

// Adapter finds the adapter named name (e.g. "hci0"), or the first one.
func (c *Client) Adapter(ctx context.Context, name string) (*Adapter, error) {
	objs, err := c.ManagedObjects(ctx)
	if err != nil {
		return nil, err
	}
	var paths []string
	for p, ifs := range objs {
		if _, ok := ifs[ifAdapter]; ok {
			paths = append(paths, string(p))
		}
	}
	sort.Strings(paths)
	for _, p := range paths {
		base := p[strings.LastIndex(p, "/")+1:]
		if name == "" || name == base {
			return &Adapter{c: c, Path: dbus.ObjectPath(p), Name: base}, nil
		}
	}
	if name != "" {
		return nil, fmt.Errorf("bluetooth adapter %q not found", name)
	}
	return nil, ErrNoAdapter
}

// AdapterInfo describes the adapter state.
type AdapterInfo struct {
	Name, Address, Alias string
	Powered, Discovering bool
}

// Info reads the adapter state.
func (a *Adapter) Info(ctx context.Context) (AdapterInfo, error) {
	p, err := a.c.getAll(ctx, a.Path, ifAdapter)
	if err != nil {
		return AdapterInfo{}, err
	}
	return AdapterInfo{
		Name:        a.Name,
		Address:     str(p["Address"]),
		Alias:       str(p["Alias"]),
		Powered:     boolean(p["Powered"]),
		Discovering: boolean(p["Discovering"]),
	}, nil
}

// EnsurePowered powers the adapter on if needed.
func (a *Adapter) EnsurePowered(ctx context.Context) error {
	info, err := a.Info(ctx)
	if err != nil {
		return err
	}
	if info.Powered {
		return nil
	}
	if err := a.c.setProp(ctx, a.Path, ifAdapter, "Powered", true); err != nil {
		return fmt.Errorf("power on %s: %w", a.Name, err)
	}
	return nil
}

// StartDiscovery starts an LE discovery session that reports every
// advertisement (DuplicateData) so we can track presence.
func (a *Adapter) StartDiscovery(ctx context.Context) error {
	filter := map[string]any{
		"Transport":     "le",
		"DuplicateData": true,
	}
	if err := a.c.call(ctx, a.Path, ifAdapter+".SetDiscoveryFilter", filter).Err; err != nil {
		return fmt.Errorf("SetDiscoveryFilter: %w", err)
	}
	err := a.c.call(ctx, a.Path, ifAdapter+".StartDiscovery").Err
	if err != nil && !strings.Contains(err.Error(), "InProgress") {
		return fmt.Errorf("StartDiscovery: %w", err)
	}
	return nil
}

// StopDiscovery stops this client's discovery session.
func (a *Adapter) StopDiscovery(ctx context.Context) error {
	err := a.c.call(ctx, a.Path, ifAdapter+".StopDiscovery").Err
	if err != nil && !strings.Contains(err.Error(), "NotReady") && !strings.Contains(err.Error(), "No discovery started") &&
		!strings.Contains(err.Error(), "NotAuthorized") {
		return fmt.Errorf("StopDiscovery: %w", err)
	}
	return nil
}

// RemoveDevice drops a device from BlueZ's cache.
func (a *Adapter) RemoveDevice(ctx context.Context, path dbus.ObjectPath) error {
	err := a.c.call(ctx, a.Path, ifAdapter+".RemoveDevice", path).Err
	if err != nil && !strings.Contains(err.Error(), "DoesNotExist") && !strings.Contains(err.Error(), "does not exist") {
		return err
	}
	return nil
}

// DevicePath returns the D-Bus path a device with addr would have.
func (a *Adapter) DevicePath(addr string) dbus.ObjectPath {
	return dbus.ObjectPath(string(a.Path) + "/dev_" + strings.ReplaceAll(strings.ToUpper(addr), ":", "_"))
}

// Device returns a handle for addr (which may not exist in BlueZ yet).
func (a *Adapter) Device(addr string) *Device {
	return &Device{c: a.c, a: a, Path: a.DevicePath(addr), Address: strings.ToUpper(addr)}
}

// DeviceInfo is a snapshot of a discovered device.
type DeviceInfo struct {
	Path             dbus.ObjectPath
	Address          string
	AddressType      string
	Name             string
	RSSI             int16
	HasRSSI          bool
	Connected        bool
	Paired           bool
	ServicesResolved bool
	UUIDs            []string
	ManufacturerData map[uint16][]byte
	ServiceData      map[string][]byte
}

// ParseDeviceProps converts Device1 properties to DeviceInfo.
func ParseDeviceProps(path dbus.ObjectPath, p map[string]dbus.Variant) DeviceInfo {
	d := DeviceInfo{
		Path:             path,
		Address:          str(p["Address"]),
		AddressType:      str(p["AddressType"]),
		Name:             str(p["Name"]),
		Connected:        boolean(p["Connected"]),
		Paired:           boolean(p["Paired"]),
		ServicesResolved: boolean(p["ServicesResolved"]),
		ManufacturerData: map[uint16][]byte{},
		ServiceData:      map[string][]byte{},
	}
	if d.Name == "" {
		d.Name = str(p["Alias"])
		if strings.ReplaceAll(d.Name, "-", ":") == d.Address {
			d.Name = ""
		}
	}
	if v, ok := p["RSSI"]; ok {
		if r, ok := v.Value().(int16); ok {
			d.RSSI, d.HasRSSI = r, true
		}
	}
	if v, ok := p["UUIDs"]; ok {
		d.UUIDs, _ = v.Value().([]string)
	}
	if v, ok := p["ManufacturerData"]; ok {
		if m, ok := v.Value().(map[uint16]dbus.Variant); ok {
			for k, b := range m {
				if bs, ok := b.Value().([]byte); ok {
					d.ManufacturerData[k] = bs
				}
			}
		}
	}
	if v, ok := p["ServiceData"]; ok {
		if m, ok := v.Value().(map[string]dbus.Variant); ok {
			for k, b := range m {
				if bs, ok := b.Value().([]byte); ok {
					d.ServiceData[k] = bs
				}
			}
		}
	}
	return d
}

// Devices lists devices known to BlueZ under this adapter.
func (a *Adapter) Devices(ctx context.Context) ([]DeviceInfo, error) {
	objs, err := a.c.ManagedObjects(ctx)
	if err != nil {
		return nil, err
	}
	var out []DeviceInfo
	for p, ifs := range objs {
		props, ok := ifs[ifDevice]
		if !ok || !strings.HasPrefix(string(p), string(a.Path)+"/") {
			continue
		}
		out = append(out, ParseDeviceProps(p, props))
	}
	return out, nil
}

// ---------------------------------------------------------------- Device

// Device is a remote BLE device.
type Device struct {
	c       *Client
	a       *Adapter
	Path    dbus.ObjectPath
	Address string
}

// Client returns the bus client.
func (d *Device) Client() *Client { return d.c }

// Info reads the device properties. It fails if BlueZ doesn't know the device.
func (d *Device) Info(ctx context.Context) (DeviceInfo, error) {
	p, err := d.c.getAll(ctx, d.Path, ifDevice)
	if err != nil {
		return DeviceInfo{}, err
	}
	return ParseDeviceProps(d.Path, p), nil
}

// Exists reports whether BlueZ currently has an object for the device.
func (d *Device) Exists(ctx context.Context) bool {
	_, err := d.Info(ctx)
	return err == nil
}

// Connect connects to the device and waits until GATT services are resolved.
func (d *Device) Connect(ctx context.Context) error {
	resolved := make(chan struct{}, 1)
	unwatch := d.c.WatchProps(d.Path, func(iface string, ch map[string]dbus.Variant) {
		if iface != ifDevice {
			return
		}
		if v, ok := ch["ServicesResolved"]; ok && boolean(v) {
			select {
			case resolved <- struct{}{}:
			default:
			}
		}
	})
	defer unwatch()

	if err := d.c.call(ctx, d.Path, ifDevice+".Connect").Err; err != nil {
		if !strings.Contains(err.Error(), "AlreadyConnected") {
			return fmt.Errorf("connect %s: %w", d.Address, err)
		}
	}
	for {
		info, err := d.Info(ctx)
		if err == nil && info.ServicesResolved {
			return nil
		}
		if err == nil && !info.Connected {
			return fmt.Errorf("connect %s: device disconnected during service discovery", d.Address)
		}
		select {
		case <-resolved:
		case <-ctx.Done():
			return fmt.Errorf("connect %s: waiting for services: %w", d.Address, ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// Disconnect disconnects (also cancels a pending connection attempt).
func (d *Device) Disconnect(ctx context.Context) error {
	err := d.c.call(ctx, d.Path, ifDevice+".Disconnect").Err
	if err != nil && (strings.Contains(err.Error(), "NotConnected") || strings.Contains(err.Error(), "UnknownObject") ||
		strings.Contains(err.Error(), "doesn't exist")) {
		return nil
	}
	return err
}

// SetTrusted marks the device as trusted in BlueZ.
func (d *Device) SetTrusted(ctx context.Context, v bool) error {
	return d.c.setProp(ctx, d.Path, ifDevice, "Trusted", v)
}

// Characteristics lists the device's GATT characteristics.
func (d *Device) Characteristics(ctx context.Context) ([]*Characteristic, error) {
	objs, err := d.c.ManagedObjects(ctx)
	if err != nil {
		return nil, err
	}
	svcUUID := map[dbus.ObjectPath]string{}
	for p, ifs := range objs {
		if s, ok := ifs[ifGattSvc]; ok && strings.HasPrefix(string(p), string(d.Path)+"/") {
			svcUUID[p] = strings.ToLower(str(s["UUID"]))
		}
	}
	var out []*Characteristic
	for p, ifs := range objs {
		ch, ok := ifs[ifGattChar]
		if !ok || !strings.HasPrefix(string(p), string(d.Path)+"/") {
			continue
		}
		flags, _ := ch["Flags"].Value().([]string)
		svcPath, _ := ch["Service"].Value().(dbus.ObjectPath)
		out = append(out, &Characteristic{
			c:       d.c,
			Path:    p,
			UUID:    strings.ToLower(str(ch["UUID"])),
			Service: svcUUID[svcPath],
			Flags:   flags,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// WatchConnected calls fn(false) when the device disconnects.
func (d *Device) WatchConnected(fn func(connected bool)) func() {
	return d.c.WatchProps(d.Path, func(iface string, ch map[string]dbus.Variant) {
		if iface != ifDevice {
			return
		}
		if v, ok := ch["Connected"]; ok {
			fn(boolean(v))
		}
	})
}

// ---------------------------------------------------------- Characteristic

// Characteristic is a GATT characteristic.
type Characteristic struct {
	c       *Client
	Path    dbus.ObjectPath
	UUID    string
	Service string
	Flags   []string
}

// Has reports whether the characteristic has a flag such as "notify".
func (ch *Characteristic) Has(flag string) bool {
	for _, f := range ch.Flags {
		if f == flag {
			return true
		}
	}
	return false
}

// Write writes value, with response when the characteristic supports it.
func (ch *Characteristic) Write(ctx context.Context, value []byte) error {
	typ := "request"
	if !ch.Has("write") && ch.Has("write-without-response") {
		typ = "command"
	}
	opts := map[string]dbus.Variant{"type": dbus.MakeVariant(typ)}
	if err := ch.c.call(ctx, ch.Path, ifGattChar+".WriteValue", value, opts).Err; err != nil {
		return fmt.Errorf("write %s: %w", ch.UUID, err)
	}
	return nil
}

// Read reads the characteristic value.
func (ch *Characteristic) Read(ctx context.Context) ([]byte, error) {
	var v []byte
	err := ch.c.call(ctx, ch.Path, ifGattChar+".ReadValue", map[string]dbus.Variant{}).Store(&v)
	return v, err
}

// Subscribe enables notifications and delivers every value to fn (called
// from the dispatcher goroutine; fn must not block). The returned func
// unregisters the handler.
func (ch *Characteristic) Subscribe(ctx context.Context, fn func([]byte)) (func(), error) {
	unwatch := ch.c.WatchProps(ch.Path, func(iface string, changed map[string]dbus.Variant) {
		if iface != ifGattChar {
			return
		}
		if v, ok := changed["Value"]; ok {
			if b, ok := v.Value().([]byte); ok {
				fn(b)
			}
		}
	})
	if err := ch.c.call(ctx, ch.Path, ifGattChar+".StartNotify").Err; err != nil &&
		!strings.Contains(err.Error(), "InProgress") && !strings.Contains(err.Error(), "Already notifying") {
		unwatch()
		return nil, fmt.Errorf("StartNotify %s: %w", ch.UUID, err)
	}
	return unwatch, nil
}

func str(v dbus.Variant) string {
	s, _ := v.Value().(string)
	return s
}

func boolean(v dbus.Variant) bool {
	b, _ := v.Value().(bool)
	return b
}
