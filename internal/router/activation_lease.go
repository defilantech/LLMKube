/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package router

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SwapCoordinator decides which router-proxy replica drives a ModelPool swap.
// With spec.proxy.replicas > 1 (or two pods overlapping in a rollout) the
// in-process Activator mutex only serializes swaps inside one replica, so a
// per-pool lease makes activation a real single-writer: the owner scales the
// member, every other replica defers and waits for it (#1477).
//
// It is an interface so the swap policy stays unit-testable without a live API
// server; production injects LeaseCoordinator.
type SwapCoordinator interface {
	// Acquire claims the pool's swap lease and keeps it renewed for as long as
	// the returned release is outstanding. owned reports whether this replica
	// may write the member; a non-owner must not, and receives a nil release.
	// The release is idempotent and clears the hold outright, so the next swap
	// need not wait out the lease duration.
	Acquire(ctx context.Context, poolKey string) (release func(), owned bool, err error)
}

const (
	// defaultLeaseDuration bounds how quickly a dead owner frees the pool: a
	// hold older than this without a renewal is taken over. It does NOT bound
	// the hold itself; a cold load routinely outlives any fixed TTL
	// (ModelPool.spec.swapBudget defaults to 300s), which is why the lease is
	// renewed for the life of the hold rather than granted a long TTL.
	defaultLeaseDuration = 120 * time.Second
	leaseNamePrefix      = "llmkube-pool-"
)

// ErrActivationLeaseUnavailable signals that the swap lease could not be read
// or written, so this replica cannot know whether it owns the swap. The request
// fails closed rather than risking a double activation, but the error is kept
// distinct from a backend failure: an unavailable lease is a transient control
// plane condition the caller may retry, not a serving outage.
var ErrActivationLeaseUnavailable = errors.New("activation lease unavailable")

// LeaseCoordinator implements SwapCoordinator over coordination.k8s.io Leases.
// A lease held by another live replica means a swap is already being driven, so
// this replica defers rather than issuing a competing member write.
type LeaseCoordinator struct {
	client    client.Client
	namespace string
	identity  string
	duration  time.Duration

	// nowFn is overridable in tests.
	nowFn func() time.Time
}

// NewLeaseCoordinator builds a LeaseCoordinator whose holder identity is the
// process hostname, which is the pod name under Kubernetes.
func NewLeaseCoordinator(cl client.Client, namespace string) *LeaseCoordinator {
	identity, err := os.Hostname()
	if err != nil || identity == "" {
		identity = "unknown"
	}
	return &LeaseCoordinator{
		client:    cl,
		namespace: namespace,
		identity:  identity,
		duration:  defaultLeaseDuration,
		nowFn:     time.Now,
	}
}

// renewInterval is how often a held lease refreshes its RenewTime. A third of
// the duration means two consecutive failed renewals still cannot let a second
// replica take over a live hold.
func (c *LeaseCoordinator) renewInterval() time.Duration {
	if c.duration <= 0 {
		return defaultLeaseDuration / 3
	}
	return c.duration / 3
}

// Acquire claims the pool's swap lease for this replica and starts renewing it,
// or reports that another live replica holds it. A lease whose RenewTime plus
// duration has lapsed is taken over, so a crashed owner does not wedge the pool
// forever. A write that loses the resourceVersion race reports not-owned rather
// than retrying: the winner owns the swap.
func (c *LeaseCoordinator) Acquire(ctx context.Context, poolKey string) (func(), bool, error) {
	name := leaseNamePrefix + sanitizeLeaseName(poolKey)
	owned, err := c.claim(ctx, name)
	if err != nil || !owned {
		return nil, false, err
	}

	// The release must still run when the caller's context is done (the swap
	// goroutine's context is cancelled when the last waiter gives up), so
	// detach from its cancellation while keeping its values.
	releaseCtx := context.WithoutCancel(ctx)
	renewCtx, cancelRenew := context.WithCancel(releaseCtx)
	var once sync.Once
	release := func() {
		once.Do(func() {
			cancelRenew()
			c.release(releaseCtx, name)
		})
	}
	go c.renewLoop(renewCtx, name)
	return release, true, nil
}

