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

package controller

import (
	"context"
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// agentSlice mirrors metalEndpoints for the relay-mode "<isvc>-agent"
// EndpointSlice the 1b metal-agent writes: named and labelled for the agent
// Service, managed-by metal-agent, port 8443, with the ingress SPKI pin
// annotation when pin is non-empty.
func agentSlice(isvc, heartbeat, pin string) *discoveryv1.EndpointSlice {
	name := agentServiceName(isvc)
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels: map[string]string{
				"kubernetes.io/service-name":     name,
				inferencev1alpha1.LabelManagedBy: inferencev1alpha1.ManagedByMetalAgent,
			},
			Annotations: map[string]string{},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{{
			Addresses:  []string{"192.0.2.10"},
			Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)},
		}},
		Ports: []discoveryv1.EndpointPort{{
			Name:     ptr.To("https"),
			Port:     ptr.To(inferencev1alpha1.MetalAgentServicePort),
			Protocol: ptr.To(corev1.ProtocolTCP),
		}},
	}
	if heartbeat != "" {
		slice.Annotations[inferencev1alpha1.AnnotationAgentHeartbeat] = heartbeat
	}
	if pin != "" {
		slice.Annotations[inferencev1alpha1.AnnotationAgentIngressSPKI] = pin
	}
	return slice
}

// sliceListFailingClient fails List for EndpointSlices of one Service name
// (matched by the kubernetes.io/service-name label selector) and passes every
// other call through.
type sliceListFailingClient struct {
	client.Client
	serviceName string
}

var errInjectedSliceList = errors.New("injected EndpointSlice List failure")

func (c *sliceListFailingClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*discoveryv1.EndpointSliceList); ok {
		lo := &client.ListOptions{}
		lo.ApplyOptions(opts)
		target := labels.Set{
			"kubernetes.io/service-name":     c.serviceName,
			inferencev1alpha1.LabelManagedBy: inferencev1alpha1.ManagedByMetalAgent,
		}
		if lo.LabelSelector != nil && lo.LabelSelector.Matches(target) {
			return errInjectedSliceList
		}
	}
	return c.Client.List(ctx, list, opts...)
}

// deploymentUpdateConflictClient fails every Deployment Update with an
// optimistic-lock Conflict, as the API server does when the Deployment
// controller wrote the object between our Get and Update, and counts the
// attempts. Every other call passes through.
type deploymentUpdateConflictClient struct {
	client.Client
	attempts int
}

func (c *deploymentUpdateConflictClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if dep, ok := obj.(*appsv1.Deployment); ok {
		c.attempts++
		return apierrors.NewConflict(appsv1.Resource("deployments"), dep.Name,
			errors.New("the object has been modified; please apply your changes to the latest version and try again"))
	}
	return c.Client.Update(ctx, obj, opts...)
}

// updateCountingClient counts Deployment and Service Updates and passes every
// call through. resourceVersion alone cannot prove a pass skipped its Update:
// the API server drops an Update whose object is unchanged after defaulting
// without bumping resourceVersion, yet that Update still carries the stale
// cached resourceVersion that conflicts with the Deployment controller.
type updateCountingClient struct {
	client.Client
	deployments, services int
}

func (c *updateCountingClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	switch obj.(type) {
	case *appsv1.Deployment:
		c.deployments++
	case *corev1.Service:
		c.services++
	}
	return c.Client.Update(ctx, obj, opts...)
}

