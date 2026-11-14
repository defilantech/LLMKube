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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"go.uber.org/zap"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// tokenRenewMaxWait caps one wait between checks. Timers do not advance
	// while a Mac sleeps, so a wait of days would wake late; the wall clock is
	// re-read at least this often instead.
	tokenRenewMaxWait = 10 * time.Minute
	// tokenRenewRetry is the wait after a failed renewal.
	tokenRenewRetry = time.Minute
	// tokenRenewTimeout bounds one TokenRequest.
	tokenRenewTimeout = 30 * time.Second
	// tokenMinLifetime is the shortest lifetime TokenRequest accepts.
	tokenMinLifetime = 10 * time.Minute
)

// tokenRenewer keeps the agent's own ServiceAccount token current (#2232).
// `llmkube metal join` writes the token to the file the agent's kubeconfig
// names as tokenFile, and client-go re-reads that file about once a minute,
// so replacing it is all a renewal takes. The new token is requested with
// the current one, through TokenRequest on the agent's own ServiceAccount,
// which the per-host Role allows by resourceNames. Renewal is due at half the
// token's lifetime; deleting the ServiceAccount (llmkube metal remove)
// revokes the token and ends renewal with it.
type tokenRenewer struct {
	client client.Client
	path   string
	logger *zap.SugaredLogger
	now    func() time.Time
}

// serviceAccountToken is what the renewer reads from a token's claims. The
// claims are not verified here: the API server verifies the token, and these
// only name the ServiceAccount and the schedule.
type serviceAccountToken struct {
	namespace, name string
	issued, expires time.Time
	lifetime        time.Duration
	renewAt         time.Time
}

// parseServiceAccountToken reads the claims of a ServiceAccount JWT.
func parseServiceAccountToken(raw string) (serviceAccountToken, error) {
	parts := strings.Split(strings.TrimSpace(raw), ".")
	if len(parts) != 3 {
		return serviceAccountToken{}, errors.New("not a JWT")
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return serviceAccountToken{}, errors.New("JWT claims are not base64url")
	}
	var claims struct {
		Exp int64  `json:"exp"`
		Iat int64  `json:"iat"`
		Sub string `json:"sub"`
		K8s struct {
			Namespace      string `json:"namespace"`
			ServiceAccount struct {
				Name string `json:"name"`
			} `json:"serviceaccount"`
		} `json:"kubernetes.io"`
	}
	if err := json.Unmarshal(body, &claims); err != nil {
		return serviceAccountToken{}, errors.New("JWT claims are not JSON")
	}
	t := serviceAccountToken{namespace: claims.K8s.Namespace, name: claims.K8s.ServiceAccount.Name}
	if t.namespace == "" || t.name == "" {
		// sub is system:serviceaccount:<namespace>:<name>.
		if rest, ok := strings.CutPrefix(claims.Sub, "system:serviceaccount:"); ok {
			t.namespace, t.name, _ = strings.Cut(rest, ":")
		}
	}
	if t.namespace == "" || t.name == "" {
		return serviceAccountToken{}, errors.New("not a ServiceAccount token")
	}
	if claims.Iat <= 0 || claims.Exp <= claims.Iat {
		return serviceAccountToken{}, errors.New("token has no usable iat and exp")
	}
	t.issued, t.expires = time.Unix(claims.Iat, 0), time.Unix(claims.Exp, 0)
	t.lifetime = t.expires.Sub(t.issued)
	t.renewAt = t.issued.Add(t.lifetime / 2)
	return t, nil
}

// startTokenRenewer starts renewal when --renew-service-account-token gave
// the agent a token file. It reports whether it started.
func (a *MetalAgent) startTokenRenewer(ctx context.Context) bool {
	if a.config.RenewTokenFile == "" {
		return false
	}
	r := &tokenRenewer{
		client: a.config.K8sClient,
		path:   a.config.RenewTokenFile,
		logger: a.logger.With("subsystem", "token-renewer"),
		now:    time.Now,
	}
	a.logger.Infow("renewing the agent's ServiceAccount token", "tokenFile", r.path)
	go r.run(ctx)
	return true
}

func (r *tokenRenewer) run(ctx context.Context) {
	for {
		wait := r.renewIfDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// renewIfDue renews the token when half its lifetime has passed and returns
// how long to wait before looking again.
func (r *tokenRenewer) renewIfDue(ctx context.Context) time.Duration {
	raw, err := os.ReadFile(r.path)
	if err != nil {
		r.logger.Errorw("cannot read the ServiceAccount token file", "error", err)
		return tokenRenewRetry
	}
	cur, err := parseServiceAccountToken(string(raw))
	if err != nil {
		r.logger.Errorw("the token file does not hold a ServiceAccount token; it is not renewed", "error", err)
		return tokenRenewRetry
	}
	now := r.now()
	if now.Before(cur.renewAt) {
		return min(cur.renewAt.Sub(now), tokenRenewMaxWait)
	}
	next, err := r.renew(ctx, cur)
	if err != nil {
		if now.After(cur.expires) {
			r.logger.Errorw("the agent's token has expired and cannot be renewed; join the host again "+
				"(llmkube metal token create, then llmkube metal join)", "expired", cur.expires, "error", err)
		} else {
			r.logger.Warnw("token renewal failed; retrying", "expires", cur.expires, "error", err)
		}
		return tokenRenewRetry
	}
	r.logger.Infow("renewed the agent's ServiceAccount token", "expires", next.expires)
	// At least tokenRenewRetry: a Mac clock ahead of the API server's, or a
	// server that cut the lifetime short, can put the new token's half-life
	// in the past, and renewing at once again would spin.
	return min(max(next.renewAt.Sub(r.now()), tokenRenewRetry), tokenRenewMaxWait)
}

// renew requests a token with the current one's lifetime for the same
// ServiceAccount and replaces the file with it.
func (r *tokenRenewer) renew(ctx context.Context, cur serviceAccountToken) (serviceAccountToken, error) {
	ctx, cancel := context.WithTimeout(ctx, tokenRenewTimeout)
	defer cancel()
	seconds := int64(max(cur.lifetime, tokenMinLifetime) / time.Second)
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: cur.namespace, Name: cur.name}}
	tr := &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: &seconds}}
	if err := r.client.SubResource("token").Create(ctx, sa, tr); err != nil {
		return serviceAccountToken{}, fmt.Errorf("TokenRequest for ServiceAccount %s/%s: %w", cur.namespace, cur.name, err)
	}
	next, err := parseServiceAccountToken(tr.Status.Token)
	if err != nil {
		return serviceAccountToken{}, fmt.Errorf("the API server returned a token that does not parse: %w", err)
	}
	if next.namespace != cur.namespace || next.name != cur.name {
		return serviceAccountToken{}, fmt.Errorf("the API server returned a token for %s/%s, not %s/%s",
			next.namespace, next.name, cur.namespace, cur.name)
	}
	if err := writeFileAtomic(r.path, []byte(tr.Status.Token), 0o600); err != nil {
		return serviceAccountToken{}, fmt.Errorf("write %s: %w", r.path, err)
	}
	return next, nil
}
