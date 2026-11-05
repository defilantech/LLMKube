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
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

const testListingImage = "registry.example.com/llmkube:test"

// glmHub serves the GLM tree fixture and counts the requests it answers.
func glmHub(t *testing.T, status *atomic.Int32, auth *atomic.Value) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	body, err := os.ReadFile("testdata/hflisting/artifactory-tree-glm.json")
	if err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if auth != nil {
			auth.Store(r.Header.Get("Authorization"))
		}
		if status != nil && status.Load() != 0 {
			http.Error(w, "nope", int(status.Load()))
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/api/models/nvidia/GLM-5.3-Flash-NVFP4/tree/main") {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func runListRepoFiles(t *testing.T, args ...string) (string, error) {
	t.Helper()
	result := filepath.Join(t.TempDir(), "termination-log")
	err := RunListRepoFiles(context.Background(), append(args, "--result="+result))
	out, readErr := os.ReadFile(result)
	if readErr != nil {
		t.Fatalf("the result file was not written: %v (run error: %v)", readErr, err)
	}
	return string(out), err
}

func glmFilesArg(files ...string) string {
	b, _ := json.Marshal(files)
	return "--files=" + string(b)
}

// The listing pod lists the tree with the same code the controller uses and
// returns the files spec.files resolves to, with their sizes, in a
// termination message the kubelet keeps whole (#2451).
func TestRunListRepoFiles(t *testing.T) {
	var auth atomic.Value
	srv, _ := glmHub(t, nil, &auth)
	treeURL := srv.URL + "/api/models/nvidia/GLM-5.3-Flash-NVFP4/tree/main?recursive=true"

	t.Run("returns the resolved files and their sizes", func(t *testing.T) {
		t.Setenv("HF_TOKEN", " hf_tok\n")
		out, err := runListRepoFiles(t, "--url="+treeURL, glmFilesArg("config.json", "*.json", "*.jinja", "*.safetensors"))
		if err != nil {
			t.Fatalf("list-repo-files: %v\n%s", err, out)
		}
		if len(out) > maxListingResultBytes {
			t.Errorf("result is %d bytes, over the %d the kubelet keeps", len(out), maxListingResultBytes)
		}
		if got := auth.Load(); got != "Bearer hf_tok" {
			t.Errorf("Authorization = %q, want the trimmed HF_TOKEN from the environment", got)
		}
		res, err := decodeRepoListingResult(out)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		got := map[string]int64{}
		for _, f := range res.Files {
			got[f.Path] = f.Size
		}
		if len(got) != 8+33 {
			t.Errorf("%d files returned, want the 8 json/jinja files and 33 shards: %v", len(got), got)
		}
		for _, absent := range []string{"README.md", ".gitattributes", ".quant_summary.txt"} {
			if _, ok := got[absent]; ok {
				t.Errorf("%s matches no spec.files entry but was returned", absent)
			}
		}
		if got["config.json"] != 16770 {
			t.Errorf("config.json size = %d, want the listed 16770", got["config.json"])
		}
	})

	t.Run("no token in the environment sends none", func(t *testing.T) {
		t.Setenv("HF_TOKEN", "")
		if out, err := runListRepoFiles(t, "--url="+treeURL, glmFilesArg("config.json", "*.safetensors")); err != nil {
			t.Fatalf("list-repo-files: %v\n%s", err, out)
		}
		if got := auth.Load(); got != "" {
			t.Errorf("Authorization = %q, want none", got)
		}
	})

	t.Run("a file set that does not resolve is reported, not failed", func(t *testing.T) {
		out, err := runListRepoFiles(t, "--url="+treeURL, glmFilesArg("missing.gguf", "*.json"))
		if err != nil {
			t.Fatalf("list-repo-files: %v\n%s", err, out)
		}
		res, err := decodeRepoListingResult(out)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !strings.Contains(res.FileSetError, `file "missing.gguf" was not found`) || len(res.Files) != 0 {
			t.Errorf("result = %+v, want the file-set error ResolveFileSet gives for the full listing", res)
		}
	})

	t.Run("a failed listing exits with the cause in the result file", func(t *testing.T) {
		var status atomic.Int32
		status.Store(http.StatusUnauthorized)
		failing, _ := glmHub(t, &status, nil)
		out, err := runListRepoFiles(t, "--url="+failing.URL+"/api/models/nvidia/GLM-5.3-Flash-NVFP4/tree/main", glmFilesArg("*.json"))
		if err == nil || !strings.Contains(out, "401") || strings.HasPrefix(out, repoListingResultPrefix) {
			t.Errorf("err = %v, result = %q; want a failure naming the 401", err, out)
		}
	})

	t.Run("trusts the CA certificates in --ca-directory", func(t *testing.T) {
		body, err := os.ReadFile("testdata/hflisting/artifactory-tree-glm.json")
		if err != nil {
			t.Fatal(err)
		}
		tlsHub := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(body)
		}))
		defer tlsHub.Close()
		caDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(caDir, "ca.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsHub.Certificate().Raw}), 0o600); err != nil {
			t.Fatal(err)
		}
		url := "--url=" + tlsHub.URL + "/api/models/nvidia/GLM-5.3-Flash-NVFP4/tree/main"
		if out, err := runListRepoFiles(t, url, glmFilesArg("config.json", "*.json")); err == nil || !strings.Contains(out, "certificate") {
			t.Fatalf("err = %v, result = %q; want the private CA refused without --ca-directory", err, out)
		}
		if out, err := runListRepoFiles(t, url, glmFilesArg("config.json", "*.json"), "--ca-directory="+caDir); err != nil {
			t.Fatalf("list-repo-files with the CA: %v\n%s", err, out)
		}
	})

	t.Run("a result over the termination message limit fails and says so", func(t *testing.T) {
		var entries []string
		for i := 0; i < 600; i++ {
			entries = append(entries, fmt.Sprintf(`{"type":"file","path":"q%03d/%x.gguf","size":%d}`, i, i*7919, 1_000_003*i+17))
		}
		big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("[" + strings.Join(entries, ",") + "]"))
		}))
		defer big.Close()
		out, err := runListRepoFiles(t, "--url="+big.URL+"/api/models/o/r/tree/main", glmFilesArg("q000/*.gguf", "*/*.gguf"))
		if err == nil || !strings.Contains(out, "4096") || !strings.Contains(out, "spec.files") {
			t.Errorf("err = %v, result = %q; want a failure naming the 4096-byte limit and the explicit list", err, out)
		}
	})
}

