package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"github.com/ricardo-duarte-av/matrix-archivist/internal/archiver"
	"github.com/ricardo-duarte-av/matrix-archivist/internal/bot"
	"github.com/ricardo-duarte-av/matrix-archivist/internal/config"
	"github.com/ricardo-duarte-av/matrix-archivist/internal/store"
	"github.com/ricardo-duarte-av/matrix-archivist/internal/web"
)

var version = "dev"

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to config.yaml")
	showVersion := flag.Bool("version", false, "print version and exit")
	healthcheck := flag.Bool("healthcheck", false, "check the running instance's HTTP server and exit (for Docker HEALTHCHECK)")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	if *healthcheck {
		os.Exit(runHealthcheck(*cfgPath))
	}

	noColor := true
	if st, err := os.Stderr.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 {
		noColor = false
	}
	log := zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.DateTime, NoColor: noColor}).With().Timestamp().Logger()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to load config")
	}
	if lvl, err := zerolog.ParseLevel(cfg.LogLevel); err == nil {
		log = log.Level(lvl)
	}
	if _, err := exec.LookPath("git"); err != nil {
		log.Fatal().Msg("git binary not found in PATH")
	}
	if err := os.MkdirAll(cfg.StorageDir, 0o755); err != nil {
		log.Fatal().Err(err).Msg("Failed to create storage dir")
	}

	st, err := store.Open(cfg.Database)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to open database")
	}
	defer st.Close()

	arch := archiver.New(cfg, st, log.With().Str("component", "archiver").Logger())
	b, err := bot.New(cfg, st, arch, log.With().Str("component", "bot").Logger())
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to create bot")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info().Str("version", version).Str("room", cfg.Matrix.RoomID).Msg("Starting matrix-archivist")
	go arch.Run(ctx)
	if cfg.HTTP.Enabled {
		srv, err := web.New(cfg.HTTP, cfg.StorageDir, log.With().Str("component", "http").Logger())
		if err != nil {
			log.Fatal().Err(err).Msg("Failed to create HTTP server")
		}
		srv.Metrics = arch.WriteMetrics
		go func() {
			if err := srv.Run(ctx); err != nil {
				log.Error().Err(err).Msg("HTTP server stopped")
			}
		}()
	}
	if err := b.Run(ctx); err != nil {
		log.Error().Err(err).Msg("Bot stopped with error")
		stop()
		os.Exit(1)
	}
	log.Info().Msg("Shutting down")
}

func runHealthcheck(cfgPath string) int {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if !cfg.HTTP.Enabled {
		return 0
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", cfg.HTTP.Port))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, resp.Status)
		return 1
	}
	return 0
}
