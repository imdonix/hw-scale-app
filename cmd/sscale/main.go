// Command sscale reads weight measurements from a Huawei Scale 3 over BLE.
//
// Without a command it runs the daemon: it scans for the scale, connects
// once each time the scale wakes up, logs the measurement and disconnects so
// the scale can go back to sleep.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
	_ "time/tzdata" // honour TZ inside minimal containers

	"github.com/tamasmagyar/sscale/internal/api"
	"github.com/tamasmagyar/sscale/internal/app"
	"github.com/tamasmagyar/sscale/internal/bluez"
	"github.com/tamasmagyar/sscale/internal/config"
	"github.com/tamasmagyar/sscale/internal/daemon"
	"github.com/tamasmagyar/sscale/internal/db"
	"github.com/tamasmagyar/sscale/internal/logx"
	"github.com/tamasmagyar/sscale/internal/store"
	"github.com/tamasmagyar/sscale/internal/webui"
)

var version = "dev"

const usage = `sscale - Huawei Scale 3 BLE reader

Usage:
  sscale [flags]                 start the daemon in the background
  sscale -f [flags]              run the daemon in the foreground (logs to the terminal)
  sscale <command> [flags]

Commands:
  daemon     scan for the scale and log every weighing (default; -f = foreground)
  scan       list nearby Bluetooth LE devices / Huawei scales
  pair       wake the scale, authenticate with keys.json and register the user
  once       wait for one weighing, print it and exit
  status     show daemon, pairing and last measurement status
  history    show recorded measurements
  stop       stop the running daemon
  apikey     print the web app / REST API key (--rotate for a new one)
  version    print the version

The daemon also serves the web app and REST API (env SSCALE_HTTP, default :8080).

Common flags (accepted by every command):
  --keys PATH       keys file (env SSCALE_KEYS, default keys.json)
  --data DIR        data directory (env SSCALE_DATA, default data)
  --adapter NAME    Bluetooth adapter, e.g. hci0 (env SSCALE_ADAPTER, default first)
  --address MAC     scale address (default: "address" in keys.json)
  --log-level LVL   debug|info|warn|error (env SSCALE_LOG_LEVEL)
  --trace           log every BLE frame (env SSCALE_TRACE)

Run "sscale <command> -h" for command specific flags.
`

func main() {
	args := os.Args[1:]
	cmd := "daemon"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") && cmd == "daemon" && len(os.Args) == 2 {
		fmt.Print(usage)
		return
	}
	cfg := config.Defaults()
	var err error
	switch cmd {
	case "daemon", "run":
		err = cmdDaemon(&cfg, args)
	case "scan":
		err = cmdScan(&cfg, args)
	case "pair":
		err = cmdPair(&cfg, args)
	case "once", "read":
		err = cmdOnce(&cfg, args)
	case "status":
		err = cmdStatus(&cfg, args)
	case "history":
		err = cmdHistory(&cfg, args)
	case "stop":
		err = cmdStop(&cfg, args)
	case "apikey":
		err = cmdAPIKey(&cfg, args)
	case "healthcheck":
		err = cmdHealthcheck(&cfg)
	case "version":
		fmt.Println("sscale", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newFlags(name string, cfg *config.Config) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.StringVar(&cfg.KeysPath, "keys", cfg.KeysPath, "keys file")
	fs.StringVar(&cfg.DataDir, "data", cfg.DataDir, "data directory")
	fs.StringVar(&cfg.Adapter, "adapter", cfg.Adapter, "Bluetooth adapter (e.g. hci0)")
	fs.StringVar(&cfg.Address, "address", cfg.Address, "scale Bluetooth address")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "log level: debug|info|warn|error")
	fs.BoolVar(&cfg.Trace, "trace", cfg.Trace, "log every BLE frame")
	return fs
}

