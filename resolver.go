package main

import (
	"log/slog"
	"sync/atomic"
)

type Resolver struct {
	logger *slog.Logger
	hosts  atomic.Value
}

func NewResolver(logger *slog.Logger) *Resolver {
	r := &Resolver{
		logger: logger,
	}
	r.hosts.Store(Hosts{})
	return r
}

func (r *Resolver) Set(hosts Hosts) {
	r.logger.Warn("reload resolver")
	r.hosts.Store(hosts)
}

func (r *Resolver) Resolve(hostname string) (string, bool) {
	hosts := r.hosts.Load().(Hosts)
	ip, ok := hosts[hostname]
	return ip, ok
}
