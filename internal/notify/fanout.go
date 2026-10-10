package notify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// Channel is one configured notification channel.
type Channel struct {
	Name   string
	Sender Sender
}

// Fanout sends each message to every channel at once, so a channel that
// does not answer does not hold up the others. A message counts as
// delivered when one channel accepts it; retrying it would repeat it on the
// channels that already did. The failed channels go to OnChannelError.
type Fanout struct {
	Channels []Channel
	// OnChannelError runs on the caller's goroutine for each channel that
	// failed while another one delivered the message.
	OnChannelError func(msg Message, err error)
}

// Send delivers msg to all channels. It fails only when every channel failed.
func (f *Fanout) Send(ctx context.Context, msg Message) error {
	if len(f.Channels) == 0 {
		return ErrNotConfigured
	}
	errs := make([]error, len(f.Channels))
	var wg sync.WaitGroup
	for i, channel := range f.Channels {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := channel.Sender.Send(ctx, msg); err != nil {
				errs[i] = fmt.Errorf("%s：%w", channel.Name, err)
			}
		}()
	}
	wg.Wait()
	delivered := false
	for _, err := range errs {
		delivered = delivered || err == nil
	}
	if !delivered {
		texts := make([]string, len(errs))
		for i, err := range errs {
			texts[i] = err.Error()
		}
		return errors.New(strings.Join(texts, "；"))
	}
	for _, err := range errs {
		if err != nil && f.OnChannelError != nil {
			f.OnChannelError(msg, err)
		}
	}
	return nil
}
