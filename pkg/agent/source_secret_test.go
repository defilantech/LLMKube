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

package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// modelSourceCredentialsLabels is the marker a Secret's owner sets to let a
// Model's sourceSecretRef name it on the Metal path.
func modelSourceCredentialsLabels() map[string]string {
	return map[string]string{inferencev1alpha1.LabelModelSourceCredentials: "true"}
}

// hfCredentialSecret is the HF_TOKEN sourceSecretRef "creds" in "default".
func hfCredentialSecret(labels map[string]string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "default", Labels: labels},
		Data:       map[string][]byte{"HF_TOKEN": []byte("hf_secret")},
	}
}

// secretGetCounter is an interceptor that counts every Get of a Secret (full
// or metadata-only) and, while fail holds an error, returns it instead.
type secretGetCounter struct {
	gets atomic.Int32
	fail atomic.Pointer[error]
}

func (c *secretGetCounter) funcs() *interceptor.Funcs {
	return &interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			if isSecretObject(obj) {
				c.gets.Add(1)
				if p := c.fail.Load(); p != nil {
					return *p
				}
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	}
}

func isSecretObject(obj client.Object) bool {
	switch o := obj.(type) {
	case *corev1.Secret:
		return true
	case *metav1.PartialObjectMetadata:
		return o.Kind == "Secret"
	}
	return false
}

// realExecutorFixture is refusalFixture with the agent's real llama.cpp
// executor registered (buildExecutors) in place of the recording one, so a
// start that is not refused really resolves credentials and downloads. Memory
// admission is enforced (refusalFixture leaves it in warn mode), so a size
// probe that fails surfaces as MemoryCheckFailed instead of proceeding.
func realExecutorFixture(
	t *testing.T, cfg MetalAgentConfig, funcs *interceptor.Funcs, objs ...runtimeObject,
) (*MetalAgent, *record.FakeRecorder) {
	t.Helper()
	if cfg.MemoryProvider == nil {
		cfg.MemoryProvider = &mockMemoryProvider{totalBytes: 128 * 1024 * 1024 * 1024}
		cfg.MemoryFraction = 0.75
	}
	a, _, rec := refusalFixtureWithInterceptor(t, cfg, funcs, objs...)
	a.memoryCheckWarnOnly = false
	a.config.LlamaServerBin = "/nonexistent/llama-server"
	a.config.AllowedDownloadHosts = []string{"127.0.0.1"}
	a.buildExecutors()
	return a, rec
}

// freshISVC re-reads the fixtures' InferenceService "svc", so an assertion
// sees the status the agent wrote and a second ensureProcess call sees the
// resourceVersion the first one's status write left behind.
func freshISVC(t *testing.T, a *MetalAgent) *inferencev1alpha1.InferenceService {
	t.Helper()
	cur := &inferencev1alpha1.InferenceService{}
	if err := a.config.K8sClient.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: "svc"}, cur); err != nil {
		t.Fatalf("get InferenceService svc: %v", err)
	}
	return cur
}

// isvcStatus is the agent-owned scheduling status freshISVC reads.
func isvcStatus(t *testing.T, a *MetalAgent) (reason, message string) {
	t.Helper()
	cur := freshISVC(t, a)
	return cur.Status.SchedulingStatus, cur.Status.SchedulingMessage
}

