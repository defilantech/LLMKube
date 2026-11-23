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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

const (
	teamNamespace  = "team-a"
	minioRefName   = "minio-models"
	s3ObjectBody   = "fake-gguf-bytes"
	s3ObjectSource = "s3://models/org/repo/model.gguf"
)

// signedS3Server stands in for a private origin: a MinIO bucket that answers
// only SigV4-signed requests, or a gated Hugging Face repository that answers
// only the test's HF_TOKEN bearer (HEAD with the object size, GET with the
// object). It counts every request it receives, authenticated or not, so a
// test can assert that nothing reached it.
func signedS3Server(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKIAEXAMPLE0000000/") && auth != "Bearer hf_secret" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(s3ObjectBody)))
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = w.Write([]byte(s3ObjectBody))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// s3CredentialSecret is a complete s3 sourceSecretRef pointing at endpoint.
// labels is applied as given (nil means unlabeled).
func s3CredentialSecret(namespace, name, endpoint string, labels map[string]string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Data: map[string][]byte{
			"AWS_ACCESS_KEY_ID":     []byte("AKIAEXAMPLE0000000"),
			"AWS_SECRET_ACCESS_KEY": []byte("secretaccesskeyvalue0000000000000"),
			"AWS_REGION":            []byte("us-east-1"),
			"AWS_ENDPOINT_URL":      []byte(endpoint),
		},
	}
}

// sourceSecretTestModel is an unsized s3:// Model in namespace naming
// secretName as its sourceSecretRef.
func sourceSecretTestModel(namespace, secretName string) *inferencev1alpha1.Model {
	return &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "s3-model", Namespace: namespace},
		Spec: inferencev1alpha1.ModelSpec{
			Source:          s3ObjectSource,
			SourceSecretRef: &corev1.LocalObjectReference{Name: secretName},
		},
	}
}

// sourceSecretTestAgent builds an agent watching agentNamespace (empty means
// every namespace, the chart's scope: cluster) whose fake client holds objs,
// with the loopback test server allowlisted for downloads.
func sourceSecretTestAgent(t *testing.T, agentNamespace string, objs ...client.Object) *MetalAgent {
	t.Helper()
	builder := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(objs...)
	for _, o := range objs {
		if isvc, ok := o.(*inferencev1alpha1.InferenceService); ok {
			builder = builder.WithStatusSubresource(isvc)
		}
	}
	return NewMetalAgent(MetalAgentConfig{
		K8sClient:            builder.Build(),
		Namespace:            agentNamespace,
		ModelStorePath:       t.TempDir(),
		LlamaServerBin:       "/usr/local/bin/llama-server",
		AllowedDownloadHosts: []string{"127.0.0.1"},
		MemoryProvider:       &mockMemoryProvider{totalBytes: 128 * 1024 * 1024 * 1024},
		MemoryFraction:       0.75,
	})
}

// The sourceSecretRef Secret is read from the Model's namespace, not from the
// agent's --namespace flag. Before the fix every read used the flag: with the
// chart's scope: cluster the flag is empty, so an s3 source failed with a
// NotFound for a Secret that exists, and an agent watching one namespace
// looked in that namespace for a Model's Secret in another. Both the memory
// admission size probe (before and after buildExecutors registers the shared
// executor) and the download path are exercised, through the agent's own
// wiring (buildExecutors, buildExecutorConfig), since that is where the
// namespace was taken from the flag.
func TestSourceSecret_ReadFromModelNamespace(t *testing.T) {
	for _, agentNS := range []string{"", "default"} {
		t.Run(fmt.Sprintf("agent namespace %q", agentNS), func(t *testing.T) {
			srv, hits := signedS3Server(t)
			secret := s3CredentialSecret(teamNamespace, minioRefName, srv.URL, modelSourceCredentialsLabels())
			a := sourceSecretTestAgent(t, agentNS, secret)
			model := sourceSecretTestModel(teamNamespace, minioRefName)

			// Before buildExecutors: s3SizeProbe's stand-in executor.
			est, err := a.estimateModelMemory(t.Context(), model, 2048, "", "", 0)
			if err != nil {
				t.Fatalf("size probe without registered executor: %v", err)
			}
			if est.WeightsBytes != uint64(len(s3ObjectBody)) {
				t.Errorf("WeightsBytes = %d, want %d", est.WeightsBytes, len(s3ObjectBody))
			}

			a.buildExecutors()
			if _, err := a.estimateModelMemory(t.Context(), model, 2048, "", "", 0); err != nil {
				t.Fatalf("size probe through the registered executor: %v", err)
			}

			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: teamNamespace},
				Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: model.Name},
			}
			cfg := buildExecutorConfig(isvc, model, executorBaseConfig{})
			exec := a.executors[runtimeLlamaServer].(*MetalExecutor)
			path, err := exec.ensureModel(t.Context(), cfg.ModelSource, cfg.ModelName, cfg.SourceSecret, cfg.SHA256)
			if err != nil {
				t.Fatalf("ensureModel: %v", err)
			}
			if b, _ := os.ReadFile(path); string(b) != s3ObjectBody {
				t.Errorf("downloaded body = %q, want %q", b, s3ObjectBody)
			}
			if n := hits.Load(); n != 3 {
				t.Errorf("object store saw %d requests, want 3 (two signed HEADs, one signed GET)", n)
			}
		})
	}
}
