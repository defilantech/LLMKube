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
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// omlxModelSettingsFile is the per-model settings file oMLX keeps under its
// base path (omlx/model_settings.py, ModelSettingsManager). Its shape is
// {"version": 1, "models": {"<model dir name>": {"model_alias": "...", ...}}}.
// oMLX resolves a request's model by exact directory name first and then by
// model_alias (omlx/engine_pool.py, resolve_model_id), and /v1/models lists
// the alias in place of the directory name. The file is read once when the
// daemon starts, so a changed alias only takes effect after a restart.
const omlxModelSettingsFile = "model_settings.json"

// omlxModelAliasKey is the ModelSettings field oMLX treats as the API-visible
// name of a model directory.
const omlxModelAliasKey = "model_alias"

// omlxDefaultBasePath resolves the base path NewOMLXExecutor uses. It is a
// variable so TestMain can point every executor a test builds at a temp dir;
// otherwise a test that sets a served name would write the developer's real
// ~/.omlx/model_settings.json.
var omlxDefaultBasePath = defaultOMLXBasePath

// defaultOMLXBasePath mirrors oMLX's DEFAULT_BASE_PATH (~/.omlx, see
// omlx/settings.py). The executor passes it to the daemon as --base-path so
// the settings file the agent writes is the one the daemon reads. It returns
// "" when the home directory cannot be resolved, which disables alias
// registration rather than guessing a path.
func defaultOMLXBasePath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".omlx")
}

// setOMLXModelAlias makes alias the model_alias of modelID in the oMLX
// settings file at path, creating the file if needed. An empty alias clears
// any alias modelID carries. The same alias is removed from every other model
// entry first, because oMLX resolves an alias to the first entry that claims
// it and the admin API refuses duplicates. Every other key in the file is
// preserved byte for byte, since the file also holds settings the operator
// set through the oMLX admin UI. It reports whether the file changed; an
// unchanged file is not rewritten.
func setOMLXModelAlias(path, modelID, alias string) (bool, error) {
	doc := map[string]json.RawMessage{}
	models := map[string]map[string]json.RawMessage{}
	mode := fs.FileMode(0o600)

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &doc); err != nil {
			return false, fmt.Errorf("failed to parse oMLX settings %s: %w", path, err)
		}
		if raw, ok := doc["models"]; ok {
			if err := json.Unmarshal(raw, &models); err != nil {
				return false, fmt.Errorf("failed to parse models in oMLX settings %s: %w", path, err)
			}
		}
		if fi, err := os.Stat(path); err == nil {
			mode = fi.Mode().Perm()
		}
	case errors.Is(err, fs.ErrNotExist):
	default:
		return false, fmt.Errorf("failed to read oMLX settings %s: %w", path, err)
	}
	if models == nil {
		models = map[string]map[string]json.RawMessage{}
	}

	changed := false
	if alias != "" {
		for id, entry := range models {
			if id != modelID && omlxEntryAlias(entry) == alias {
				delete(entry, omlxModelAliasKey)
				changed = true
			}
		}
	}

	entry := models[modelID]
	switch {
	case alias == "":
		if _, ok := entry[omlxModelAliasKey]; ok {
			delete(entry, omlxModelAliasKey)
			changed = true
		}
	case omlxEntryAlias(entry) != alias:
		if entry == nil {
			entry = map[string]json.RawMessage{}
			models[modelID] = entry
		}
		raw, err := json.Marshal(alias)
		if err != nil {
			return false, err
		}
		entry[omlxModelAliasKey] = raw
		changed = true
	}
	if !changed {
		return false, nil
	}

	rawModels, err := json.Marshal(models)
	if err != nil {
		return false, err
	}
	doc["models"] = rawModels
	if _, ok := doc["version"]; !ok {
		doc["version"] = json.RawMessage("1")
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return false, err
	}
	if err := writeFileAtomic(path, append(out, '\n'), mode); err != nil {
		return false, fmt.Errorf("failed to write oMLX settings %s: %w", path, err)
	}
	return true, nil
}

// omlxEntryAlias returns the model_alias string of one settings entry, or ""
// when it is absent, null, or not a string.
func omlxEntryAlias(entry map[string]json.RawMessage) string {
	var s string
	if raw, ok := entry[omlxModelAliasKey]; ok {
		_ = json.Unmarshal(raw, &s)
	}
	return s
}

// writeFileAtomic writes data to a temporary file next to path and renames it
// into place, so a daemon starting concurrently never reads a torn file.
func writeFileAtomic(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".model_settings-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