// A Model whose sourceSecretRef names a Secret without the
// model-source-credentials label is refused with SourceSecretNotAllowed (status
// and Warning Event, with the exact kubectl label command), and the Secret's
// credentials never reach the object store or Hugging Face: no request at all
// arrives there. The s3:// case is an unsized Model, so memory admission would
// size it with a signed HEAD through the Secret; the refusal must come first,
// which is also why memory admission is enforced here: were the pre-flight
// check missing, the executor-level gate would fail the size probe and the
// status would read MemoryCheckFailed instead.
func TestEnsureProcess_RefusesUnlabeledSourceSecret(t *testing.T) {
	wantCommand := "kubectl label secret creds -n default " +
		inferencev1alpha1.LabelModelSourceCredentials + "=true"
	for _, tc := range []string{"s3", "hf"} {
		t.Run(tc, func(t *testing.T) {
			srv, hits := signedS3Server(t)
			model := refusalModel()
			model.Spec.SourceSecretRef = &corev1.LocalObjectReference{Name: "creds"}
			var secret *corev1.Secret
			if tc == "s3" {
				model.Spec.Source = s3ObjectSource
				secret = s3CredentialSecret("default", "creds", srv.URL, nil)
			} else {
				pinHFHost(t, srv.URL)
				model.Spec.Source = "hf://org/repo/m.gguf"
				model.Status.Size = "1.0 GiB"
				secret = hfCredentialSecret(nil)
			}
			isvc := refusalISVC("svc")
			a, events := realExecutorFixture(t, MetalAgentConfig{}, nil, isvc, model, secret)

			err := a.ensureProcess(context.Background(), isvc)
			if err == nil || !strings.Contains(err.Error(), EventReasonSourceSecretNotAllowed) {
				t.Fatalf("ensureProcess error = %v, want %s", err, EventReasonSourceSecretNotAllowed)
			}
			reason, msg := isvcStatus(t, a)
			if reason != EventReasonSourceSecretNotAllowed {
				t.Errorf("SchedulingStatus = %q, want %q", reason, EventReasonSourceSecretNotAllowed)
			}
			if !strings.Contains(msg, wantCommand) {
				t.Errorf("SchedulingMessage = %q, want it to give the command %q", msg, wantCommand)
			}
			if got := strings.Join(drainEvents(events), "\n"); !strings.Contains(got,
				"Warning "+EventReasonSourceSecretNotAllowed) {
				t.Errorf("events = %q, want a Warning %s", got, EventReasonSourceSecretNotAllowed)
			}
			if n := hits.Load(); n != 0 {
				t.Errorf("the download origin received %d requests, want 0", n)
			}
		})
	}
}

// The relay token Secret is never model-source credentials: naming it is
// refused even when it carries the marker and even with the escape flag, and
// the refusal is decided by name, so the agent does not read it at all.
func TestEnsureProcess_RefusesRelaySourceSecret(t *testing.T) {
	srv, hits := signedS3Server(t)
	relay := s3CredentialSecret("default", inferencev1alpha1.MetalRelaySecretName, srv.URL,
		modelSourceCredentialsLabels())
	model := refusalModel()
	model.Spec.Source = s3ObjectSource
	model.Spec.SourceSecretRef = &corev1.LocalObjectReference{Name: inferencev1alpha1.MetalRelaySecretName}
	isvc := refusalISVC("svc")
	counter := &secretGetCounter{}
	a, events := realExecutorFixture(t, MetalAgentConfig{AllowUnlabeledSourceSecrets: true},
		counter.funcs(), isvc, model, relay)

	err := a.ensureProcess(context.Background(), isvc)
	if err == nil || !strings.Contains(err.Error(), EventReasonSourceSecretNotAllowed) {
		t.Fatalf("ensureProcess error = %v, want %s", err, EventReasonSourceSecretNotAllowed)
	}
	reason, msg := isvcStatus(t, a)
	if reason != EventReasonSourceSecretNotAllowed || !strings.Contains(msg, "relay token Secret") {
		t.Errorf("status = (%q, %q), want %s naming the relay token Secret",
			reason, msg, EventReasonSourceSecretNotAllowed)
	}
	if got := strings.Join(drainEvents(events), "\n"); !strings.Contains(got,
		"Warning "+EventReasonSourceSecretNotAllowed) {
		t.Errorf("events = %q, want a Warning %s", got, EventReasonSourceSecretNotAllowed)
	}
	if n := counter.gets.Load(); n != 0 {
		t.Errorf("the relay Secret was read %d times, want 0", n)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the object store received %d requests, want 0", n)
	}
}

