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

package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func newMetalRemoveCommand() *cobra.Command {
	var namespace string
	var includeUnlabeled bool
	cmd := &cobra.Command{
		Use:   "remove <mac-name>",
		Short: "Revoke a Mac and delete its objects",
		Long: `Revoke a Mac's access to the cluster and delete what 'llmkube metal token create'
made for it.

The Mac's ServiceAccount goes first, and the command waits until it is gone:
that revokes every token the Mac holds, including ones it minted for itself.
A token used in the last ~10 seconds can keep working for up to ~10 seconds
more (the API server caches successful authentications). Then the per-Mac
Role, both RoleBindings, the Lease, the join Secret and the MetalNode are
deleted. The shared llmkube-metal-agent Role stays. Objects that are already
gone are skipped, so running it twice is fine.

Only objects labeled inference.llmkube.dev/metal-node=<mac-name> are deleted.
Anything else with one of the Mac's names (another app's Lease, objects
labeled for another Mac, objects applied by hand) is left in place and
listed. The Lease is also deleted, with a warning, when the Mac's MetalNode,
labeled for the Mac, names it as its Lease: the Mac can change its Lease's
labels. --include-unlabeled also deletes unlabeled objects with the Mac's
names, for a Mac set up by hand; objects labeled for another Mac are never
deleted. If the ServiceAccount is left in place, nothing was revoked and the
command fails.

This does not stop the agent on the Mac.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMetalRemove(cmd, args[0], namespace, includeUnlabeled)
		},
	}
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "",
		"Namespace of the Mac's objects (default: the kubeconfig context's namespace, else default)")
	cmd.Flags().BoolVar(&includeUnlabeled, "include-unlabeled", false,
		"Also delete unlabeled objects with the Mac's names (a Mac whose objects were applied by hand)")
	return cmd
}

func runMetalRemove(cmd *cobra.Command, mac, namespaceFlag string, includeUnlabeled bool) error {
	if err := validateMacName(mac); err != nil {
		return err
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	cfg, namespace, err := loadMetalAdminConfig(namespaceFlag)
	if err != nil {
		return err
	}
	scheme, err := metalScheme()
	if err != nil {
		return err
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("create client: %w", err)
	}
	return metalRemove(ctx, c, namespace, mac, includeUnlabeled, cmd.OutOrStdout(), cmd.ErrOrStderr())
}

// metalRemove deletes the Mac's objects, ServiceAccount first, but only
// those labeled for this Mac (and, with includeUnlabeled, unlabeled ones with
// its names). It never deletes the shared Role. Removed and skipped objects
// go to out, warnings to log. It fails when the ServiceAccount was left in
// place, because then nothing was revoked.
func metalRemove(ctx context.Context, c client.Client, namespace, mac string, includeUnlabeled bool,
	out, log io.Writer) error {
	n := newMetalNames(mac)
	targets := metalDerivedObjects(namespace, n)

	removed, skippedSA, skippedUnlabeled := 0, false, false
	for _, t := range targets {
		found, err := getMetalObject(ctx, c, t.kind, t.obj)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		owner := t.obj.GetLabels()[inferencev1alpha1.LabelMetalNode]
		claimed := false
		if owner != mac && t.kind == "Lease" {
			// The Mac may change its Lease's labels; its MetalNode decides.
			if claimed, err = metalNodeClaimsLease(ctx, c, namespace, n); err != nil {
				return err
			}
		}
		switch {
		case owner == mac:
		case claimed:
			_, _ = fmt.Fprintf(log, "warning: Lease %s/%s is labeled %s=%q, but MetalNode %s/%s names it as its "+
				"Lease; deleting it\n", namespace, t.obj.GetName(), inferencev1alpha1.LabelMetalNode, owner,
				namespace, n.MetalNode)
		case owner == "" && includeUnlabeled:
			_, _ = fmt.Fprintf(log, "warning: deleting unlabeled %s %s/%s by its name (--include-unlabeled)\n",
				t.kind, namespace, t.obj.GetName())
		default:
			why := "not labeled " + inferencev1alpha1.LabelMetalNode + "=" + mac
			if owner != "" {
				why = "it belongs to Mac " + owner
			} else {
				skippedUnlabeled = true
			}
			_, _ = fmt.Fprintf(out, "skipped %s %s/%s: %s\n", t.kind, namespace, t.obj.GetName(), why)
			skippedSA = skippedSA || t.kind == "ServiceAccount"
			continue
		}
		gone, err := deleteMetalObject(ctx, c, t.kind, t.obj)
		if err != nil {
			return err
		}
		if !gone {
			continue
		}
		removed++
		if t.kind != "ServiceAccount" {
			_, _ = fmt.Fprintf(out, "removed %s %s/%s\n", t.kind, namespace, t.obj.GetName())
			continue
		}
		if err := waitGone(ctx, c, t.obj); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(out, "removed ServiceAccount %s/%s: the Mac's tokens are revoked "+
			"(one used in the last ~10 s may work ~10 s longer)\n", namespace, n.ServiceAccount)
	}
	if removed == 0 && !skippedSA && !skippedUnlabeled {
		_, _ = fmt.Fprintf(out, "nothing to remove for Mac %s in namespace %s\n", mac, namespace)
	}
	if skippedUnlabeled {
		_, _ = fmt.Fprintf(log, "unlabeled objects were left in place; if they are this Mac's (applied by hand), "+
			"rerun with --include-unlabeled\n")
	}
	if skippedSA {
		return fmt.Errorf("ServiceAccount %s/%s is not this Mac's, so it was left in place and the Mac's tokens "+
			"are NOT revoked", namespace, n.ServiceAccount)
	}
	return nil
}

// getMetalObject reads obj by name and reports whether it exists. A missing
// MetalNode resource counts as not found.
func getMetalObject(ctx context.Context, c client.Client, kind string, obj client.Object) (bool, error) {
	err := c.Get(ctx, client.ObjectKeyFromObject(obj), obj)
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err), meta.IsNoMatchError(err):
		return false, nil
	}
	return false, fmt.Errorf("get %s %s/%s: %w", kind, obj.GetNamespace(), obj.GetName(), err)
}

// deleteMetalObject deletes the object that was read (the UID precondition
// keeps a replacement created since from being deleted in its place) and
// reports whether it was there to delete.
func deleteMetalObject(ctx context.Context, c client.Client, kind string, obj client.Object) (bool, error) {
	uid := obj.GetUID()
	err := c.Delete(ctx, obj, client.Preconditions{UID: &uid})
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err), meta.IsNoMatchError(err):
		return false, nil
	}
	return false, fmt.Errorf("delete %s %s/%s: %w", kind, obj.GetNamespace(), obj.GetName(), err)
}
