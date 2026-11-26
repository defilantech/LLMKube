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
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func TestSGLangAPIKeyPrivateVolume(t *testing.T) {
	isvc := keyedISvc(RuntimeSGLANG)
	isvc.Spec.SecurityContext = &corev1.SecurityContext{ReadOnlyRootFilesystem: boolPtr(true)}
	model := &inferencev1alpha1.Model{}
	r := &InferenceServiceReconciler{}
	pod := r.deploymentFor(isvc, model, resolveEffectiveModelCache(isvc, model), nil, 1, "", "").Spec.Template.Spec
	c := pod.Containers[0]
	if len(c.Command) != 4 || c.Command[1] != "-c" || c.Command[3] != sglangAPIKeyConfigPath {
		t.Fatalf("keyed container has no config launcher: %v", c.Command)
	}
	if !slices.ContainsFunc(c.VolumeMounts, func(m corev1.VolumeMount) bool {
		return m.Name == sglangAPIKeyVolume && m.MountPath == sglangAPIKeyDir
	}) {
		t.Fatal("serving container does not mount the key config volume")
	}
	if !slices.ContainsFunc(pod.Volumes, func(v corev1.Volume) bool {
		return v.Name == sglangAPIKeyVolume && v.EmptyDir != nil && v.EmptyDir.Medium == corev1.StorageMediumMemory
	}) {
		t.Fatal("key config is not backed by memory")
	}
	for _, c := range pod.InitContainers {
		if slices.ContainsFunc(c.VolumeMounts, func(m corev1.VolumeMount) bool { return m.Name == sglangAPIKeyVolume }) {
			t.Fatalf("init container %s can read the key file", c.Name)
		}
	}
}

// Execute the rendered launcher: removing its file write, env removal or exec
// breaks these assertions even if the rendered --config flag remains correct.
func TestSGLangAPIKeyLauncher(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required to execute the SGLang launcher")
	}
	for _, tc := range []struct {
		name, key string
		config    bool
	}{
		{name: "quoted key", key: "test-'\"\\\n:$() secret"},
		{name: "empty key fails closed"},
		{name: "user config", key: "test-key", config: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.config {
				if err := exec.Command(python, "-c", "import yaml").Run(); err != nil {
					t.Skip("PyYAML is required for the user config case (included in SGLang images)")
				}
			}
			dir := t.TempDir()
			module := filepath.Join(dir, "sglang")
			if err := os.Mkdir(module, 0o700); err != nil {
				t.Fatal(err)
			}
			// Inspect what the replacement process actually receives, including
			// the file contents and permission bits, without importing SGLang/GPU code.
			stub := `import json, os, sys
path = sys.argv[sys.argv.index("--config") + 1]
with open(path) as source:
    config = json.load(source)
print(json.dumps({"args": sys.argv, "env": os.environ.get("LLMKUBE_API_KEY"), "config": config, "mode": os.stat(path).st_mode & 0o777}))
`
			if err := os.WriteFile(filepath.Join(module, "launch_server.py"), []byte(stub), 0o600); err != nil {
				t.Fatal(err)
			}
			c := renderContainer(t, keyedISvc(RuntimeSGLANG))
			path := filepath.Join(dir, "config.yaml")
			command := append([]string{}, c.Command[1:]...)
			for i := range command {
				if command[i] == sglangAPIKeyConfigPath {
					command[i] = path
				}
			}
			args := append([]string{}, c.Args...)
			for i := range args {
				if args[i] == sglangAPIKeyConfigPath {
					args[i] = path
				}
			}
			if tc.config {
				userPath := filepath.Join(dir, "user.yaml")
				if err := os.WriteFile(userPath, []byte("api_key: old-key\ncontext-length: 8192\nrevision: 2024-05-01\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				args = append(args, "--config", userPath)
			}
			cmd := exec.Command(python, append(command, args...)...)
			cmd.Env = append(os.Environ(), "LLMKUBE_API_KEY="+tc.key,
				"PYTHONPATH="+dir+string(os.PathListSeparator)+os.Getenv("PYTHONPATH"))
			out, err := cmd.CombinedOutput()
			if tc.key == "" {
				if err == nil || !strings.Contains(string(out), "must not be empty") {
					t.Fatalf("empty key did not stop startup: %s, %v", out, err)
				}
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("empty key created a config file")
				}
				return
			}
			if err != nil {
				t.Fatalf("launcher failed: %s, %v", out, err)
			}
			var got struct {
				Args   []string
				Env    *string
				Config map[string]any
				Mode   int
			}
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("decode launcher result: %v: %s", err, out)
			}
			if got.Config["api-key"] != tc.key || got.Mode != 0o600 || got.Env != nil {
				t.Fatal("key was not transferred exactly to a private file and removed from the environment")
			}
			if slices.ContainsFunc(got.Args, func(a string) bool { return strings.Contains(a, tc.key) || strings.Contains(a, "--api-key") }) {
				t.Fatal("replacement process arguments expose the key")
			}
			if tc.config && (got.Config["context-length"] != float64(8192) || got.Config["api_key"] != nil) {
				t.Fatal("user config was not preserved with the Secret taking precedence")
			}
			if tc.config && got.Config["revision"] != "2024-05-01" {
				t.Fatal("YAML date revision was not preserved as SGLang's string value")
			}
		})
	}
}