func encodeRaw(t *testing.T, payload []byte) string {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return repoListingResultPrefix + base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestRepoListingResultCodec(t *testing.T) {
	in := repoListingResult{Files: []repoListedFile{{Path: "a/b.safetensors", Size: 12}, {Path: "c.json"}}}
	msg, err := encodeRepoListingResult(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := decodeRepoListingResult(msg)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Errorf("round trip = %+v, want %+v", out, in)
	}

	for name, msg := range map[string]string{
		"no prefix":           "W10=",
		"not base64":          repoListingResultPrefix + "!!!",
		"not gzip":            repoListingResultPrefix + base64.StdEncoding.EncodeToString([]byte("[]")),
		"a path leaves repo":  encodeRaw(t, []byte(`{"files":[{"path":"../x"}]}`)),
		"an absolute path":    encodeRaw(t, []byte(`{"files":[{"path":"/etc/x"}]}`)),
		"a negative size":     encodeRaw(t, []byte(`{"files":[{"path":"x","size":-1}]}`)),
		"over the size bound": encodeRaw(t, append(append([]byte(`{"fileSetError":"`), bytes.Repeat([]byte("a"), maxListingDecodedBytes)...), []byte(`"}`)...)),
	} {
		if _, err := decodeRepoListingResult(msg); err == nil {
			t.Errorf("%s: decoded, want an error", name)
		}
	}
}

func listingModel() *inferencev1alpha1.Model {
	return &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "glm", Namespace: "default", Generation: 1, UID: "glm-uid"},
		Spec: inferencev1alpha1.ModelSpec{
			Source:               "hf://nvidia/GLM-5.3-Flash-NVFP4",
			Format:               "safetensors",
			SourceSecretRef:      &corev1.LocalObjectReference{Name: "huggingface"},
			Files:                []string{"config.json", "*.json", "*.safetensors"},
			PrefetchNodeSelector: map[string]string{"egress": "hub"},
			PrefetchTolerations:  []corev1.Toleration{{Key: "egress", Operator: corev1.TolerationOpExists}},
		},
	}
}

const testTreeURL = "https://huggingface.co/api/models/nvidia/GLM-5.3-Flash-NVFP4/tree/main?recursive=true"

