// Copyright 2011-2018 Paul Ruane.
// Modified 2026: support FUSE-T on macOS

// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.

// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

// +build darwin

package vfs

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Mount struct {
	DatabasePath string
	MountPath    string
}

func GetMountTable() ([]Mount, error) {
	return detectMounts()
}

// detectMounts scans candidate mount points for TMSU mounts by looking
// for the .database symlink that TMSU creates at the root of its VFS.
func detectMounts() ([]Mount, error) {
	mountTable := make([]Mount, 0, 10)

	// Parse mount output to find all mount points
	cmd := exec.Command("/sbin/mount")
	out, err := cmd.Output()
	if err != nil {
		// mount command not available — return empty table
		return mountTable, nil
	}

	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		// Format: "devfs on /path (fstype, options)"
		parts := strings.Split(line, " on ")
		if len(parts) < 2 {
			continue
		}
		mountParts := strings.Split(parts[1], " (")
		if len(mountParts) < 1 {
			continue
		}
		mountpoint := strings.TrimSpace(mountParts[0])

		// Check if this mount point has a .database symlink (TMSU marker)
		databaseSymlink := filepath.Join(mountpoint, databaseFilename)
		databasePath, err := os.Readlink(databaseSymlink)
		if err != nil {
			continue
		}

		mountTable = append(mountTable, Mount{databasePath, mountpoint})
	}

	return mountTable, nil
}