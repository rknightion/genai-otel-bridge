// SPDX-License-Identifier: AGPL-3.0-only

package lease

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

// TestRunReCampaignsOnLeadershipLapse [#110]: a leadership lapse WITHOUT a SIGTERM (the root ctx stays
// alive) must re-campaign in-process — onElected is entered a SECOND time — instead of Run returning
// nil and letting main exit 0 mid-pod-life. Symmetric with the dynamodb coordinator's acquire loop.
// The lapse is forced by blocking Lease renewals (update) after the first election; once leadership is
// lost the block clears so the in-process re-campaign can re-acquire.
func TestRunReCampaignsOnLeadershipLapse(t *testing.T) {
	cs := fake.NewSimpleClientset()
	var blockRenew atomic.Bool
	cs.PrependReactor("update", "leases", func(ktesting.Action) (bool, kruntime.Object, error) {
		if blockRenew.Load() {
			return true, nil, errors.New("renewal blocked (forced lapse)")
		}
		return false, nil, nil // fall through to the default tracker
	})

	c := New(cs, "genai-otel-bridge", "genai-otel-bridge-leader", "replica-a",
		time.Second, 700*time.Millisecond, 150*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var terms atomic.Int32
	reElected := make(chan struct{}, 1)
	returned := make(chan error, 1)
	go func() {
		returned <- c.Run(ctx, func(lc context.Context) {
			switch terms.Add(1) {
			case 1:
				// We are leading. Block renewals to force a genuine lapse, then wait for the leaderCtx
				// to be cancelled (leadership lost) and re-allow renewals so the re-campaign succeeds.
				blockRenew.Store(true)
				<-lc.Done()
				blockRenew.Store(false)
			default:
				// [#110] Re-campaigned in-process — Run did NOT return on the lapse.
				select {
				case reElected <- struct{}{}:
				default:
				}
				<-lc.Done()
			}
		})
	}()

	select {
	case <-reElected:
	case err := <-returned:
		t.Fatalf("Run returned on a leadership lapse (err=%v) instead of re-campaigning in-process", err)
	case <-time.After(15 * time.Second):
		t.Fatal("never re-campaigned after the leadership lapse")
	}

	// A real shutdown (ctx cancel) must still return promptly.
	cancel()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the root context was cancelled")
	}
}