// fileListingJobFixture builds listingModel's listing Job with a CA ConfigMap
// configured.
func fileListingJobFixture(t *testing.T, auth bool) (*ModelReconciler, *inferencev1alpha1.Model, *batchv1.Job) {
	t.Helper()
	model := listingModel()
	r := newMultiFileGateReconciler(t, model)
	r.FileListingImage = testListingImage
	r.InitContainerImage = defaultPrefetchImage
	r.CACertConfigMap = "corp-ca"
	job, err := r.buildFileListingJob(model, testTreeURL, auth)
	if err != nil {
		t.Fatal(err)
	}
	return r, model, job
}

// A NetworkPolicy written for the prefetch pods must select the listing pod
// too, so its labels are the prefetch pod's, built by the same code, and it
// is placed where the Model's prefetch pods are.
func TestBuildFileListingJob_SelectedLikePrefetch(t *testing.T) {
	r, model, job := fileListingJobFixture(t, true)
	prefetch, err := r.buildPrefetchJob(context.Background(), model, ModelCachePVCName, "fp")
	if err != nil {
		t.Fatalf("prefetch Job: %v", err)
	}
	if !reflect.DeepEqual(job.Spec.Template.Labels, prefetch.Spec.Template.Labels) {
		t.Errorf("listing pod labels = %v, want the prefetch pod's %v", job.Spec.Template.Labels, prefetch.Spec.Template.Labels)
	}
	if job.Labels["app.kubernetes.io/component"] == "model-prefetch" {
		t.Error("the listing Job is labeled as a prefetch Job")
	}
	spec := job.Spec.Template.Spec
	if !reflect.DeepEqual(spec.NodeSelector, model.Spec.PrefetchNodeSelector) || !reflect.DeepEqual(spec.Tolerations, model.Spec.PrefetchTolerations) {
		t.Errorf("placement = %v/%v, want the Model's prefetch placement", spec.NodeSelector, spec.Tolerations)
	}
}

func TestBuildFileListingJob_Container(t *testing.T) {
	_, _, job := fileListingJobFixture(t, true)
	spec := job.Spec.Template.Spec
	if len(spec.Containers) != 1 || len(spec.InitContainers) != 0 {
		t.Fatalf("containers = %d, init = %d; want one listing container", len(spec.Containers), len(spec.InitContainers))
	}
	c := spec.Containers[0]
	if c.Image != testListingImage || !reflect.DeepEqual(c.Command, []string{"/manager"}) || c.Args[0] != "list-repo-files" {
		t.Errorf("container runs %s %v %v, want %s /manager list-repo-files", c.Image, c.Command, c.Args, testListingImage)
	}
	args := strings.Join(c.Args, " ")
	if !strings.Contains(args, "--url="+testTreeURL) || !strings.Contains(args, `--files=["config.json","*.json","*.safetensors"]`) ||
		!strings.Contains(args, "--ca-directory=/custom-certs") {
		t.Errorf("args = %q, want the tree URL, spec.files and the CA directory", args)
	}
	if strings.Contains(args, "hf_") || strings.Contains(args, "TOKEN") {
		t.Errorf("args = %q carry a credential", args)
	}
	if len(c.Env) != 1 || c.Env[0].Name != "HF_TOKEN" || c.Env[0].ValueFrom == nil || c.Env[0].ValueFrom.SecretKeyRef == nil ||
		c.Env[0].ValueFrom.SecretKeyRef.Name != "huggingface" || c.Env[0].ValueFrom.SecretKeyRef.Key != "HF_TOKEN" ||
		c.Env[0].ValueFrom.SecretKeyRef.Optional == nil || !*c.Env[0].ValueFrom.SecretKeyRef.Optional {
		t.Errorf("env = %+v, want only HF_TOKEN from the sourceSecretRef, optional", c.Env)
	}
	if len(c.EnvFrom) != 0 {
		t.Errorf("envFrom = %+v, want none: the pod needs the token, not the whole Secret", c.EnvFrom)
	}
	if c.TerminationMessagePolicy != corev1.TerminationMessageReadFile {
		t.Errorf("terminationMessagePolicy = %q", c.TerminationMessagePolicy)
	}
}

