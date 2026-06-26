// SPDX-License-Identifier: AGPL-3.0-only

package lease

import (
	"context"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/rknightion/genai-otel-bridge/internal/coordinate"
)

func TestLeaseElectsAndCancelsOnStop(t *testing.T) {
	cs := fake.NewSimpleClientset()
	c := New(cs, "genai-otel-bridge", "genai-otel-bridge-leader", "replica-a", time.Second, 700*time.Millisecond, 150*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	elected := make(chan int64, 1)
	leaderCancelled := make(chan struct{}, 1)
	go c.Run(ctx, func(lc context.Context) {
		elected <- coordinate.EpochFromContext(lc)
		<-lc.Done()
		leaderCancelled <- struct{}{}
	})
	select {
	case <-elected:
	case <-time.After(5 * time.Second):
		t.Fatal("never elected leader against fake clientset")
	}
	cancel()
	select {
	case <-leaderCancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("leaderCtx not cancelled on stop")
	}
}
