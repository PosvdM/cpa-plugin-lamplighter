package quota

import (
	"context"
	"errors"
	"fmt"
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
// tokens in the background, so the caller skips this round. An HTTPError
// with status 401 matches it in errors.Is and keeps the response.
var ErrUnauthorized = errors.New("上游返回 401，access token 可能刚过期，等待 CPA 刷新")

// errorTextLength is how much of an error response Error includes; the text
// is shown on the management page.
const errorTextLength = 300

// HTTPError is an upstream response with a status other than 2xx.
// Body keeps the start of the response, longer than the text of Error, so
// that failure.Diagnose can read reasons placed late in a JSON error.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	body := e.Body
	if len(body) > errorTextLength {
		body = body[:errorTextLength]
	}
	return fmt.Sprintf("上游 HTTP %d: %s", e.Status, body)
}

// Is makes a 401 response match ErrUnauthorized.
func (e *HTTPError) Is(target error) bool {
	return target == ErrUnauthorized && e.Status == http.StatusUnauthorized
}