func logger(cfg *config.Config) *slog.Logger {
	lvl := cfg.LogLevel
	if cfg.Trace {
		lvl = "debug"
	}
	return logx.New(os.Stdout, lvl)
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func loadConfig(cfg *config.Config) error {
	if err := cfg.LoadKeys(); err != nil {
		return err
	}
	return cfg.Validate()
}

// ------------------------------------------------------------------ daemon

func cmdDaemon(cfg *config.Config, args []string) error {
	fs := newFlags("daemon", cfg)
	fs.DurationVar(&cfg.ScanWindow, "scan-window", cfg.ScanWindow, "length of one discovery window")
	fs.DurationVar(&cfg.ScanPause, "scan-pause", cfg.ScanPause, "pause between discovery windows (0 = continuous)")
	fs.DurationVar(&cfg.QuietPeriod, "quiet-period", cfg.QuietPeriod, "scale must be silent this long before reconnecting")
	fs.DurationVar(&cfg.MaxCooldown, "max-cooldown", cfg.MaxCooldown, "upper bound for the post-session cooldown")
	fs.DurationVar(&cfg.IdleGrace, "idle-grace", cfg.IdleGrace, "wait this long for a weighing on an idle scale")
	var foreground bool
	fs.BoolVar(&foreground, "f", false, "run in the foreground (shorthand)")
	fs.BoolVar(&foreground, "foreground", false, "run in the foreground and log to the terminal")
	fs.Parse(args)
	if err := loadConfig(cfg); err != nil {
		return err
	}
	if !foreground {
		return startBackground(cfg, args)
	}
	log := logger(cfg)

	lock, err := store.AcquireLock(cfg.DataDir)
	if err != nil {
		if errors.Is(err, store.ErrLocked) {
			return errors.New("another sscale daemon is already running with this data directory")
		}
		return err
	}
	defer lock.Release()
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	if err := store.WritePID(cfg.DataDir); err != nil {
		return err
	}
	defer store.RemovePID(cfg.DataDir)

	ctx, cancel := signalContext()
	defer cancel()
	a, err := app.Open(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer a.Close()

	httpErr := make(chan error, 1)
	if cfg.HTTPAddr != "" {
		srv, err := startHTTP(ctx, cfg, a, st, log)
		if err != nil {
			return err
		}
		go func() { httpErr <- srv() }()
	}
	d := &daemon.Daemon{Cfg: cfg, Store: st, App: a, Log: log}
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	select {
	case err := <-done:
		return err
	case err := <-httpErr:
		cancel()
		<-done
		if err != nil {
			return fmt.Errorf("web server: %w", err)
		}
		return nil
	}
}

// startBackground re-executes sscale detached from the terminal with
// --foreground, logging to <data>/sscale.log.
func startBackground(cfg *config.Config, args []string) error {
	if store.IsLocked(cfg.DataDir) {
		return errors.New("the sscale daemon is already running (sscale status / sscale stop)")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return err
	}
	logPath := filepath.Join(cfg.DataDir, "sscale.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()

	cmd := exec.Command(exe, append([]string{"daemon", "--foreground"}, args...)...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	// Wait until the daemon holds its lock, or report why it died.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case err := <-exited:
			return fmt.Errorf("daemon exited during startup (%v), see %s", err, logPath)
		case <-deadline:
			return fmt.Errorf("daemon did not start within 5s, see %s", logPath)
		case <-time.After(100 * time.Millisecond):
			if store.IsLocked(cfg.DataDir) {
				fmt.Printf("sscale daemon started in the background (pid %d), log: %s\n", cmd.Process.Pid, logPath)
				fmt.Println("Run it in the foreground instead with: sscale -f")
				return nil
			}
		}
	}
}

// startHTTP prepares the REST API / web app server. The returned func
// serves until ctx is cancelled.
func startHTTP(ctx context.Context, cfg *config.Config, a *app.App, st *store.Store, log *slog.Logger) (func() error, error) {
	key := cfg.APIKey
	if key == "" {
		k, created, err := a.DB.APIKey(ctx)
		if err != nil {
			return nil, err
		}
		key = k
		if created {
			log.Info("generated a new API key")
		}
	}
	scheme := "http"
	if cfg.TLSCert != "" {
		scheme = "https"
	}
	log.Info("web app and REST API listening", "addr", scheme+"://"+displayAddr(cfg.HTTPAddr))
	log.Info("API key (enter it in the web app; rotate with: sscale apikey --rotate)", "key", key)

	server := &api.Server{
		App:         a,
		State:       st.Snapshot,
		UI:          webui.FS(),
		Log:         log,
		KeyOverride: cfg.APIKey,
	}
	hs := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	ln, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", cfg.HTTPAddr, err)
	}
	return func() error {
		go func() {
			<-ctx.Done()
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = hs.Shutdown(sctx)
		}()
		var err error
		if cfg.TLSCert != "" {
			err = hs.ServeTLS(ln, cfg.TLSCert, cfg.TLSKey)
		} else {
			err = hs.Serve(ln)
		}
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}, nil
}

func displayAddr(a string) string {
	if strings.HasPrefix(a, ":") {
		return "0.0.0.0" + a
	}
	return a
}

// cmdHealthcheck probes the local web server (used by the Docker HEALTHCHECK).
func cmdHealthcheck(cfg *config.Config) error {
	if cfg.HTTPAddr == "" {
		return nil
	}
	_, port, err := net.SplitHostPort(cfg.HTTPAddr)
	if err != nil {
		return err
	}
	scheme := "http"
	client := &http.Client{Timeout: 3 * time.Second}
	if cfg.TLSCert != "" {
		scheme = "https"
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} // localhost probe only
	}
	resp, err := client.Get(scheme + "://127.0.0.1:" + port + "/api/health")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health endpoint returned %s", resp.Status)
	}
	return nil
}

