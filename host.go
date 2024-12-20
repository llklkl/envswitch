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
	"sort"
	"strings"
	"sync"
	"time"
)

type Hosts map[string]string

func DecodeHostsFile(file io.Reader) (Hosts, error) {
	ret := make(Hosts)
	reader := bufio.NewReader(file)
	for {
		line, err := reader.ReadString('\n')
		line, _, _ = strings.Cut(line, "#")
		line = strings.TrimSpace(line)
		sps := strings.Split(line, " ")
		if len(sps) > 1 && len(line) > 0 {
			ip := sps[0]
			for _, domain := range sps[1:] {
				if len(domain) > 0 {
					ret[domain] = ip
				}
			}
		}

		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
	}

	return ret, nil
}

func (h Hosts) String() string {
	ipHosts := make(map[string][]string)
	ips := make([]string, 0)
	for host, ip := range h {
		if _, ok := ipHosts[ip]; !ok {
			ips = append(ips, ip)
		}
		ipHosts[ip] = append(ipHosts[ip], host)
	}
	sort.Strings(ips)

	builder := &strings.Builder{}
	for _, ip := range ips {
		hostList := ipHosts[ip]
		sort.Strings(hostList)
		builder.WriteString(ip)
		builder.WriteByte(' ')
		builder.WriteString(strings.Join(hostList, " "))
		builder.WriteByte('\n')
	}

	return builder.String()
}

func (h Hosts) Clone() Hosts {
	res := make(Hosts, len(h))
	for k, v := range h {
		res[k] = v
	}
	return res
}

type HostsChangeWatcher func(Hosts)

type HostsType string

const HostsFromNetwork HostsType = "network"
const HostsFromFile HostsType = "file"

type HostsEntry struct {
	Name           string        `json:"name"`
	Enable         bool          `json:"enable"`
	Type           HostsType     `json:"type,omitempty"`
	RemoteUrl      string        `json:"remote_url,omitempty"` // 远程链接
	Period         time.Duration `json:"period,omitempty"`     // 定时更新周期
	LastUpdateTime time.Time     `json:"last_update_time"`
	Origin         string        `json:"origin,omitempty"` // 原始文件
	Hosts          Hosts         `json:"hosts,omitempty"`  // 缓存
}

func (h *HostsEntry) Clone() *HostsEntry {
	return &HostsEntry{
		Type:           h.Type,
		Name:           h.Name,
		Enable:         h.Enable,
		RemoteUrl:      h.RemoteUrl,
		Period:         h.Period,
		LastUpdateTime: h.LastUpdateTime,
		Origin:         h.Origin,
		Hosts:          h.Hosts.Clone(),
	}
}

type HostsKeeper struct {
	cfg    *Config
	logger *slog.Logger

	entries     map[string]*HostsEntry
	enableEntry string
	watchers    []HostsChangeWatcher // 监听当前已经激活的hosts变更通知

	httpcli *http.Client

	mu sync.Mutex
}

func NewHostsKeeper(cfg *Config, logger *slog.Logger) *HostsKeeper {
	meta := cfg.Metadata.Hosts
	h := &HostsKeeper{
		cfg:         cfg,
		logger:      logger,
		entries:     make(map[string]*HostsEntry),
		enableEntry: meta.EnableEntry,
		watchers:    make([]HostsChangeWatcher, 0),
		httpcli:     &http.Client{},
	}
	for _, e := range meta.Entries {
		hosts, err := DecodeHostsFile(strings.NewReader(e.Origin))
		if err != nil {
			h.logger.Error("decode hosts failed", slog.String("name", e.Name),
				slog.Any("err", err))
			continue
		}
		h.entries[e.Name] = &HostsEntry{
			Name:           e.Name,
			Enable:         e.Enable,
			Type:           e.Type,
			RemoteUrl:      e.RemoteUrl,
			Period:         e.Period,
			LastUpdateTime: e.LastUpdateTime,
			Origin:         e.Origin,
			Hosts:          hosts,
		}
	}
	go func() {
		time.Sleep(time.Second * 3)
		h.notify()
	}()

	return h
}

