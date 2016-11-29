package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"

	"github.com/eBay/fabio/admin"
	"github.com/eBay/fabio/config"
	"github.com/eBay/fabio/exit"
	"github.com/eBay/fabio/metrics"
	"github.com/eBay/fabio/proxy"
	"github.com/eBay/fabio/registry"
	"github.com/eBay/fabio/registry/consul"
	"github.com/eBay/fabio/registry/file"
	"github.com/eBay/fabio/registry/static"
	"github.com/eBay/fabio/route"
	dmp "github.com/sergi/go-diff/diffmatchpatch"
)

// version contains the version number
//
// It is set by build/release.sh for tagged releases
// so that 'go get' just works.
//
// It is also set by the linker when fabio
// is built via the Makefile or the build/docker.sh
// script to ensure the correct version nubmer
var version = "1.3.7"

func main() {
	cfg, err := config.Load(os.Args, os.Environ())
	if err != nil {
		exit.Fatalf("[FATAL] %s. %s", version, err)
	}
	if cfg == nil {
		fmt.Println(version)
		return
	}

	listeners := make([]net.Listener, len(cfg.Listen))
	if cfg.Proxy.Username != "" {
		if os.Getuid() == 0 {
			drop(cfg)
			return
		}

		for i, l := range cfg.Listen {
			fd := uintptr(3 + i)
			ln, err := net.FileListener(os.NewFile(fd, "fabio-"+l.Addr))
			if err != nil {
				log.Fatalf("[FATAL] Failed to open socket %d for %s. %s", fd, l.Addr, err)
			}
			listeners[i] = ln
		}
	}

	u, err := user.Current()
	if err != nil {
		log.Fatalf("[FATAL] Cannot determine current user. %s", err)
	}

	log.Printf("[INFO] Runtime config\n" + toJSON(cfg))
	log.Printf("[INFO] Version %s starting", version)
	log.Printf("[INFO] Go runtime is %s", runtime.Version())
	log.Printf("[INFO] Running as user %s uid=%s gid=%s", u.Username, u.Uid, u.Gid)

	exit.Listen(func(s os.Signal) {
		if registry.Default == nil {
			return
		}
		registry.Default.Deregister()
	})

	// init metrics early since that create the global metric registries
	// that are used by other parts of the code.
	initMetrics(cfg)

	initRuntime(cfg)
	initBackend(cfg)
	go watchBackend(cfg)
	startAdmin(cfg)

	// create proxies after metrics since they use the metrics registry.
	httpProxy := newHTTPProxy(cfg)
	tcpProxy := proxy.NewTCPSNIProxy(cfg.Proxy)
	startListeners(cfg.Listen, cfg.Proxy.ShutdownWait, httpProxy, tcpProxy, listeners)
	exit.Wait()
}

// drop spawns all the listeners and then drops privileges by
// starting fabio as a different user.
func drop(cfg *config.Config) {
	log.Printf("[INFO] root: fabio started as root")

	u, err := user.Lookup(cfg.Proxy.Username)
	if err != nil {
		log.Fatalf("[FATAL] root: Unknown user %s. %s", cfg.Proxy.Username, err)
	}

	listen := func(addr string) (*os.File, error) {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, err
		}
		return ln.(*net.TCPListener).File()
	}

	// open sockets and then then re-exec as different user
	var sockets []*os.File
	for _, l := range cfg.Listen {
		f, err := listen(l.Addr)
		if err != nil {
			for _, f := range sockets {
				f.Close()
			}
			log.Fatalf("[FATAL] root: Cannot listen on %s. %s", l.Addr, err)
		}
		sockets = append(sockets, f)
		log.Printf("[INFO] root: Listening on %s", l.Addr)
	}

	atoi := func(s string) uint32 {
		n, _ := strconv.Atoi(s)
		return uint32(n)
	}

	cmd := exec.Command(os.Args[0], os.Args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = sockets
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{
			Uid: atoi(u.Uid),
			Gid: atoi(u.Gid),
		},
		Setsid: true,
	}

	if err := cmd.Start(); err != nil {
		log.Fatalf("[FATAL] root: Failed to start fabio as user %s", cfg.Proxy.Username)
	}
	log.Printf("[INFO] root: Started fabio as user %s. Exiting", cfg.Proxy.Username)

	cmd.Process.Release()
	os.Exit(0)
}

func newHTTPProxy(cfg *config.Config) http.Handler {
	if err := route.SetPickerStrategy(cfg.Proxy.Strategy); err != nil {
		exit.Fatal("[FATAL] ", err)
	}
	log.Printf("[INFO] Using routing strategy %q", cfg.Proxy.Strategy)

	if err := route.SetMatcher(cfg.Proxy.Matcher); err != nil {
		exit.Fatal("[FATAL] ", err)
	}
	log.Printf("[INFO] Using routing matching %q", cfg.Proxy.Matcher)

	tr := &http.Transport{
		ResponseHeaderTimeout: cfg.Proxy.ResponseHeaderTimeout,
		MaxIdleConnsPerHost:   cfg.Proxy.MaxConn,
		Dial: (&net.Dialer{
			Timeout:   cfg.Proxy.DialTimeout,
			KeepAlive: cfg.Proxy.KeepAliveTimeout,
		}).Dial,
	}

	return proxy.NewHTTPProxy(tr, cfg.Proxy)
}

