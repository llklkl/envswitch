package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
)

var (
	helpFlag = flag.Bool("h", false, "help")
	confPath = flag.String("conf", "config.json", "配置文件路径")
	logLevel = flag.String("log", "WARN", "日志级别")
)

func help() {
	fmt.Println(`
usage: envswitch [-conf <path>] [-h] [-log <level>]
	-h: 打开帮助页面
	-conf <path>: 指定配置文件路径, 默认使用 config.json
	-log <level>: 指定日志级别, 默认WARN. 支持 INFO, WARN, ERROR`)
}

type App struct {
	cfg     *Config
	logger  *slog.Logger
	manager *Manager
	tunnel  *Tunnel
}

func newLogger() *slog.Logger {
	level := slog.LevelWarn
	switch strings.ToLower(*logLevel) {
	case "info":
		level = slog.LevelInfo
	case "error":
		level = slog.LevelError
	}
	logHandler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
	})
	return slog.New(logHandler)
}

func initApp() *App {
	logger := newLogger()
	cfg := NewConfig(*confPath)
	hostKeeper := NewHostsKeeper(cfg, logger.With(slog.String("module", "hostskeeper")))
	resolver := NewResolver(logger.With(slog.String("module", "resolver")))

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

func (a *App) writePid() {
	pid := os.Getpid()
	fp, err := os.OpenFile("./.pid", os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0755)
	if err != nil {
		return
	}
	defer fp.Close()

	fmt.Fprintf(fp, "%d", pid)
}

func (a *App) Start() {
	a.protectRun(a.manager.Start)
	a.protectRun(a.tunnel.Start)
	a.writePid()
}

func (a *App) Wait() {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)
	for {
		select {
		case sig := <-sigs:
			a.logger.Warn("quit", slog.String("signal", sig.String()))
			return
		}
	}
}

func (a *App) Stop() {
	a.manager.Stop()
	a.tunnel.Stop()
	_ = os.Remove("./.pid")
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
	flag.Parse()

	if *helpFlag {
		help()
		return
	}

	app := initApp()
	app.Start()
	defer app.Stop()
	app.Wait()
}
