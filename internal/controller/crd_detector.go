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

package controller

import (
	"fmt"
	"sync"

	"github.com/go-logr/logr"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// crdDetector is a generic CRD-presence gate for integrations that depend on
// external CRDs. The Envoy AI Gateway integration (InferenceService and
// ModelRouter gateway reconcilers) and the Pyrra SLO integration each embed
// one to self-gate on their required CRDs being installed.
//
// It caches a POSITIVE detection only: once the required CRDs are seen
// registered we stop re-checking. While absent we re-check on every call so
// an integration installed after the operator starts is picked up without a
// restart. The mutex guards the cache for concurrent reconciles; loggedAbsent
// keeps the disabled message to a single log line.
//
// A transient discovery error (not a missing kind) is surfaced to the caller so
// it requeues rather than caching a false negative (the install-order footgun:
// caching "absent" forever would never recover when the integration is
// installed after the operator).
type crdDetector struct {
	// name identifies the integration in user-facing log/error messages
	// (for example "gateway" or "pyrra-slo"). Set at construction.
	name string

	// gvks are the kinds that must all be registered for the integration to
	// activate. Set at construction.
	gvks []schema.GroupVersionKind

	mu           sync.Mutex
	present      bool
	loggedAbsent bool
}

// newCRDDetector builds a detector for the given integration name and
// required kinds. name identifies the integration in the disabled log line
// and any discovery error, so pick something a cluster operator recognizes
// (for example "gateway" or "pyrra-slo").
func newCRDDetector(name string, gvks []schema.GroupVersionKind) *crdDetector {
	return &crdDetector{name: name, gvks: gvks}
}

// Present reports whether all required CRDs are registered. A positive
// result is cached; while absent it re-checks on every call. A transient
// discovery error (not a missing kind) is returned so the caller requeues
// instead of caching a false negative. The disabled message is logged once.
func (d *crdDetector) Present(c client.Client, log logr.Logger) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.present {
		return true, nil
	}

	present, err := d.detect(c)
	if err != nil {
		return false, err
	}
	if present {
		d.present = true
		return true, nil
	}
	if !d.loggedAbsent {
		log.Info(fmt.Sprintf("%s integration disabled (CRDs not installed)", d.name))
		d.loggedAbsent = true
	}
	return false, nil
}

// detect queries the RESTMapper for each required kind. A missing kind
// (NoKindMatchError) means "not installed" and returns (false, nil). Any other
// error is treated as transient (for example a lazy-discovery hiccup) and
// returned so the caller requeues rather than caching a false negative.
func (d *crdDetector) detect(c client.Client) (bool, error) {
	mapper := c.RESTMapper()
	for _, gvk := range d.gvks {
		if _, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version); err != nil {
			if apimeta.IsNoMatchError(err) {
				return false, nil
			}
			return false, fmt.Errorf("check %s CRD %s: %w", d.name, gvk.Kind, err)
		}
	}
	return true, nil
}
