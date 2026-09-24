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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/internal/router"
)

// TestBudgetStatusFromUsage covers the mapping the status poller applies to
// the proxy's snapshot, without a cluster.
func TestBudgetStatusFromUsage(t *testing.T) {
	cases := []struct {
		name        string
		usage       []budgetUsageWire
		wantNames   []string
		wantTokens  map[string]int64
		wantUtil    map[string]string
		wantUSDUsed map[string]string
	}{
		{
			name: "token cap only",
			usage: []budgetUsageWire{
				{Name: "router-cap", UsedTokens: 250, MaxTokens: 1000},
			},
			wantNames:   []string{"router-cap"},
			wantTokens:  map[string]int64{"router-cap": 250},
			wantUtil:    map[string]string{"router-cap": "0.250000"},
			wantUSDUsed: map[string]string{"router-cap": "0.000000"},
		},
		{
			name: "utilization is the larger of token and usd fractions",
			usage: []budgetUsageWire{
				{Name: "both", UsedTokens: 100, MaxTokens: 1000, UsedUSD: 8, MaxUSD: 10},
			},
			wantNames:   []string{"both"},
			wantTokens:  map[string]int64{"both": 100},
			wantUtil:    map[string]string{"both": "0.800000"},
			wantUSDUsed: map[string]string{"both": "8.000000"},
		},
		{
			name: "team-scope counters with one name are summed",
			usage: []budgetUsageWire{
				{Name: "team-cap", UsedTokens: 100, MaxTokens: 1000},
				{Name: "team-cap", UsedTokens: 150, MaxTokens: 1000},
			},
			wantNames:   []string{"team-cap"},
			wantTokens:  map[string]int64{"team-cap": 250},
			wantUtil:    map[string]string{"team-cap": "0.125000"},
			wantUSDUsed: map[string]string{"team-cap": "0.000000"},
		},
		{
			name:      "no usage yields no status entries",
			usage:     nil,
			wantNames: []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := budgetStatusFromUsage(tc.usage)
			if len(got) != len(tc.wantNames) {
				t.Fatalf("entries = %d, want %d (%+v)", len(got), len(tc.wantNames), got)
			}
			for i, want := range tc.wantNames {
				if got[i].Name != want {
					t.Errorf("entry %d name = %q, want %q", i, got[i].Name, want)
				}
				if wt, ok := tc.wantTokens[want]; ok && got[i].TokensUsed != wt {
					t.Errorf("%s tokensUsed = %d, want %d", want, got[i].TokensUsed, wt)
				}
				if wu, ok := tc.wantUtil[want]; ok && got[i].Utilization != wu {
					t.Errorf("%s utilization = %q, want %q", want, got[i].Utilization, wu)
				}
				if wu, ok := tc.wantUSDUsed[want]; ok && got[i].USDUsed != wu {
					t.Errorf("%s usdUsed = %q, want %q", want, got[i].USDUsed, wu)
				}
			}
		})
	}
}

// TestRouterHasBudgets pins the gate that decides whether status polling is
// wired at all.
func TestRouterHasBudgets(t *testing.T) {
	mr := &inferencev1alpha1.ModelRouter{}
	if routerHasBudgets(mr) {
		t.Error("router with no policy must not report budgets")
	}
	mr.Spec.Policy = &inferencev1alpha1.RouterPolicy{}
	if routerHasBudgets(mr) {
		t.Error("router with an empty policy must not report budgets")
	}
	maxTokens := int64(1000)
	mr.Spec.Policy.Budgets = []inferencev1alpha1.BudgetSpec{{Name: "cap", Scope: "router", MaxTokens: &maxTokens}}
	if !routerHasBudgets(mr) {
		t.Error("router with a declared budget must report budgets")
	}
}