func TestBuildFileListingJob_Restricted(t *testing.T) {
	_, _, job := fileListingJobFixture(t, true)
	spec := job.Spec.Template.Spec
	sc := spec.Containers[0].SecurityContext
	if sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation ||
		sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem || sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 {
		t.Errorf("container securityContext = %+v, want non-root, no escalation, read-only root, all capabilities dropped", sc)
	}
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Error("the listing pod mounts a service account token")
	}
	if spec.SecurityContext == nil || spec.SecurityContext.SeccompProfile == nil || spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod securityContext = %+v, want the RuntimeDefault seccomp profile", spec.SecurityContext)
	}
	if len(spec.Volumes) != 1 || spec.Volumes[0].ConfigMap == nil || spec.Volumes[0].ConfigMap.Name != "corp-ca" {
		t.Errorf("volumes = %+v, want only the CA ConfigMap", spec.Volumes)
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 || job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds <= 0 {
		t.Errorf("backoffLimit = %v, activeDeadlineSeconds = %v; want one attempt, bounded in time", job.Spec.BackoffLimit, job.Spec.ActiveDeadlineSeconds)
	}
}

func TestBuildFileListingJob_NoTokenForAnotherEndpoint(t *testing.T) {
	_, _, job := fileListingJobFixture(t, false)
	if env := job.Spec.Template.Spec.Containers[0].Env; len(env) != 0 {
		t.Errorf("env = %+v, want none for an endpoint the token may not reach", env)
	}
}

func TestBuildFileListingJob_FingerprintFollowsInputsAndGeneration(t *testing.T) {
	r, model, job := fileListingJobFixture(t, true)
	fp := func(m *inferencev1alpha1.Model) string {
		j, err := r.buildFileListingJob(m, testTreeURL, true)
		if err != nil {
			t.Fatal(err)
		}
		return j.Annotations[annotationFileListingFingerprint]
	}
	base := job.Annotations[annotationFileListingFingerprint]
	if base == "" || base != fp(model) {
		t.Fatal("the fingerprint is empty or not stable")
	}
	files := listingModel()
	files.Spec.Files = []string{"config.json", "*.safetensors"}
	gen := listingModel()
	gen.Generation = 2
	for name, m := range map[string]*inferencev1alpha1.Model{"spec.files": files, "generation": gen} {
		if fp(m) == base {
			t.Errorf("changing %s kept the fingerprint, so an old listing would be reused", name)
		}
	}
}

// listingRig is a pod-mode reconciler over the fake client, with the listing
// Job's pod and conditions written by hand, as the kubelet and Job controller
// would.
type listingRig struct {
	t     *testing.T
	r     *ModelReconciler
	ctx   context.Context
	model *inferencev1alpha1.Model
}

func newListingRig(t *testing.T, model *inferencev1alpha1.Model, secrets ...*corev1.Secret) *listingRig {
	r := newMultiFileGateReconciler(t, model, secrets...)
	r.Scheme = r.Client.Scheme()
	r.FileListingImage = testListingImage
	r.InitContainerImage = defaultPrefetchImage
	r.AllowedRemoteHosts = []string{"127.0.0.1"}
	return &listingRig{t: t, r: r, ctx: context.Background(), model: model}
}

func (g *listingRig) job() *batchv1.Job {
	g.t.Helper()
	job := &batchv1.Job{}
	if err := g.r.Get(g.ctx, types.NamespacedName{Name: prefetchJobNameFor(g.model.Name, fileListingJobSuffix), Namespace: g.model.Namespace}, job); err != nil {
		g.t.Fatalf("listing Job: %v", err)
	}
	return job
}

func (g *listingRig) jobGone() bool {
	err := g.r.Get(g.ctx, types.NamespacedName{Name: prefetchJobNameFor(g.model.Name, fileListingJobSuffix), Namespace: g.model.Namespace}, &batchv1.Job{})
	return err != nil
}

// finish writes the listing pod's termination message and the Job's terminal
// condition.
func (g *listingRig) finish(exitCode int32, message string, failed bool) {
	g.t.Helper()
	job := g.job()
	ctrlTrue := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: job.Name + "-x", Namespace: job.Namespace,
			Labels:          map[string]string{"batch.kubernetes.io/job-name": job.Name},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: &ctrlTrue}},
		},
		Spec: job.Spec.Template.Spec,
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: fileListingContainer,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exitCode, Message: message}}}}},
	}
	if err := g.r.Create(g.ctx, pod); err != nil {
		g.t.Fatalf("listing pod: %v", err)
	}
	cond := batchv1.JobComplete
	if failed {
		cond = batchv1.JobFailed
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: cond, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded",
		Message: "Job has reached the specified backoff limit", LastTransitionTime: metav1.Now()}}
	if err := g.r.Status().Update(g.ctx, job); err != nil {
		g.t.Fatalf("finish Job: %v", err)
	}
}

