// Copyright 2011-2018 Paul Ruane.
// Modified 2026: ported from go-fuse to cgofuse

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

// +build !windows

package cli

import (
	"fmt"
	"github.com/winfsp/cgofuse/fuse"
	"tmsu/common/log"
	"tmsu/vfs"
	"os"
	"strings"
)

var VfsCommand = Command{
	Name:     "tmsu/vfs",
	Synopsis: "Hosts the virtual filesystem",
	Usages:   []string{"tmsu vfs [OPTION]... MOUNTPOINT"},
	Description: `This subcommand is the foreground process which hosts the virtual filesystem. It is run automatically when a virtual filesystem is mounted using the 'mount' subcommand and terminated when the virtual filesystem is unmounted.

It is not normally necessary to issue this subcommand manually unless debugging the virtual filesystem. For debug output use the --verbose option.`,
	Options: Options{{"--options", "-o", "mount options", true, ""}},
	Exec:    vfsExec,
	Hidden:  true,
}

// unexported

func vfsExec(options Options, args []string, databasePath string) (error, warnings) {
	if len(args) == 0 {
		return fmt.Errorf("mountpoint not specified"), nil
	}

	mountOptions := []string{}
	if options.HasOption("--options") {
		mountOptions = strings.Split(options.Get("--options").Argument, ",")
	}

	mountPath := args[0]

	store, err := openDatabase(databasePath)
	if err != nil {
		return err, nil
	}
	defer store.Close()

	// Create the TMSU filesystem instance
	tmsuFs := vfs.NewTmsuFs(store, databasePath, mountPath)

	// Create the cgofuse host and mount
	host := fuse.NewFileSystemHost(tmsuFs)
	host.SetCapReaddirPlus(true)

	// Tell cgofuse to use FUSE-T on macOS
	os.Setenv("CGOFUSE_LIBFUSE_PATH", "/usr/local/lib/libfuse-t.dylib")

	log.Infof(2, "mounting virtual filesystem at '%v'", mountPath)

	// Mount is a blocking call — it returns when the filesystem is unmounted.
	// It handles the full FUSE lifecycle: mount → init → loop → destroy → unmount.
	if !host.Mount(mountPath, mountOptions) {
		return fmt.Errorf("could not mount virtual filesystem at '%v'", mountPath), nil
	}

	log.Infof(2, "virtual filesystem unmounted from '%v'", mountPath)

	return nil, nil
}