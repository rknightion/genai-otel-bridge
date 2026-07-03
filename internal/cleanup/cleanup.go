// SPDX-License-Identifier: AGPL-3.0-only

// Package cleanup deletes the app-created HA state objects that `helm uninstall` cannot remove on
// its own — the leader-election Lease and the watermark-checkpoint ConfigMap. The binary creates
// both at runtime (not the chart), so Helm never tracks them; without this they are orphaned in the
// namespace after an uninstall. The chart's post-delete hook runs `genai-otel-bridge -cleanup`, which calls Run.
// Post-delete (not pre-delete) is deliberate: by then the Deployment is gone, so no live leader can
// re-create the lease/checkpoint after we delete them.
package cleanup

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Run deletes the leader-election Lease and the checkpoint ConfigMap in namespace ns. A missing object
// is treated as success (idempotent — the app may never have become leader, or a prior run already
// cleaned up).
//
// retainCheckpoint keeps BOTH objects. The Lease is NOT "pure ephemeral coordination state": its
// LeaseTransitions field is the durable epoch fence — the checkpoint stores that epoch in each
// watermark (Watermark.Epoch), and CheckMonotonic rejects any Save whose epoch < the stored epoch.
// Deleting only the Lease while keeping the checkpoint recreates the exact hazard the DynamoDB
// coordinator is designed around (dynamodb.go: "auto-deletion of the lock would reset fence ...
// permanently fencing all writes"): a reinstall's fresh Lease starts at LeaseTransitions=nil ⇒ epoch 0,
// which is < the retained checkpoint's epoch N≥1, so every commit is fenced and the watermark stalls
// (with duplicate log re-emits) until enough post-reinstall leadership transitions accrue past N. [#33]
//
// Retaining the Lease alongside the checkpoint keeps the two durable halves of the HA state
// consistent: the epoch domain continues monotonically across the reinstall (the new leader's
// acquisition of the retained Lease increments LeaseTransitions to N+1 ≥ N), so the first commit
// advances instead of being fenced. This is the least-surprising, invariant-STRENGTHENING option
// (vs. zeroing the stored epoch, which would reset the fence domain and couple cleanup to the
// backend record format). retainCheckpoint therefore means "retain the durable resume state",
// mirroring Helm's deliberate non-deletion of PVCs on uninstall.
func Run(ctx context.Context, cs kubernetes.Interface, ns, leaseName, checkpointCM string, retainCheckpoint bool) error {
	if retainCheckpoint {
		// Keep both durable halves (checkpoint watermark + its epoch fence in the Lease) for resume.
		return nil
	}
	var errs []error
	if err := cs.CoordinationV1().Leases(ns).Delete(ctx, leaseName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("delete lease %q: %w", leaseName, err))
	}
	if err := cs.CoreV1().ConfigMaps(ns).Delete(ctx, checkpointCM, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("delete checkpoint configmap %q: %w", checkpointCM, err))
	}
	return errors.Join(errs...)
}