func (g *listingRig) list() ([]hfTreeEntry, error) {
	g.t.Helper()
	model := &inferencev1alpha1.Model{}
	if err := g.r.Get(g.ctx, client.ObjectKeyFromObject(g.model), model); err != nil {
		g.t.Fatal(err)
	}
	return g.r.repoFileListing(g.ctx, model)
}

func mustEncode(t *testing.T, res repoListingResult) string {
	t.Helper()
	msg, err := encodeRepoListingResult(res)
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

// The listing Job is created, waited for, and its files returned and reused.
func TestRepoFileListing_FromPod(t *testing.T) {
	g := newListingRig(t, listingModel())
	if _, err := g.list(); !errors.Is(err, errRepoListingPending) {
		t.Fatalf("first listing = %v, want pending while the Job runs", err)
	}
	job := g.job()
	if !metav1.IsControlledBy(job, g.model) {
		t.Errorf("the listing Job is not controlled by the Model: %+v", job.OwnerReferences)
	}
	if _, err := g.list(); !errors.Is(err, errRepoListingPending) {
		t.Fatalf("listing with the Job running = %v, want pending", err)
	}
	g.finish(0, mustEncode(t, repoListingResult{Files: []repoListedFile{{Path: "config.json", Size: 5}, {Path: "a.safetensors", Size: 7}}}), false)
	entries, err := g.list()
	if err != nil {
		t.Fatalf("listing after the Job completed: %v", err)
	}
	if got := hfFilePaths(entries); !reflect.DeepEqual(got, []string{"config.json", "a.safetensors"}) {
		t.Errorf("paths = %v", got)
	}
	if total, ok := hfTotalSize(entries, []string{"config.json", "a.safetensors"}); !ok || total != 12 {
		t.Errorf("size = %d, %v; want 12 from the pod's sizes", total, ok)
	}
	if _, err := g.list(); err != nil || g.jobGone() {
		t.Errorf("a second read = %v, gone = %v; want the finished listing reused while its inputs match", err, g.jobGone())
	}
}

// An empty result is still a listing.
func TestRepoFileListing_FromPodEmptyResultIsAListing(t *testing.T) {
	g := newListingRig(t, listingModel())
	_, _ = g.list()
	g.finish(0, mustEncode(t, repoListingResult{}), false)
	entries, err := g.list()
	if err != nil || entries == nil {
		t.Errorf("entries = %#v, err = %v; want an empty, non-nil listing, so a glob matching nothing is not read as no listing", entries, err)
	}
}

// A spec change replaces the Job.
func TestRepoFileListing_FromPodSpecChangeReplacesTheJob(t *testing.T) {
	g := newListingRig(t, listingModel())
	_, _ = g.list()
	g.finish(0, mustEncode(t, repoListingResult{Files: []repoListedFile{{Path: "config.json"}}}), false)
	stored := &inferencev1alpha1.Model{}
	if err := g.r.Get(g.ctx, client.ObjectKeyFromObject(g.model), stored); err != nil {
		t.Fatal(err)
	}
	stored.Spec.Files = []string{"config.json", "*.safetensors"}
	stored.Generation = 2
	if err := g.r.Update(g.ctx, stored); err != nil {
		t.Fatal(err)
	}
	if _, err := g.list(); !errors.Is(err, errRepoListingPending) || !g.jobGone() {
		t.Fatalf("listing after a spec change = %v, gone = %v; want the old Job deleted and pending", err, g.jobGone())
	}
	if _, err := g.list(); !errors.Is(err, errRepoListingPending) || g.jobGone() {
		t.Fatalf("next listing = %v, gone = %v; want a new Job", err, g.jobGone())
	}
}

// A failed Job reports its cause, then retries after a minute.
func TestRepoFileListing_FromPodFailedJobRetriesAfterAMinute(t *testing.T) {
	g := newListingRig(t, listingModel())
	_, _ = g.list()
	g.finish(1, "listing https://huggingface.co/api/models/x: 401 Unauthorized", true)
	_, err := g.list()
	if err == nil || errors.Is(err, errRepoListingPending) || !strings.Contains(err.Error(), "401") {
		t.Fatalf("listing after a failure = %v, want the pod's cause", err)
	}
	job := g.job()
	job.Status.Conditions[0].LastTransitionTime = metav1.NewTime(time.Now().Add(-2 * fileListingRetryAfterFailure))
	if err := g.r.Status().Update(g.ctx, job); err != nil {
		t.Fatal(err)
	}
	if _, err := g.list(); !errors.Is(err, errRepoListingPending) || !g.jobGone() {
		t.Errorf("listing after the retry interval = %v, gone = %v; want the Job replaced", err, g.jobGone())
	}
}

// A Job that never ran reports the Job's reason.
func TestRepoFileListing_FromPodJobThatNeverRan(t *testing.T) {
	g := newListingRig(t, listingModel())
	_, _ = g.list()
	job := g.job()
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "DeadlineExceeded",
		Message: "Job was active longer than specified deadline", LastTransitionTime: metav1.Now()}}
	if err := g.r.Status().Update(g.ctx, job); err != nil {
		t.Fatal(err)
	}
	if _, err := g.list(); err == nil || !strings.Contains(err.Error(), "DeadlineExceeded") {
		t.Errorf("listing = %v, want the Job's DeadlineExceeded", err)
	}
}

