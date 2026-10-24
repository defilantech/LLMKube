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

package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// These tests drive runCacheList, the command's entry point, so they pin what
// the user sees and the exit status, not just what the inspection helpers
// return. config.GetConfig reads KUBECONFIG, so the environment is pointed at
// a kubeconfig generated from the envtest config.

var (
	cacheListEnvOnce sync.Once
	cacheListEnvCfg  *rest.Config
	cacheListEnvErr  error
)

// cacheListEnvtest starts one envtest control plane for the whole package run.
func cacheListEnvtest(t *testing.T) *rest.Config {
	t.Helper()
	cacheListEnvOnce.Do(func() {
		binDir := ""
		if dir := os.Getenv("KUBEBUILDER_ASSETS"); dir != "" {
			binDir = dir
		} else if matches, err := filepath.Glob(filepath.Join("..", "..", "bin", "k8s", "*")); err == nil &&
			len(matches) > 0 {
			binDir = matches[0]
		} else {
			cacheListEnvErr = errNoEnvtestAssets
			return
		}
		env := &envtest.Environment{
			CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
			ErrorIfCRDPathMissing: true,
			BinaryAssetsDirectory: binDir,
		}
		cfg, err := env.Start()
		if err != nil {
			cacheListEnvErr = err
			return
		}
		cacheListEnvCfg = cfg
	})
	if cacheListEnvErr != nil {
		t.Fatalf("start envtest: %v", cacheListEnvErr)
	}
	return cacheListEnvCfg
}

var errNoEnvtestAssets = errString("no envtest binaries found: run `make envtest` first")

type errString string

func (e errString) Error() string { return string(e) }

// pointKubeconfigAt makes config.GetConfig return cfg for this test.
func pointKubeconfigAt(t *testing.T, cfg *rest.Config) {
	t.Helper()
	kc := clientcmdapi.NewConfig()
	kc.Clusters["envtest"] = &clientcmdapi.Cluster{
		Server:                   cfg.Host,
		CertificateAuthorityData: cfg.CAData,
		InsecureSkipTLSVerify:    cfg.Insecure,
	}
	kc.AuthInfos["envtest"] = &clientcmdapi.AuthInfo{
		ClientCertificateData: cfg.CertData,
		ClientKeyData:         cfg.KeyData,
		Token:                 cfg.BearerToken,
	}
	kc.Contexts["envtest"] = &clientcmdapi.Context{Cluster: "envtest", AuthInfo: "envtest"}
	kc.CurrentContext = "envtest"

	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(*kc, path); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	t.Setenv("KUBECONFIG", path)
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what
// it printed.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		out, _ := io.ReadAll(r)
		done <- string(out)
	}()
	defer func() { os.Stdout = orig }()
	fn()
	_ = w.Close()
	os.Stdout = orig
	return <-done
}

// seedShelllessSharedCache creates a namespace whose shared cache claim is
// mounted by a shell-less running pod, the shape the default chart install
// brings (#2140).
func seedShelllessSharedCache(t *testing.T, cfg *rest.Config, namespace string) {
	t.Helper()
	ctx := context.Background()
	c, err := client.New(cfg, client.Options{Scheme: cacheListEnvScheme()})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	pvc := cacheClaim()
	pvc.Namespace = namespace
	if err := c.Create(ctx, pvc); err != nil {
		t.Fatalf("create claim: %v", err)
	}
	pod := shelllessCachePod()
	pod.Namespace = namespace
	if err := c.Create(ctx, pod); err != nil {
		t.Fatalf("create pod: %v", err)
	}
	pod.Status.Phase = corev1.PodRunning
	if err := c.Status().Update(ctx, pod); err != nil {
		t.Fatalf("mark pod running: %v", err)
	}
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "a-model", Namespace: namespace},
		Spec:       inferencev1alpha1.ModelSpec{Source: "https://example.com/model.gguf"},
	}
	if err := c.Create(ctx, model); err != nil {
		t.Fatalf("create model: %v", err)
	}
}

func cacheListEnvScheme() *runtime.Scheme {
	s := cacheListScheme()
	if err := inferencev1alpha1.AddToScheme(s); err != nil {
		panic(err)
	}
	return s
}

// TestCacheListFailsWhenNoPVCIsReadable is the reported failure: the cache
// could not be read, yet the command printed "No orphaned cache entries found."
// and exited 0.
func TestCacheListFailsWhenNoPVCIsReadable(t *testing.T) {
	cfg := cacheListEnvtest(t)
	pointKubeconfigAt(t, cfg)
	ns := "cache-list-unreadable"
	seedShelllessSharedCache(t, cfg, ns)

	origExec, origWait, origSleep := purgeExec, purgeWait, purgeSleep
	t.Cleanup(func() { purgeExec, purgeWait, purgeSleep = origExec, origWait, origSleep })
	purgeSleep = func(time.Duration) {}
	purgeWait = func(_ context.Context, _ kubernetes.Interface, _, _ string, _ time.Duration) error {
		return nil
	}
	// The real exec against this apiserver fails: the inspector pod is never
	// really scheduled, so the exec endpoint has no running container.
	purgeExec = func(context.Context, *rest.Config, kubernetes.Interface,
		string, string, string, []string) (string, error) {
		return "", errString("exec failed: unable to start container process")
	}

	var err error
	out := captureStdout(t, func() { err = runCacheList(ns, false, true) })

	if err == nil {
		t.Fatalf("runCacheList returned nil and printed:\n%s\nwant an error: no claim could be read", out)
	}
	if strings.Contains(out, "No orphaned cache entries found.") {
		t.Errorf("output claims nothing is orphaned after failing to read the cache:\n%s", out)
	}
}

// TestCacheListAllNamespacesSkipsInspection pins that -A creates no pod and
// execs nowhere.
func TestCacheListAllNamespacesSkipsInspection(t *testing.T) {
	cfg := cacheListEnvtest(t)
	pointKubeconfigAt(t, cfg)
	ns := "cache-list-all-ns"
	seedShelllessSharedCache(t, cfg, ns)

	origExec := purgeExec
	t.Cleanup(func() { purgeExec = origExec })
	purgeExec = func(context.Context, *rest.Config, kubernetes.Interface,
		string, string, string, []string) (string, error) {
		t.Error("cache list -A exec'd into a pod")
		return "", nil
	}

	var err error
	captureStdout(t, func() { err = runCacheList(ns, true, false) })
	if err != nil {
		t.Fatalf("runCacheList -A: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}
	pods, err := clientset.CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	for _, pod := range pods.Items {
		if pod.Name != "llmkube-controller" {
			t.Errorf("cache list -A created pod %q", pod.Name)
		}
	}
}
