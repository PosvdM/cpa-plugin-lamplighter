// Package egress sends quota requests through the same network exit that CPA
// uses for model requests of the same credential.
//
// CPA's model executors pick the credential's proxy_url first, then the global
// proxy-url, then environment proxies, then a direct connection. A credential
// with its own proxy_url is sent through a transport built by CPA's proxyutil;
// every other request goes through host.http.do, which applies the remaining
// steps inside CPA. A proxy_url that cannot be parsed fails the request instead
// of falling back to a direct connection.
package egress

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/quota"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/proxyutil"
)

// HostHTTP is the part of the host API used for requests without a credential proxy.
type HostHTTP interface {
	HTTPDo(ctx context.Context, req pluginapi.HTTPRequest, timeout time.Duration) (pluginapi.HTTPResponse, error)
}

const maxResponseBytes = 4 << 20

// Egress implements quota.Doer.
type Egress struct {
	Host    HostHTTP
	Timeout time.Duration

	mu         sync.Mutex
	transports map[string]*http.Transport
}

// Do sends req through the credential proxy when proxyURL is set and through
// host.http.do otherwise.
func (e *Egress) Do(ctx context.Context, proxyURL string, req quota.Request) (quota.Response, error) {
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		resp, err := e.Host.HTTPDo(ctx, pluginapi.HTTPRequest{
			Method:  req.Method,
			URL:     req.URL,
			Headers: req.Header,
			Body:    req.Body,
		}, timeout)
		if err != nil {
			return quota.Response{}, err
		}
		return quota.Response{Status: resp.StatusCode, Header: resp.Headers, Body: resp.Body}, nil
	}

	transport, err := e.transport(proxyURL)
	if err != nil {
		return quota.Response{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, req.URL, bytes.NewReader(req.Body))
	if err != nil {
		return quota.Response{}, err
	}
	httpReq.Header = req.Header.Clone()
	resp, err := (&http.Client{Transport: transport}).Do(httpReq)
	if err != nil {
		return quota.Response{}, fmt.Errorf("经凭证代理请求失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return quota.Response{}, err
	}
	return quota.Response{Status: resp.StatusCode, Header: resp.Header, Body: body}, nil
}

func (e *Egress) transport(proxyURL string) (*http.Transport, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if transport, ok := e.transports[proxyURL]; ok {
		return transport, nil
	}
	transport, mode, err := proxyutil.BuildHTTPTransport(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("凭证 proxy_url 无效，已跳过请求: %s", proxyutil.Redact(proxyURL))
	}
	if transport == nil || mode == proxyutil.ModeInherit {
		return nil, fmt.Errorf("凭证 proxy_url 无效，已跳过请求: %s", proxyutil.Redact(proxyURL))
	}
	if e.transports == nil {
		e.transports = make(map[string]*http.Transport)
	}
	e.transports[proxyURL] = transport
	return transport, nil
}
