package types

import "testing"

func TestProducerDownReason_ConnectionDownMapsToStatusReason(t *testing.T) {
	if got := ConnectionDownProducerDownReason.ToProducerStatusReason(); got != ConnectionDownProducerStatusReason {
		t.Fatalf("ToProducerStatusReason = %v, want ConnectionDown", got)
	}
}
