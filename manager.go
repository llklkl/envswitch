package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"sort"
	"time"
)

//go:embed public
var public embed.FS

type statusError struct {
	Code    int
	Status  int
	Message string
}

const (
	statusOk        = 0
	statusServerErr = 5000
)

func (e *statusError) Error() string {
	return fmt.Sprintf("[%d] %s", e.Code, e.Message)
}

type apiHandleFn func(context.Context, http.Header, []byte) (any, error)

type Manager struct {
	svr    *http.Server
	logger *slog.Logger

	hostsKeeper *HostsKeeper

	apiHandlers map[string]apiHandleFn
}

func NewManager(
	cfg *Config,
	logger *slog.Logger,
	hostsKeeper *HostsKeeper,
) *Manager {
	m := &Manager{
		logger:      logger,
		hostsKeeper: hostsKeeper,
		apiHandlers: make(map[string]apiHandleFn),
	}
	m.initHttpServer(cfg.ListenAddr)
	return m
}

func (m *Manager) Start() {
	m.logger.Warn("manager start", slog.String("listen_on", m.svr.Addr))
	if err := m.svr.ListenAndServe(); err != nil {
		m.logger.Error("server quit unexpected", slog.Any("err", err))
	}
}

func (m *Manager) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*2)
	defer cancel()
	m.svr.Shutdown(ctx)
}

func (m *Manager) initHttpServer(listenAddr string) {
	mux := http.NewServeMux()
	svr := &http.Server{
		Addr:    listenAddr,
		Handler: mux,
	}
	mux.HandleFunc("/api", m.handleApi)
	content, _ := fs.Sub(public, "public")
	mux.Handle("/", http.FileServer(http.FS(content)))
	m.svr = svr

	m.apiHandlers["listEntries"] = m.listEntries
	m.apiHandlers["addEntry"] = m.addEntry
	m.apiHandlers["getEntry"] = m.getEntry
	m.apiHandlers["deleteEntry"] = m.deleteEntry
	m.apiHandlers["enableEntry"] = m.enableEntry
}

func (m *Manager) handleApi(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	action := r.Form.Get("action")
	if action == "" {
		action = r.PostForm.Get("action")
	}
	handler, ok := m.apiHandlers[action]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	reqData, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), time.Second*10)
	defer cancel()
	resp, err := handler(ctx, r.Header, reqData)

	respErr := &statusError{
		Code:    statusOk,
		Status:  http.StatusOK,
		Message: "ok",
	}
	if err != nil {
		switch e := err.(type) {
		case *statusError:
			respErr = e
			if respErr.Status == 0 {
				respErr.Status = http.StatusBadRequest
			}
		default:
			respErr.Message = e.Error()
			respErr.Code = statusServerErr
			respErr.Status = http.StatusInternalServerError
		}
	}

	var responseData []byte
	responseData, err = json.Marshal(map[string]any{
		"code":    respErr.Code,
		"message": respErr.Message,
		"data":    resp,
	})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.WriteHeader(respErr.Status)
	w.Write(responseData)
}

func (m *Manager) addEntry(
	ctx context.Context, header http.Header, body []byte,
) (any, error) {
	reqObj := struct {
		Type       string `json:"type"`
		Name       string `json:"name"`
		RemoteUrl  string `json:"remote_url"`
		Data       string `json:"data"`
		SyncPeriod string `json:"sync_period"`
	}{}
	if err := json.Unmarshal(body, &reqObj); err != nil {
		return nil, err
	}

	var err error
	switch HostsType(reqObj.Type) {
	case HostsFromNetwork:
		err = m.hostsKeeper.FromNetwork(ctx, reqObj.Name, reqObj.RemoteUrl, reqObj.SyncPeriod, true)
	case HostsFromFile:
		err = m.hostsKeeper.FromFile(ctx, reqObj.Name, reqObj.Data, true)
	default:
		err = fmt.Errorf("bad argument")
	}
	if err != nil {
		return nil, err
	}

	return nil, nil
}

func (m *Manager) deleteEntry(
	ctx context.Context, header http.Header, body []byte,
) (any, error) {
	reqObj := struct {
		Name string `json:"name"`
	}{}
	if err := json.Unmarshal(body, &reqObj); err != nil {
		return nil, err
	}

	err := m.hostsKeeper.Delete(reqObj.Name)
	if err != nil {
		return nil, err
	}

	return nil, nil
}

func (m *Manager) getEntry(
	ctx context.Context, header http.Header, body []byte,
) (any, error) {
	reqObj := struct {
		Name string `json:"name"`
	}{}
	if err := json.Unmarshal(body, &reqObj); err != nil {
		return nil, err
	}

	entry, err := m.hostsKeeper.Get(reqObj.Name)
	if err != nil {
		return nil, err
	}

	res := map[string]any{
		"name":       entry.Name,
		"enable":     entry.Enable,
		"type":       entry.Type,
		"remote_url": entry.RemoteUrl,
	}
	if entry.Type == HostsFromNetwork {
		res["data"] = entry.Hosts.String()
	} else {
		res["data"] = entry.Origin
	}

	return res, nil
}

func (m *Manager) enableEntry(
	ctx context.Context, header http.Header, body []byte,
) (any, error) {
	reqObj := struct {
		Name   string `json:"name"`
		Enable bool   `json:"enable"`
	}{}
	if err := json.Unmarshal(body, &reqObj); err != nil {
		return nil, err
	}
	err := m.hostsKeeper.Enable(reqObj.Name, reqObj.Enable)
	if err != nil {
		return nil, err
	}

	return nil, nil
}

func (m *Manager) listEntries(
	ctx context.Context, header http.Header, body []byte,
) (any, error) {
	entries := m.hostsKeeper.ListEntries()
	list := make([]map[string]any, 0, len(entries))
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		list = append(list, map[string]any{
			"name":   name,
			"enable": entries[name],
		})
	}
	return map[string]any{
		"list": list,
	}, nil
}
