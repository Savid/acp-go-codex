package codexacp

import (
	"context"
	"errors"
	"time"

	"github.com/coder/acp-go-sdk"
)

const sessionLifecycleDeliveryBuffer = 1024

type lifecycleDelivery struct {
	notification acp.SessionNotification
	timeout      time.Duration
	result       chan error
}

// newLifecycleDeliveryOwner creates the session-owned cancellation boundary
// for an ordered delivery run. Individual writes carry their own deadline; the
// owner deliberately has none, so a continuously non-empty queue cannot age
// out merely because it has been making progress for a long time.
func newLifecycleDeliveryOwner() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}

func lifecycleDeliveryTimeout(ctx context.Context) time.Duration {
	timeout := promptSettlementTimeout

	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining < timeout {
			timeout = remaining
		}
	}

	if timeout <= 0 {
		return time.Nanosecond
	}

	return timeout
}

func (s *session) enqueueLifecycleDeliveryLocked(ctx context.Context, notification acp.SessionNotification) (<-chan error, error) {
	if s.lifecycleDeliveryStop {
		return nil, errors.New("codex lifecycle delivery is closed")
	}

	if len(s.lifecycleDeliveries) == sessionLifecycleDeliveryBuffer {
		return nil, errors.New("codex lifecycle delivery buffer is full")
	}

	result := make(chan error, 1)

	s.lifecycleDeliveries = append(s.lifecycleDeliveries, lifecycleDelivery{
		notification: notification,
		timeout:      lifecycleDeliveryTimeout(ctx),
		result:       result,
	})
	if !s.lifecycleDeliveryRun {
		ownerCtx, cancel := newLifecycleDeliveryOwner()
		done := make(chan struct{})
		s.lifecycleDeliveryRun = true
		s.lifecycleDeliveryCancel = cancel
		s.lifecycleDeliveryDone = done

		go s.runLifecycleDeliveries(ownerCtx, done)
	}

	return result, nil
}

func (s *session) runLifecycleDeliveries(ownerCtx context.Context, done chan struct{}) {
	defer close(done)

	for {
		s.lifecycleMu.Lock()
		if ownerCtx.Err() != nil || s.lifecycleDeliveryStop {
			pending := s.lifecycleDeliveries
			s.lifecycleDeliveries = nil
			s.lifecycleDeliveryRun = false
			s.lifecycleMu.Unlock()

			for index := range pending {
				delivery := pending[index]
				delivery.result <- context.Canceled

				close(delivery.result)
			}

			return
		}

		if len(s.lifecycleDeliveries) == 0 {
			cancel := s.lifecycleDeliveryCancel
			s.lifecycleDeliveryRun = false
			s.lifecycleMu.Unlock()

			if cancel != nil {
				cancel()
			}

			return
		}

		delivery := s.lifecycleDeliveries[0]
		s.lifecycleDeliveries = s.lifecycleDeliveries[1:]
		s.lifecycleDeliveryActive = true
		s.lifecycleMu.Unlock()

		deliveryCtx, cancel := context.WithTimeout(ownerCtx, delivery.timeout)
		conn := s.agent.connection()

		var err error
		if conn == nil {
			err = errors.New("ACP lifecycle delivery connection is unavailable")
		} else if lifecycleConn, ok := conn.(lifecycleNotificationClient); ok {
			err = lifecycleConn.SessionUpdateLifecycle(deliveryCtx, delivery.notification)
		} else {
			err = conn.SessionUpdate(deliveryCtx, delivery.notification)
		}

		cancel()

		err = wrapHostDeliveryError(err)

		s.lifecycleMu.Lock()

		s.lifecycleDeliveryActive = false
		if err != nil {
			_ = s.latchLifecycleFailureLocked(err)
			s.lifecycleDeliveryStop = true
		}

		finished := err == nil && len(s.lifecycleDeliveries) == 0

		var ownerCancel context.CancelFunc
		if finished {
			ownerCancel = s.lifecycleDeliveryCancel
			s.lifecycleDeliveryRun = false
		}
		s.lifecycleMu.Unlock()

		delivery.result <- err

		close(delivery.result)

		if finished {
			if ownerCancel != nil {
				ownerCancel()
			}

			return
		}
	}
}

func (s *session) stopLifecycleDeliveries(ctx context.Context) error {
	s.lifecycleMu.Lock()
	s.lifecycleDeliveryStop = true
	cancel := s.lifecycleDeliveryCancel
	done := s.lifecycleDeliveryDone
	s.lifecycleMu.Unlock()

	if cancel != nil {
		cancel()
	}

	if done == nil {
		return nil
	}

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