// A file-set error is reported as one, then rechecked after five minutes.
func TestRepoFileListing_FromPodFileSetError(t *testing.T) {
	g := newListingRig(t, listingModel())
	_, _ = g.list()
	g.finish(0, mustEncode(t, repoListingResult{FileSetError: `file "missing.gguf" was not found in repository file list`}), false)
	_, err := g.list()
	var fsErr *listedFileSetError
	if !errors.As(err, &fsErr) || !strings.Contains(err.Error(), "missing.gguf") {
		t.Fatalf("listing = %v, want a file-set error", err)
	}
	job := g.job()
	job.Status.Conditions[0].LastTransitionTime = metav1.NewTime(time.Now().Add(-2 * fileListingRetryAfterFileSetError))
	if err := g.r.Status().Update(g.ctx, job); err != nil {
		t.Fatal(err)
	}
	if _, err := g.list(); !errors.Is(err, errRepoListingPending) || !g.jobGone() {
		t.Errorf("listing after the recheck interval = %v, gone = %v; want the Job replaced", err, g.jobGone())
	}
}

// A Job of that name the Model does not control is left alone.
func TestRepoFileListing_FromPodLeavesAForeignJobAlone(t *testing.T) {
	g := newListingRig(t, listingModel())
	other := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: prefetchJobNameFor("glm", fileListingJobSuffix), Namespace: "default"}}
	if err := g.r.Create(g.ctx, other); err != nil {
		t.Fatal(err)
	}
	if _, err := g.list(); err == nil || errors.Is(err, errRepoListingPending) || g.jobGone() {
		t.Errorf("listing = %v, gone = %v; want an error and the Job kept", err, g.jobGone())
	}
}

// mirrorSecret is a sourceSecretRef naming the GLM hub as the HF_ENDPOINT
// mirror, so a controller listing would reach a server the test counts.
func mirrorSecret(t *testing.T) (secret *corev1.Secret, endpoint string, hits *atomic.Int32) {
	t.Helper()
	srv, hits := glmHub(t, nil, nil)
	endpoint = srv.URL + "/artifactory/api/huggingfaceml/hf-remote"
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "huggingface", Namespace: "default"},
		Data:       map[string][]byte{"HF_TOKEN": []byte("hf_tok"), "HF_ENDPOINT": []byte(endpoint)},
	}, endpoint, hits
}

var glmRequest = reconcile.Request{NamespacedName: types.NamespacedName{Name: "glm", Namespace: "default"}}

