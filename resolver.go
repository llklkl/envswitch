package main

import "sync/atomic"

type Resolver struct {
	hosts atomic.Value
}

func NewResolver() *Resolver {
	r := &Resolver{}
	return r
}

func (r *Resolver) Set(hosts Hosts) {
	r.hosts.Store(hosts)
}

func (r *Resolver) Resolve(hostname string) (string, bool) {
	hosts := r.hosts.Load().(Hosts)
	ip, ok := hosts[hostname]
	return ip, ok
}
