package notify

import (
	"context"
	"errors"
)

// Multi delivers a message to every sender in turn. One failing channel does
// not stop the others, and the errors are joined so the caller can report
// all of them. An empty Multi reports ErrNoChannel, which keeps the alert
// pending so it is retried once a channel is configured.
type Multi []Sender

// ErrNoChannel means no notification channel is configured at all.
var ErrNoChannel = errors.New("未配置通知渠道（bark_url 或 feishu_webhook）")

// Send delivers msg to every configured sender.
func (m Multi) Send(ctx context.Context, msg Message) error {
	if len(m) == 0 {
		return ErrNoChannel
	}
	var errs []error
	for _, s := range m {
		if s == nil {
			continue
		}
		if err := s.Send(ctx, msg); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
