package notify

import (
	"context"
	"errors"
	"sync"
)

// Multi sends to configured channels concurrently. Any successful delivery
// completes an alert; OnPartial reports other channels' failures without
// causing a retry on the channel that already accepted it.
type Multi struct {
	Senders     []Sender
	ConfigError error
	// OnPartial runs on the caller's goroutine, after all sends finish.
	OnPartial func(Message, error)
}

// ErrNoChannel means no notification channel is configured at all.
var ErrNoChannel = errors.New("未配置通知渠道（bark_url 或 feishu_webhook）")

func (m Multi) deliver(ctx context.Context, msg Message) (bool, error) {
	errs := make([]error, len(m.Senders))
	accepted := make([]bool, len(m.Senders))
	var wg sync.WaitGroup
	for i, sender := range m.Senders {
		if sender == nil {
			continue
		}
		wg.Add(1)
		go func(i int, sender Sender) {
			defer wg.Done()
			defer func() {
				if recover() != nil {
					errs[i] = errors.New("通知渠道异常退出")
				}
			}()
			errs[i] = sender.Send(ctx, msg)
			accepted[i] = errs[i] == nil
		}(i, sender)
	}
	wg.Wait()
	errs = append(errs, m.ConfigError)
	for _, ok := range accepted {
		if ok {
			return true, errors.Join(errs...)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return false, err
	}
	return false, ErrNoChannel
}

// Send succeeds when at least one channel accepts msg.
func (m Multi) Send(ctx context.Context, msg Message) error {
	accepted, err := m.deliver(ctx, msg)
	if !accepted {
		return err
	}
	if err != nil && m.OnPartial != nil {
		m.OnPartial(msg, err)
	}
	return nil
}

// SendAll is used by the test endpoint, where every configured channel must
// work before the UI reports success.
func (m Multi) SendAll(ctx context.Context, msg Message) error {
	_, err := m.deliver(ctx, msg)
	return err
}