// With --file-listing=pod the controller makes no request to the model host:
// the glob is expanded from the listing pod's result, through the same
// reconcile paths as a controller listing (#2451).
func TestReconcile_GlobListedFromPod(t *testing.T) {
	secret, endpoint, hits := mirrorSecret(t)
	req := glmRequest
	podResult := mustEncode(t, repoListingResult{Files: []repoListedFile{
		{Path: "config.json", Size: 100}, {Path: "model-00001-of-00002.safetensors", Size: 1000}, {Path: "model-00002-of-00002.safetensors", Size: 2000},
	}})
	const expanded = "config.json model-00001-of-00002.safetensors model-00002-of-00002.safetensors"

	for _, prefetch := range []bool{false, true} {
		t.Run(fmt.Sprintf("prefetch=%v", prefetch), func(t *testing.T) {
			model := listingModel()
			model.Spec.Prefetch = prefetch
			model.Spec.Files = []string{"config.json", "*.safetensors"}
			g := newListingRig(t, model, secret)

			result, err := g.r.Reconcile(g.ctx, req)
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			stored := getModel(t, g.r, "glm")
			cond := meta.FindStatusCondition(stored.Status.Conditions, ConditionProgressing)
			if stored.Status.Phase != "Pending" || cond == nil || cond.Reason != ReasonRepoListingPending || !strings.Contains(cond.Message, g.job().Name) {
				t.Fatalf("phase = %q, progressing = %+v; want Pending with %s naming the Job", stored.Status.Phase, cond, ReasonRepoListingPending)
			}
			if result.RequeueAfter <= 0 || result.RequeueAfter > time.Minute {
				t.Errorf("requeue = %v, want a short poll while the listing runs", result.RequeueAfter)
			}
			if args := strings.Join(g.job().Spec.Template.Spec.Containers[0].Args, " "); !strings.Contains(args, "--url="+endpoint+"/api/models/nvidia/GLM-5.3-Flash-NVFP4/tree/main") {
				t.Errorf("the listing pod lists %q, want the HF_ENDPOINT mirror's tree", args)
			}

			g.finish(0, podResult, false)
			for i := 0; i < 2; i++ {
				if _, err := g.r.Reconcile(g.ctx, req); err != nil {
					t.Fatalf("reconcile after the listing %d: %v", i, err)
				}
			}
			stored = getModel(t, g.r, "glm")
			if prefetch {
				var job batchv1.Job
				if err := g.r.Get(g.ctx, types.NamespacedName{Name: prefetchJobName(stored), Namespace: "default"}, &job); err != nil {
					t.Fatalf("prefetch Job: %v (phase %q, conditions %+v)", err, stored.Status.Phase, stored.Status.Conditions)
				}
				if got := strings.ReplaceAll(prefetchJobFiles(&job), "\n", " "); got != expanded {
					t.Errorf("prefetch Job stages %q, want the pod's expansion %q", got, expanded)
				}
			} else {
				if got := strings.Join(stored.Status.StagedFiles, " "); stored.Status.Phase != PhaseReady || got != expanded {
					t.Errorf("phase = %q, stagedFiles = %q; want Ready with %q (conditions=%+v)", stored.Status.Phase, got, expanded, stored.Status.Conditions)
				}
				if stored.Status.Size != formatBytes(3100) {
					t.Errorf("size = %q, want the pod's total %q", stored.Status.Size, formatBytes(3100))
				}
				if cond := meta.FindStatusCondition(stored.Status.Conditions, ConditionProgressing); cond != nil && cond.Reason == ReasonRepoListingPending {
					t.Errorf("progressing = %+v, still pending on a Ready Model", cond)
				}
			}
			if n := hits.Load(); n != 0 {
				t.Errorf("the controller made %d requests to the model host; with --file-listing=pod it must make none", n)
			}
		})
	}
}

