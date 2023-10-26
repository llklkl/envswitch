package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Hosts map[string]string
type HostsChangeWatcher func(Hosts)

type HostsType string

const HostsFromNetwork HostsType = "network"
const HostsFromFile HostsType = "file"

type HostsEntry struct {
	Type           HostsType
	RemoteUrl      string        // 远程链接
	Period         time.Duration // 定时更新周期
	LastUpdateTime time.Time
	Origin         string // 原始文件
	Hosts          Hosts  // 缓存
}

type HostsKeeper struct {
	logger *slog.Logger

	entries     map[string]*HostsEntry
	enableEntry string
	watchers    []HostsChangeWatcher // 监听当前已经激活的hosts变更通知

	httpcli *http.Client

	mu sync.Mutex
}

func (h *HostsKeeper) decode(p []byte) Hosts {
	ret := make(Hosts)
	reader := bufio.NewReader(bytes.NewReader(p))
	for {
		line, err := reader.ReadString('\n')
		line, _, _ = strings.Cut(line, "#")
		line = strings.TrimSpace(line)
		sps := strings.Split(line, " ")
		if len(sps) > 0 {
			ip := sps[0]
			for _, domain := range sps {
				ret[domain] = ip
			}
		}

		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			h.logger.Warn("Decode failed", slog.Any("err", err))
			break
		}
	}

	return ret
}

func (h *HostsKeeper) FromFile(
	ctx context.Context, name string, file string, overwrite bool,
) error {
	hosts := h.decode([]byte(file))
	return h.save(name, &HostsEntry{
		Type:   HostsFromFile,
		Origin: file,
		Hosts:  hosts,
	}, overwrite)
}

func (h *HostsKeeper) download(ctx context.Context, remoteUrl string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", remoteUrl, nil)
	if err != nil {
		return nil, err
	}

	resp, err := h.httpcli.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		resp.Body.Close()
	}()
	if resp.StatusCode/200 != 1 {
		return nil, fmt.Errorf("bad response code %d", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}

func (h *HostsKeeper) FromNetwork(
	ctx context.Context, name, remoteUrl, period string, overwrite bool,
) error {
	data, err := h.download(ctx, remoteUrl)
	if err != nil {
		return err
	}
	entry := &HostsEntry{
		Type:           HostsFromNetwork,
		RemoteUrl:      remoteUrl,
		Period:         0,
		LastUpdateTime: time.Now(),
		Origin:         string(data),
		Hosts:          h.decode(data),
	}
	return h.save(name, entry, overwrite)
}

func (h *HostsKeeper) save(name string, entry *HostsEntry, overwrite bool) error {
	h.mu.Lock()
	defer func() {
		h.mu.Unlock()
	}()
	_, exists := h.entries[name]
	if !overwrite && exists {
		return fmt.Errorf("%s already exists", name)
	}

	h.entries[name] = entry
	return nil
}

func (h *HostsKeeper) Enable(name string) error {
	h.mu.Lock()
	defer func() {
		h.mu.Unlock()
	}()
	if h.enableEntry == name {
		return nil
	}
	entry, err := h.getLocked(name)
	if err != nil {
		return err
	}
	h.enableEntry = name
	h.notifyLocked(entry)

	return nil
}

func (h *HostsKeeper) Get(name string) (*HostsEntry, error) {
	h.mu.Lock()
	defer func() {
		h.mu.Unlock()
	}()

	return h.getLocked(name)
}

func (h *HostsKeeper) getLocked(name string) (*HostsEntry, error) {
	entry, ok := h.entries[name]
	if !ok {
		return nil, fmt.Errorf("%s not exists", name)
	}

	return entry, nil
}

func (h *HostsKeeper) Format(hosts Hosts) string {
	ipHosts := make(map[string][]string)
	ips := make([]string, 0)
	for host, ip := range hosts {
		if _, ok := ipHosts[ip]; !ok {
			ips = append(ips, ip)
		}
		ipHosts[ip] = append(ipHosts[ip], host)
	}

	builder := &strings.Builder{}
	for _, ip := range ips {
		hostList := ipHosts[ip]
		builder.WriteString(ip)
		builder.WriteByte(' ')
		builder.WriteString(strings.Join(hostList, " "))
		builder.WriteByte('\n')
	}

	return builder.String()
}

func (h *HostsKeeper) Watch(watcher HostsChangeWatcher) {
	h.mu.Lock()
	h.watchers = append(h.watchers, watcher)
	h.mu.Unlock()
}

func (h *HostsKeeper) notifyLocked(entry *HostsEntry) {
	for i := range h.watchers {
		h.watchers[i](entry.Hosts)
	}
}