func cmdAPIKey(cfg *config.Config, args []string) error {
	fs := newFlags("apikey", cfg)
	rotate := fs.Bool("rotate", false, "replace the API key with a new one")
	fs.Parse(args)
	_ = cfg.LoadKeys()
	if cfg.APIKey != "" {
		fmt.Println(cfg.APIKey)
		fmt.Fprintln(os.Stderr, "(set via SSCALE_API_KEY)")
		return nil
	}
	ctx := context.Background()
	d, err := db.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	defer d.Close()
	var key string
	if *rotate {
		key, err = d.RotateAPIKey(ctx)
		if err == nil {
			fmt.Fprintln(os.Stderr, "API key rotated; the old key stops working immediately.")
		}
	} else {
		key, _, err = d.APIKey(ctx)
	}
	if err != nil {
		return err
	}
	fmt.Println(key)
	return nil
}

// ------------------------------------------------------------------ scan

var scaleMarkers = []string{"HUAWEI", "HONOR", "HEM-B19", "HERM-B19", "HAG-B19", "HAGRID", "LUP-B19", "LUPIN", "DOBBY", "M00D", "M00F", "M0CJ", "N001"}

const huaweiCompanyID = 0x027D

func looksLikeScale(d bluez.DeviceInfo, target string) (bool, string) {
	if strings.EqualFold(d.Address, target) {
		return true, "configured scale"
	}
	texts := []string{strings.ToUpper(d.Name)}
	for _, b := range d.ManufacturerData {
		texts = append(texts, strings.ToUpper(printable(b)))
	}
	for _, b := range d.ServiceData {
		texts = append(texts, strings.ToUpper(printable(b)))
	}
	for _, t := range texts {
		for _, m := range scaleMarkers {
			if strings.Contains(t, m) {
				return true, "marker " + m
			}
		}
	}
	if _, ok := d.ManufacturerData[huaweiCompanyID]; ok {
		return true, "Huawei manufacturer data"
	}
	return false, ""
}

func printable(b []byte) string {
	out := make([]byte, len(b))
	for i, c := range b {
		if c >= 0x20 && c <= 0x7E {
			out[i] = c
		} else {
			out[i] = ' '
		}
	}
	return string(out)
}

func cmdScan(cfg *config.Config, args []string) error {
	fs := newFlags("scan", cfg)
	dur := fs.Duration("duration", 20*time.Second, "how long to scan")
	all := fs.Bool("all", false, "list all LE devices, not only Huawei scales")
	fs.Parse(args)
	_ = cfg.LoadKeys() // optional: only used to highlight the configured scale

	ctx, cancel := signalContext()
	defer cancel()
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

	fmt.Printf("Scanning on %s for %s (step on the scale to wake it up)...\n", adapter.Name, *dur)
	unwatch := bus.WatchAdded(func(path bluez.ObjectPath, ifaces map[string]map[string]bluez.Variant) {
		if p, ok := ifaces["org.bluez.Device1"]; ok {
			info := bluez.ParseDeviceProps(path, p)
			if ok, why := looksLikeScale(info, cfg.Address); ok {
				fmt.Printf("  found %s  %-24s rssi=%d  (%s)\n", info.Address, info.Name, info.RSSI, why)
			}
		}
	})
	defer unwatch()
	// Forget cached (unpaired) scale entries so a waking scale is reported
	// as a new device. Other devices are left alone.
	if devs, err := adapter.Devices(ctx); err == nil {
		for _, d := range devs {
			if ok, _ := looksLikeScale(d, cfg.Address); ok && !d.Connected && !d.Paired {
				_ = adapter.RemoveDevice(ctx, d.Path)
			}
		}
	}
	if err := adapter.StartDiscovery(ctx); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
	case <-time.After(*dur):
	}
	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	devs, derr := adapter.Devices(sctx)
	_ = adapter.StopDiscovery(sctx)
	scancel()
	if derr != nil {
		return derr
	}

	sort.Slice(devs, func(i, j int) bool { return devs[i].RSSI > devs[j].RSSI })
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "\nADDRESS\tNAME\tRSSI\tSCALE?")
	shown, scales := 0, 0
	for _, d := range devs {
		if !d.HasRSSI && !d.Connected {
			continue // cached, not seen in this scan
		}
		ok, why := looksLikeScale(d, cfg.Address)
		if ok {
			scales++
		}
		if !ok && !*all {
			continue
		}
		shown++
		name := d.Name
		if name == "" {
			name = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", d.Address, name, d.RSSI, why)
	}
	tw.Flush()
	if shown == 0 {
		fmt.Println("(none)")
	}
	fmt.Printf("\n%d device(s) seen, %d look like a Huawei scale.\n", len(devs), scales)
	if scales == 0 {
		fmt.Println("The scale only advertises while awake: step on it and scan again.")
	} else if cfg.Address == "" {
		fmt.Println("Put the scale's address into keys.json (\"address\") and run: sscale pair")
	}
	return nil
}

