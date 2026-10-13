package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/unimpl/Iota/internal/app"
	"github.com/unimpl/Iota/internal/server"
	"github.com/unimpl/Iota/web"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	config, err := app.LoadConfig()
	if err != nil {
		return err
	}
	settings, err := app.SettingsFromConfig(config)
	if err != nil {
		return err
	}
	if err := app.ApplyEnvironment(&settings, os.LookupEnv); err != nil {
		return err
	}
	listen := flag.String("listen", "127.0.0.1:9281", "HTTP listening address")
	origin := flag.String("origin", "", "public origin when served through an HTTPS gateway")
	staticDir := flag.String("web-dir", "", "serve web assets from this directory instead of the embedded client")
	flag.StringVar(&settings.Model, "model", settings.Model, "model name")
	flag.StringVar(&settings.BaseURL, "base-url", settings.BaseURL, "OpenAI-compatible API root")
	flag.StringVar(&settings.CWD, "cwd", settings.CWD, "default project directory for new sessions")
	flag.StringVar(&settings.System, "system", settings.System, "additional system prompt")
	flag.StringVar(&settings.Tools, "tools", settings.Tools, "comma-separated tools, or none")
	flag.IntVar(&settings.MaxTurns, "max-turns", settings.MaxTurns, "maximum model turns per run")
	flag.DurationVar(&settings.Timeout, "timeout", settings.Timeout, "model response idle timeout and bash duration limit")
	flag.StringVar(&settings.Mode, "mode", settings.Mode, "initial collaboration mode")
	flag.Parse()
	if flag.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	flag.Visit(func(item *flag.Flag) {
		if item.Name == "mode" {
			settings.ModeRequested = true
		}
	})
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	var static fs.FS = web.Files()
	if *staticDir != "" {
		static = os.DirFS(*staticDir)
	}
	agentServer, err := server.New(server.Config{Settings: settings, Home: home, PublicOrigin: *origin, Static: static})
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		agentServer.Close()
		return err
	}
	httpServer := &http.Server{Handler: agentServer.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- httpServer.Serve(listener) }()
	fmt.Printf("Iota: http://%s/\n", listener.Addr())
	select {
	case err := <-done:
		return errors.Join(err, agentServer.Close())
	case <-ctx.Done():
		closeErr := agentServer.Close()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := httpServer.Shutdown(shutdownCtx)
		if err != nil {
			httpServer.Close()
		}
		serveErr := <-done
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		return errors.Join(closeErr, err, serveErr)
	}
}
