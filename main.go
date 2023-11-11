package main

import (
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
)

var (
	confPath = flag.String("conf", "config.json", "config path")
)

type App struct {
	cfg     *Config
	logger  *slog.Logger
	manager *Manager
	tunnel  *Tunnel
}

func initApp() *App {
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	cfg := NewConfig(*confPath)
	hostKeeper := NewHostsKeeper(cfg, logger.With(slog.String("module", "hostskeeper")))
	resolver := NewResolver()

	app := &App{
		cfg:     cfg,
		logger:  logger.With(slog.String("module", "app")),
		manager: NewManager(cfg, logger.With(slog.String("module", "manager")), hostKeeper),
		tunnel:  NewTunnel(cfg, logger.With(slog.String("module", "tunnel")), resolver),
	}
	hostKeeper.Watch(resolver.Set)
	hostKeeper.Watch(func(Hosts) {
		app.tunnel.Reload()
	})

	return app
}

func (a *App) Start() {
	a.protectRun(a.manager.Start)
	a.protectRun(a.tunnel.Start)
}

func (a *App) Wait() {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)
	sig := <-sigs

	a.logger.Warn("quit", slog.String("signal", sig.String()))
}

func (a *App) Stop() {
	a.manager.Stop()
	a.tunnel.Stop()
}

func (a *App) protectRun(fn func()) {
	go func() {
		e := recover()
		if e != nil {
			a.logger.Error("panic", slog.Any("err", e),
				slog.String("stack", string(debug.Stack())))
		}

		fn()
	}()
}

func main() {
	app := initApp()
	app.Start()
	defer app.Stop()
	app.Wait()
}
