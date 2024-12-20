package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

type Config struct {
	cfgPath string `json:"-"`
	mutex   sync.Mutex

	ListenAddr string `json:"listen_addr"`
	TunnelAddr string `json:"tunnel_addr"`

	Metadata *Metadata `json:"metadata"`
}

type Metadata struct {
	Hosts  *HostsMetadata  `json:"hosts"`
	Tunnel *TunnelMetadata `json:"tunnel"`
}

type HostsMetadata struct {
	EnableEntry string           `json:"enable_entry"`
	Entries     []*HostsEntryCfg `json:"entries"`
}

type HostsEntryCfg struct {
	Name           string        `json:"name"`
	Enable         bool          `json:"enable"`
	Type           HostsType     `json:"type,omitempty"`
	RemoteUrl      string        `json:"remote_url,omitempty"` // 远程链接
	Period         time.Duration `json:"period,omitempty"`     // 定时更新周期
	LastUpdateTime time.Time     `json:"last_update_time"`
	Origin         string        `json:"origin,omitempty"` // 原始文件
}

type TunnelMetadata struct {
}

func NewConfig(cfgPath string) *Config {
	cfg := &Config{
		cfgPath: cfgPath,
	}
	err := cfg.load()
	if err != nil {
		fmt.Printf("load config[%s] failed, err: %v\n", cfgPath, err)
		os.Exit(1)
	}
	cfg.fillEmpty()
	return cfg
}

func (c *Config) load() error {
	fp, err := os.Open(c.cfgPath)
	if err != nil {
		return err
	}
	defer func() {
		fp.Close()
	}()
	dec := json.NewDecoder(fp)
	return dec.Decode(c)
}

func (c *Config) fillEmpty() {
	if c.Metadata == nil {
		c.Metadata = &Metadata{}
	}
	if c.Metadata.Tunnel == nil {
		c.Metadata.Tunnel = &TunnelMetadata{}
	}
	if c.Metadata.Hosts == nil {
		c.Metadata.Hosts = &HostsMetadata{}
	}
}

func (c *Config) Save() {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	data, err := json.MarshalIndent(c, "", "    ")
	if err != nil {
		fmt.Printf("save config marshale failed, err: %v\n", err)
		return
	}

	fp, err := os.OpenFile(c.cfgPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0755)
	if err != nil {
		fmt.Printf("save config open file[%s] failed, err: %v\n", c.cfgPath, err)
		return
	}
	defer func() {
		fp.Sync()
		fp.Close()
	}()

	_, err = io.Copy(fp, bytes.NewReader(data))
	if err != nil {
		fmt.Printf("save config write file[%s] failed, err: %v\n", c.cfgPath, err)
		return
	}
}