// With --allow-unlabeled-source-secrets an unmarked Secret is still used, so
// existing Models keep working through the upgrade, and every start that
// relies on it emits a SourceSecretUnlabeled Warning Event naming the label
// command, so the operator can find the Secrets to mark.
func TestEnsureProcess_AllowUnlabeledSourceSecretsFlag(t *testing.T) {
	model := refusalModel()
	model.Spec.SourceSecretRef = &corev1.LocalObjectReference{Name: "creds"}
	isvc := refusalISVC("svc")
	a, ex, rec := refusalFixture(t, MetalAgentConfig{AllowUnlabeledSourceSecrets: true},
		isvc, model, hfCredentialSecret(nil))

	if err := a.ensureProcess(context.Background(), isvc); err != nil {
		t.Fatalf("ensureProcess with the escape flag: %v", err)
	}
	if ex.starts != 1 {
		t.Errorf("engine started %d times, want 1", ex.starts)
	}
	got := strings.Join(drainEvents(rec), "\n")
	if !strings.Contains(got, "Warning "+EventReasonSourceSecretUnlabeled) ||
		!strings.Contains(got, inferencev1alpha1.LabelModelSourceCredentials+"=true") {
		t.Errorf("events = %q, want a Warning %s with the label command", got, EventReasonSourceSecretUnlabeled)
	}
	if reason, _ := isvcStatus(t, a); reason == EventReasonSourceSecretNotAllowed {
		t.Errorf("SchedulingStatus = %q, want no refusal with the escape flag", reason)
	}
}

// The escape flag reaches the executor the agent builds: an unmarked s3
// Secret downloads with it and is refused, with nothing sent to the object
// store, without it.
func TestBuildExecutors_PlumbsAllowUnlabeledSourceSecrets(t *testing.T) {
	for _, allow := range []bool{false, true} {
		t.Run(fmt.Sprintf("allow=%v", allow), func(t *testing.T) {
			srv, hits := signedS3Server(t)
			a := sourceSecretTestAgent(t, "default", s3CredentialSecret("default", minioRefName, srv.URL, nil))
			a.config.AllowUnlabeledSourceSecrets = allow
			a.buildExecutors()
			exec := a.executors[runtimeLlamaServer].(*MetalExecutor)
			key := &types.NamespacedName{Namespace: "default", Name: minioRefName}

			path, err := exec.ensureModel(t.Context(), s3ObjectSource, "s3-model", key, "")
			if !allow {
				var notAllowed *SourceSecretNotAllowedError
				if !errors.As(err, &notAllowed) {
					t.Fatalf("ensureModel = %v, want a *SourceSecretNotAllowedError", err)
				}
				if n := hits.Load(); n != 0 {
					t.Errorf("the object store received %d requests, want 0", n)
				}
				return
			}
			if err != nil {
				t.Fatalf("ensureModel with the escape flag: %v", err)
			}
			if b, _ := os.ReadFile(path); string(b) != s3ObjectBody {
				t.Errorf("downloaded body = %q, want %q", b, s3ObjectBody)
			}
		})
	}
}

// A transient failure reading the Secret (here a 503 from the API server) is
// not a refusal: no status is written, no Event emitted, and once the API
// server recovers the next reconcile starts the service. A sticky refusal for
// a cause that clears on its own would leave the service down with a
// misleading reason.
func TestEnsureProcess_SourceSecretTransientErrorNotRefused(t *testing.T) {
	model := refusalModel()
	model.Spec.SourceSecretRef = &corev1.LocalObjectReference{Name: "creds"}
	isvc := refusalISVC("svc")
	counter := &secretGetCounter{}
	unavailable := error(apierrors.NewServiceUnavailable("etcd leader changed"))
	counter.fail.Store(&unavailable)
	a, ex, rec := refusalFixtureWithInterceptor(t, MetalAgentConfig{}, counter.funcs(),
		isvc, model, hfCredentialSecret(modelSourceCredentialsLabels()))

	err := a.ensureProcess(context.Background(), isvc)
	if err == nil || strings.Contains(err.Error(), EventReasonSourceSecretNotAllowed) {
		t.Fatalf("ensureProcess error = %v, want a plain retryable error, not a refusal", err)
	}
	if reason, _ := isvcStatus(t, a); reason != "" {
		t.Errorf("SchedulingStatus = %q after a transient read error, want empty", reason)
	}
	if got := drainEvents(rec); strings.Contains(strings.Join(got, "\n"), EventReasonSourceSecretNotAllowed) {
		t.Errorf("events = %q, want no %s", got, EventReasonSourceSecretNotAllowed)
	}
	if ex.starts != 0 {
		t.Errorf("engine started %d times during the outage, want 0", ex.starts)
	}

	counter.fail.Store(nil)
	if err := a.ensureProcess(context.Background(), freshISVC(t, a)); err != nil {
		t.Fatalf("ensureProcess after the API server recovered: %v", err)
	}
	if ex.starts != 1 {
		t.Errorf("engine started %d times after recovery, want 1", ex.starts)
	}
}