// ------------------------------------------------------------------ pair / once

func oneShot(cfg *config.Config, timeout time.Duration, mode daemon.Mode) (daemon.Outcome, error) {
	var out daemon.Outcome
	if err := loadConfig(cfg); err != nil {
		return out, err
	}
	log := logger(cfg)
	lock, err := store.AcquireLock(cfg.DataDir)
	if err != nil {
		if errors.Is(err, store.ErrLocked) {
			return out, daemon.ErrDaemonRunning
		}
		return out, err
	}
	defer lock.Release()
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return out, err
	}

	ctx, cancel := signalContext()
	defer cancel()
	a, err := app.Open(ctx, cfg, log)
	if err != nil {
		return out, err
	}
	defer a.Close()
	bus, err := bluez.Dial()
	if err != nil {
		return out, err
	}
	defer bus.Close()
	adapter, err := bus.Adapter(ctx, cfg.Adapter)
	if err != nil {
		return out, err
	}
	if err := adapter.EnsurePowered(ctx); err != nil {
		return out, err
	}
	dev := adapter.Device(cfg.Address)
	log.Info("waiting for the scale - step on it to wake it up", "address", cfg.Address, "timeout", timeout)
	if err := daemon.WaitForScale(ctx, bus, adapter, dev, timeout); err != nil {
		return out, err
	}
	log.Info("scale is awake")
	runner := &daemon.Runner{Cfg: cfg, Store: st, App: a, Log: log}
	return runner.Run(ctx, adapter, mode)
}

func cmdPair(cfg *config.Config, args []string) error {
	fs := newFlags("pair", cfg)
	timeout := fs.Duration("timeout", 2*time.Minute, "how long to wait for the scale to wake up")
	fs.Parse(args)
	out, err := oneShot(cfg, *timeout, daemon.Mode{ReadInfo: true})
	if err != nil {
		if daemon.IsAuthError(err) {
			return fmt.Errorf("%w\nThe scale rejected the keys in %s.", err, cfg.KeysPath)
		}
		return err
	}
	d := out.Device
	fmt.Println()
	fmt.Println("Paired successfully with", cfg.Address)
	fmt.Printf("  model:        %s\n", orDash(cfg.Model))
	fmt.Printf("  product info: %s\n", orDash(d.ProductInfo))
	fmt.Printf("  firmware:     %s\n", orDash(d.Firmware))
	fmt.Printf("  serial/info:  %s\n", orDash(d.Serial))
	fmt.Printf("  weight unit:  %s\n", orDash(d.Unit))
	fmt.Printf("  account HUID: %s\n", map[string]string{"scale": "taken from the scale's bound account", "local": "local (scale not bound to a Huawei account)"}[out.HUIDSource])
	fmt.Println("\nStart the daemon with: sscale   (or ./sscale start when using Docker)")
	return nil
}

func cmdOnce(cfg *config.Config, args []string) error {
	fs := newFlags("once", cfg)
	timeout := fs.Duration("timeout", 10*time.Minute, "how long to wait for the scale to wake up")
	asJSON := fs.Bool("json", false, "print the captured records as JSON")
	fs.Parse(args)
	if *asJSON {
		cfg.LogLevel = "error"
	}
	out, err := oneShot(cfg, *timeout, daemon.Mode{Measure: true})
	if *asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(out.New)
	} else if err == nil && len(out.New) == 0 {
		fmt.Println("No new measurement (already recorded or nobody stepped on the scale).")
	}
	return err
}

