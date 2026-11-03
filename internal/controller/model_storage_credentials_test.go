package controller

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Credentials reach curl as config on stdin, never as arguments: a process's
// arguments are readable from /proc/<pid>/cmdline by anything in its PID
// namespace, whatever its uid, while stdin is not (#2097). Each helper runs
// under a real shell against a fake curl that records what it was given.
func TestCredentialHelpers_KeepSecretsOffArgv(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	tests := []struct {
		name            string
		script          string
		env             []string
		secrets         []string
		wantConfig      string
		wantConfigExact string
		wantArgs        []string
	}{
		{
			name:       "hf_curl with a token",
			script:     hfAuthFn + `hf_curl -fsS -o /dev/null "https://huggingface.co/org/repo/resolve/main/config.json"`,
			env:        []string{"HF_TOKEN=hf_s3cr3tT0ken"},
			secrets:    []string{"hf_s3cr3tT0ken"},
			wantConfig: `header = "Authorization: Bearer hf_s3cr3tT0ken"`,
			wantArgs:   []string{"-K", "-", "-fsS", "https://huggingface.co/org/repo/resolve/main/config.json"},
		},
		{
			name:     "hf_curl without a token",
			script:   hfAuthFn + `hf_curl -fsS -o /dev/null "https://huggingface.co/org/repo/resolve/main/config.json"`,
			wantArgs: []string{"-fsS", "https://huggingface.co/org/repo/resolve/main/config.json"},
		},
		{
			name:            "s3_curl",
			script:          s3AuthFn + `s3_curl -f -L -o /dev/null "https://minio.example/bucket/key"`,
			env:             []string{"AWS_REGION=us-east-1", "AWS_ACCESS_KEY_ID=AKIAEXAMPLEKEYID", "AWS_SECRET_ACCESS_KEY=wJalr/XUtnFEMI+K7MDENG=="},
			secrets:         []string{"AKIAEXAMPLEKEYID", "wJalr/XUtnFEMI+K7MDENG=="},
			wantConfig:      `user = "AKIAEXAMPLEKEYID:wJalr/XUtnFEMI+K7MDENG=="`,
			wantConfigExact: "user = \"AKIAEXAMPLEKEYID:wJalr/XUtnFEMI+K7MDENG==\"\n",
			wantArgs:        []string{"--aws-sigv4", "aws:amz:us-east-1:s3", "-K", "-", "https://minio.example/bucket/key"},
		},
		{
			// A Secret built with kubectl create secret --from-file keeps the
			// token file's trailing newline; the helper must strip it, not
			// split curl's line-based config around it.
			name:            "hf_curl with a token ending in a newline",
			script:          hfAuthFn + `hf_curl -fsS -o /dev/null "https://huggingface.co/org/repo/resolve/main/config.json"`,
			env:             []string{"HF_TOKEN=hf_s3cr3tT0ken\n"},
			secrets:         []string{"hf_s3cr3tT0ken"},
			wantConfig:      `header = "Authorization: Bearer hf_s3cr3tT0ken"`,
			wantConfigExact: "header = \"Authorization: Bearer hf_s3cr3tT0ken\"\n",
			wantArgs:        []string{"-K", "-", "-fsS", "https://huggingface.co/org/repo/resolve/main/config.json"},
		},
		{
			name:            "s3_curl with a secret ending in a newline",
			script:          s3AuthFn + `s3_curl -f -L -o /dev/null "https://minio.example/bucket/key"`,
			env:             []string{"AWS_REGION=us-east-1", "AWS_ACCESS_KEY_ID=AKIAEXAMPLEKEYID", "AWS_SECRET_ACCESS_KEY=wJalr/XUtnFEMI+K7MDENG==\n"},
			secrets:         []string{"AKIAEXAMPLEKEYID", "wJalr/XUtnFEMI+K7MDENG=="},
			wantConfig:      `user = "AKIAEXAMPLEKEYID:wJalr/XUtnFEMI+K7MDENG=="`,
			wantConfigExact: "user = \"AKIAEXAMPLEKEYID:wJalr/XUtnFEMI+K7MDENG==\"\n",
			wantArgs:        []string{"--aws-sigv4", "aws:amz:us-east-1:s3", "-K", "-", "https://minio.example/bucket/key"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			argv, stdin := runWithFakeCurl(t, tt.script, tt.env)
			for _, s := range tt.secrets {
				if strings.Contains(argv, s) {
					t.Errorf("secret %q is in curl's arguments:\n%s", s, argv)
				}
			}
			if tt.wantConfig == "" {
				if stdin != "" {
					t.Errorf("curl got config on stdin without credentials: %q", stdin)
				}
			} else if !strings.Contains(stdin, tt.wantConfig) {
				t.Errorf("curl's stdin config = %q, want it to contain %q", stdin, tt.wantConfig)
			}
			if tt.wantConfigExact != "" && stdin != tt.wantConfigExact {
				t.Errorf("curl's stdin config = %q, want exactly %q", stdin, tt.wantConfigExact)
			}
			for _, a := range tt.wantArgs {
				if !strings.Contains("\n"+argv, "\n"+a+"\n") {
					t.Errorf("curl's arguments lack %q:\n%s", a, argv)
				}
			}
		})
	}
}