var _ = Describe("metal relay mode", func() {
	const (
		namespace = "default"
		pinA      = "cGluQS1zcGtpLXNoYTI1Ni1wbGFjZWhvbGRlci0zMmI="
		pinB      = "cGluQi1zcGtpLXNoYTI1Ni1wbGFjZWhvbGRlci0zMmI="
	)

	var (
		ctx        context.Context
		reconciler *InferenceServiceReconciler
		recorder   *events.FakeRecorder
	)

	// drainEvents empties the FakeRecorder channel into a slice of
	// "<type> <reason> <note>" strings.
	drainEvents := func() []string {
		var out []string
		for {
			select {
			case e := <-recorder.Events:
				out = append(out, e)
			default:
				return out
			}
		}
	}

	now := func() string { return time.Now().UTC().Format(time.RFC3339) }
	tenMinutesAgo := func() string { return time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339) }

	create := func(obj client.Object) {
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), obj) })
	}

	newISVC := func(name string) *inferencev1alpha1.InferenceService {
		replicas := int32(1)
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: name + "-model",
				Replicas: &replicas,
			},
		}
		create(isvc)
		return isvc
	}

	// cleanupRelay removes what reconcileRelay creates; envtest runs no
	// garbage collector, so owner references do not cascade.
	cleanupRelay := func(name string) {
		DeferCleanup(func() {
			bg := context.Background()
			_ = k8sClient.Delete(bg, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: relayDeploymentName(name), Namespace: namespace}})
			_ = k8sClient.Delete(bg, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: sanitizeDNSName(name), Namespace: namespace}})
		})
	}

	relayEnv := func(dep *appsv1.Deployment, name string) string {
		for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
			if e.Name == name {
				return e.Value
			}
		}
		return ""
	}

	BeforeEach(func() {
		ctx = context.Background()
		recorder = events.NewFakeRecorder(100)
		reconciler = &InferenceServiceReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: recorder,
		}
	})

	Context("decideMetalMode", func() {
		It("stays legacy without an agent ingress slice", func() {
			isvc := newISVC("relay-mode-legacy")
			create(metalEndpoints(isvc.Name, now()))

			mode, pin, err := reconciler.decideMetalMode(ctx, isvc)
			Expect(err).NotTo(HaveOccurred())
			Expect(mode).To(Equal(metalModeLegacy))
			Expect(pin).To(BeEmpty())
		})

		It("enters relay mode on a fresh pinned agent slice", func() {
			isvc := newISVC("relay-mode-pinned")
			create(agentSlice(isvc.Name, now(), pinA))

			mode, pin, err := reconciler.decideMetalMode(ctx, isvc)
			Expect(err).NotTo(HaveOccurred())
			Expect(mode).To(Equal(metalModeRelay))
			Expect(pin).To(Equal(pinA))
		})

		It("ignores an agent slice without a pin", func() {
			isvc := newISVC("relay-mode-nopin")
			create(agentSlice(isvc.Name, now(), ""))

			mode, _, err := reconciler.decideMetalMode(ctx, isvc)
			Expect(err).NotTo(HaveOccurred())
			Expect(mode).To(Equal(metalModeLegacy))
		})

		It("prefers relay when both are fresh", func() {
			isvc := newISVC("relay-mode-both")
			create(metalEndpoints(isvc.Name, now()))
			create(agentSlice(isvc.Name, now(), pinA))

			mode, pin, err := reconciler.decideMetalMode(ctx, isvc)
			Expect(err).NotTo(HaveOccurred())
			Expect(mode).To(Equal(metalModeRelay))
			Expect(pin).To(Equal(pinA))
		})

		It("falls back to legacy when the agent slice is stale and legacy is fresh", func() {
			isvc := newISVC("relay-mode-rollback")
			create(agentSlice(isvc.Name, tenMinutesAgo(), pinA))
			create(metalEndpoints(isvc.Name, now()))
			// Even an existing relay Deployment must not win over a fresh
			// legacy slice (rule 2 precedes rule 3).
			dep := reconciler.newRelayDeployment(isvc, pinA)
			Expect(setControllerReferenceUnblocked(isvc, dep, k8sClient.Scheme())).To(Succeed())
			create(dep)

			mode, _, err := reconciler.decideMetalMode(ctx, isvc)
			Expect(err).NotTo(HaveOccurred())
			Expect(mode).To(Equal(metalModeLegacy))
		})

		It("keeps relay mode while the agent is offline", func() {
			isvc := newISVC("relay-mode-offline")
			create(agentSlice(isvc.Name, tenMinutesAgo(), pinB))
			dep := reconciler.newRelayDeployment(isvc, pinA)
			Expect(setControllerReferenceUnblocked(isvc, dep, k8sClient.Scheme())).To(Succeed())
			create(dep)

			mode, pin, err := reconciler.decideMetalMode(ctx, isvc)
			Expect(err).NotTo(HaveOccurred())
			Expect(mode).To(Equal(metalModeRelay))
			Expect(pin).To(Equal(pinA))
		})

		It("stays legacy while the agent is offline and no relay exists", func() {
			isvc := newISVC("relay-mode-offline-legacy")
			create(metalEndpoints(isvc.Name, tenMinutesAgo()))

			mode, _, err := reconciler.decideMetalMode(ctx, isvc)
			Expect(err).NotTo(HaveOccurred())
			Expect(mode).To(Equal(metalModeLegacy))
		})

		// A List failure must not read as "no slice": with a fresh legacy
		// slice and an owned relay Deployment, falling through to rule 3
		// would pick relay and set a selector on an agent-owned "<isvc>".
		DescribeTable("fails instead of keeping relay mode when a slice List errors",
			func(isvcName string, failFor func(string) string) {
				isvc := newISVC(isvcName)
				create(metalEndpoints(isvc.Name, now()))
				dep := reconciler.newRelayDeployment(isvc, pinA)
				Expect(setControllerReferenceUnblocked(isvc, dep, k8sClient.Scheme())).To(Succeed())
				create(dep)

				reconciler.Client = &sliceListFailingClient{Client: k8sClient, serviceName: failFor(isvc.Name)}
				mode, _, err := reconciler.decideMetalMode(ctx, isvc)
				Expect(err).To(MatchError(errInjectedSliceList))
				Expect(mode).NotTo(Equal(metalModeRelay))
			},
			Entry("agent slices", "relay-mode-listerr-agent", agentServiceName),
			Entry("legacy slices", "relay-mode-listerr-legacy", sanitizeDNSName),
		)

		DescribeTable("treats a malformed agent pin as no pin and warns",
			func(isvcName, badPin string) {
				isvc := newISVC(isvcName)
				create(agentSlice(isvc.Name, now(), badPin))

				mode, pin, err := reconciler.decideMetalMode(ctx, isvc)
				Expect(err).NotTo(HaveOccurred())
				Expect(mode).To(Equal(metalModeLegacy))
				Expect(pin).To(BeEmpty())
				Expect(drainEvents()).To(ContainElement(SatisfyAll(
					HavePrefix("Warning"), ContainSubstring("InvalidAgentIngressPin"))))
			},
			Entry("not base64", "relay-mode-badpin-text", "not-base64"),
			// base64 of 16 bytes: valid encoding, wrong digest length.
			Entry("16-byte digest", "relay-mode-badpin-short", "c2l4dGVlbi1ieXRlcy0xNg=="),
		)

		It("leaves relay mode when the offline relay Deployment carries a malformed pin", func() {
			isvc := newISVC("relay-mode-badpin-offline")
			dep := reconciler.newRelayDeployment(isvc, "c2l4dGVlbi1ieXRlcy0xNg==")
			Expect(setControllerReferenceUnblocked(isvc, dep, k8sClient.Scheme())).To(Succeed())
			create(dep)

			mode, pin, err := reconciler.decideMetalMode(ctx, isvc)
			Expect(err).NotTo(HaveOccurred())
			Expect(mode).To(Equal(metalModeLegacy))
			Expect(pin).To(BeEmpty())
			Expect(drainEvents()).To(ContainElement(ContainSubstring("InvalidAgentIngressPin")))
		})
	})

	It("newRelayDeployment", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "relay-build", Namespace: "team-a"},
		}

		dep := reconciler.newRelayDeployment(isvc, pinA)
		Expect(dep.Name).To(Equal("relay-build-relay"))
		Expect(dep.Namespace).To(Equal("team-a"))
		Expect(dep.Spec.Replicas).To(HaveValue(Equal(int32(1))))
		Expect(dep.Spec.RevisionHistoryLimit).To(HaveValue(Equal(int32(2))))

		selector := map[string]string{
			"app":                               "relay-build-relay",
			"inference.llmkube.dev/metal-relay": "relay-build",
		}
		Expect(dep.Spec.Selector.MatchLabels).To(Equal(selector))
		podLabels := dep.Spec.Template.Labels
		for k, v := range selector {
			Expect(podLabels).To(HaveKeyWithValue(k, v))
		}
		Expect(podLabels).To(HaveKeyWithValue("app.kubernetes.io/component", "metal-relay"))
		Expect(podLabels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "llmkube-controller"))
		// NetworkPolicy parity: pods carry the service label, the selector does not.
		Expect(podLabels).To(HaveKeyWithValue("inference.llmkube.dev/service", "relay-build"))
		Expect(dep.Spec.Selector.MatchLabels).NotTo(HaveKey("inference.llmkube.dev/service"))
		Expect(dep.Labels).NotTo(HaveKey("inference.llmkube.dev/service"))
		// A relay pod event maps back to its InferenceService, and pod Lists
		// that pair the label with app=<isvc> never match a relay pod.
		relayPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Labels: podLabels}}
		reqs := reconciler.findInferenceServiceForPod(ctx, relayPod)
		Expect(reqs).To(HaveLen(1))
		Expect(reqs[0].Name).To(Equal("relay-build"))
		Expect(podLabels["app"]).NotTo(Equal("relay-build"))

		pod := dep.Spec.Template.Spec
		Expect(pod.AutomountServiceAccountToken).To(HaveValue(BeFalse()))
		Expect(pod.SecurityContext).To(Equal(routerProxyPodSecurityContext()))
		Expect(pod.Containers).To(HaveLen(1))
		c := pod.Containers[0]
		Expect(c.Image).To(Equal(defaultRouterProxyImage))
		Expect(c.Args).To(Equal([]string{"--relay", "--listen", ":8080", "--metrics-bind-address", ":9090", "--log-format", "json"}))
		Expect(c.SecurityContext).To(Equal(routerProxyContainerSecurityContext()))
		Expect(c.Env).To(ConsistOf(
			corev1.EnvVar{Name: "RELAY_TARGET", Value: "team-a/relay-build"},
			corev1.EnvVar{Name: "RELAY_UPSTREAM", Value: "https://relay-build-agent.team-a.svc:8443"},
			corev1.EnvVar{Name: "RELAY_SPKI_PIN", Value: pinA},
			corev1.EnvVar{Name: "RELAY_TOKEN_FILE", Value: "/var/run/llmkube/relay/token"},
		))

		Expect(c.VolumeMounts).To(HaveLen(1))
		Expect(c.VolumeMounts[0].MountPath).To(Equal("/var/run/llmkube/relay"))
		Expect(c.VolumeMounts[0].ReadOnly).To(BeTrue())
		Expect(pod.Volumes).To(HaveLen(1))
		Expect(pod.Volumes[0].Name).To(Equal(c.VolumeMounts[0].Name))
		Expect(pod.Volumes[0].Secret).NotTo(BeNil())
		Expect(pod.Volumes[0].Secret.SecretName).To(Equal("llmkube-metal-relay"))

		Expect(c.ReadinessProbe).NotTo(BeNil())
		Expect(c.ReadinessProbe.HTTPGet).NotTo(BeNil())
		Expect(c.ReadinessProbe.HTTPGet.Path).To(Equal("/readyz"))
		Expect(c.ReadinessProbe.HTTPGet.Port).To(Equal(intstr.FromInt(9090)))
		Expect(c.LivenessProbe).NotTo(BeNil())
		Expect(c.LivenessProbe.HTTPGet).NotTo(BeNil())
		Expect(c.LivenessProbe.HTTPGet.Path).To(Equal("/livez"))
		Expect(c.LivenessProbe.HTTPGet.Port).To(Equal(intstr.FromInt(9090)))

		Expect(c.Resources.Requests.Cpu().String()).To(Equal("10m"))
		Expect(c.Resources.Requests.Memory().String()).To(Equal("32Mi"))
		Expect(c.Resources.Limits.Memory().String()).To(Equal("128Mi"))

		reconciler.RelayImage = "registry.example/relay:v1"
		Expect(reconciler.newRelayDeployment(isvc, pinA).Spec.Template.Spec.Containers[0].Image).
			To(Equal("registry.example/relay:v1"))
	})

	It("ensureRelaySecret creates a 64-hex token once", func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "relay-secret-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())

		Expect(reconciler.ensureRelaySecret(ctx, ns.Name)).To(Succeed())
		first := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "llmkube-metal-relay", Namespace: ns.Name}, first)).To(Succeed())
		Expect(string(first.Data["token"])).To(MatchRegexp(`^[0-9a-f]{64}$`))
		Expect(first.Labels).To(HaveKeyWithValue("llmkube.ai/managed-by", "llmkube-controller"))

		Expect(reconciler.ensureRelaySecret(ctx, ns.Name)).To(Succeed())
		list := &corev1.SecretList{}
		Expect(k8sClient.List(ctx, list, client.InNamespace(ns.Name))).To(Succeed())
		Expect(list.Items).To(HaveLen(1))
		Expect(list.Items[0].Data["token"]).To(Equal(first.Data["token"]))
	})

	It("ensureRelaySecret refuses an existing Secret the controller did not create", func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "relay-secret-planted-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		planted := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "llmkube-metal-relay", Namespace: ns.Name},
			Data:       map[string][]byte{"token": []byte("known-to-the-attacker")},
		}
		Expect(k8sClient.Create(ctx, planted)).To(Succeed())

		err := reconciler.ensureRelaySecret(ctx, ns.Name)
		var unmanaged *relaySecretNotManagedError
		Expect(errors.As(err, &unmanaged)).To(BeTrue(), "want *relaySecretNotManagedError, got %v", err)
		Expect(err.Error()).To(SatisfyAll(
			ContainSubstring(ns.Name+"/llmkube-metal-relay"),
			ContainSubstring("llmkube.ai/managed-by=llmkube-controller"),
			ContainSubstring("Delete it")))
		Expect(err.Error()).NotTo(ContainSubstring("known-to-the-attacker"))

		got := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(planted), got)).To(Succeed())
		Expect(got.ResourceVersion).To(Equal(planted.ResourceVersion), "the planted Secret must be left untouched")

		By("a label with another value is not the controller's either")
		got.Labels = map[string]string{"llmkube.ai/managed-by": "someone-else"}
		Expect(k8sClient.Update(ctx, got)).To(Succeed())
		Expect(errors.As(reconciler.ensureRelaySecret(ctx, ns.Name), &unmanaged)).To(BeTrue())
	})

	It("ensureRelaySecret keeps using a controller Secret rotated in place", func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "relay-secret-rotated-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		Expect(reconciler.ensureRelaySecret(ctx, ns.Name)).To(Succeed())

		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "llmkube-metal-relay", Namespace: ns.Name}, secret)).To(Succeed())
		secret.Data["token"] = []byte("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
		Expect(k8sClient.Update(ctx, secret)).To(Succeed())

		Expect(reconciler.ensureRelaySecret(ctx, ns.Name)).To(Succeed())
		got := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(secret), got)).To(Succeed())
		Expect(got.Data["token"]).To(Equal(secret.Data["token"]))
	})

	Context("reconcileRelay", func() {
		It("adopts an agent-written Service in place", func() {
			isvc := newISVC("relay-adopt")
			cleanupRelay(isvc.Name)
			svc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      isvc.Name,
					Namespace: namespace,
					Labels: map[string]string{
						"app":                            isvc.Name,
						inferencev1alpha1.LabelManagedBy: inferencev1alpha1.ManagedByMetalAgent,
						"llmkube.ai/inference-service":   isvc.Name,
					},
				},
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{{Name: "http", Port: 8080, Protocol: corev1.ProtocolTCP}},
				},
			}
			Expect(k8sClient.Create(ctx, svc)).To(Succeed())
			clusterIP := svc.Spec.ClusterIP
			Expect(clusterIP).NotTo(BeEmpty())

			_, err := reconciler.reconcileRelay(ctx, isvc, pinA)
			Expect(err).NotTo(HaveOccurred())

			got := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvc.Name, Namespace: namespace}, got)).To(Succeed())
			Expect(got.Spec.ClusterIP).To(Equal(clusterIP))
			Expect(got.Spec.Selector).To(Equal(relayPodSelector(isvc.Name)))
			Expect(got.Spec.Ports).To(HaveLen(1))
			Expect(got.Spec.Ports[0].Name).To(Equal("http"))
			Expect(got.Spec.Ports[0].Port).To(Equal(int32(8080)))
			Expect(got.Spec.Ports[0].TargetPort).To(Equal(intstr.FromInt(8080)))
			Expect(metav1.IsControlledBy(got, isvc)).To(BeTrue())
			Expect(got.Labels).NotTo(HaveKey(inferencev1alpha1.LabelManagedBy))

			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: relayDeploymentName(isvc.Name), Namespace: namespace}, dep)).To(Succeed())
			Expect(metav1.IsControlledBy(dep, isvc)).To(BeTrue())

			evs := drainEvents()
			Expect(evs).To(ContainElement(SatisfyAll(HavePrefix("Normal"), ContainSubstring("ServiceAdopted"))))
			Expect(evs).To(ContainElement(SatisfyAll(HavePrefix("Normal"), ContainSubstring("RelayCreated"))))

			By("a second pass neither re-adopts nor re-creates")
			_, err = reconciler.reconcileRelay(ctx, isvc, pinA)
			Expect(err).NotTo(HaveOccurred())
			evs = drainEvents()
			Expect(evs).NotTo(ContainElement(ContainSubstring("ServiceAdopted")))
			Expect(evs).NotTo(ContainElement(ContainSubstring("RelayCreated")))
		})

		// agentService is the Service a metal-agent writes: ClusterIP,
		// selectorless, managed-by metal-agent and the inference-service label
		// naming the InferenceService it serves.
		agentService := func(name string, port int32) *corev1.Service {
			return &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: namespace,
					Labels: map[string]string{
						inferencev1alpha1.LabelManagedBy: inferencev1alpha1.ManagedByMetalAgent,
						"llmkube.ai/inference-service":   name,
					},
				},
				Spec: corev1.ServiceSpec{
					Type:  corev1.ServiceTypeClusterIP,
					Ports: []corev1.ServicePort{{Name: "http", Port: port, Protocol: corev1.ProtocolTCP}},
				},
			}
		}

		newEndpointISVC := func(name string, endpoint *inferencev1alpha1.EndpointSpec) *inferencev1alpha1.InferenceService {
			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: name + "-model",
					Replicas: &replicas,
					Endpoint: endpoint,
				},
			}
			create(isvc)
			return isvc
		}

		It("adopts an agent ClusterIP Service as NodePort when spec.endpoint asks for one", func() {
			// A fixed port in the upper NodePort range no other spec uses.
			nodePort := int32(32611)
			isvc := newEndpointISVC("relay-adopt-nodeport", &inferencev1alpha1.EndpointSpec{
				Type:     "NodePort",
				NodePort: &nodePort,
			})
			cleanupRelay(isvc.Name)
			svc := agentService(isvc.Name, 8080)
			Expect(k8sClient.Create(ctx, svc)).To(Succeed())
			clusterIP := svc.Spec.ClusterIP
			Expect(clusterIP).NotTo(BeEmpty())

			_, err := reconciler.reconcileRelay(ctx, isvc, pinA)
			Expect(err).NotTo(HaveOccurred())

			got := &corev1.Service{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(svc), got)).To(Succeed())
			Expect(got.Spec.Type).To(Equal(corev1.ServiceTypeNodePort))
			Expect(got.Spec.Ports).To(HaveLen(1))
			Expect(got.Spec.Ports[0].NodePort).To(Equal(nodePort))
			Expect(got.Spec.ClusterIP).To(Equal(clusterIP))

			By("a steady-state pass keeps the node port")
			_, err = reconciler.reconcileRelay(ctx, isvc, pinA)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(svc), got)).To(Succeed())
			Expect(got.Spec.Ports[0].NodePort).To(Equal(nodePort))
		})

		It("moves an adopted Service to spec.endpoint.port", func() {
			isvc := newEndpointISVC("relay-adopt-port", &inferencev1alpha1.EndpointSpec{Port: 9000})
			cleanupRelay(isvc.Name)
			svc := agentService(isvc.Name, 8080)
			Expect(k8sClient.Create(ctx, svc)).To(Succeed())

			_, err := reconciler.reconcileRelay(ctx, isvc, pinA)
			Expect(err).NotTo(HaveOccurred())

			got := &corev1.Service{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(svc), got)).To(Succeed())
			Expect(got.Spec.Ports).To(HaveLen(1))
			Expect(got.Spec.Ports[0].Port).To(Equal(int32(9000)))
			Expect(got.Spec.Ports[0].TargetPort).To(Equal(intstr.FromInt(8080)))
		})

		// expectServiceConverged runs reconcileRelayService again with nothing
		// changed and asserts it sent no Service Update. A no-op Update is
		// dropped by the API server without bumping resourceVersion (see
		// updateCountingClient's doc comment above), so resourceVersion alone
		// cannot tell a converged pass apart from one that redundantly
		// re-submits the same content; count the Update call too.
		expectServiceConverged := func(isvc *inferencev1alpha1.InferenceService, key client.ObjectKey) {
			before := &corev1.Service{}
			Expect(k8sClient.Get(ctx, key, before)).To(Succeed())
			counting := &updateCountingClient{Client: k8sClient}
			reconciler.Client = counting
			Expect(reconciler.reconcileRelayService(ctx, isvc)).To(Succeed())
			reconciler.Client = k8sClient
			Expect(counting.services).To(BeZero(), "a converged second pass must not Update the Service")

			after := &corev1.Service{}
			Expect(k8sClient.Get(ctx, key, after)).To(Succeed())
			Expect(after.ResourceVersion).To(Equal(before.ResourceVersion), "a converged second pass must not Update the Service")
		}

		// relayedService builds the Service the way an earlier relay pass
		// left it: constructService for the given endpoint, on the relay
		// selector and listener, controlled by isvc.
		relayedService := func(isvc *inferencev1alpha1.InferenceService, endpoint *inferencev1alpha1.EndpointSpec) *corev1.Service {
			shaped := isvc.DeepCopy()
			shaped.Spec.Endpoint = endpoint
			svc := reconciler.constructService(shaped)
			svc.Spec.Selector = relayPodSelector(isvc.Name)
			for i := range svc.Spec.Ports {
				svc.Spec.Ports[i].TargetPort = intstr.FromInt(relayListenPort)
			}
			Expect(setControllerReferenceUnblocked(isvc, svc, k8sClient.Scheme())).To(Succeed())
			return svc
		}

		It("creates the relay Service with the externalTrafficPolicy spec.endpoint asks for", func() {
			isvc := newEndpointISVC("relay-create-etp", &inferencev1alpha1.EndpointSpec{
				Type:                  "NodePort",
				ExternalTrafficPolicy: "Local",
			})
			cleanupRelay(isvc.Name)

			Expect(reconciler.reconcileRelayService(ctx, isvc)).To(Succeed())

			key := types.NamespacedName{Name: sanitizeDNSName(isvc.Name), Namespace: namespace}
			got := &corev1.Service{}
			Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
			Expect(got.Spec.ExternalTrafficPolicy).To(Equal(corev1.ServiceExternalTrafficPolicyLocal))
			Expect(got.Spec.Selector).To(Equal(relayPodSelector(isvc.Name)))
			expectServiceConverged(isvc, key)
		})

		It("keeps a Local externalTrafficPolicy carried over from the pod-backed Service", func() {
			endpoint := &inferencev1alpha1.EndpointSpec{
				Type:                  "NodePort",
				ExternalTrafficPolicy: "Local",
			}
			isvc := newEndpointISVC("relay-keep-etp", endpoint)
			cleanupRelay(isvc.Name)
			svc := relayedService(isvc, endpoint)
			Expect(svc.Spec.ExternalTrafficPolicy).To(Equal(corev1.ServiceExternalTrafficPolicyLocal))
			Expect(k8sClient.Create(ctx, svc)).To(Succeed())

			Expect(reconciler.reconcileRelayService(ctx, isvc)).To(Succeed())

			got := &corev1.Service{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(svc), got)).To(Succeed())
			Expect(got.Spec.ExternalTrafficPolicy).To(Equal(corev1.ServiceExternalTrafficPolicyLocal))
			expectServiceConverged(isvc, client.ObjectKeyFromObject(svc))
		})

		It("reverts a Local externalTrafficPolicy to Cluster when spec.endpoint drops it", func() {
			isvc := newEndpointISVC("relay-revert-etp", &inferencev1alpha1.EndpointSpec{Type: "NodePort"})
			cleanupRelay(isvc.Name)
			svc := relayedService(isvc, &inferencev1alpha1.EndpointSpec{
				Type:                  "NodePort",
				ExternalTrafficPolicy: "Local",
			})
			Expect(k8sClient.Create(ctx, svc)).To(Succeed())

			Expect(reconciler.reconcileRelayService(ctx, isvc)).To(Succeed())

			got := &corev1.Service{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(svc), got)).To(Succeed())
			Expect(got.Spec.ExternalTrafficPolicy).To(Equal(corev1.ServiceExternalTrafficPolicyCluster))
			expectServiceConverged(isvc, client.ObjectKeyFromObject(svc))
		})

		It("adopts an agent ClusterIP Service as a Local LoadBalancer when spec.endpoint asks for one", func() {
			isvc := newEndpointISVC("relay-adopt-lb-etp", &inferencev1alpha1.EndpointSpec{
				Type:                  "LoadBalancer",
				ExternalTrafficPolicy: "Local",
			})
			cleanupRelay(isvc.Name)
			svc := agentService(isvc.Name, 8080)
			Expect(k8sClient.Create(ctx, svc)).To(Succeed())

			_, err := reconciler.reconcileRelay(ctx, isvc, pinA)
			Expect(err).NotTo(HaveOccurred())

			got := &corev1.Service{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(svc), got)).To(Succeed())
			Expect(got.Spec.Type).To(Equal(corev1.ServiceTypeLoadBalancer))
			Expect(got.Spec.ExternalTrafficPolicy).To(Equal(corev1.ServiceExternalTrafficPolicyLocal))
			Expect(got.Spec.HealthCheckNodePort).NotTo(BeZero(),
				"the API server allocates a health check node port for a Local LoadBalancer")
			expectServiceConverged(isvc, client.ObjectKeyFromObject(svc))
		})

		// expectSteadyState runs a second reconcileRelay with nothing changed
		// and asserts neither relay object was written: an unconditional
		// Update from a cached copy races the Deployment controller's own
		// writes and surfaces as an optimistic-lock conflict.
		expectSteadyState := func(isvc *inferencev1alpha1.InferenceService) {
			depKey := types.NamespacedName{Name: relayDeploymentName(isvc.Name), Namespace: namespace}
			svcKey := types.NamespacedName{Name: sanitizeDNSName(isvc.Name), Namespace: namespace}
			depBefore, svcBefore := &appsv1.Deployment{}, &corev1.Service{}
			Expect(k8sClient.Get(ctx, depKey, depBefore)).To(Succeed())
			Expect(k8sClient.Get(ctx, svcKey, svcBefore)).To(Succeed())

			counting := &updateCountingClient{Client: k8sClient}
			reconciler.Client = counting
			_, err := reconciler.reconcileRelay(ctx, isvc, pinA)
			reconciler.Client = k8sClient
			Expect(err).NotTo(HaveOccurred())
			Expect(counting.deployments).To(BeZero(), "no-op pass must not Update the relay Deployment")
			Expect(counting.services).To(BeZero(), "no-op pass must not Update the Service")

			depAfter, svcAfter := &appsv1.Deployment{}, &corev1.Service{}
			Expect(k8sClient.Get(ctx, depKey, depAfter)).To(Succeed())
			Expect(k8sClient.Get(ctx, svcKey, svcAfter)).To(Succeed())
			Expect(depAfter.ResourceVersion).To(Equal(depBefore.ResourceVersion), "no-op pass must not Update the relay Deployment")
			Expect(svcAfter.ResourceVersion).To(Equal(svcBefore.ResourceVersion), "no-op pass must not Update the Service")
		}

		It("leaves an adopted ClusterIP Service and the relay Deployment untouched on a no-op pass", func() {
			isvc := newISVC("relay-steady-clusterip")
			cleanupRelay(isvc.Name)
			// The agent wrote another port; adoption moves it to 8080 first.
			Expect(k8sClient.Create(ctx, agentService(isvc.Name, 8000))).To(Succeed())

			_, err := reconciler.reconcileRelay(ctx, isvc, pinA)
			Expect(err).NotTo(HaveOccurred())
			expectSteadyState(isvc)
		})

		It("leaves a NodePort Service with an allocated node port untouched on a no-op pass", func() {
			isvc := newEndpointISVC("relay-steady-nodeport", &inferencev1alpha1.EndpointSpec{Type: "NodePort"})
			cleanupRelay(isvc.Name)
			Expect(k8sClient.Create(ctx, agentService(isvc.Name, 8080))).To(Succeed())

			_, err := reconciler.reconcileRelay(ctx, isvc, pinA)
			Expect(err).NotTo(HaveOccurred())
			got := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvc.Name, Namespace: namespace}, got)).To(Succeed())
			Expect(got.Spec.Ports[0].NodePort).NotTo(BeZero(), "the API allocates a node port")
			expectSteadyState(isvc)
		})

		It("creates the Service when absent", func() {
			isvc := newISVC("relay-create")
			cleanupRelay(isvc.Name)

			available, err := reconciler.reconcileRelay(ctx, isvc, pinA)
			Expect(err).NotTo(HaveOccurred())
			// envtest runs no Deployment controller, so nothing becomes available.
			Expect(available).To(BeFalse())

			got := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvc.Name, Namespace: namespace}, got)).To(Succeed())
			Expect(got.Spec.Selector).To(Equal(relayPodSelector(isvc.Name)))
			Expect(got.Spec.Ports).To(HaveLen(1))
			Expect(got.Spec.Ports[0].Port).To(Equal(int32(8080)))
			Expect(got.Spec.Ports[0].TargetPort).To(Equal(intstr.FromInt(8080)))
			Expect(metav1.IsControlledBy(got, isvc)).To(BeTrue())
			Expect(got.Labels).NotTo(HaveKey(inferencev1alpha1.LabelManagedBy))
		})

		It("refuses a foreign Service", func() {
			isvc := newISVC("relay-foreign")
			cleanupRelay(isvc.Name)
			svc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      isvc.Name,
					Namespace: namespace,
					Labels:    map[string]string{"app": "someone-else"},
				},
				Spec: corev1.ServiceSpec{
					Selector: map[string]string{"app": "someone-else"},
					Ports:    []corev1.ServicePort{{Name: "web", Port: 80, TargetPort: intstr.FromInt(3000), Protocol: corev1.ProtocolTCP}},
				},
			}
			Expect(k8sClient.Create(ctx, svc)).To(Succeed())
			before := &corev1.Service{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(svc), before)).To(Succeed())

			_, err := reconciler.reconcileRelay(ctx, isvc, pinA)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("is not controlled by this InferenceService or written by its metal-agent"))

			after := &corev1.Service{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(svc), after)).To(Succeed())
			Expect(after.ResourceVersion).To(Equal(before.ResourceVersion))
			Expect(after.Spec.Selector).To(Equal(map[string]string{"app": "someone-else"}))
			Expect(after.OwnerReferences).To(BeEmpty())
		})

		It("pin change updates the Deployment env", func() {
			isvc := newISVC("relay-pin-change")
			cleanupRelay(isvc.Name)

			_, err := reconciler.reconcileRelay(ctx, isvc, pinA)
			Expect(err).NotTo(HaveOccurred())
			dep := &appsv1.Deployment{}
			key := types.NamespacedName{Name: relayDeploymentName(isvc.Name), Namespace: namespace}
			Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
			Expect(relayEnv(dep, "RELAY_SPKI_PIN")).To(Equal(pinA))

			_, err = reconciler.reconcileRelay(ctx, isvc, pinB)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
			Expect(relayEnv(dep, "RELAY_SPKI_PIN")).To(Equal(pinB))
		})
	})

	It("teardownRelay deletes only an owned relay Deployment", func() {
		owned := newISVC("relay-teardown-owned")
		foreign := newISVC("relay-teardown-foreign")

		ownedDep := reconciler.newRelayDeployment(owned, pinA)
		Expect(setControllerReferenceUnblocked(owned, ownedDep, k8sClient.Scheme())).To(Succeed())
		create(ownedDep)
		foreignDep := reconciler.newRelayDeployment(foreign, pinA)
		create(foreignDep)

		Expect(reconciler.teardownRelay(ctx, owned)).To(Succeed())
		Expect(reconciler.teardownRelay(ctx, foreign)).To(Succeed())
		Expect(drainEvents()).To(ConsistOf(SatisfyAll(HavePrefix("Normal"), ContainSubstring("RelayRemoved"))))

		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(ownedDep), &appsv1.Deployment{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "owned relay Deployment should be deleted, got %v", err)
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(foreignDep), &appsv1.Deployment{})).To(Succeed())

		// A missing Deployment is not an error, and nothing was removed.
		Expect(reconciler.teardownRelay(ctx, owned)).To(Succeed())
		Expect(drainEvents()).To(BeEmpty())
	})

	Context("full Reconcile", func() {
		// metalModel creates the Ready metal Model newISVC refers to.
		metalModel := func(isvcName string) {
			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: isvcName + "-model", Namespace: namespace},
				Spec: inferencev1alpha1.ModelSpec{
					Source:   "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "metal"},
				},
			}
			create(model)
			model.Status.Phase = PhaseReady
			Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())
		}

		reconcileISVC := func(name string) *inferencev1alpha1.InferenceService {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: name, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())
			got := &inferencev1alpha1.InferenceService{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, got)).To(Succeed())
			return got
		}

		// serveThroughRelay drives name into relay mode on a fresh pinned
		// agent slice and marks the relay available, returning the slice.
		serveThroughRelay := func(name string) *discoveryv1.EndpointSlice {
			metalModel(name)
			newISVC(name)
			cleanupRelay(name)
			slice := agentSlice(name, now(), pinA)
			create(slice)

			got := reconcileISVC(name)
			Expect(drainEvents()).To(ContainElement(ContainSubstring("RelayCreated")))
			dep := &appsv1.Deployment{}
			depKey := types.NamespacedName{Name: relayDeploymentName(name), Namespace: namespace}
			Expect(k8sClient.Get(ctx, depKey, dep)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &corev1.Service{})).To(Succeed())
			// envtest runs no Deployment controller, so the relay has no
			// available replica yet and the service must not be Ready.
			Expect(got.Status.Phase).NotTo(Equal(PhaseReady))
			Expect(got.Status.ReadyReplicas).To(BeZero())

			// The API rejects availableReplicas above replicas or
			// readyReplicas, so report one replica throughout.
			dep.Status.Replicas = 1
			dep.Status.ReadyReplicas = 1
			dep.Status.AvailableReplicas = 1
			Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())

			got = reconcileISVC(name)
			Expect(got.Status.Phase).To(Equal(PhaseReady))
			Expect(got.Status.ReadyReplicas).To(Equal(int32(1)))
			Expect(drainEvents()).NotTo(ContainElement(ContainSubstring("RelayCreated")))
			return slice
		}

		It("metal ISVC in relay mode becomes Ready only when the relay is available", func() {
			serveThroughRelay("relay-full-ready")
		})

		It("rollback hands the Service back", func() {
			name := "relay-full-rollback"
			slice := serveThroughRelay(name)

			create(metalEndpoints(name, now()))
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(slice), slice)).To(Succeed())
			slice.Annotations[inferencev1alpha1.AnnotationAgentHeartbeat] = tenMinutesAgo()
			Expect(k8sClient.Update(ctx, slice)).To(Succeed())

			svcKey := types.NamespacedName{Name: name, Namespace: namespace}
			before := &corev1.Service{}
			Expect(k8sClient.Get(ctx, svcKey, before)).To(Succeed())

			reconcileISVC(name)
			Expect(drainEvents()).To(ContainElement(ContainSubstring("RelayRemoved")))

			err := k8sClient.Get(ctx, types.NamespacedName{Name: relayDeploymentName(name), Namespace: namespace}, &appsv1.Deployment{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "relay Deployment should be deleted on hand-back, got %v", err)
			after := &corev1.Service{}
			Expect(k8sClient.Get(ctx, svcKey, after)).To(Succeed())
			Expect(after.ResourceVersion).To(Equal(before.ResourceVersion), "legacy reconcile must not touch <isvc>")

			By("the rolled-back agent clears the selector; the controller leaves it cleared")
			after.Spec.Selector = nil
			Expect(k8sClient.Update(ctx, after)).To(Succeed())
			reconcileISVC(name)
			Expect(drainEvents()).NotTo(ContainElement(ContainSubstring("RelayRemoved")))
			final := &corev1.Service{}
			Expect(k8sClient.Get(ctx, svcKey, final)).To(Succeed())
			Expect(final.Spec.Selector).To(BeNil())
		})

		It("surfaces a foreign Service as a reconcile error and never goes Ready", func() {
			name := "relay-full-foreign"
			metalModel(name)
			newISVC(name)
			cleanupRelay(name)
			create(agentSlice(name, now(), pinA))
			create(&corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{"app": "someone-else"}},
				Spec: corev1.ServiceSpec{
					Selector: map[string]string{"app": "someone-else"},
					Ports:    []corev1.ServicePort{{Name: "web", Port: 80, Protocol: corev1.ProtocolTCP}},
				},
			})

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: name, Namespace: namespace},
			})
			Expect(err).To(MatchError(ContainSubstring("is not controlled by this InferenceService or written by its metal-agent")))
			Expect(drainEvents()).To(ContainElement(SatisfyAll(
				HavePrefix("Warning"), ContainSubstring("RelayReconcileFailed"),
				ContainSubstring("is not controlled by this InferenceService or written by its metal-agent"))))
			got := &inferencev1alpha1.InferenceService{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, got)).To(Succeed())
			Expect(got.Status.Phase).NotTo(Equal(PhaseReady))
		})

		It("requeues quietly when a relay Deployment Update conflicts", func() {
			name := "relay-full-conflict"
			slice := serveThroughRelay(name)

			By("a pin change makes the next pass Update the relay Deployment")
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(slice), slice)).To(Succeed())
			slice.Annotations[inferencev1alpha1.AnnotationAgentIngressSPKI] = pinB
			Expect(k8sClient.Update(ctx, slice)).To(Succeed())

			conflicting := &deploymentUpdateConflictClient{Client: k8sClient}
			reconciler.Client = conflicting
			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: name, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred(), "a lost optimistic-lock race is not a reconcile error")
			Expect(conflicting.attempts).To(Equal(1), "the Update must have been attempted and conflicted")
			Expect(result.RequeueAfter).To(Equal(relayConflictRequeueAfter))
			Expect(drainEvents()).NotTo(ContainElement(HavePrefix("Warning")))

			By("the requeued pass converges once the conflict clears")
			reconciler.Client = k8sClient
			reconcileISVC(name)
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: relayDeploymentName(name), Namespace: namespace}, dep)).To(Succeed())
			Expect(relayEnv(dep, relayPinEnv)).To(Equal(pinB))
			Expect(drainEvents()).NotTo(ContainElement(HavePrefix("Warning")))
		})

		It("requeues with the error when the mode decision cannot list slices", func() {
			name := "relay-full-listerr"
			metalModel(name)
			newISVC(name)
			reconciler.Client = &sliceListFailingClient{Client: k8sClient, serviceName: agentServiceName(name)}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: name, Namespace: namespace},
			})
			Expect(err).To(MatchError(errInjectedSliceList))
		})

		// Runs in its own namespace: the relay Secret is per namespace and
		// the other relay specs share "default".
		It("refuses an unmanaged relay Secret without touching the relay Deployment", func() {
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "relay-unmanaged-"}}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())
			name := "relay-unmanaged"
			nsKey := types.NamespacedName{Name: name, Namespace: ns.Name}
			secretKey := types.NamespacedName{Name: "llmkube-metal-relay", Namespace: ns.Name}
			depKey := types.NamespacedName{Name: relayDeploymentName(name), Namespace: ns.Name}
			// envtest deletes neither namespaces nor owned objects, and specs
			// such as the federation summary count InferenceServices in every
			// namespace, so remove everything this spec leaves behind.
			DeferCleanup(func() {
				bg := context.Background()
				for _, obj := range []client.Object{
					&inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns.Name}},
					&inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: name + "-model", Namespace: ns.Name}},
					&discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: agentServiceName(name), Namespace: ns.Name}},
					&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: depKey.Name, Namespace: ns.Name}},
					&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: sanitizeDNSName(name), Namespace: ns.Name}},
					&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretKey.Name, Namespace: ns.Name}},
				} {
					_ = k8sClient.Delete(bg, obj)
				}
			})

			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-model", Namespace: ns.Name},
				Spec: inferencev1alpha1.ModelSpec{
					Source:   "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "metal"},
				},
			}
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			model.Status.Phase = PhaseReady
			Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())
			replicas := int32(1)
			Expect(k8sClient.Create(ctx, &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns.Name},
				Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: name + "-model", Replicas: &replicas},
			})).To(Succeed())
			slice := agentSlice(name, now(), pinA)
			slice.Namespace = ns.Name
			Expect(k8sClient.Create(ctx, slice)).To(Succeed())

			plant := func() {
				Expect(k8sClient.Create(ctx, &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: secretKey.Name, Namespace: ns.Name},
					Data:       map[string][]byte{"token": []byte("known-to-the-attacker")},
				})).To(Succeed())
			}
			reconcileNS := func() (reconcile.Result, *inferencev1alpha1.InferenceService) {
				result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nsKey})
				Expect(err).NotTo(HaveOccurred())
				got := &inferencev1alpha1.InferenceService{}
				Expect(k8sClient.Get(ctx, nsKey, got)).To(Succeed())
				return result, got
			}
			expectRefused := func(result reconcile.Result, got *inferencev1alpha1.InferenceService) {
				GinkgoHelper()
				Expect(drainEvents()).To(ContainElement(SatisfyAll(
					HavePrefix("Warning"), ContainSubstring("RelaySecretNotManaged"),
					ContainSubstring(ns.Name+"/llmkube-metal-relay"), ContainSubstring("Delete it"))))
				Expect(got.Status.SchedulingStatus).To(Equal("RelaySecretNotManaged"))
				Expect(got.Status.SchedulingMessage).To(ContainSubstring("Delete it"))
				Expect(got.Status.Phase).NotTo(Equal(PhaseReady))
				Expect(result.RequeueAfter).To(BeNumerically(">", 0))
				Expect(result.RequeueAfter).To(BeNumerically("<=", relaySecretNotManagedRequeueAfter))
			}

			By("a planted Secret blocks the relay before its Deployment exists")
			plant()
			result, got := reconcileNS()
			expectRefused(result, got)
			err := k8sClient.Get(ctx, depKey, &appsv1.Deployment{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "no relay Deployment may mount a planted Secret, got %v", err)

			By("deleting it lets the controller create its own Secret and the relay")
			Expect(k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretKey.Name, Namespace: ns.Name}})).To(Succeed())
			_, got = reconcileNS()
			Expect(drainEvents()).To(ContainElement(ContainSubstring("RelayCreated")))
			Expect(got.Status.SchedulingStatus).NotTo(Equal("RelaySecretNotManaged"))
			own := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, secretKey, own)).To(Succeed())
			Expect(own.Labels).To(HaveKeyWithValue("llmkube.ai/managed-by", "llmkube-controller"))
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, depKey, dep)).To(Succeed())
			Expect(relayEnv(dep, relayPinEnv)).To(Equal(pinA))

			By("a Secret replaced by an unlabelled one stops relay updates")
			Expect(k8sClient.Delete(ctx, own)).To(Succeed())
			plant()
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(slice), slice)).To(Succeed())
			slice.Annotations[inferencev1alpha1.AnnotationAgentIngressSPKI] = pinB
			Expect(k8sClient.Update(ctx, slice)).To(Succeed())
			result, got = reconcileNS()
			expectRefused(result, got)
			after := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, depKey, after)).To(Succeed())
			Expect(after.ResourceVersion).To(Equal(dep.ResourceVersion), "the relay Deployment must not be updated")
			Expect(relayEnv(after, relayPinEnv)).To(Equal(pinA))

			By("deleting it again goes straight to Ready and drops the diagnosis")
			// envtest runs no Deployment controller: report the relay available.
			after.Status.Replicas = 1
			after.Status.ReadyReplicas = 1
			after.Status.AvailableReplicas = 1
			Expect(k8sClient.Status().Update(ctx, after)).To(Succeed())
			Expect(k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretKey.Name, Namespace: ns.Name}})).To(Succeed())
			_, got = reconcileNS()
			Expect(got.Status.Phase).To(Equal(PhaseReady))
			Expect(got.Status.SchedulingStatus).To(BeEmpty())
			Expect(got.Status.SchedulingMessage).To(BeEmpty())
			Expect(k8sClient.Get(ctx, depKey, after)).To(Succeed())
			Expect(relayEnv(after, relayPinEnv)).To(Equal(pinB))
		})
	})

	It("removes the relay when the model moves off Metal", func() {
		name := "relay-to-cuda"
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-model", Namespace: namespace},
			Spec:       inferencev1alpha1.ModelSpec{Source: "https://example.com/model.gguf"},
		}
		create(model)
		model.Status.Phase = PhaseReady
		Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())
		isvc := newISVC(name)
		cleanupRelay(name)
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: sanitizeDNSName(name), Namespace: namespace}})
		})
		dep := reconciler.newRelayDeployment(isvc, pinA)
		Expect(setControllerReferenceUnblocked(isvc, dep, k8sClient.Scheme())).To(Succeed())
		Expect(k8sClient.Create(ctx, dep)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: name, Namespace: namespace},
		})
		Expect(err).NotTo(HaveOccurred())

		err = k8sClient.Get(ctx, client.ObjectKeyFromObject(dep), &appsv1.Deployment{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "relay Deployment should be removed off Metal, got %v", err)
		Expect(drainEvents()).To(ContainElement(ContainSubstring("RelayRemoved")))
	})

	Context("findInferenceServiceForEndpoints", func() {
		requestNames := func(reqs []reconcile.Request) []string {
			names := make([]string, 0, len(reqs))
			for _, r := range reqs {
				Expect(r.Namespace).To(Equal(namespace))
				names = append(names, r.Name)
			}
			return names
		}

		It("the EndpointSlice mapper enqueues the ISVC for an agent slice", func() {
			reqs := reconciler.findInferenceServiceForEndpoints(ctx, agentSlice("relay-mapper", now(), pinA))
			Expect(requestNames(reqs)).To(ConsistOf("relay-mapper-agent", "relay-mapper"))
		})

		It("still enqueues an ISVC literally named with the -agent suffix for its legacy slice", func() {
			reqs := reconciler.findInferenceServiceForEndpoints(ctx, metalEndpoints("chat-agent", now()))
			Expect(requestNames(reqs)).To(ConsistOf("chat-agent", "chat"))
		})

		It("does not strip -agent from a slice not written by the metal-agent", func() {
			slice := agentSlice("relay-mapper-mirrored", now(), "")
			delete(slice.Labels, inferencev1alpha1.LabelManagedBy)
			reqs := reconciler.findInferenceServiceForEndpoints(ctx, slice)
			Expect(requestNames(reqs)).To(ConsistOf("relay-mapper-mirrored-agent"))
		})

		It("maps a legacy slice to its ISVC only", func() {
			reqs := reconciler.findInferenceServiceForEndpoints(ctx, metalEndpoints("relay-mapper-legacy", now()))
			Expect(requestNames(reqs)).To(ConsistOf("relay-mapper-legacy"))
		})
	})
})
