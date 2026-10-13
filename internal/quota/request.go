package quota

import (
	"context"
	"errors"
	"net/http"
)

// Request is one upstream HTTP request.
type Request struct {
	Method string
	URL    string
	Header http.Header
	Body   []byte
}

// Response is one upstream HTTP response.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// Doer sends an upstream request. proxyURL is the credential's own proxy and
// is empty when the credential has none.
type Doer interface {
	Do(ctx context.Context, proxyURL string, req Request) (Response, error)
}

// ErrUnauthorized means the upstream rejected the access token. CPA refreshes
// tokens in the background, so the caller skips this round.
var ErrUnauthorized = errors.New("上游返回 401，access token 可能刚过期，等待 CPA 刷新")