// A listing pod's failure and its file-set error fail the Model with the
// reasons, messages and requeues a controller listing gives.
func TestReconcile_GlobListedFromPodFailures(t *testing.T) {
	secret, _, _ := mirrorSecret(t)
	req := glmRequest

	t.Run("a failed listing pod fails the Model as a controller listing does", func(t *testing.T) {
		model := listingModel()
		g := newListingRig(t, model, secret)
		if _, err := g.r.Reconcile(g.ctx, req); err != nil {
			t.Fatal(err)
		}
		g.finish(1, "listing https://mirror/api/models/x: 401 Unauthorized", true)
		result, err := g.r.Reconcile(g.ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		stored := getModel(t, g.r, "glm")
		cond := meta.FindStatusCondition(stored.Status.Conditions, ConditionDegraded)
		if stored.Status.Phase != PhaseFailed || cond == nil || cond.Reason != "RepoListingFailed" || !strings.Contains(cond.Message, "401") {
			t.Errorf("phase = %q, degraded = %+v; want Failed with RepoListingFailed naming the 401", stored.Status.Phase, cond)
		}
		if result.RequeueAfter != time.Minute {
			t.Errorf("requeue = %v, want the minute a controller listing retries after", result.RequeueAfter)
		}
		if cond := meta.FindStatusCondition(stored.Status.Conditions, ConditionProgressing); cond != nil && cond.Reason == ReasonRepoListingPending {
			t.Errorf("progressing = %+v, still pending on a failed Model", cond)
		}
	})

	for _, prefetch := range []bool{false, true} {
		t.Run(fmt.Sprintf("a file-set error from the pod fails the Model as InvalidFileSet, prefetch=%v", prefetch), func(t *testing.T) {
			model := listingModel()
			model.Spec.Prefetch = prefetch
			g := newListingRig(t, model, secret)
			if _, err := g.r.Reconcile(g.ctx, req); err != nil {
				t.Fatal(err)
			}
			g.finish(0, mustEncode(t, repoListingResult{FileSetError: `primary file "config.json" must resolve to exactly one file, matched 0`}), false)
			if _, err := g.r.Reconcile(g.ctx, req); err != nil {
				t.Fatal(err)
			}
			stored := getModel(t, g.r, "glm")
			cond := meta.FindStatusCondition(stored.Status.Conditions, ConditionDegraded)
			if stored.Status.Phase != PhaseFailed || cond == nil || cond.Reason != "InvalidFileSet" || !strings.Contains(cond.Message, "matched 0") {
				t.Errorf("phase = %q, degraded = %+v; want Failed with InvalidFileSet and the pod's message", stored.Status.Phase, cond)
			}
			if cond := meta.FindStatusCondition(stored.Status.Conditions, ConditionProgressing); cond != nil && cond.Reason == ReasonRepoListingPending {
				t.Errorf("progressing = %+v, still pending on a failed Model", cond)
			}
		})
	}
}

func TestFileListingImageFor(t *testing.T) {
	for _, tc := range []struct {
		mode, image, want string
		fails             bool
	}{
		{mode: FileListingController, image: testListingImage, want: ""},
		{mode: FileListingPod, image: testListingImage, want: testListingImage},
		{mode: FileListingPod, fails: true},
		{mode: "", image: testListingImage, fails: true},
		{mode: "Pod", image: testListingImage, fails: true},
	} {
		got, err := FileListingImageFor(tc.mode, tc.image)
		if (err != nil) != tc.fails || got != tc.want {
			t.Errorf("FileListingImageFor(%q, %q) = %q, %v; want %q, failing %v", tc.mode, tc.image, got, err, tc.want, tc.fails)
		}
	}
}

// The OCI prefetch waits on a listing pod as the PVC prefetch does, and builds
// its publishing Job from the pod's expansion.
func TestOCIPrefetchWaitsForListingPod(t *testing.T) {
	r, model := ociPrefetchFixture(t)
	r.FileListingImage = testListingImage
	ctx := context.Background()
	model.Spec.Source = "hf://org/model"
	model.Spec.Format = "safetensors"
	model.Spec.Files = []string{"config.json", "*.safetensors"}
	if err := r.Update(ctx, model); err != nil {
		t.Fatal(err)
	}
	jobs := func() []batchv1.Job {
		list := &batchv1.JobList{}
		if err := r.List(ctx, list); err != nil {
			t.Fatal(err)
		}
		return list.Items
	}

	if _, _, err := r.reconcilePrefetch(ctx, model); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(model), model); err != nil {
		t.Fatal(err)
	}
	if cond := meta.FindStatusCondition(model.Status.Conditions, ConditionProgressing); cond == nil || cond.Reason != ReasonRepoListingPending {
		t.Fatalf("progressing = %+v, want %s while the listing pod runs", cond, ReasonRepoListingPending)
	}
	created := jobs()
	if len(created) != 1 || created[0].Name != prefetchJobNameFor(model.Name, fileListingJobSuffix) {
		t.Fatalf("jobs = %d, want only the listing Job before the expansion is known", len(created))
	}

	listing := created[0]
	ctrlTrue := true
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: listing.Name + "-x", Namespace: listing.Namespace,
		Labels:          map[string]string{"batch.kubernetes.io/job-name": listing.Name},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: listing.Name, UID: listing.UID, Controller: &ctrlTrue}},
	}, Spec: listing.Spec.Template.Spec}
	if err := r.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: fileListingContainer, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		Message: mustEncode(t, repoListingResult{Files: []repoListedFile{{Path: "config.json"}, {Path: "model-00001-of-00002.safetensors"}, {Path: "model-00002-of-00002.safetensors"}}}),
	}}}}
	if err := r.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	listing.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: metav1.Now()}}
	if err := r.Status().Update(ctx, &listing); err != nil {
		t.Fatal(err)
	}

	if _, _, err := r.reconcilePrefetch(ctx, model); err != nil {
		t.Fatal(err)
	}
	var publisher *batchv1.Job
	for _, j := range jobs() {
		if j.Name == prefetchJobNameFor(model.Name, "-prefetch-oci") {
			publisher = &j
		}
	}
	if publisher == nil || !strings.Contains(publisher.Annotations[annotationOCIFiles], "model-00002-of-00002.safetensors") {
		t.Fatalf("publisher = %+v, want a publishing Job staging the pod's expansion", publisher)
	}
}
