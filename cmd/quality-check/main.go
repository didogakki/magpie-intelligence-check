package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/gofrs/flock"
	"github.com/mu-zi-lee/magpie-intelligence-check/internal/check"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func main() {
	if handled, err := subcommand(os.Args[1:]); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	dir := flag.String("data", "data", "private data directory")
	gate := flag.String("gate", "plugin/gate.middleware.js", "middleware entrypoint")
	listen := flag.String("listen", "127.0.0.1:0", "loopback address")
	cloudflared := flag.String("cloudflared", "", "official cloudflared executable")
	owned := flag.Bool("owned", false, "stop when parent input closes")
	flag.Parse()
	if err := run(*dir, *gate, *listen, *cloudflared, *owned); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// subcommand runs the maintenance commands; the plugin host starts the plain server.
func subcommand(args []string) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	switch args[0] {
	case "export-legacy-seed":
		return true, exportLegacySeed(args[1:])
	case "import-trace-seed":
		return true, importTraceSeed(args[1:])
	}
	return false, nil
}

// exportLegacySeed reads the old collector's state (read only) and writes a seed file.
func exportLegacySeed(args []string) error {
	fs := flag.NewFlagSet("export-legacy-seed", flag.ContinueOnError)
	home, _ := os.UserHomeDir()
	state := fs.String("legacy-state", filepath.Join(home, ".local/state/magpie-modeltrace"), "old collector state directory (never modified)")
	models := fs.String("models", "", "comma separated model IDs of this plugin, e.g. magpie-252-ca704414/codex/gpt-6-astra")
	status := fs.String("status-md", "", "optional old status.md to compare against (display check only)")
	out := fs.String("out", "", "seed file to write (default stdout)")
	snapshot := fs.Bool("legacy-snapshot", false, "opt in to refreshing the old snapshot.json for the old widget; the seed then holds the old account identifiers and needs -out (written 0600)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *models == "" {
		return fmt.Errorf("-models is required")
	}
	export := check.ExportLegacySeed
	if *snapshot {
		if *out == "" {
			return fmt.Errorf("-legacy-snapshot puts old account identifiers in the seed: use -out, never stdout")
		}
		if abs, err := filepath.Abs(*state); err == nil {
			*state = abs
		}
		export = check.ExportLegacySeedWithSnapshot
	}
	seed, warnings, err := export(*state, strings.Split(*models, ","), *status, time.Now())
	if err != nil {
		return err
	}
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	b, _ := json.MarshalIndent(seed, "", "  ")
	b = append(b, '\n')
	if *out == "" {
		_, err = os.Stdout.Write(b)
		return err
	}
	return check.AtomicWrite(*out, b)
}

// importTraceSeed merges a seed file into the data directory. It is a dry run unless
// -apply is given, takes the data directory lock (so the service must not be running),
// is idempotent, and does not enable the schedule.
func importTraceSeed(args []string) error {
	fs := flag.NewFlagSet("import-trace-seed", flag.ContinueOnError)
	dir := fs.String("data", "data", "private data directory")
	file := fs.String("seed", "", "seed file written by export-legacy-seed")
	apply := fs.Bool("apply", false, "write the state (default: dry run)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	raw, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	var seed check.TraceSeed
	if err = json.Unmarshal(raw, &seed); err != nil {
		return err
	}
	if seed.LegacySnapshot != nil {
		if st, err := os.Stat(*file); err != nil || st.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("种子含有旧账号标识，文件权限必须是 0600：%s", *file)
		}
	}
	if err = os.MkdirAll(*dir, 0700); err != nil {
		return err
	}
	lock := flock.New(filepath.Join(*dir, "service.lock"))
	locked, err := lock.TryLock()
	if err != nil {
		return err
	}
	if !locked {
		return fmt.Errorf("此数据目录已有检测服务运行：请先停止它，或在控制台使用 POST /api/trace-seed")
	}
	defer lock.Unlock()
	store, err := check.OpenStore(filepath.Join(*dir, "check.db"))
	if err != nil {
		return err
	}
	defer store.Close()
	var state check.State
	if err = store.Load(&state); err != nil {
		return err
	}
	report, err := check.ApplyTraceSeed(&state, seed, time.Now(), *apply)
	if err != nil {
		return err
	}
	if *apply {
		if err = store.Save(state); err != nil {
			return err
		}
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"applied": *apply, "entries": report, "legacy_snapshot": check.LegacySnapshotReport(seed)})
}

func run(dir, gate, addr, binary string, owned bool) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	lock := flock.New(filepath.Join(dir, "service.lock"))
	locked, err := lock.TryLock()
	if err != nil {
		return err
	}
	if !locked {
		return fmt.Errorf("此数据目录已有检测服务运行，请关闭另一个 Magpie 实例或使用独立 dataDir")
	}
	defer lock.Unlock()
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("management listener must be loopback")
	}
	abs, err := filepath.Abs(gate)
	if err != nil {
		return err
	}
	s, err := check.New(dir, abs)
	if err != nil {
		return err
	}
	defer s.Close()
	s.WatchLog()
	s.StartTraceSchedule()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	token := check.Secret()
	origin := "http://" + ln.Addr().String()
	access := check.NewAccess(token, origin)
	console := check.NewConsole(access, origin, dir, binary)
	defer console.Close()
	server := &http.Server{Handler: s.ManagedHandler(access, console), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	json.NewEncoder(os.Stdout).Encode(map[string]string{"url": origin + "/open/" + access.Ticket(), "origin": origin, "control": token})
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)
	done := make(chan error, 1)
	parentClosed := make(chan struct{})
	if owned {
		go func() { io.Copy(io.Discard, os.Stdin); close(parentClosed) }()
	}
	go func() { done <- server.Serve(ln) }()
	select {
	case err := <-done:
		if err != http.ErrServerClosed {
			return err
		}
	case <-stop:
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(ctx)
	case <-parentClosed:
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(ctx)
	}
	return nil
}