// renewLoop refreshes RenewTime until the hold is released. Without it a second
// replica takes over mid-swap once the TTL lapses, and both replicas drive an
// activation: the double-write #1477 exists to close, reached by the ordinary
// slow-load path.
func (c *LeaseCoordinator) renewLoop(ctx context.Context, name string) {
	ticker := time.NewTicker(c.renewInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// A failed renewal is not fatal: the lease ages toward expiry and
			// the next tick retries, so a single API blip cannot hand the pool
			// to another replica.
			_ = c.renew(ctx, name)
		}
	}
}

// renew refreshes the lease's RenewTime while this replica still holds it. A
// lease taken over by another replica is left alone. The plain Update carries
// resourceVersion, so a concurrent write conflicts rather than clobbering.
func (c *LeaseCoordinator) renew(ctx context.Context, name string) error {
	lease := &coordinationv1.Lease{}
	if err := c.client.Get(ctx, types.NamespacedName{Namespace: c.namespace, Name: name}, lease); err != nil {
		return err
	}
	if ptr.Deref(lease.Spec.HolderIdentity, "") != c.identity {
		return nil
	}
	lease.Spec.RenewTime = &metav1.MicroTime{Time: c.nowFn()}
	lease.Spec.LeaseDurationSeconds = ptr.To(int32(c.duration.Seconds()))
	return c.client.Update(ctx, lease)
}

// claim performs the one-shot takeover-or-create decision behind Acquire.
func (c *LeaseCoordinator) claim(ctx context.Context, name string) (bool, error) {
	lease := &coordinationv1.Lease{}
	err := c.client.Get(ctx, types.NamespacedName{Namespace: c.namespace, Name: name}, lease)
	if apierrors.IsNotFound(err) {
		lease = &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.namespace},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       ptr.To(c.identity),
				LeaseDurationSeconds: ptr.To(int32(c.duration.Seconds())),
				RenewTime:            &metav1.MicroTime{Time: c.nowFn()},
			},
		}
		if cerr := c.client.Create(ctx, lease); cerr != nil {
			if apierrors.IsAlreadyExists(cerr) {
				return false, nil
			}
			return false, fmt.Errorf("%w: create lease %s: %w", ErrActivationLeaseUnavailable, name, cerr)
		}
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w: get lease %s: %w", ErrActivationLeaseUnavailable, name, err)
	}
	if holder := ptr.Deref(lease.Spec.HolderIdentity, ""); holder != "" && holder != c.identity {
		if !c.expired(lease) {
			return false, nil
		}
	}

	// We hold it, nobody holds it, or the previous holder lapsed: take or renew.
	lease.Spec.HolderIdentity = ptr.To(c.identity)
	lease.Spec.LeaseDurationSeconds = ptr.To(int32(c.duration.Seconds()))
	lease.Spec.RenewTime = &metav1.MicroTime{Time: c.nowFn()}
	if uerr := c.client.Update(ctx, lease); uerr != nil {
		if apierrors.IsConflict(uerr) {
			return false, nil
		}
		return false, fmt.Errorf("%w: renew lease %s: %w", ErrActivationLeaseUnavailable, name, uerr)
	}
	return true, nil
}

// release clears this replica's hold so the next swap can be driven without
// waiting out the duration. A lease now held by another replica is left alone.
func (c *LeaseCoordinator) release(ctx context.Context, name string) {
	lease := &coordinationv1.Lease{}
	if err := c.client.Get(ctx, types.NamespacedName{Namespace: c.namespace, Name: name}, lease); err != nil {
		return
	}
	if ptr.Deref(lease.Spec.HolderIdentity, "") != c.identity {
		return
	}
	lease.Spec.HolderIdentity = ptr.To("")
	lease.Spec.RenewTime = &metav1.MicroTime{Time: c.nowFn().Add(-2 * c.duration)}
	_ = c.client.Update(ctx, lease)
}

// expired reports whether the lease's hold has lapsed by its RenewTime and
// duration.
func (c *LeaseCoordinator) expired(lease *coordinationv1.Lease) bool {
	if lease.Spec.RenewTime == nil {
		return true
	}
	d := time.Duration(ptr.Deref(lease.Spec.LeaseDurationSeconds, 0)) * time.Second
	if d <= 0 {
		d = defaultLeaseDuration
	}
	return c.nowFn().After(lease.Spec.RenewTime.Add(d))
}

// sanitizeLeaseName folds an arbitrary pool key into a DNS-1123 subdomain so it
// can name a Lease.
func sanitizeLeaseName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "pool"
	}
	return out
}
