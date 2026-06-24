// SPDX-License-Identifier: AGPL-3.0-only

// Package cleanup deletes the app-created HA state objects that `helm uninstall` cannot remove on
// its own — the leader-election Lease and the watermark-checkpoint ConfigMap. The binary creates
// both at runtime (not the chart), so Helm never tracks them; without this they are orphaned in the
// namespace after an uninstall. The chart's post-delete hook runs `aip-oi -cleanup`, which calls Run.
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

// Run deletes the leader-election Lease and (unless retainCheckpoint) the checkpoint ConfigMap in
// namespace ns. A missing object is treated as success (idempotent — the app may never have become
// leader, or a prior run already cleaned up). The lease is ALWAYS removed: it is pure ephemeral
// coordination state. The checkpoint holds the durable watermark, so its retention is an explicit
// opt-in (mirroring Helm's deliberate non-deletion of PVCs on uninstall).
func Run(ctx context.Context, cs kubernetes.Interface, ns, leaseName, checkpointCM string, retainCheckpoint bool) error {
	var errs []error
	if err := cs.CoordinationV1().Leases(ns).Delete(ctx, leaseName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("delete lease %q: %w", leaseName, err))
	}
	if !retainCheckpoint {
		if err := cs.CoreV1().ConfigMaps(ns).Delete(ctx, checkpointCM, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("delete checkpoint configmap %q: %w", checkpointCM, err))
		}
	}
	return errors.Join(errs...)
}
