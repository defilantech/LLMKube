//go:build unix

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

import "golang.org/x/sys/unix"

// filesystemFreeBytes returns the bytes an unprivileged process may still
// write on the filesystem holding path (statfs f_bavail times the block
// size), the figure a download competes for.
func filesystemFreeBytes(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	// The statfs field types differ per GOOS; the counts are non-negative.
	return uint64(st.Bavail) * uint64(st.Bsize), nil //nolint:gosec,unconvert // G115: non-negative
}
