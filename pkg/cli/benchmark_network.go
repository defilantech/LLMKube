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
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func initK8sClient() (client.Client, error) {
	cfg, err := config.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get kubeconfig: %w", err)
	}

	if err := inferencev1alpha1.AddToScheme(scheme.Scheme); err != nil {
		return nil, fmt.Errorf("failed to add scheme: %w", err)
	}

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		return nil, fmt.Errorf("failed to create client: %w", err)
	}

	return k8sClient, nil
}

func getEndpoint(ctx context.Context, opts *benchmarkOptions) (string, func(), error) {
	if opts.endpoint != "" {
		return opts.endpoint, nil, nil
	}

	k8sClient, err := initK8sClient()
	if err != nil {
		return "", nil, err
	}

	isvc := &inferencev1alpha1.InferenceService{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: opts.name, Namespace: opts.namespace}, isvc); err != nil {
		return "", nil, fmt.Errorf("failed to get InferenceService '%s': %w", opts.name, err)
	}

	if isvc.Status.Phase != phaseReady {
		return "", nil, fmt.Errorf("InferenceService '%s' is not ready (phase: %s)", opts.name, isvc.Status.Phase)
	}

	if opts.portForward {
		return setupPortForward(opts)
	}

	if isvc.Status.Endpoint != "" {
		return isvc.Status.Endpoint, nil, nil
	}

	return "", nil, fmt.Errorf(
		"no endpoint found for service '%s'. Use --endpoint to specify manually or --port-forward",
		opts.name)
}

func setupPortForward(opts *benchmarkOptions) (string, func(), error) {
	klog.SetOutput(io.Discard)
	klog.LogToStderr(false)

	serviceName := strings.ReplaceAll(opts.name, ".", "-")

	fmt.Printf("⚡ Port forwarding to service/%s...\n", serviceName)

	restConfig, err := config.GetConfig()
	if err != nil {
		return "", nil, fmt.Errorf("failed to get kubeconfig: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return "", nil, fmt.Errorf("failed to create clientset: %w", err)
	}

	podName, err := findReadyPodForService(clientset, opts.namespace, serviceName)
	if err != nil {
		return "", nil, fmt.Errorf("failed to find pod for service %s: %w", serviceName, err)
	}

	localPort, err := findAvailablePort()
	if err != nil {
		return "", nil, fmt.Errorf("failed to find available port: %w", err)
	}

	stopChan := make(chan struct{}, 1)
	readyChan := make(chan struct{})
	errChan := make(chan error, 1)

	path := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/portforward", opts.namespace, podName)
	hostIP := strings.TrimPrefix(restConfig.Host, "https://")
	hostIP = strings.TrimPrefix(hostIP, "http://")

	serverURL := url.URL{Scheme: "https", Host: hostIP, Path: path}
	if strings.HasPrefix(restConfig.Host, "http://") {
		serverURL.Scheme = "http"
	}

	transport, upgrader, err := spdy.RoundTripperFor(restConfig)
	if err != nil {
		return "", nil, fmt.Errorf("failed to create SPDY transport: %w", err)
	}

	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, &serverURL)

	ports := []string{fmt.Sprintf("%d:8080", localPort)}

	pf, err := portforward.New(dialer, ports, stopChan, readyChan, io.Discard, io.Discard)
	if err != nil {
		return "", nil, fmt.Errorf("failed to create port forwarder: %w", err)
	}

	go func() {
		if err := pf.ForwardPorts(); err != nil {
			errChan <- err
		}
	}()

	select {
	case <-readyChan:
	case err := <-errChan:
		return "", nil, fmt.Errorf("port forward failed: %w", err)
	case <-time.After(10 * time.Second):
		close(stopChan)
		return "", nil, fmt.Errorf("timeout waiting for port forward to be ready")
	}

	endpoint := fmt.Sprintf("http://localhost:%d", localPort)
	cleanup := func() {
		close(stopChan)
	}

	if err := waitForHealthCheck(endpoint, cleanup); err != nil {
		return "", nil, err
	}

	if err := waitForModelLoad(endpoint, cleanup); err != nil {
		return "", nil, err
	}

	return endpoint, cleanup, nil
}

func waitForHealthCheck(endpoint string, cleanup func()) error {
	httpClient := &http.Client{Timeout: 5 * time.Second}
	localPort := strings.TrimPrefix(endpoint, "http://localhost:")

	var lastErr error
	for i := 0; i < 5; i++ {
		resp, err := httpClient.Get(endpoint + "/health")
		if err == nil {
			_ = resp.Body.Close()
			fmt.Printf("   ✅ Connected on port %s\n", localPort)
			return nil
		}
		lastErr = err
		time.Sleep(500 * time.Millisecond)
	}
	cleanup()
	return fmt.Errorf("cannot connect to %s after port forward: %w", endpoint, lastErr)
}

func waitForModelLoad(endpoint string, cleanup func()) error {
	httpClient := &http.Client{Timeout: 5 * time.Second}

	fmt.Printf("   ⏳ Waiting for model to load...\n")
	modelLoadTimeout := 10 * time.Minute
	startTime := time.Now()
	lastStatus := 0
	for {
		if time.Since(startTime) > modelLoadTimeout {
			cleanup()
			return fmt.Errorf("timeout waiting for model to load (last status: %d)", lastStatus)
		}

		resp, err := httpClient.Get(endpoint + "/health")
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}

		lastStatus = resp.StatusCode
		_ = resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			fmt.Printf("   ✅ Model loaded (took %s)\n\n", time.Since(startTime).Round(time.Second))
			return nil
		}

		time.Sleep(2 * time.Second)
	}
}

func findReadyPodForService(clientset *kubernetes.Clientset, namespace, serviceName string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	svc, err := clientset.CoreV1().Services(namespace).Get(ctx, serviceName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get service: %w", err)
	}

	selectors := make([]string, 0, len(svc.Spec.Selector))
	for k, v := range svc.Spec.Selector {
		selectors = append(selectors, fmt.Sprintf("%s=%s", k, v))
	}
	labelSelector := strings.Join(selectors, ",")

	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return "", fmt.Errorf("failed to list pods: %w", err)
	}

	for _, pod := range pods.Items {
		if isPodReady(&pod) {
			return pod.Name, nil
		}
	}

	return "", fmt.Errorf("no ready pods found for service %s", serviceName)
}

func isPodReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func findAvailablePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	return port, nil
}