// startBudgetTestEnv boots an envtest environment with the llmkube CRDs, the
// same shape as the SLO suite, so the reconciler can create and read real
// ModelRouter objects.
func startBudgetTestEnv(t *testing.T) (client.Client, *rest.Config, func()) {
	t.Helper()
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	if dir := getFirstFoundEnvTestBinaryDir(); dir != "" {
		env.BinaryAssetsDirectory = dir
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	if err := inferencev1alpha1.AddToScheme(scheme.Scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return c, cfg, func() { _ = env.Stop() }
}

// budgetedModelRouter builds a minimal router that declares one token budget,
// so the reconciler takes the budget-status path.
func budgetedModelRouter(name string) *inferencev1alpha1.ModelRouter {
	maxTokens := int64(1000)
	mr := &inferencev1alpha1.ModelRouter{}
	mr.Name = name
	mr.Namespace = "default"
	mr.Spec.Backends = []inferencev1alpha1.RouterBackend{
		{
			Name: "cloud-opus",
			External: &inferencev1alpha1.ExternalProvider{
				Provider: "anthropic",
				Model:    "claude-opus-4-7",
				URL:      "https://api.anthropic.com",
			},
			Tier: "cloud",
		},
	}
	mr.Spec.Rules = []inferencev1alpha1.RouterRule{
		{Name: "all", Route: inferencev1alpha1.RuleRoute{Backends: []string{"cloud-opus"}}},
	}
	mr.Spec.DefaultRoute = "cloud-opus"
	mr.Spec.Policy = &inferencev1alpha1.RouterPolicy{
		Budgets: []inferencev1alpha1.BudgetSpec{{Name: "router-cap", Scope: "router", MaxTokens: &maxTokens}},
	}
	return mr
}

// budgetStubServer serves the proxy's admin snapshot shape and records a close
// hook so a test can simulate an unreachable proxy. It encodes the router
// package's own BudgetUsage, the real producer, rather than a local mirror, so
// drift between the served JSON tags and the operator's decoder fails here.
func budgetStubServer(t *testing.T, usedTokens int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/budgets" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode([]router.BudgetUsage{
			{Name: "router-cap", UsedTokens: usedTokens, MaxTokens: 1000},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newBudgetReconciler(t *testing.T, cfg *rest.Config, url func(*inferencev1alpha1.ModelRouter) string) *ModelRouterReconciler {
	t.Helper()
	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatalf("reconciler client: %v", err)
	}
	return &ModelRouterReconciler{
		Client:           c,
		Scheme:           scheme.Scheme,
		RouterProxyImage: "test-image",
		BudgetStatusURL:  url,
	}
}

// TestModelRouterBudgetStatusPublishes is the acceptance test for #1851: a
// budgeted router gets a status.budgetUtilization entry whose tokensUsed
// matches what the proxy reports.
func TestModelRouterBudgetStatusPublishes(t *testing.T) {
	c, cfg, stop := startBudgetTestEnv(t)
	defer stop()

	mr := budgetedModelRouter("budget-publish")
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create router: %v", err)
	}

	stub := budgetStubServer(t, 420)
	r := newBudgetReconciler(t, cfg, func(*inferencev1alpha1.ModelRouter) string {
		return stub.URL + "/admin/budgets"
	})
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(mr)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(mr), got); err != nil {
		t.Fatalf("get router: %v", err)
	}
	if len(got.Status.BudgetUtilization) != 1 {
		t.Fatalf("budgetUtilization = %+v, want one entry", got.Status.BudgetUtilization)
	}
	if got.Status.BudgetUtilization[0].TokensUsed != 420 {
		t.Errorf("tokensUsed = %d, want 420 (the value the proxy reported)", got.Status.BudgetUtilization[0].TokensUsed)
	}
	cond := apimeta.FindStatusCondition(got.Status.Conditions, ModelRouterConditionBudgetReporting)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("BudgetReporting condition = %+v, want True", cond)
	}
}

// TestModelRouterBudgetStatusUnreachableKeepsPrior is the guard from #1851: a
// proxy that does not answer must leave the previous values and mark the
// condition, never reset status to zero.
func TestModelRouterBudgetStatusUnreachableKeepsPrior(t *testing.T) {
	c, cfg, stop := startBudgetTestEnv(t)
	defer stop()

	mr := budgetedModelRouter("budget-stale")
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create router: %v", err)
	}

	// First reconcile publishes a real value.
	live := budgetStubServer(t, 420)
	r := newBudgetReconciler(t, cfg, func(*inferencev1alpha1.ModelRouter) string {
		return live.URL + "/admin/budgets"
	})
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(mr)}); err != nil {
		t.Fatalf("reconcile (live): %v", err)
	}

	// Take the proxy away and reconcile again.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	r.BudgetStatusURL = func(*inferencev1alpha1.ModelRouter) string { return deadURL + "/admin/budgets" }
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(mr)}); err != nil {
		t.Fatalf("reconcile (unreachable): %v", err)
	}

	got := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(mr), got); err != nil {
		t.Fatalf("get router: %v", err)
	}
	if len(got.Status.BudgetUtilization) != 1 || got.Status.BudgetUtilization[0].TokensUsed != 420 {
		t.Fatalf("budgetUtilization = %+v, want the prior value 420 retained (an unreachable proxy must not zero it)", got.Status.BudgetUtilization)
	}
	cond := apimeta.FindStatusCondition(got.Status.Conditions, ModelRouterConditionBudgetReporting)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Errorf("BudgetReporting condition = %+v, want False", cond)
	}
}