// A Secret the agent's RBAC does not let it read (Forbidden: a Secret outside
// a pinned resourceNames list) is refused with a message pointing at the pin,
// rather than retried silently forever, and the refusal lifts once the agent
// may read the Secret.
func TestEnsureProcess_ForbiddenSourceSecretRefused(t *testing.T) {
	model := refusalModel()
	model.Spec.SourceSecretRef = &corev1.LocalObjectReference{Name: "creds"}
	isvc := refusalISVC("svc")
	counter := &secretGetCounter{}
	forbidden := error(apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "creds",
		errors.New("not in resourceNames")))
	counter.fail.Store(&forbidden)
	a, ex, _ := refusalFixtureWithInterceptor(t, MetalAgentConfig{}, counter.funcs(),
		isvc, model, hfCredentialSecret(modelSourceCredentialsLabels()))

	err := a.ensureProcess(context.Background(), isvc)
	if err == nil || !strings.Contains(err.Error(), EventReasonSourceSecretNotAllowed) {
		t.Fatalf("ensureProcess error = %v, want %s", err, EventReasonSourceSecretNotAllowed)
	}
	if _, msg := isvcStatus(t, a); !strings.Contains(msg, "metalAgent.rbac.secretNames") {
		t.Errorf("SchedulingMessage = %q, want it to point at the pinned secret names", msg)
	}

	counter.fail.Store(nil)
	if err := a.ensureProcess(context.Background(), freshISVC(t, a)); err != nil {
		t.Fatalf("ensureProcess once the Secret is readable: %v", err)
	}
	if ex.starts != 1 {
		t.Errorf("engine started %d times, want 1", ex.starts)
	}
}

// A sourceSecretRef naming a Secret that does not exist is refused (an hf://
// download would otherwise go out without its token), and the refusal is not
// remembered: once the marked Secret is created the next reconcile starts.
func TestEnsureProcess_MissingSourceSecretRefusedUntilCreated(t *testing.T) {
	model := refusalModel()
	model.Spec.SourceSecretRef = &corev1.LocalObjectReference{Name: "creds"}
	isvc := refusalISVC("svc")
	a, ex, _ := refusalFixture(t, MetalAgentConfig{}, isvc, model)

	err := a.ensureProcess(context.Background(), isvc)
	if err == nil || !strings.Contains(err.Error(), EventReasonSourceSecretNotAllowed) {
		t.Fatalf("ensureProcess error = %v, want %s", err, EventReasonSourceSecretNotAllowed)
	}
	if _, msg := isvcStatus(t, a); !strings.Contains(msg, "does not exist") {
		t.Errorf("SchedulingMessage = %q, want it to say the Secret does not exist", msg)
	}

	if err := a.config.K8sClient.Create(context.Background(),
		hfCredentialSecret(modelSourceCredentialsLabels())); err != nil {
		t.Fatalf("create Secret: %v", err)
	}
	if err := a.ensureProcess(context.Background(), freshISVC(t, a)); err != nil {
		t.Fatalf("ensureProcess after the Secret was created: %v", err)
	}
	if ex.starts != 1 {
		t.Errorf("engine started %d times, want 1", ex.starts)
	}
}

