/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/defilantech/llmkube/pkg/foreman/audit"
)

// NewAuditCommand is the `llmkube audit` group: export the durable Foreman
// audit records as a portable JSONL bundle for compliance review.
func NewAuditCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Inspect and export Foreman run audit records",
		Long:  "Aggregate the durable per-run audit records Foreman writes for compliance review.",
	}
	cmd.AddCommand(newAuditExportCommand())
	return cmd
}

func newAuditExportCommand() *cobra.Command {
	var namespace, repo, since, output string
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export audit records as JSONL",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newAuditClient()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if output != "" {
				f, ferr := os.Create(output)
				if ferr != nil {
					return fmt.Errorf("create %s: %w", output, ferr)
				}
				defer func() { _ = f.Close() }()
				out = f
			}
			return exportAuditRecords(cmd.Context(), c, namespace, repo, since, out)
		},
	}
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "default", "namespace to read audit records from")
	cmd.Flags().StringVar(&repo, "repo", "", "filter to records for this repo (owner/name)")
	cmd.Flags().StringVar(&since, "since", "", "only records with recordedAt >= this RFC3339 timestamp")
	cmd.Flags().StringVarP(&output, "output", "o", "", "write JSONL to this file instead of stdout")
	return cmd
}

func newAuditClient() (client.Client, error) {
	cfg, err := config.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get kubeconfig: %w", err)
	}
	return client.New(cfg, client.Options{Scheme: scheme.Scheme})
}

// exportAuditRecords lists audit ConfigMaps, decodes them, applies repo/since
// filters, sorts by recordedAt, and writes one JSON Record per line.
func exportAuditRecords(ctx context.Context, c client.Client, namespace, repo, since string, w io.Writer) error {
	var cms corev1.ConfigMapList
	if err := c.List(ctx, &cms,
		client.InNamespace(namespace),
		client.MatchingLabels{audit.AuditLabel: "true"},
	); err != nil {
		return fmt.Errorf("list audit records: %w", err)
	}

	records := make([]audit.Record, 0, len(cms.Items))
	for i := range cms.Items {
		raw, ok := cms.Items[i].Data["audit.json"]
		if !ok {
			continue
		}
		var rec audit.Record
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			continue // skip malformed record rather than abort the export
		}
		if repo != "" && rec.Repo != repo {
			continue
		}
		if since != "" && rec.RecordedAt < since {
			continue // RFC3339 strings sort lexicographically by time
		}
		records = append(records, rec)
	}
	sort.SliceStable(records, func(i, j int) bool {
		return records[i].RecordedAt < records[j].RecordedAt
	})

	enc := json.NewEncoder(w)
	for i := range records {
		if err := enc.Encode(records[i]); err != nil {
			return fmt.Errorf("encode record: %w", err)
		}
	}
	return nil
}
