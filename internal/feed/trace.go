package feed

import (
	"strings"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/oddin-gg/gosdk/internal/utils"
)

// TEMPORARY DIAGNOSTICS — branch temp/recovery-tracing only, do not merge.
// Answers "did the broker actually hand us this message, and where did
// the pipeline stall". Everything is prefixed "trace:".

const (
	// traceSlowAdmitThreshold flags one delivery that waited this long
	// to enter the session loop. The AMQP consumer is serialized, so
	// this IS the pipeline's head-of-line blocking.
	traceSlowAdmitThreshold = 250 * time.Millisecond

	traceSummaryPeriod = 30 * time.Second
)

// isSystemRoute reports the rare, correctness-critical routes. Those are
// logged individually at INFO; ordinary event traffic stays at DEBUG.
func isSystemRoute(route string) bool {
	return strings.Contains(route, "snapshot_complete") || strings.Contains(route, "alive")
}

func (c *ChannelConsumer) traceDelivery(d amqp.Delivery) {
	n := c.traceDeliveries.Add(1)
	c.traceBytes.Add(uint64(len(d.Body)))

	if isSystemRoute(d.RoutingKey) {
		c.traceSystemMsgs.Add(1)
		c.logger.Info("trace: feed: system delivery received",
			"route", d.RoutingKey,
			"delivery_tag", d.DeliveryTag,
			"redelivered", d.Redelivered,
			"body_bytes", len(d.Body),
			"body", utils.PayloadPreview(d.Body),
			"deliveries_total", n,
			"unsettled", c.unsettled())
		return
	}
	c.logger.Debug("trace: feed: delivery received",
		"route", d.RoutingKey,
		"delivery_tag", d.DeliveryTag,
		"redelivered", d.Redelivered,
		"body_bytes", len(d.Body),
		"deliveries_total", n)
}

func (c *ChannelConsumer) traceAdmit(d amqp.Delivery, start time.Time, admitted bool) {
	waited := time.Since(start)
	c.traceAdmitNS.Add(int64(waited))
	for {
		prev := c.traceAdmitMaxNS.Load()
		if int64(waited) <= prev || c.traceAdmitMaxNS.CompareAndSwap(prev, int64(waited)) {
			break
		}
	}
	if waited < traceSlowAdmitThreshold && admitted {
		return
	}
	c.logger.Warn("trace: feed: delivery admission slow or aborted",
		"route", d.RoutingKey,
		"delivery_tag", d.DeliveryTag,
		"waited_ms", waited.Milliseconds(),
		"admitted", admitted,
		"unsettled", c.unsettled())
}

// traceSummary emits a throughput line every traceSummaryPeriod from the
// consume loop itself: a gap between summaries means the loop is stuck.
func (c *ChannelConsumer) traceSummary() {
	now := time.Now().UnixNano()
	last := c.traceLastSummary.Load()
	if last == 0 {
		c.traceLastSummary.CompareAndSwap(0, now)
		return
	}
	window := time.Duration(now - last)
	if window < traceSummaryPeriod {
		return
	}
	if !c.traceLastSummary.CompareAndSwap(last, now) {
		return
	}
	deliveries := c.traceDeliveries.Load()
	c.logger.Info("trace: feed: consumer summary",
		"window_s", int64(window.Seconds()),
		"deliveries_total", deliveries,
		"deliveries_per_s", float64(deliveries-c.traceLastDeliveries.Swap(deliveries))/window.Seconds(),
		"system_messages_total", c.traceSystemMsgs.Load(),
		"bytes_total", c.traceBytes.Load(),
		"admit_blocked_ms", c.traceAdmitNS.Swap(0)/int64(time.Millisecond),
		"admit_blocked_max_ms", c.traceAdmitMaxNS.Swap(0)/int64(time.Millisecond),
		"reopens_total", c.traceReopens.Load(),
		"unsettled", c.unsettled(),
		"prefetch", c.prefetch)
}

// unsettled reports deliveries handed downstream whose ack/nack has not
// fired yet — the broker-side in-flight window.
func (c *ChannelConsumer) unsettled() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.unsettledN
}
