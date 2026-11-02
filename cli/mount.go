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
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

var MountCommand = Command{
	Name:     "mount",
	Synopsis: "Mount the virtual filesystem",
	Usages: []string{"tmsu mount",
		"tmsu mount [OPTION]... [FILE] MOUNTPOINT"},
	Description: `Without arguments, lists the currently mounted file-systems, otherwise mounts a virtual file-system at the path MOUNTPOINT.

By default the VFS runs as a background daemon — use 'tmsu unmount' to unmount.
Pass --foreground to run in the foreground (Ctrl+C to unmount).

Where FILE is specified, the database at FILE is mounted.

If FILE is not specified but the TMSU_DB environment variable is defined then the database at TMSU_DB is mounted.

Where neither FILE is specified nor TMSU_DB defined then the default database is mounted.

To allow other users access to the mounted filesystem, pass the 'allow_other' FUSE option, e.g. 'tmsu mount --options=allow_other mp'. (FUSE only allows the root user to use this option unless 'user_allow_other' is present in '/etc/fuse.conf'.)`,
	Examples: []string{"$ tmsu mount mp",
		"$ tmsu mount /tmp/db mp",
		"$ tmsu mount --foreground mp",
		"$ tmsu mount --options=allow_other mp"},
	Options: Options{
		Option{"--options", "-o", "mount options (passed to FUSE)", true, ""},
		Option{"--foreground", "-F", "run in foreground (Ctrl+C to unmount)", false, ""},
	},
	Exec:    mountExec,
}

// unexported

func mountExec(options Options, args []string, databasePath string) (error, warnings) {
	var mountOptions string
	if options.HasOption("--options") {
		mountOptions = options.Get("--options").Argument
	}

	if options.HasOption("--foreground") {
		return mountForeground(args, mountOptions, databasePath)
	}

	store, err := openDatabase(databasePath)
	if err != nil {
		return err, nil
	}
	defer store.Close()

	tx, err := store.Begin()
	if err != nil {
		return err, nil
	}
	defer tx.Commit()

	switch len(args) {
	case 0:
		if err := listMounts(); err != nil {
			return err, nil
		}
	case 1:
		mountPath := args[0]

		if err := mountExplicit(store.DbPath, mountPath, mountOptions); err != nil {
			return err, nil
		}
	case 2:
		databasePath := args[0]
		mountPath := args[1]

		if err := mountExplicit(databasePath, mountPath, mountOptions); err != nil {
			return err, nil
		}
	default:
		return fmt.Errorf("too many arguments"), nil
	}

	return nil, nil
}

// mountForeground runs the VFS in the foreground (blocking, Ctrl+C to unmount).
func mountForeground(args []string, mountOptions string, databasePath string) (error, warnings) {
	if len(args) == 0 {
		return fmt.Errorf("mountpoint not specified"), nil
	}

	mountPath, err := filepath.Abs(args[0])
	if err != nil {
		return fmt.Errorf("could not resolve mount path '%v': %v", args[0], err), nil
	}

	// Parse mount options string into list
	mountOptionList := []string{}
	if mountOptions != "" {
		mountOptionList = strings.Split(mountOptions, ",")
	}

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
	if !host.Mount(mountPath, mountOptionList) {
		return fmt.Errorf("could not mount virtual filesystem at '%v'", mountPath), nil
	}

	log.Infof(2, "virtual filesystem unmounted from '%v'", mountPath)

	return nil, nil
}

func listMounts() error {
	log.Info(2, "retrieving mount table.")

	mt, err := vfs.GetMountTable()
	if err != nil {
		return fmt.Errorf("could not get mount table: %v", err)
	}

	if len(mt) == 0 {
		log.Info(2, "mount table is empty.")
	}

	dbPathWidth := 0
	for _, mount := range mt {
		if len(mount.DatabasePath) > dbPathWidth {
			dbPathWidth = len(mount.DatabasePath)
		}
	}

	for _, mount := range mt {
		fmt.Printf("%-*v\tat\t%v\n", dbPathWidth, mount.DatabasePath, mount.MountPath)
	}

	return nil
}

func mountExplicit(databasePath string, mountPath string, mountOptions string) error {
	if alreadyMounted(mountPath) {
		return fmt.Errorf("%v: mount path already in use", mountPath)
	}

	stat, err := os.Stat(mountPath)
	if err != nil {
		return fmt.Errorf("%v: could not stat: %v", mountPath, err)
	}
	if stat == nil {
		return fmt.Errorf("%v: mount point does not exist", mountPath)
	}
	if !stat.IsDir() {
		return fmt.Errorf("%v: mount point is not a directory", mountPath)
	}

	stat, err = os.Stat(databasePath)
	if err != nil {
		return fmt.Errorf("%v: could not stat: %v", databasePath, err)
	}
	if stat == nil {
		return fmt.Errorf("%v: database does not exist", databasePath)
	}

	log.Infof(2, "spawning daemon to mount VFS for database '%v' at '%v'", databasePath, mountPath)

	// Spawn self with --foreground instead of the old hidden tmsu/vfs command
	args := []string{"mount", "--foreground", "--database", databasePath, mountPath}
	if mountOptions != "" {
		args = append(args, "--options", mountOptions)
	}
	daemon := exec.Command(os.Args[0], args...)

	tempFile, err := ioutil.TempFile("", "tmsu-vfs-")
	if err != nil {
		return fmt.Errorf("could not get a temporary file: %v", err)
	}
	daemon.Stderr = tempFile

	err = daemon.Start()
	if err != nil {
		return fmt.Errorf("could not start daemon: %v", err)
	}

	log.Info(2, "sleeping.")

	const halfSecond = 500000000
	time.Sleep(halfSecond)

	log.Info(2, "checking whether daemon started successfully.")

	var waitStatus syscall.WaitStatus
	var rusage syscall.Rusage
	_, err = syscall.Wait4(daemon.Process.Pid, &waitStatus, syscall.WNOHANG, &rusage)
	if err != nil {
		return fmt.Errorf("could not check daemon status: %v", err)
	}

	if waitStatus.Exited() {
		if waitStatus.ExitStatus() != 0 {
			return fmt.Errorf("virtual filesystem mount failed: see standard error output: %v", tempFile.Name())
		}
	}

	return nil
}

func alreadyMounted(path string) bool {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}

	mt, err := vfs.GetMountTable()
	if err != nil {
		return false
	}

	for _, mount := range mt {
		if mount.MountPath == absPath {
			return true
		}
	}

	return false
}