// ------------------------------------------------------------------ status

func cmdStatus(cfg *config.Config, args []string) error {
	fs := newFlags("status", cfg)
	asJSON := fs.Bool("json", false, "print raw state as JSON")
	fs.Parse(args)
	_ = cfg.LoadKeys()

	st, err := store.LoadState(cfg.DataDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	running := store.IsLocked(cfg.DataDir) && !st.Daemon.Heartbeat.IsZero()
	ctx := context.Background()
	a, err := app.Open(ctx, cfg, logx.New(io.Discard, "error"))
	if err != nil {
		return err
	}
	defer a.Close()
	total, valid, err := a.DB.Count(ctx)
	if err != nil {
		return err
	}
	latest, err := a.Latest(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"daemon_running": running, "state": st,
			"measurements": map[string]int{"total": total, "valid": valid}, "latest": latest})
	}

	now := time.Now()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer w.Flush()
	if running {
		fmt.Fprintf(w, "Daemon:\trunning (pid %d, since %s, %s, heartbeat %s ago)\n", st.Daemon.PID,
			st.Daemon.StartedAt.Format(time.DateTime), st.Daemon.Phase, ago(now, st.Daemon.Heartbeat))
	} else if store.IsLocked(cfg.DataDir) {
		fmt.Fprintf(w, "Daemon:\tbusy (a pair/once command is running)\n")
	} else {
		fmt.Fprintf(w, "Daemon:\tnot running\n")
	}
	if st.Daemon.Adapter != "" {
		fmt.Fprintf(w, "Adapter:\t%s\n", st.Daemon.Adapter)
	}
	addr := cfg.Address
	if addr == "" {
		addr = st.Scale.Address
	}
	fmt.Fprintf(w, "Scale:\t%s %s\n", orDash(addr), paren(cfg.Model))
	if st.Pairing.Paired {
		d := st.Pairing.Device
		fmt.Fprintf(w, "Paired:\tyes, since %s (firmware %s, product %s, unit %s)\n", fmtTime(st.Pairing.PairedAt),
			orDash(d.Firmware), orDash(d.ProductInfo), orDash(d.Unit))
	} else {
		fmt.Fprintf(w, "Paired:\tno - run: sscale pair\n")
	}
	if st.Scale.LastSeen != nil {
		state := "asleep"
		if running && st.Scale.Awake {
			state = "awake"
		}
		fmt.Fprintf(w, "Scale state:\t%s, last seen %s (%s ago, RSSI %d)\n", state, st.Scale.LastSeen.Format(time.DateTime),
			ago(now, *st.Scale.LastSeen), st.Scale.RSSI)
	} else {
		fmt.Fprintf(w, "Scale state:\tnever seen\n")
	}
	if s := st.LastSession; s != nil {
		line := fmt.Sprintf("%s, %s, %d new record(s), took %s", s.Start.Format(time.DateTime), s.Result, s.Records,
			s.End.Sub(s.Start).Round(time.Second))
		if s.Error != "" {
			line += " - " + s.Error
		}
		fmt.Fprintf(w, "Last session:\t%s\n", line)
	}
	fmt.Fprintf(w, "Sessions:\t%d\n", st.Sessions)
	fmt.Fprintf(w, "Measurements:\t%d recorded, %d filtered out as not yours\n", total, total-valid)
	if latest != nil {
		fmt.Fprintf(w, "Last measurement:\t%s\n", summary(latest))
	}
	return nil
}

func summary(m *app.View) string {
	s := fmt.Sprintf("%s  %.2f kg", m.Time.Format(time.DateTime), m.WeightKg)
	if m.BMI > 0 {
		s += fmt.Sprintf(", BMI %.1f", m.BMI)
	}
	if c := m.Composition; c != nil {
		s += fmt.Sprintf(", fat %.1f%%, water %.1f%%, muscle %.1f kg, bone %.2f kg, protein %.1f%%, BMR %d kcal, visceral %d",
			c.BodyFatPercent, c.WaterPercent, c.SkeletalMuscleKg, c.BoneMineralKg, c.ProteinPercent, c.BMRKcal, c.VisceralFatLevel)
	}
	if m.HeartRateBPM > 0 {
		s += fmt.Sprintf(", HR %d bpm", m.HeartRateBPM)
	}
	return s + " (" + m.Source + ")"
}