// A value curl's config parser would rewrite (a quote ends the string, a
// backslash starts an escape, a newline ends the line) must fail the transfer,
// not send a mangled credential.
func TestCredentialHelpers_RejectUnquotableValues(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	for _, tt := range []struct{ name, script, env string }{
		{"hf token with a quote", hfAuthFn + `hf_curl -fsS https://huggingface.co/x`, `HF_TOKEN=abc"def`},
		{"hf token with a backslash", hfAuthFn + `hf_curl -fsS https://huggingface.co/x`, `HF_TOKEN=abc\def`},
		{"hf token with an embedded newline", hfAuthFn + `hf_curl -fsS https://huggingface.co/x`, "HF_TOKEN=abc\ndef"},
		{"s3 secret with a quote", s3AuthFn + `s3_curl -f https://minio.example/b/k`, `AWS_SECRET_ACCESS_KEY=abc"def`},
		{"s3 secret with a backslash", s3AuthFn + `s3_curl -f https://minio.example/b/k`, `AWS_SECRET_ACCESS_KEY=abc\def`},
		{"s3 secret with an embedded newline", s3AuthFn + `s3_curl -f https://minio.example/b/k`, "AWS_SECRET_ACCESS_KEY=abc\ndef"},
		{"s3 access key id with a quote", s3AuthFn + `s3_curl -f https://minio.example/b/k`, `AWS_ACCESS_KEY_ID=abc"def`},
		{"s3 access key id with an embedded newline", s3AuthFn + `s3_curl -f https://minio.example/b/k`, "AWS_ACCESS_KEY_ID=abc\ndef"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFakeCurl(t, dir)
			cmd := exec.Command("sh", "-c", tt.script)
			// The fixed defaults come before tt.env: os/exec keeps the last
			// value for a duplicate key, so the case's value wins.
			cmd.Env = append([]string{"PATH=" + dir + ":/usr/bin:/bin", "CAPTURE_DIR=" + dir, "AWS_REGION=r", "AWS_ACCESS_KEY_ID=id"}, tt.env)
			if err := cmd.Run(); err == nil {
				t.Error("helper succeeded with a value curl's config would rewrite")
			}
			if _, err := os.Stat(filepath.Join(dir, "argv")); err == nil {
				t.Error("curl ran with a value curl's config would rewrite")
			}
		})
	}
}

// Every credential the downloader uses goes through hf_curl or s3_curl. Any
// other mention of a credential variable in a rendered init command is a new
// call site that could put it back on the command line.
func TestInitCommands_CredentialsOnlyThroughHelpers(t *testing.T) {
	cmds := make([]string, 0, 64)
	for _, useCache := range []bool{true, false} {
		for _, isS3 := range []bool{true, false} {
			for _, isHFAuth := range []bool{true, false} {
				for _, sha := range []bool{true, false} {
					for _, policy := range []string{RefreshPolicyIfNotPresent, RefreshPolicyOnChange} {
						cmds = append(cmds,
							buildModelInitCommand(false, isS3, useCache, isHFAuth, sha, policy),
							buildMultiFileInitCommand(useCache, isS3, isHFAuth, sha, false, policy))
					}
				}
			}
		}
	}
	for i, cmd := range cmds {
		rest := strings.ReplaceAll(strings.ReplaceAll(cmd, hfAuthFn, ""), s3AuthFn, "")
		for _, v := range []string{"HF_TOKEN", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"} {
			if idx := strings.Index(rest, v); idx >= 0 {
				lo, hi := max(0, idx-80), min(len(rest), idx+80)
				t.Errorf("command %d uses %s outside the credential helpers: …%s…", i, v, rest[lo:hi])
			}
		}
	}
}

func runWithFakeCurl(t *testing.T, script string, env []string) (argv, stdin string) {
	t.Helper()
	dir := t.TempDir()
	writeFakeCurl(t, dir)
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append([]string{"PATH=" + dir + ":/usr/bin:/bin", "CAPTURE_DIR=" + dir}, env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	a, err := os.ReadFile(filepath.Join(dir, "argv"))
	if err != nil {
		t.Fatalf("fake curl did not run: %v", err)
	}
	s, _ := os.ReadFile(filepath.Join(dir, "stdin"))
	return string(a), string(s)
}

func writeFakeCurl(t *testing.T, dir string) {
	t.Helper()
	fake := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$CAPTURE_DIR/argv\"\ncat > \"$CAPTURE_DIR/stdin\"\n"
	if err := os.WriteFile(filepath.Join(dir, "curl"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
}

// The never-in-argv guarantee rests on printf being a shell builtin. A helper
// that reaches for an external printf would run this PATH fake and fail here.
func TestCredentialHelpers_PrintfIsBuiltin(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	writeFakeCurl(t, dir)
	fake := "#!/bin/sh\necho ran >> \"$CAPTURE_DIR/printf-ran\"\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "printf"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	script := hfAuthFn + `hf_curl -fsS -o /dev/null "https://huggingface.co/org/repo/resolve/main/config.json"`
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin", "CAPTURE_DIR=" + dir, "HF_TOKEN=hf_s3cr3tT0ken"}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "printf-ran")); err == nil {
		t.Error("sh resolved printf from PATH; the container guarantee needs printf builtin")
	}
}
