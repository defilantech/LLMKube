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
	"io/fs"
	"path/filepath"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// modelStoreReportInterval is how often the model store gauges are refreshed.
const modelStoreReportInterval = time.Minute

// modelStoreUsedBytes and modelStoreFreeBytes describe the model store
// (--model-store) so model churn that fills the disk can be alerted on
// before a start fails (#2409).
var (
	modelStoreUsedBytes = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "llmkube_metal_agent_model_store_used_bytes",
		Help: "Bytes of regular files under the agent's model store: downloaded models, partial downloads, " +
			"digest stamps and engine logs. Refreshed every minute.",
	})
	modelStoreFreeBytes = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "llmkube_metal_agent_model_store_free_bytes",
		Help: "Bytes still free on the filesystem holding the agent's model store, the space a model download " +
			"competes for. Refreshed every minute.",
	})
)

// reportModelStore sets the model store gauges: the bytes of the regular files
// under store (a symlink is not followed or counted) and the free space of its
// filesystem. A gauge it cannot measure keeps its last value.
func reportModelStore(store string, freeBytes func(string) (uint64, error), logger *zap.SugaredLogger) {
	var used int64
	err := filepath.WalkDir(store, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == store {
				return err // the store itself is unreadable: keep the last value
			}
			return nil // a file removed during the walk, or one we cannot read
		}
		if d.Type().IsRegular() {
			if info, ierr := d.Info(); ierr == nil {
				used += info.Size()
			}
		}
		return nil
	})
	if err != nil {
		logger.Debugw("could not measure the model store", "path", store, "error", err)
	} else {
		modelStoreUsedBytes.Set(float64(used))
	}
	if free, ferr := freeBytes(store); ferr == nil {
		modelStoreFreeBytes.Set(float64(free))
	} else {
		logger.Debugw("could not read the model store's free space", "path", store, "error", ferr)
	}
}

// watchModelStore refreshes the model store gauges at once and then every
// modelStoreReportInterval until ctx ends.
func (a *MetalAgent) watchModelStore(ctx context.Context) {
	store := a.config.ModelStorePath
	if store == "" {
		return
	}
	logger := a.logger.With("subsystem", "model-store")
	reportModelStore(store, filesystemFreeBytes, logger)
	ticker := time.NewTicker(modelStoreReportInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reportModelStore(store, filesystemFreeBytes, logger)
		}
	}
}
