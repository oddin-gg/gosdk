package gosdk

import (
	"context"
	"time"

	"github.com/oddin-gg/gosdk/types"
)

// TEMPORARY DIAGNOSTICS — branch temp/recovery-tracing only, do not merge.
// See internal/recovery/trace.go for the why. Everything here is prefixed
// "trace:" so `|= "trace:"` in Loki returns the whole picture.

const (
	// traceSlowSendThreshold flags a single hand-off to the public
	// subscription buffer that blocked this long. This is the consumer's
	// (kollector's) backpressure — the whole AMQP pipeline is serialized
	// behind it.
	traceSlowSendThreshold = 250 * time.Millisecond

	// traceStuckThreshold flags a message still being processed after
	// this long; the watchdog re-reports every traceWatchdogPeriod while
	// it stays stuck, so a wedged pipeline is visible even though the
	// message loop emits nothing.
	traceStuckThreshold = 2 * time.Second

	traceWatchdogPeriod = 5 * time.Second
	traceSummaryPeriod  = 30 * time.Second
)

// traceMgrGen reads the recovery manager's generation without widening
// the recoveryMessageProcessor interface (test doubles stay valid).
func (o *oddsFeedSessionImpl) traceMgrGen() int64 {
	if tg, ok := o.recoveryMessageProcessor.(interface{ TraceGen() int64 }); ok {
		return tg.TraceGen()
	}
	return -1
}

func traceRoute(feedMessage *types.FeedMessage) string {
	if feedMessage == nil || feedMessage.RoutingKey == nil {
		return ""
	}
	return feedMessage.RoutingKey.FullRoutingKey
}

// traceDrop records a delivery the session terminates without handing it
// to the consumer. Upstream, three of these paths log nothing at all.
func (o *oddsFeedSessionImpl) traceDrop(reason string, producerID int, route string) {
	o.traceDropTotal.Add(1)
	o.logger.Warn("trace: session: delivery dropped",
		"session_id", o.sessionID.String(),
		"reason", reason,
		"producer_id", producerID,
		"route", route,
		"dropped_total", o.traceDropTotal.Load())
}

func (o *oddsFeedSessionImpl) traceBeginMessage(route string) {
	o.traceMsgTotal.Add(1)
	r := route
	o.traceInFlightRoute.Store(&r)
	o.traceInFlightSince.Store(time.Now().UnixNano())
}

func (o *oddsFeedSessionImpl) traceEndMessage() {
	o.traceInFlightSince.Store(0)
}

func (o *oddsFeedSessionImpl) traceSend(start time.Time, admitted bool) {
	blocked := time.Since(start)
	o.traceSendBlockedNS.Add(int64(blocked))
	for {
		prev := o.traceSendMaxNS.Load()
		if int64(blocked) <= prev || o.traceSendMaxNS.CompareAndSwap(prev, int64(blocked)) {
			break
		}
	}
	if blocked >= traceSlowSendThreshold || !admitted {
		route := ""
		if r := o.traceInFlightRoute.Load(); r != nil {
			route = *r
		}
		o.logger.Warn("trace: session: consumer backpressure",
			"session_id", o.sessionID.String(),
			"blocked_ms", blocked.Milliseconds(),
			"admitted", admitted,
			"route", route,
			"msg_buffer_len", len(o.msgCh),
			"msg_buffer_cap", cap(o.msgCh))
	}
}

// traceWatchdog reports a wedged message loop and a periodic throughput
// summary. It runs on its own goroutine precisely so a blocked pipeline
// still produces log lines.
func (o *oddsFeedSessionImpl) traceWatchdog(ctx context.Context) {
	ticker := time.NewTicker(traceWatchdogPeriod)
	defer ticker.Stop()

	var (
		lastSummary   = time.Now()
		lastMsgTotal  uint64
		lastBlockedNS int64
	)
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if since := o.traceInFlightSince.Load(); since != 0 {
				stuck := now.Sub(time.Unix(0, since))
				if stuck >= traceStuckThreshold {
					route := ""
					if r := o.traceInFlightRoute.Load(); r != nil {
						route = *r
					}
					o.logger.Error("trace: session: message stuck in processing",
						"session_id", o.sessionID.String(),
						"stuck_ms", stuck.Milliseconds(),
						"route", route,
						"msg_buffer_len", len(o.msgCh),
						"msg_buffer_cap", cap(o.msgCh))
				}
			}

			if now.Sub(lastSummary) < traceSummaryPeriod {
				continue
			}
			total := o.traceMsgTotal.Load()
			blocked := o.traceSendBlockedNS.Load()
			window := now.Sub(lastSummary)
			o.logger.Info("trace: session: summary",
				"session_id", o.sessionID.String(),
				"mgr_gen", o.traceMgrGen(),
				"window_s", int64(window.Seconds()),
				"messages", total-lastMsgTotal,
				"messages_per_s", float64(total-lastMsgTotal)/window.Seconds(),
				"messages_total", total,
				"dropped_total", o.traceDropTotal.Load(),
				"send_blocked_ms", (blocked-lastBlockedNS)/int64(time.Millisecond),
				"send_blocked_max_ms", o.traceSendMaxNS.Swap(0)/int64(time.Millisecond),
				"msg_buffer_len", len(o.msgCh),
				"msg_buffer_cap", cap(o.msgCh))
			lastSummary = now
			lastMsgTotal = total
			lastBlockedNS = blocked
		}
	}
}