// A *SourceSecretNotAllowedError from StartProcess (the Secret lost its marker
// or was deleted after the pre-flight check) is refused as
// SourceSecretNotAllowed, not logged as a plain start failure.
func TestEnsureProcess_SourceSecretErrorFromStartIsRefused(t *testing.T) {
	model := refusalModel()
	isvc := refusalISVC("svc")
	a, ex, rec := refusalFixture(t, MetalAgentConfig{}, isvc, model)
	ex.startErr = fmt.Errorf("failed to ensure model: %w",
		&SourceSecretNotAllowedError{Namespace: "default", Name: "creds", Why: "it is not marked"})

	err := a.ensureProcess(context.Background(), isvc)
	if err == nil || !strings.Contains(err.Error(), EventReasonSourceSecretNotAllowed) {
		t.Fatalf("ensureProcess error = %v, want %s", err, EventReasonSourceSecretNotAllowed)
	}
	if reason, _ := isvcStatus(t, a); reason != EventReasonSourceSecretNotAllowed {
		t.Errorf("SchedulingStatus = %q, want %q", reason, EventReasonSourceSecretNotAllowed)
	}
	if got := strings.Join(drainEvents(rec), "\n"); !strings.Contains(got,
		"Warning "+EventReasonSourceSecretNotAllowed) {
		t.Errorf("events = %q, want a Warning %s", got, EventReasonSourceSecretNotAllowed)
	}
}

// The executor checks the marker again at the point of use, so a download or
// size probe that bypasses the agent's pre-flight check still never uses an
// unmarked, missing or relay Secret, and sends nothing to the origin. The
// marked Secret is the control: the same call downloads.
func TestEnsureModel_SourceSecretGate(t *testing.T) {
	cases := []struct {
		name, secretName string
		labels           map[string]string
		create           bool
		wantRefused      bool
	}{
		{name: "marked", secretName: minioRefName, labels: modelSourceCredentialsLabels(), create: true},
		{name: "unmarked", secretName: minioRefName, create: true, wantRefused: true},
		{name: "marker not true", secretName: minioRefName, create: true, wantRefused: true,
			labels: map[string]string{inferencev1alpha1.LabelModelSourceCredentials: "yes"}},
		{name: "missing", secretName: minioRefName, wantRefused: true},
		{name: "relay, marked", secretName: inferencev1alpha1.MetalRelaySecretName,
			labels: modelSourceCredentialsLabels(), create: true, wantRefused: true},
	}
	for _, tc := range cases {
		for _, source := range []string{s3ObjectSource, "hf://org/repo/m.gguf"} {
			t.Run(tc.name+" "+source[:2], func(t *testing.T) {
				srv, hits := signedS3Server(t)
				pinHFHost(t, srv.URL)
				var objs []client.Object
				if tc.create {
					s := s3CredentialSecret("default", tc.secretName, srv.URL, tc.labels)
					s.Data["HF_TOKEN"] = []byte("hf_secret")
					objs = append(objs, s)
				}
				k8s := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(objs...).Build()
				exec := NewMetalExecutor("/bin/llama-server", t.TempDir(), newNopLogger(),
					WithKubeClient(k8s, nil), allowTestServers())
				key := &types.NamespacedName{Namespace: "default", Name: tc.secretName}

				_, err := exec.ensureModel(t.Context(), source, "m", key, "")
				var notAllowed *SourceSecretNotAllowedError
				if tc.wantRefused != errors.As(err, &notAllowed) {
					t.Fatalf("ensureModel = %v, want refused=%v", err, tc.wantRefused)
				}
				if !tc.wantRefused && err != nil {
					t.Fatalf("ensureModel with a marked Secret: %v", err)
				}
				if tc.wantRefused && hits.Load() != 0 {
					t.Errorf("the origin received %d requests, want 0", hits.Load())
				}
			})
		}
	}
}

// The memory admission size probe goes through the same gate: an unsized
// s3:// Model with an unmarked Secret fails the estimate without a signed HEAD.
func TestEstimateModelMemory_S3UnmarkedSecretNotProbed(t *testing.T) {
	srv, hits := signedS3Server(t)
	a := sourceSecretTestAgent(t, "default", s3CredentialSecret("default", minioRefName, srv.URL, nil))
	model := sourceSecretTestModel("default", minioRefName)

	_, err := a.estimateModelMemory(t.Context(), model, 2048, "", "", 0)
	if err == nil || !strings.Contains(err.Error(), "not marked for model-source credentials") {
		t.Fatalf("estimateModelMemory = %v, want the size probe refused for an unmarked Secret", err)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the object store received %d requests, want 0", n)
	}
}