func (h *HostsKeeper) FromFile(
	ctx context.Context, name string, file string, overwrite bool,
) error {
	hosts, err := DecodeHostsFile(strings.NewReader(file))
	if err != nil {
		return err
	}
	return h.save(name, &HostsEntry{
		Name:           name,
		Type:           HostsFromFile,
		Origin:         file,
		Hosts:          hosts,
		LastUpdateTime: time.Now(),
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
	ctx context.Context, name, remoteUrl, periodStr string, overwrite bool,
) error {
	period, err := time.ParseDuration(periodStr)
	if err != nil {
		return err
	}
	data, err := h.download(ctx, remoteUrl)
	if err != nil {
		h.logger.Warn("download hosts failed", slog.String("name", name),
			slog.String("url", remoteUrl), slog.Any("err", err))
		return err
	}
	hosts, err := DecodeHostsFile(bytes.NewReader(data))
	if err != nil {
		return err
	}
	entry := &HostsEntry{
		Name:           name,
		Type:           HostsFromNetwork,
		RemoteUrl:      remoteUrl,
		Period:         period,
		LastUpdateTime: time.Now(),
		Origin:         string(data),
		Hosts:          hosts,
	}
	return h.save(name, entry, overwrite)
}

func (h *HostsKeeper) save(name string, entry *HostsEntry, overwrite bool) error {
	h.mu.Lock()
	defer func() {
		h.mu.Unlock()
	}()
	existEntry, exists := h.entries[name]
	if !overwrite && exists {
		return fmt.Errorf("%s already exists", name)
	}
	if exists {
		entry.Enable = existEntry.Enable
	}
	h.entries[name] = entry
	h.dumpConfig()
	return nil
}

func (h *HostsKeeper) Delete(name string) error {
	h.mu.Lock()
	delete(h.entries, name)
	h.dumpConfig()
	h.mu.Unlock()
	return nil
}

func (h *HostsKeeper) Enable(name string, enable bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	entry, ok := h.entries[name]
	if !ok {
		return fmt.Errorf("%s not exists", name)
	}
	if entry.Enable != enable {
		entry.Enable = enable
		h.notifyLocked()
		h.dumpConfig()
	}

	return nil
}

func (h *HostsKeeper) Enabled() string {
	h.mu.Lock()
	res := h.enableEntry
	h.mu.Unlock()
	return res
}

func (h *HostsKeeper) dumpConfig() {
	h.cfg.Metadata.Hosts.EnableEntry = h.enableEntry
	h.cfg.Metadata.Hosts.Entries = make([]*HostsEntryCfg, 0, len(h.entries))
	for _, entry := range h.entries {
		h.cfg.Metadata.Hosts.Entries = append(h.cfg.Metadata.Hosts.Entries, &HostsEntryCfg{
			Name:           entry.Name,
			Enable:         entry.Enable,
			Type:           entry.Type,
			RemoteUrl:      entry.RemoteUrl,
			Period:         entry.Period,
			LastUpdateTime: entry.LastUpdateTime,
			Origin:         entry.Origin,
		})
	}
	h.cfg.Save()
}

func (h *HostsKeeper) Get(name string) (*HostsEntry, error) {
	h.mu.Lock()
	entry, err := h.getLocked(name)
	h.mu.Unlock()

	return entry, err
}

func (h *HostsKeeper) List() map[string]*HostsEntry {
	h.mu.Lock()
	res := make(map[string]*HostsEntry, len(h.entries))
	for k, entry := range h.entries {
		res[k] = entry.Clone()
	}
	h.mu.Unlock()

	return res
}

func (h *HostsKeeper) ListEntries() map[string]bool {
	h.mu.Lock()
	res := make(map[string]bool, len(h.entries))
	for _, e := range h.entries {
		res[e.Name] = e.Enable
	}
	h.mu.Unlock()

	return res
}

func (h *HostsKeeper) getLocked(name string) (*HostsEntry, error) {
	entry, ok := h.entries[name]
	if !ok {
		return nil, fmt.Errorf("%s not exists", name)
	}

	return entry.Clone(), nil
}

func (h *HostsKeeper) Watch(watcher HostsChangeWatcher) {
	h.mu.Lock()
	h.watchers = append(h.watchers, watcher)
	h.mu.Unlock()
}

func (h *HostsKeeper) notify() {
	h.mu.Lock()
	h.notifyLocked()
	h.mu.Unlock()
}

func (h *HostsKeeper) notifyLocked() {
	merged := Hosts{}
	for _, entry := range h.entries {
		if !entry.Enable {
			continue
		}

		for k, v := range entry.Hosts {
			merged[k] = v
		}
	}
	for i := range h.watchers {
		h.watchers[i](merged)
	}
}
