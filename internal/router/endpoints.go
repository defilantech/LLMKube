/*
Copyright 2026.

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
	"fmt"

	discoveryv1 "k8s.io/api/discovery/v1"
)

// EndpointTarget is the Pod and node behind one endpoint URL, for metric
// labels. Both are empty when the slice does not name them.
type EndpointTarget struct {
	Pod  string
	Node string
}

// ReadyEndpoint is one ready endpoint URL and its target.
type ReadyEndpoint struct {
	URL string
	EndpointTarget
}

// ReadyEndpointURLs returns one "http://<address>:<port>" per address of every
// endpoint the slices do not explicitly mark unready, in slice order. A nil
// ready condition counts as ready, per the EndpointSlice API. The controller
// (status, drain) and the proxy (dispatch) share this rule so they agree on
// which pods are serving.
func ReadyEndpointURLs(slices []*discoveryv1.EndpointSlice, port int32) []string {
	eps := ReadyEndpoints(slices, port)
	urls := make([]string, 0, len(eps))
	for _, ep := range eps {
		urls = append(urls, ep.URL)
	}
	if len(urls) == 0 {
		return nil
	}
	return urls
}

// ReadyEndpoints is ReadyEndpointURLs with each URL's Pod and node.
func ReadyEndpoints(slices []*discoveryv1.EndpointSlice, port int32) []ReadyEndpoint {
	var out []ReadyEndpoint
	for _, s := range slices {
		for j := range s.Endpoints {
			ep := &s.Endpoints[j]
			if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
				continue
			}
			var t EndpointTarget
			if ep.TargetRef != nil && ep.TargetRef.Kind == "Pod" {
				t.Pod = ep.TargetRef.Name
			}
			if ep.NodeName != nil {
				t.Node = *ep.NodeName
			}
			for _, addr := range ep.Addresses {
				out = append(out, ReadyEndpoint{URL: fmt.Sprintf("http://%s:%d", addr, port), EndpointTarget: t})
			}
		}
	}
	return out
}