// TestModelRouterBudgetStatusBadResponseKeepsPrior guards the other way a
// proxy can fail to answer: it responds, but not with a snapshot. A non-200
// whose body happens to decode (an empty list) must still be treated as no
// report, not as "no spend".
func TestModelRouterBudgetStatusBadResponseKeepsPrior(t *testing.T) {
	c, cfg, stop := startBudgetTestEnv(t)
	defer stop()

	mr := budgetedModelRouter("budget-bad-response")
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create router: %v", err)
	}

	live := budgetStubServer(t, 420)
	r := newBudgetReconciler(t, cfg, func(*inferencev1alpha1.ModelRouter) string {
		return live.URL + "/admin/budgets"
	})
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(mr)}); err != nil {
		t.Fatalf("reconcile (live): %v", err)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("[]"))
	}))
	t.Cleanup(bad.Close)
	r.BudgetStatusURL = func(*inferencev1alpha1.ModelRouter) string { return bad.URL + "/admin/budgets" }
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(mr)}); err != nil {
		t.Fatalf("reconcile (bad response): %v", err)
	}

	got := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(mr), got); err != nil {
		t.Fatalf("get router: %v", err)
	}
	if len(got.Status.BudgetUtilization) != 1 || got.Status.BudgetUtilization[0].TokensUsed != 420 {
		t.Fatalf("budgetUtilization = %+v, want the prior value 420 retained (a non-200 answer is not a snapshot)", got.Status.BudgetUtilization)
	}
	cond := apimeta.FindStatusCondition(got.Status.Conditions, ModelRouterConditionBudgetReporting)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Errorf("BudgetReporting condition = %+v, want False", cond)
	}
}

// TestModelRouterBudgetStatusNoBudgetsSkipsPoll pins the gate: a router with no
// budget neither polls nor carries the reporting condition.
func TestModelRouterBudgetStatusNoBudgetsSkipsPoll(t *testing.T) {
	c, cfg, stop := startBudgetTestEnv(t)
	defer stop()

	mr := budgetedModelRouter("budget-none")
	mr.Spec.Policy = nil
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create router: %v", err)
	}

	var polled bool
	r := newBudgetReconciler(t, cfg, func(*inferencev1alpha1.ModelRouter) string {
		polled = true
		return "http://127.0.0.1:1/admin/budgets"
	})
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(mr)})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if polled {
		t.Error("a router with no budgets must not poll the proxy")
	}
	if res.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %v, want 0 for a router with no budgets", res.RequeueAfter)
	}
	got := &inferencev1alpha1.ModelRouter{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(mr), got); err != nil {
		t.Fatalf("get router: %v", err)
	}
	if len(got.Status.BudgetUtilization) != 0 {
		t.Errorf("budgetUtilization = %+v, want empty", got.Status.BudgetUtilization)
	}
}

// TestModelRouterBudgetStatusRequeues pins that a budgeted router schedules the
// next poll, since the proxy pushes nothing.
func TestModelRouterBudgetStatusRequeues(t *testing.T) {
	c, cfg, stop := startBudgetTestEnv(t)
	defer stop()

	mr := budgetedModelRouter("budget-requeue")
	if err := c.Create(context.Background(), mr); err != nil {
		t.Fatalf("create router: %v", err)
	}
	stub := budgetStubServer(t, 1)
	r := newBudgetReconciler(t, cfg, func(*inferencev1alpha1.ModelRouter) string { return stub.URL + "/admin/budgets" })
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(mr)})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != modelRouterBudgetPollInterval {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, modelRouterBudgetPollInterval)
	}
}