// ------------------------------------------------------------------ history

func cmdHistory(cfg *config.Config, args []string) error {
	fs := newFlags("history", cfg)
	n := fs.Int("n", 20, "number of records (0 = all)")
	asJSON := fs.Bool("json", false, "print JSON lines")
	all := fs.Bool("all", false, "include readings filtered out as not yours")
	fs.Parse(args)
	ctx := context.Background()
	_ = cfg.LoadKeys()
	a, err := app.Open(ctx, cfg, logx.New(io.Discard, "error"))
	if err != nil {
		return err
	}
	defer a.Close()
	views, err := a.List(ctx, time.Time{}, time.Time{})
	if err != nil {
		return err
	}
	var recs []app.View
	for _, v := range views {
		if v.Valid || *all {
			recs = append(recs, v)
		}
	}
	if *n > 0 && len(recs) > *n {
		recs = recs[len(recs)-*n:]
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		for _, r := range recs {
			_ = enc.Encode(r)
		}
		return nil
	}
	if len(recs) == 0 {
		fmt.Println("No measurements recorded yet.")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "ID\tTIME\tWEIGHT kg\tBMI\tFAT %\tWATER %\tMUSCLE kg\tBONE kg\tBMR\tVISC\tHR\tIMP Ω\tSOURCE\tVALID\t")
	for _, r := range recs {
		fat, water, muscle, bone, bmr, visc := "-", "-", "-", "-", "-", "-"
		if c := r.Composition; c != nil {
			fat, water = f1(c.BodyFatPercent), f1(c.WaterPercent)
			muscle, bone = f1(c.SkeletalMuscleKg), fmt.Sprintf("%.2f", c.BoneMineralKg)
			bmr, visc = fmt.Sprint(c.BMRKcal), fmt.Sprint(c.VisceralFatLevel)
		}
		hr := "-"
		if r.HeartRateBPM > 0 {
			hr = fmt.Sprint(r.HeartRateBPM)
		}
		imp := "-"
		if r.ImpedanceOhm > 0 {
			imp = f1(r.ImpedanceOhm)
		}
		ok := "yes"
		if !r.Valid {
			ok = "no"
		}
		if r.ManualValid != nil {
			ok += " (manual)"
		}
		fmt.Fprintf(tw, "%d\t%s\t%.2f\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t\n", r.ID, r.Time.Format(time.DateTime), r.WeightKg,
			f1(r.BMI), fat, water, muscle, bone, bmr, visc, hr, imp, r.Source, ok)
	}
	return tw.Flush()
}

// ------------------------------------------------------------------ stop

func cmdStop(cfg *config.Config, args []string) error {
	fs := newFlags("stop", cfg)
	timeout := fs.Duration("timeout", 20*time.Second, "how long to wait for the daemon to exit")
	fs.Parse(args)
	if !store.IsLocked(cfg.DataDir) {
		fmt.Println("sscale daemon is not running")
		return nil
	}
	pid, err := store.ReadPID(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("daemon is running but its pid file is unreadable: %w", err)
	}
	if pid == os.Getpid() || !isSscale(pid) {
		return fmt.Errorf("daemon pid %d is not visible from here (it runs in another container?) - use \"./sscale stop\" on the host or \"docker stop sscale\"", pid)
	}
	if pid == 1 {
		fmt.Println("Note: the daemon is this container's main process; with restart=unless-stopped Docker will start it again. Use \"./sscale stop\" on the host to stop it for good.")
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("signal daemon (pid %d): %w", pid, err)
	}
	fmt.Printf("Stopping sscale daemon (pid %d)...", pid)
	deadline := time.Now().Add(*timeout)
	for time.Now().Before(deadline) {
		if !store.IsLocked(cfg.DataDir) {
			fmt.Println(" stopped.")
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	fmt.Println()
	return fmt.Errorf("daemon did not stop within %s", *timeout)
}

func isSscale(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false
	}
	return strings.Contains(string(b), "sscale")
}

// ------------------------------------------------------------------ helpers

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func paren(s string) string {
	if s == "" {
		return ""
	}
	return "(" + s + ")"
}

func f1(v float64) string {
	if v == 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f", v)
}

func fmtTime(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.Format(time.DateTime)
}

func ago(now, t time.Time) string {
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return d.Round(time.Second).String()
	case d < time.Hour:
		return d.Round(time.Minute).String()
	default:
		return d.Round(time.Hour).String()
	}
}