func startAdmin(cfg *config.Config) {
	log.Printf("[INFO] Admin server listening on %q", cfg.UI.Addr)
	go func() {
		srv := &admin.Server{
			Color:    cfg.UI.Color,
			Title:    cfg.UI.Title,
			Version:  version,
			Commands: route.Commands,
			Cfg:      cfg,
		}
		if err := srv.ListenAndServe(cfg.UI.Addr); err != nil {
			exit.Fatal("[FATAL] ui: ", err)
		}
	}()
}

func initMetrics(cfg *config.Config) {
	if cfg.Metrics.Target == "" {
		log.Printf("[INFO] Metrics disabled")
		return
	}

	var err error
	if metrics.DefaultRegistry, err = metrics.NewRegistry(cfg.Metrics); err != nil {
		exit.Fatal("[FATAL] ", err)
	}
	if route.ServiceRegistry, err = metrics.NewRegistry(cfg.Metrics); err != nil {
		exit.Fatal("[FATAL] ", err)
	}
}

func initRuntime(cfg *config.Config) {
	if os.Getenv("GOGC") == "" {
		log.Print("[INFO] Setting GOGC=", cfg.Runtime.GOGC)
		debug.SetGCPercent(cfg.Runtime.GOGC)
	} else {
		log.Print("[INFO] Using GOGC=", os.Getenv("GOGC"), " from env")
	}

	if os.Getenv("GOMAXPROCS") == "" {
		log.Print("[INFO] Setting GOMAXPROCS=", cfg.Runtime.GOMAXPROCS)
		runtime.GOMAXPROCS(cfg.Runtime.GOMAXPROCS)
	} else {
		log.Print("[INFO] Using GOMAXPROCS=", os.Getenv("GOMAXPROCS"), " from env")
	}
}

func initBackend(cfg *config.Config) {
	var err error

	switch cfg.Registry.Backend {
	case "file":
		registry.Default, err = file.NewBackend(cfg.Registry.File.Path)
	case "static":
		registry.Default, err = static.NewBackend(cfg.Registry.Static.Routes)
	case "consul":
		registry.Default, err = consul.NewBackend(&cfg.Registry.Consul)
	default:
		exit.Fatal("[FATAL] Unknown registry backend ", cfg.Registry.Backend)
	}

	if err != nil {
		exit.Fatal("[FATAL] Error initializing backend. ", err)
	}
	if err := registry.Default.Register(); err != nil {
		exit.Fatal("[FATAL] Error registering backend. ", err)
	}
}

func watchBackend(cfg *config.Config) {
	var (
		last   string
		svccfg string
		mancfg string
	)

	svc := registry.Default.WatchServices()
	man := registry.Default.WatchManual()

	for {
		select {
		case svccfg = <-svc:
		case mancfg = <-man:
		}

		// manual config overrides service config
		// order matters
		next := svccfg + "\n" + mancfg
		if next == last {
			continue
		}

		t, err := route.NewTable(next)
		if err != nil {
			log.Printf("[WARN] %s", err)
			continue
		}
		route.SetTable(t)
		logRoutes(last, next, cfg.Proxy.LogRoutes)
		last = next
	}
}

func logRoutes(last, next, format string) {
	fmtDiff := func(diffs []dmp.Diff) string {
		var b bytes.Buffer
		for _, d := range diffs {
			t := strings.TrimSpace(d.Text)
			if t == "" {
				continue
			}
			switch d.Type {
			case dmp.DiffDelete:
				b.WriteString("- ")
				b.WriteString(strings.Replace(t, "\n", "\n- ", -1))
			case dmp.DiffInsert:
				b.WriteString("+ ")
				b.WriteString(strings.Replace(t, "\n", "\n+ ", -1))
			}
		}
		return b.String()
	}

	const defFormat = "delta"
	switch format {
	case "delta":
		if delta := fmtDiff(dmp.New().DiffMain(last, next, true)); delta != "" {
			log.Printf("[INFO] Config updates\n%s", delta)
		}

	case "all":
		log.Printf("[INFO] Updated config to\n%s", next)

	default:
		log.Printf("[WARN] Invalid route format %q. Defaulting to %q", format, defFormat)
		logRoutes(last, next, defFormat)
	}
}

func toJSON(v interface{}) string {
	data, err := json.MarshalIndent(v, "", "    ")
	if err != nil {
		panic("json: " + err.Error())
	}
	return string(data)
}
