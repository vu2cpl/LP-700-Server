package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"lp700-server/internal/config"
	"lp700-server/internal/hub"
	"lp700-server/internal/lpmeter"
	"lp700-server/internal/web"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "probe" {
		os.Exit(runProbeSubcommand(os.Args[2:]))
	}

	configPath := flag.String("config", "/etc/lp700-server/config.toml", "config file path (missing file is fine; defaults are used)")
	backendOverride := flag.String("backend", "", "override meter.backend: hid | simulator | auto (defaults to config)")
	verbose := flag.Bool("v", false, "verbose logging at start (debug-level); the level can also be changed at runtime via /api/log-level or the web UI's Setup overlay")
	flag.Parse()

	var levelVar slog.LevelVar
	levelVar.Set(slog.LevelError)
	if *verbose {
		levelVar.Set(slog.LevelDebug)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: &levelVar}))

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("config", "err", err)
		os.Exit(1)
	}
	if *backendOverride != "" {
		cfg.Meter.Backend = *backendOverride
	}

	backend := resolveBackend(cfg.Meter.Backend, logger)
	logger.Info("config loaded",
		"path", *configPath,
		"backend", backend,
		"vendor_id", fmt.Sprintf("0x%04x", cfg.Meter.VendorID),
		"product_id", fmt.Sprintf("0x%04x", cfg.Meter.ProductID),
		"poll_ms", cfg.Meter.PollMs,
		"listen", cfg.Server.Listen,
	)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	snapCh := make(chan lpmeter.Snapshot, 8)
	// Sample-buffer channels: buffer one full assembly per type. If
	// the hub falls behind, the owner drops to the next assembly
	// rather than blocking the meter loop.
	scopeCh := make(chan lpmeter.ScopeFrame, 2)
	spectrumCh := make(chan lpmeter.SpectrumFrame, 2)
	pollEvery := time.Duration(cfg.Meter.PollMs) * time.Millisecond

	var source lpmeter.Source
	switch backend {
	case lpmeter.BackendHID:
		source = lpmeter.NewHIDOwner(cfg.Meter.VendorID, cfg.Meter.ProductID, pollEvery, snapCh, scopeCh, spectrumCh, logger)
	case lpmeter.BackendSimulator:
		// The simulator does not synthesize scope/spectrum buffers
		// yet; the hub's nil-channel handling means no frames of
		// those types will be broadcast under the simulator backend.
		source = lpmeter.NewSimulator(pollEvery, snapCh, logger)
	default:
		logger.Error("unknown backend", "backend", backend)
		os.Exit(1)
	}

	h := hub.NewHub(snapCh, scopeCh, spectrumCh, source, hub.Options{
		Heartbeat:    time.Duration(cfg.Server.HeartbeatMs) * time.Millisecond,
		MaxClients:   cfg.Server.MaxClients,
		AllowControl: cfg.Server.AllowControl,
	}, logger)

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", h.ServeWS)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"backend":       string(backend),
			"allow_control": cfg.Server.AllowControl,
			"title":         cfg.UI.Title,
		})
	})
	mux.HandleFunc("/api/log-level", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]string{"level": slogLevelName(levelVar.Level())})
		case http.MethodPost:
			var req struct {
				Level string `json:"level"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, `{"error":"bad json"}`, http.StatusBadRequest)
				return
			}
			lvl, ok := parseSlogLevel(req.Level)
			if !ok {
				http.Error(w, `{"error":"level must be one of: error, warn, info, debug"}`, http.StatusBadRequest)
				return
			}
			prev := levelVar.Level()
			levelVar.Set(lvl)
			logger.Error("log level changed", "from", slogLevelName(prev), "to", slogLevelName(lvl), "remote", r.RemoteAddr)
			_ = json.NewEncoder(w).Encode(map[string]string{"level": slogLevelName(lvl)})
		default:
			w.Header().Set("Allow", "GET, POST")
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	})
	mux.Handle("/", web.StaticHandler())

	srv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go source.Run(ctx)
	go h.Run(ctx)

	go func() {
		logger.Info("http listening", "addr", cfg.Server.Listen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server", "err", err)
			cancel()
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")
	shutCtx, sc := context.WithTimeout(context.Background(), 5*time.Second)
	defer sc()
	_ = srv.Shutdown(shutCtx)
}

func resolveBackend(name string, logger *slog.Logger) lpmeter.Backend {
	switch name {
	case "hid":
		return lpmeter.BackendHID
	case "simulator":
		return lpmeter.BackendSimulator
	case "", "auto":
		if lpmeter.HasLPMeterAttached() {
			return lpmeter.BackendHID
		}
		logger.Warn("auto backend: no LP-500/700 attached, falling back to simulator")
		return lpmeter.BackendSimulator
	}
	return lpmeter.BackendSimulator
}

func runProbeSubcommand(args []string) int {
	fs := flag.NewFlagSet("probe", flag.ExitOnError)
	list := fs.Bool("list", false, "list every HID device, marking those whose product string matches LP-500/700")
	dump := fs.Bool("dump", false, "open the matched LP-500/700 and print every IN report (raw + best-effort decode) until ^C")
	capture := fs.String("capture", "", "open the matched LP-500/700 and write IN reports to this path until duration elapses")
	duration := fs.Duration("duration", 0, "for -capture, how long to record (0 = until ^C)")
	samples := fs.Bool("samples", false, "cycle OUT cmds '1'..'5' and dump the secondary slot (bytes 40..63) — reverse-engineering aid for scope/spec buffers")
	cycleModes := fs.Bool("cycle-modes", false, "for -samples: mode_step through power_swr / waveform / spectrum and repeat the cmd sweep in each")
	framesPerCmd := fs.Int("frames-per-cmd", 16, "for -samples: number of IN frames captured per OUT cmd")
	targetChannel := fs.Int("channel", 0, "for -samples: channel_step until on manual channel N (1..4) before the sweep; 0 = leave as-is")
	targetRange := fs.String("range", "", "for -samples: range_step until on this range (e.g. 100W, 1K, auto) before the sweep; requires -channel set or already-manual channel")
	vid := fs.Uint("vid", 0, "match this vendor id (hex, e.g. 0x0000); 0 = match by product string")
	pid := fs.Uint("pid", 0, "match this product id; 0 = match by product string")
	fs.Parse(args)

	mode := lpmeter.ProbeMode("")
	switch {
	case *list:
		mode = lpmeter.ProbeList
	case *dump:
		mode = lpmeter.ProbeDump
	case *capture != "":
		mode = lpmeter.ProbeCapture
	case *samples:
		mode = lpmeter.ProbeSamples
	default:
		fmt.Fprintln(os.Stderr, "usage: lp700-server probe [-list | -dump | -capture <path> [-duration 10s] | -samples [-cycle-modes] [-frames-per-cmd N] [-channel N] [-range NAME]] [-vid 0xNNNN -pid 0xNNNN]")
		return 2
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	err := lpmeter.RunProbe(ctx, lpmeter.ProbeOptions{
		Mode:          mode,
		OutPath:       *capture,
		Duration:      *duration,
		VendorID:      uint16(*vid),
		ProductID:     uint16(*pid),
		FramesPerCmd:  *framesPerCmd,
		CycleModes:    *cycleModes,
		TargetChannel: *targetChannel,
		TargetRange:   *targetRange,
	}, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "probe:", err)
		return 1
	}
	return 0
}

func slogLevelName(l slog.Level) string {
	switch {
	case l <= slog.LevelDebug:
		return "debug"
	case l <= slog.LevelInfo:
		return "info"
	case l <= slog.LevelWarn:
		return "warn"
	default:
		return "error"
	}
}

func parseSlogLevel(s string) (slog.Level, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, true
	case "info":
		return slog.LevelInfo, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "error", "err":
		return slog.LevelError, true
	}
	return 0, false
}
