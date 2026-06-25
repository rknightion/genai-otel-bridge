// SPDX-License-Identifier: AGPL-3.0-only

package lease

import (
	"context"
	goruntime "runtime"
	"sync"
	"testing"
	"time"

	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestReviewRunCanReturnBeforeAsyncOnStartedLeadingStarts(t *testing.T) {
	oldProcs := goruntime.GOMAXPROCS(1)
	defer goruntime.GOMAXPROCS(oldProcs)

	ctx, cancel := context.WithCancel(context.Background())
	cs := fake.NewSimpleClientset()
	created := make(chan struct{})
	var createdOnce sync.Once
	cs.PrependReactor("create", "leases", func(action ktesting.Action) (bool, kruntime.Object, error) {
		createdOnce.Do(func() { close(created) })
		cancel()
		return false, nil, nil
	})
	c := New(cs, "decant", "decant-leader", "replica-a", time.Second, 700*time.Millisecond, 150*time.Millisecond)
	started := make(chan struct{})
	release := make(chan struct{})
	returned := make(chan struct{})
	go func() {
		_ = c.Run(ctx, func(context.Context) {
			close(started)
			<-release
		})
		close(returned)
	}()

	select {
	case <-started:
		select {
		case <-returned:
			close(release)
			t.Fatal("Run returned before the blocking OnStartedLeading callback completed")
		case <-time.After(100 * time.Millisecond):
		}
		close(release)
		select {
		case <-returned:
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not return after callback completed")
		}
	case <-returned:
		select {
		case <-created:
			t.Fatal("Run returned after acquiring the Lease but before OnStartedLeading completed")
		default:
		}
	case <-time.After(5 * time.Second):
		t.Fatal("neither OnStartedLeading nor Run returned")
	}
	select {
	case <-returned:
	default:
		close(release)
	}
}
