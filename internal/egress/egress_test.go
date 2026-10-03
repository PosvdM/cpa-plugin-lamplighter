package egress

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/quota"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type fakeHost struct{ calls int }

func (f *fakeHost) HTTPDo(_ context.Context, req pluginapi.HTTPRequest, _ time.Duration) (pluginapi.HTTPResponse, error) {
	f.calls++
	return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte("host")}, nil
}

func TestWithoutCredentialProxyUsesHost(t *testing.T) {
	h := &fakeHost{}
	resp, err := (&Egress{Host: h}).Do(context.Background(), "", quota.Request{Method: "GET", URL: "https://example.com"})
	if err != nil || h.calls != 1 || string(resp.Body) != "host" {
		t.Fatalf("resp %+v err %v calls %d", resp, err, h.calls)
	}
}

func TestCredentialProxyIsUsed(t *testing.T) {
	var seen string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.String()
		w.Write([]byte("proxied"))
	}))
	defer proxy.Close()
	h := &fakeHost{}
	resp, err := (&Egress{Host: h}).Do(context.Background(), proxy.URL, quota.Request{Method: "GET", URL: "http://upstream.invalid/usage", Header: http.Header{}})
	if err != nil || string(resp.Body) != "proxied" || h.calls != 0 {
		t.Fatalf("resp %+v err %v host calls %d", resp, err, h.calls)
	}
	if seen != "http://upstream.invalid/usage" {
		t.Fatalf("proxy saw %q", seen)
	}
}

func TestInvalidCredentialProxyFailsClosed(t *testing.T) {
	h := &fakeHost{}
	_, err := (&Egress{Host: h}).Do(context.Background(), "ftp://bad", quota.Request{Method: "GET", URL: "https://example.com"})
	if err == nil || h.calls != 0 {
		t.Fatalf("invalid proxy must not fall back to the host: err %v calls %d", err, h.calls)
	}
}
