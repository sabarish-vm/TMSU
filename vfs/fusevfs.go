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

package vfs

import (
	"fmt"
	"github.com/winfsp/cgofuse/fuse"
	"tmsu/common/log"
	"tmsu/entities"
	"tmsu/query"
	"tmsu/storage"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const helpFilename = "README.md"
const databaseFilename = ".database"
const filesDir = "files"

const tagsDir = "tags"
const tagsDirHelp = `Tags Directories
----------------

Tags you create will appear here as directories. Inside a tag directory are the
files that have that tag and the other tags applied to those files.

Descend the tag directories to hone in on the files you want:

    $ ls
    cheese  mushroom  tomato  wine
    $ ls cheese
    edam_blanc.14  funghi.11  margherita.7  mushroom  pino_cheddar.12  tomato  wine
    $ ls cheese/tomato
    margherita.7

The tags directory also allows some operations to be performed:

  * Create a tag by creating a new directory
  * Rename a tag by renaming the tag directory
  * Untag a file by deleting the file symlink from the tag directory
  * Delete an unused tag by deleting the directory

(This file will hide once you have created a few tags.)`

const queriesDir = "queries"
const queryDirHelp = `Query Directories
-----------------

Change to any directory that is a valid query to see a view of the files that
match the query. (It is not necessary to create the directory first.)

    $ ls
    README.md
    $ ls "cheese and wine"
    pinot_cheddar.12  edam_blanc.14
    $ ls "cheese and (tomato or mushroom)"
    margherita.7  funghi.11
    $ ls
    cheese and (tomato or mushroom)  cheese and wine

You can even create new queries by typing the query into the file chooser of a
graphical program.

Use ` + "`rmdir`" + ` to remove any query directory you no longer need. Do not use ` + "`rm -r`" + `
as this will untag the contained files.

(This file will hide once you have created a query.)`

// TmsuFs implements fuse.FileSystemInterface by embedding fuse.FileSystemBase.
// This delegates operations to the TMSU storage backend and presents a virtual
// filesystem with tags, queries, and symlinks to tagged files.
type TmsuFs struct {
	fuse.FileSystemBase

	store     *storage.Storage
	mountPath string
	dbPath    string
	nextFh    uint64
	lock      sync.Mutex

	// In-memory file handles for help text files and virtual content.
	// Key: fh (uint64), Value: file content ([]byte)
	fileData map[uint64][]byte
}

func NewTmsuFs(store *storage.Storage, dbPath string, mountPath string) *TmsuFs {
	return &TmsuFs{
		store:     store,
		mountPath: mountPath,
		dbPath:    dbPath,
		nextFh:    1,
		fileData:  map[uint64][]byte{},
	}
}

// --- Thread safety ---

func (fs *TmsuFs) synchronize() func() {
	fs.lock.Lock()
	return func() {
		fs.lock.Unlock()
	}
}

// --- Init / Destroy ---

func (fs *TmsuFs) Init() {
	log.Infof(2, "TmsuFs.Init()")
}

func (fs *TmsuFs) Destroy() {
	log.Infof(2, "TmsuFs.Destroy()")
}

// --- Statfs ---

func (fs *TmsuFs) Statfs(path string, stat *fuse.Statfs_t) int {
	log.Infof(2, "Statfs(%v)", path)
	defer fs.synchronize()()
	stat.Bsize = 4096
	stat.Frsize = 4096
	stat.Blocks = 0
	stat.Bfree = 0
	stat.Bavail = 0
	stat.Files = 0
	stat.Ffree = 0
	stat.Favail = 0
	stat.Flag = 1 // ST_RDONLY
	stat.Namemax = 255
	return 0
}

// --- Getattr ---

func (fs *TmsuFs) Getattr(path string, stat *fuse.Stat_t, fh uint64) int {
	defer fs.synchronize()()
	log.Infof(2, "Getattr(%v, fh=%v)", path, fh)

	switch path {
	case "/":
		return fillDirAttr(stat)
	case "/" + databaseFilename:
		return fs.getDatabaseFileAttr(stat)
	case "/" + tagsDir:
		return fs.getTagsAttr(stat)
	case "/" + queriesDir:
		return fs.getQueryAttr(stat)
	}
	
	// Check help files (special paths under /tags/ and /queries/)
	if path == "/" + tagsDir + "/" + helpFilename {
		return fillRegAttr(stat, uint64(len(tagsDirHelp)))
	}
	if path == "/" + queriesDir + "/" + helpFilename {
		return fillRegAttr(stat, uint64(len(queryDirHelp)))
	}

	// Strip leading "/" and split path
	cleanPath := path
	if cleanPath[0] == '/' && cleanPath != "/" {
		cleanPath = cleanPath[1:]
	}
	parts := fs.splitPath(cleanPath)

	if len(parts) < 1 || parts[0] == "" {
		return fillDirAttr(stat)
	}

	switch parts[0] {
	case tagsDir:
		return fs.getTaggedEntryAttr(stat, parts[1:])
	case queriesDir:
		return fs.getQueryEntryAttr(stat, parts[1:])
	}

	return -fuse.ENOENT
}

// --- Readdir ---

func (fs *TmsuFs) Readdir(path string,
	fill func(name string, stat *fuse.Stat_t, ofst int64) bool,
	ofst int64,
	fh uint64) int {

	defer fs.synchronize()()
	log.Infof(2, "Readdir(%v, fh=%v)", path, fh)

	// The root entries are always available locally
	if fill != nil {
		// fill with "." and ".." first
		var dotStat fuse.Stat_t
		fillDirAttr(&dotStat)
		fill(".", &dotStat, 0)
		fill("..", nil, 0)
	}

	switch path {
	case "/":
		return fs.readdirRoot(fill)
	case "/" + tagsDir:
		return fs.readdirTags(fill)
	case "/" + queriesDir:
		return fs.readdirQueries(fill)
	}

	cleanPath := path
	if cleanPath[0] == '/' {
		cleanPath = cleanPath[1:]
	}
	parts := fs.splitPath(cleanPath)

	if len(parts) >= 1 {
		switch parts[0] {
		case tagsDir:
			return fs.readdirTaggedEntry(fill, parts[1:])
		case queriesDir:
			return fs.readdirQueryEntry(fill, parts[1:])
		}
	}

	return -fuse.ENOENT
}

func (fs *TmsuFs) Opendir(path string) (int, uint64) {
	defer fs.synchronize()()
	log.Infof(2, "Opendir(%v)", path)
	return 0, 0
}

func (fs *TmsuFs) Releasedir(path string, fh uint64) int {
	defer fs.synchronize()()
	log.Infof(2, "Releasedir(%v, fh=%v)", path, fh)
	return 0
}

// --- Readlink ---

func (fs *TmsuFs) Readlink(path string) (int, string) {
	defer fs.synchronize()()
	log.Infof(2, "Readlink(%v)", path)

	if path == "/" + databaseFilename {
		return 0, fs.dbPath
	}

	cleanPath := path
	if cleanPath[0] == '/' {
		cleanPath = cleanPath[1:]
	}
	parts := fs.splitPath(cleanPath)

	tx, err := fs.store.Begin()
	if err != nil {
		log.Fatalf("could not begin transaction: %v", err)
	}
	defer tx.Commit()

	switch {
	case len(parts) >= 2 && (parts[0] == tagsDir || parts[0] == queriesDir):
		return fs.readTaggedEntryLink(tx, parts)
	}

	return -fuse.ENOENT, ""
}

// --- Open / Read / Release ---

func (fs *TmsuFs) Open(path string, flags int) (int, uint64) {
	defer fs.synchronize()()
	log.Infof(2, "Open(%v, flags=%v)", path, flags)

	// Help files can be opened for reading
	switch path {
	case "/" + queriesDir + "/" + helpFilename:
		return fs.openHelpFile([]byte(queryDirHelp))
	case "/" + tagsDir + "/" + helpFilename:
		return fs.openHelpFile([]byte(tagsDirHelp))
	}

	return -fuse.ENOSYS, ^uint64(0)
}

func (fs *TmsuFs) Create(path string, flags int, mode uint32) (int, uint64) {
	defer fs.synchronize()()
	log.Infof(2, "Create(%v, flags=%v, mode=%v)", path, flags, mode)
	return -fuse.ENOSYS, ^uint64(0)
}

func (fs *TmsuFs) Read(path string, buff []byte, ofst int64, fh uint64) int {
	defer fs.synchronize()()
	log.Infof(2, "Read(%v, ofst=%v, fh=%v)", path, ofst, fh)

	data, ok := fs.fileData[fh]
	if !ok {
		return -fuse.EBADF
	}

	if ofst >= int64(len(data)) {
		return 0
	}

	n := copy(buff, data[ofst:ofst+int64(len(buff))])
	return n
}

func (fs *TmsuFs) Write(path string, buff []byte, ofst int64, fh uint64) int {
	defer fs.synchronize()()
	log.Infof(2, "Write(%v, ofst=%v, fh=%v)", path, ofst, fh)
	return -fuse.EROFS
}

func (fs *TmsuFs) Release(path string, fh uint64) int {
	defer fs.synchronize()()
	log.Infof(2, "Release(%v, fh=%v)", path, fh)
	delete(fs.fileData, fh)
	return 0
}

func (fs *TmsuFs) Flush(path string, fh uint64) int {
	return 0
}

// --- Mkdir ---

func (fs *TmsuFs) Mkdir(path string, mode uint32) int {
	defer fs.synchronize()()
	log.Infof(2, "Mkdir(%v, mode=%v)", path, mode)

	cleanPath := path
	if cleanPath[0] == '/' {
		cleanPath = cleanPath[1:]
	}
	parts := fs.splitPath(cleanPath)

	if len(parts) != 2 {
		return -fuse.EPERM
	}

	tx, err := fs.store.Begin()
	if err != nil {
		log.Fatalf("could not begin transaction: %v", err)
	}
	defer tx.Commit()

	switch parts[0] {
	case tagsDir:
		tagName := unescape(parts[1])

		if _, err := fs.store.AddTag(tx, tagName); err != nil {
			log.Fatalf("could not create tag '%v': %v", tagName, err)
		}

		if err := tx.Commit(); err != nil {
			log.Fatalf("could not commit transaction: %v", err)
		}

		return 0
	case queriesDir:
		return -fuse.EINVAL
	}

	return -fuse.ENOSYS
}

// --- Rmdir ---

func (fs *TmsuFs) Rmdir(path string) int {
	defer fs.synchronize()()
	log.Infof(2, "Rmdir(%v)", path)

	cleanPath := path
	if cleanPath[0] == '/' {
		cleanPath = cleanPath[1:]
	}
	parts := fs.splitPath(cleanPath)

	tx, err := fs.store.Begin()
	if err != nil {
		log.Fatalf("could not begin transaction: %v", err)
	}
	defer tx.Commit()

	switch parts[0] {
	case tagsDir:
		if len(parts) != 2 {
			return -fuse.EPERM
		}

		tagName := unescape(parts[1])
		tag, err := fs.store.TagByName(tx, tagName)
		if err != nil {
			log.Fatalf("could not retrieve tag '%v': %v", tagName, err)
		}
		if tag == nil {
			return -fuse.ENOENT
		}

		count, err := fs.store.FileTagCountByTagId(tx, tag.Id, false)
		if err != nil {
			log.Fatalf("could not retrieve file-tag count for tag '%v': %v", tagName, err)
		}
		if count > 0 {
			return -fuse.ENOTEMPTY
		}

		if err := fs.store.DeleteTag(tx, tag.Id); err != nil {
			log.Fatalf("could not delete tag '%v': %v", tagName, err)
		}

		if err := tx.Commit(); err != nil {
			log.Fatalf("could not commit transaction: %v", err)
		}

		return 0
	case queriesDir:
		if len(parts) != 2 {
			return -fuse.EPERM
		}

		text := parts[1]

		if err := fs.store.DeleteQuery(tx, text); err != nil {
			log.Fatalf("could not remove query '%v': %v", text, err)
		}

		if err := tx.Commit(); err != nil {
			log.Fatalf("could not commit transaction: %v", err)
		}

		return 0
	}

	return -fuse.ENOSYS
}

// --- Unlink ---

func (fs *TmsuFs) Unlink(path string) int {
	defer fs.synchronize()()
	log.Infof(2, "Unlink(%v)", path)

	tx, err := fs.store.Begin()
	if err != nil {
		log.Fatalf("could not begin transaction: %v", err)
	}
	defer tx.Commit()

	cleanPath := path
	if cleanPath[0] == '/' {
		cleanPath = cleanPath[1:]
	}

	fileId := fs.parseFileId(filepath.Base(cleanPath))
	if fileId == 0 {
		return -fuse.EPERM
	}

	file, err := fs.store.File(tx, fileId)
	if err != nil {
		log.Fatalf("could not retrieve file '%v': %v", fileId, err)
	}
	if file == nil {
		return 0
	}
	parts := fs.splitPath(cleanPath)

	switch parts[0] {
	case tagsDir:
		dirName := parts[len(parts)-2]

		var tagName, valueName string
		if dirName[0] == '=' {
			tagName = unescape(parts[len(parts)-3])
			valueName = unescape(dirName[1:])
		} else {
			tagName = unescape(dirName)
			valueName = ""
		}

		tag, err := fs.store.TagByName(tx, tagName)
		if err != nil {
			log.Fatal(err)
		}
		if tag == nil {
			log.Fatalf("could not retrieve tag '%v'.", tagName)
		}

		value, err := fs.store.ValueByName(tx, valueName)
		if err != nil {
			log.Fatal(err)
		}
		if value == nil {
			log.Fatalf("could not retrieve value '%v'.", valueName)
		}

		if err = fs.store.DeleteFileTag(tx, fileId, tag.Id, value.Id); err != nil {
			log.Fatal(err)
		}

		if err := tx.Commit(); err != nil {
			log.Fatalf("could not commit transaction: %v", err)
		}

		return 0
	case queriesDir:
		return -fuse.EPERM
	}

	return -fuse.ENOSYS
}

// --- Rename ---

func (fs *TmsuFs) Rename(oldPath string, newPath string) int {
	defer fs.synchronize()()
	log.Infof(2, "Rename(%v, %v)", oldPath, newPath)

	tx, err := fs.store.Begin()
	if err != nil {
		log.Fatalf("could not begin transaction: %v", err)
	}
	defer tx.Commit()

	o := fs.splitPath(oldPath)
	n := fs.splitPath(newPath)

	if len(o) != 2 || len(n) != 2 {
		return -fuse.EPERM
	}

	if o[0] != tagsDir || n[0] != tagsDir {
		return -fuse.EPERM
	}

	oldTagName := unescape(o[1])
	newTagName := unescape(n[1])

	tag, err := fs.store.TagByName(tx, oldTagName)
	if err != nil {
		log.Fatalf("could not retrieve tag '%v': %v", oldTagName, err)
	}
	if tag == nil {
		return -fuse.ENOENT
	}

	if _, err := fs.store.RenameTag(tx, tag.Id, newTagName); err != nil {
		log.Fatalf("could not rename tag '%v' to '%v': %v", oldTagName, newTagName, err)
	}

	if err := tx.Commit(); err != nil {
		log.Fatalf("could not commit transaction: %v", err)
	}

	return 0
}

// --- Access ---

func (fs *TmsuFs) Access(path string, mask uint32) int {
	defer fs.synchronize()()
	log.Infof(2, "Access(%v, mask=%v)", path, mask)
	return 0
}

// ================================================================
// Private helpers
// ================================================================

func fillDirAttr(stat *fuse.Stat_t) int {
	now := fuse.Now()
	stat.Mode = fuse.S_IFDIR | 0755
	stat.Nlink = 2
	stat.Size = 0
	stat.Mtim = now
	stat.Atim = now
	stat.Ctim = now
	stat.Birthtim = now
	stat.Uid = uint32(os.Getuid())
	stat.Gid = uint32(os.Getgid())
	return 0
}

func fillRegAttr(stat *fuse.Stat_t, size uint64) int {
	now := fuse.Now()
	stat.Mode = fuse.S_IFREG | 0444
	stat.Nlink = 1
	stat.Size = int64(size)
	stat.Mtim = now
	stat.Atim = now
	stat.Ctim = now
	stat.Birthtim = now
	stat.Uid = uint32(os.Getuid())
	stat.Gid = uint32(os.Getgid())
	return 0
}

func fillLnkAttr(stat *fuse.Stat_t, size uint64) int {
	now := fuse.Now()
	stat.Mode = fuse.S_IFLNK | 0755
	stat.Nlink = 1
	stat.Size = int64(size)
	stat.Mtim = now
	stat.Atim = now
	stat.Ctim = now
	stat.Birthtim = now
	stat.Uid = uint32(os.Getuid())
	stat.Gid = uint32(os.Getgid())
	return 0
}

// --- Root readdir ---

func (fs *TmsuFs) readdirRoot(fill func(name string, stat *fuse.Stat_t, ofst int64) bool) int {
	log.Infof(2, "readdirRoot()")

	var lnkStat fuse.Stat_t
	fillLnkAttr(&lnkStat, 0)
	fill(databaseFilename, &lnkStat, 0)

	var dirStat fuse.Stat_t
	fillDirAttr(&dirStat)
	fill(tagsDir, &dirStat, 0)
	fill(queriesDir, &dirStat, 0)

	return 0
}

// --- Tags readdir ---

func (fs *TmsuFs) readdirTags(fill func(name string, stat *fuse.Stat_t, ofst int64) bool) int {
	log.Infof(2, "readdirTags()")

	tx, err := fs.store.Begin()
	if err != nil {
		log.Fatalf("Could not begin transaction: %v", err)
	}
	defer tx.Commit()

	tags, err := fs.store.Tags(tx)
	if err != nil {
		log.Fatalf("Could not retrieve tags: %v", err)
	}

	var dirStat fuse.Stat_t
	fillDirAttr(&dirStat)

	for _, tag := range tags {
		tagName := escape(tag.Name)
		if tagName == filesDir {
			continue
		}
		fill(tagName, &dirStat, 0)
	}

	if len(tags) < 3 {
		var regStat fuse.Stat_t
		fillRegAttr(&regStat, uint64(len(tagsDirHelp)))
		fill(helpFilename, &regStat, 0)
	}

	return 0
}

// --- Queries readdir ---

func (fs *TmsuFs) readdirQueries(fill func(name string, stat *fuse.Stat_t, ofst int64) bool) int {
	log.Infof(2, "readdirQueries()")

	tx, err := fs.store.Begin()
	if err != nil {
		log.Fatalf("could not begin transaction: %v", err)
	}
	defer tx.Commit()

	queries, err := fs.store.Queries(tx)
	if err != nil {
		log.Fatalf("could not retrieve queries: %v", err)
	}

	var dirStat fuse.Stat_t
	fillDirAttr(&dirStat)

	for _, query := range queries {
		fill(query.Text, &dirStat, 0)
	}

	if len(queries) < 1 {
		var regStat fuse.Stat_t
		fillRegAttr(&regStat, uint64(len(queryDirHelp)))
		fill(helpFilename, &regStat, 0)
	}

	return 0
}

// --- Tagged entry (subdirs under /tags/) ---

func (fs *TmsuFs) readdirTaggedEntry(fill func(name string, stat *fuse.Stat_t, ofst int64) bool, path []string) int {
	log.Infof(2, "readdirTaggedEntry(%v)", path)

	tx, err := fs.store.Begin()
	if err != nil {
		log.Fatalf("could not begin transaction: %v", err)
	}
	defer tx.Commit()

	if len(path) >= 1 && path[len(path)-1] == filesDir {
		return fs.readdirTaggedEntryFilesDir(fill, tx, path[:len(path)-1])
	}

	expression := pathToExpression(path)
	files, err := fs.store.FilesForQuery(tx, expression, "", false, false, "name")
	if err != nil {
		log.Fatalf("could not query files: %v", err)
	}

	var valueNames []string
	lastElement := path[len(path)-1]
	if lastElement[0] != '=' {
		expression2 := pathToExpression(path[:len(path)-1])
		files2, err := fs.store.FilesForQuery(tx, expression2, "", false, false, "name")
		if err != nil {
			log.Fatalf("could not query files: %v", err)
		}

		tagName := unescape(lastElement)
		valueNames, err = fs.tagValueNamesForFiles(tx, tagName, files2)
		if err != nil {
			log.Fatalf("could not retrieve values for '%v': %v", tagName, err)
		}
	} else {
		valueNames = []string{}
	}

	furtherTagNames, err := fs.tagNamesForFiles(tx, files)
	if err != nil {
		log.Fatalf("could not retrieve further tags: %v", err)
	}

	var dirStat fuse.Stat_t
	fillDirAttr(&dirStat)

	for _, tagName := range furtherTagNames {
		tagName = escape(tagName)
		if tagName == filesDir {
			continue
		}

		hasValues, err := fs.tagHasValues(tx, tagName)
		if err != nil {
			log.Fatalf("could not determine whether tag has values: %v", err)
		}

		if !hasValues && containsString(path, tagName) {
			continue
		}

		fill(tagName, &dirStat, 0)
	}

	for _, valueName := range valueNames {
		valueName = escape(valueName)
		fill("=" + valueName, &dirStat, 0)
	}

	fill(filesDir, &dirStat, 0)

	return 0
}

func (fs *TmsuFs) readdirTaggedEntryFilesDir(fill func(name string, stat *fuse.Stat_t, ofst int64) bool, tx *storage.Tx, path []string) int {
	log.Infof(2, "readdirTaggedEntryFilesDir(%v)", path)

	expression := pathToExpression(path)
	files, err := fs.store.FilesForQuery(tx, expression, "", false, false, "name")
	if err != nil {
		log.Fatalf("could not query files: %v", err)
	}

	var lnkStat fuse.Stat_t
	fillLnkAttr(&lnkStat, 0)

	for _, file := range files {
		linkName := fs.getLinkName(file)
		fill(linkName, &lnkStat, 0)
	}

	return 0
}

// --- Query entry (subdirs under /queries/) ---

func (fs *TmsuFs) readdirQueryEntry(fill func(name string, stat *fuse.Stat_t, ofst int64) bool, path []string) int {
	log.Infof(2, "readdirQueryEntry(%v)", path)

	if len(path) == 1 && path[0] == helpFilename {
		return 0  // Help file appears in root queries dir readdir, not in subdirs
	}

	tx, err := fs.store.Begin()
	if err != nil {
		log.Fatalf("could not begin transaction: %v", err)
	}
	defer tx.Commit()

	if len(path) == 0 {
		return 0
	}

	queryText := path[0]
	if queryText[len(queryText)-1] == ' ' {
		// Prevent multiple entries for same query when typing in GUI
		return 0
	}

	expression, err := query.Parse(queryText)
	if err != nil {
		return -fuse.ENOENT
	}

	if len(path) == 1 {
		// Just return files matching the query
		files, err := fs.store.FilesForQuery(tx, expression, "", false, false, "name")
		if err != nil {
			log.Fatalf("could not query files: %v", err)
		}

		var lnkStat fuse.Stat_t
		fillLnkAttr(&lnkStat, 0)

		for _, file := range files {
			linkName := fs.getLinkName(file)
			fill(linkName, &lnkStat, 0)
		}
	} else {
		fileId := fs.parseFileId(path[len(path)-1])
		if fileId != 0 {
			// This is a file entry, not a directory
			return 0
		}
		return -fuse.ENOENT
	}

	return 0
}

// --- Getattr helpers ---

func (fs *TmsuFs) getDatabaseFileAttr(stat *fuse.Stat_t) int {
	log.Infof(2, "getDatabaseFileAttr()")

	fileInfo, err := os.Stat(fs.dbPath)
	if err != nil {
		log.Fatalf("could not stat database: %v", err)
	}

	modTime := fileInfo.ModTime()
	now := fuse.Now()
	stat.Mode = fuse.S_IFLNK | 0755
	stat.Nlink = 1
	stat.Size = int64(fileInfo.Size())
	stat.Mtim = fuse.NewTimespec(modTime)
	stat.Atim = now
	stat.Ctim = now
	stat.Birthtim = now
	stat.Uid = uint32(os.Getuid())
	stat.Gid = uint32(os.Getgid())
	return 0
}

func (fs *TmsuFs) getTagsAttr(stat *fuse.Stat_t) int {
	log.Infof(2, "getTagsAttr()")

	tx, err := fs.store.Begin()
	if err != nil {
		log.Fatalf("could not begin transaction: %v", err)
	}
	defer tx.Commit()

	tagCount, err := fs.store.TagCount(tx)
	if err != nil {
		log.Fatalf("could not get tag count: %v", err)
	}

	now := fuse.Now()
	stat.Mode = fuse.S_IFDIR | 0755
	stat.Nlink = 2
	stat.Size = int64(tagCount)
	stat.Mtim = now
	stat.Atim = now
	stat.Ctim = now
	stat.Birthtim = now
	stat.Uid = uint32(os.Getuid())
	stat.Gid = uint32(os.Getgid())
	return 0
}

func (fs *TmsuFs) getQueryAttr(stat *fuse.Stat_t) int {
	log.Infof(2, "getQueryAttr()")

	now := fuse.Now()
	stat.Mode = fuse.S_IFDIR | 0755
	stat.Nlink = 2
	stat.Size = 0
	stat.Mtim = now
	stat.Atim = now
	stat.Ctim = now
	stat.Birthtim = now
	stat.Uid = uint32(os.Getuid())
	stat.Gid = uint32(os.Getgid())
	return 0
}

func (fs *TmsuFs) getTaggedEntryAttr(stat *fuse.Stat_t, path []string) int {
	log.Infof(2, "getTaggedEntryAttr(%v)", path)

	if len(path) == 0 {
		return fillDirAttr(stat)
	}

	name := path[len(path)-1]

	if name == helpFilename {
		return fillRegAttr(stat, uint64(len(tagsDirHelp)))
	}

	if name == filesDir {
		return fillDirAttr(stat)
	}

	fileId := fs.parseFileId(name)
	if fileId != 0 {
		return fs.getFileEntryAttr(stat, fileId)
	}

	// It's a tag directory
	tagNames := make([]string, 0, len(path))
	for _, pathElement := range path {
		if pathElement[0] != '=' {
			tagName := unescape(pathElement)
			tagNames = append(tagNames, tagName)
		}
	}

	tx, err := fs.store.Begin()
	if err != nil {
		log.Fatalf("could not begin transaction: %v", err)
	}
	defer tx.Commit()

	tagIds, err := fs.tagNamesToIds(tx, tagNames)
	if err != nil {
		log.Fatalf("could not lookup tag IDs: %v.", err)
	}
	if tagIds == nil {
		return -fuse.ENOENT
	}

	return fillDirAttr(stat)
}

func (fs *TmsuFs) getQueryEntryAttr(stat *fuse.Stat_t, path []string) int {
	log.Infof(2, "getQueryEntryAttr(%v)", path)

	if len(path) == 0 {
		return -fuse.ENOENT
	}

	name := path[len(path)-1]

	if len(path) == 1 && name == helpFilename {
		return fillRegAttr(stat, uint64(len(queryDirHelp)))
	}

	if len(path) > 1 {
		fileId := fs.parseFileId(name)
		if fileId != 0 {
			return fs.getFileEntryAttr(stat, fileId)
		}
		return -fuse.ENOENT
	}

	queryText := path[0]

	if queryText[len(queryText)-1] == ' ' {
		return -fuse.ENOENT
	}

	expression, err := query.Parse(queryText)
	if err != nil {
		return -fuse.ENOENT
	}

	tx, err := fs.store.Begin()
	if err != nil {
		log.Fatalf("could not begin transaction: %v", err)
	}
	defer tx.Commit()

	tagNames, err := query.TagNames(expression)
	if err != nil {
		log.Fatalf("could not identify tag names: %v", err)
	}

	tags, err := fs.store.TagsByNames(tx, tagNames)
	for _, tagName := range tagNames {
		if !containsTag(tags, tagName) {
			return -fuse.ENOENT
		}
	}

	q, err := fs.store.Query(tx, queryText)
	if err != nil {
		log.Fatalf("could not retrieve query '%v': %v", queryText, err)
	}
	if q == nil {
		_, err = fs.store.AddQuery(tx, queryText)
		if err != nil {
			log.Fatalf("could not add query '%v': %v", queryText, err)
		}
	}

	return fillDirAttr(stat)
}

func (fs *TmsuFs) getFileEntryAttr(stat *fuse.Stat_t, fileId entities.FileId) int {
	log.Infof(2, "getFileEntryAttr(%v)", fileId)

	tx, err := fs.store.Begin()
	if err != nil {
		log.Fatalf("could not begin transaction: %v", err)
	}
	defer tx.Commit()

	file, err := fs.store.File(tx, fileId)
	if err != nil {
		log.Fatalf("could not retrieve file #%v: %v", fileId, err)
	}
	if file == nil {
		return -fuse.ENOENT
	}

	fileInfo, err := os.Stat(file.Path())
	var size int64
	var modTime time.Time
	if err == nil {
		size = fileInfo.Size()
		modTime = fileInfo.ModTime()
	} else {
		size = 0
		modTime = time.Time{}
	}

	stat.Mode = fuse.S_IFLNK | 0755
	stat.Nlink = 1
	stat.Size = size
	stat.Mtim = fuse.NewTimespec(modTime)
	stat.Atim = fuse.Now()
	stat.Ctim = stat.Mtim
	stat.Birthtim = stat.Mtim
	stat.Uid = uint32(os.Getuid())
	stat.Gid = uint32(os.Getgid())
	return 0
}

// --- Readlink helpers ---

func (fs *TmsuFs) readTaggedEntryLink(tx *storage.Tx, path []string) (int, string) {
	log.Infof(2, "readTaggedEntryLink(%v)", path)

	name := path[len(path)-1]

	fileId := fs.parseFileId(name)
	if fileId == 0 {
		return -fuse.ENOENT, ""
	}

	file, err := fs.store.File(tx, fileId)
	if err != nil {
		log.Fatalf("could not find file %v in database.", fileId)
	}

	absDirPath := filepath.Join(fs.mountPath, filepath.Join(path[:len(path)-1]...))
	relPath, err := filepath.Rel(absDirPath, file.Path())
	if err != nil {
		log.Fatalf("could not make relative path: %v", err)
	}

	return 0, relPath
}

// --- Open helper ---

func (fs *TmsuFs) openHelpFile(data []byte) (int, uint64) {
	fh := fs.nextFh
	fs.nextFh++
	fs.fileData[fh] = data
	return 0, fh
}

// --- Utility functions ---

func (fs *TmsuFs) splitPath(path string) []string {
	return strings.Split(path, string(filepath.Separator))
}

func (fs *TmsuFs) parseFileId(name string) entities.FileId {
	parts := strings.Split(name, ".")

	var index int
	switch len(parts) {
	case 1:
		return 0
	case 2:
		index = 1
	default:
		index = len(parts) - 2
	}

	id, err := asciiToFileId(parts[index])
	if err != nil {
		return 0
	}

	return entities.FileId(id)
}

func (fs *TmsuFs) getLinkName(file *entities.File) string {
	extension := filepath.Ext(file.Path())
	fileName := filepath.Base(file.Path())
	linkName := fileName[0 : len(fileName)-len(extension)]
	suffix := "." + fileIdToAscii(file.Id) + extension

	if len(linkName)+len(suffix) > 255 {
		linkName = linkName[0 : 255-len(suffix)]
	}

	return linkName + suffix
}

func (fs *TmsuFs) tagNamesToIds(tx *storage.Tx, tagNames []string) (entities.TagIds, error) {
	tagIds := make(entities.TagIds, len(tagNames))

	for index, tagName := range tagNames {
		tag, err := fs.store.TagByName(tx, tagName)
		if err != nil {
			return nil, fmt.Errorf("could not retrieve tag '%v': %v", tagName, err)
		}
		if tag == nil {
			return nil, nil
		}

		tagIds[index] = tag.Id
	}

	return tagIds, nil
}

func (fs *TmsuFs) tagValueNamesForFiles(tx *storage.Tx, tagName string, files entities.Files) ([]string, error) {
	tag, err := fs.store.TagByName(tx, tagName)
	if err != nil {
		log.Fatalf("could not look up tag '%v': %v", tagName, err)
	}
	if tag == nil {
		return []string{}, nil
	}

	valueIds := make(entities.ValueIds, 0, 10)

	predicate := func(fileTag entities.FileTag) bool {
		return fileTag.TagId == tag.Id
	}

	for _, file := range files {
		fileTags, err := fs.store.FileTagsByFileId(tx, file.Id, false)
		if err != nil {
			return nil, fmt.Errorf("could not retrieve file-tags for file '%v': %v", file.Id, err)
		}

		for _, valueId := range fileTags.Where(predicate).ValueIds() {
			valueIds = append(valueIds, valueId)
		}
	}

	values, err := fs.store.ValuesByIds(tx, valueIds.Uniq())
	if err != nil {
		return nil, fmt.Errorf("could not retrieve values: %v", err)
	}

	valueNames := make([]string, 0, len(values))
	for _, value := range values {
		valueNames = append(valueNames, value.Name)
	}

	return valueNames, nil
}

func (fs *TmsuFs) tagNamesForFiles(tx *storage.Tx, files entities.Files) ([]string, error) {
	tagNames := make([]string, 0, 10)

	for _, file := range files {
		fileTags, err := fs.store.FileTagsByFileId(tx, file.Id, false)
		if err != nil {
			return nil, fmt.Errorf("could not retrieve file-tags for file '%v': %v", file.Id, err)
		}

		tagIds := make(entities.TagIds, len(fileTags))
		for index, fileTag := range fileTags {
			tagIds[index] = fileTag.TagId
		}

		tags, err := fs.store.TagsByIds(tx, tagIds)
		if err != nil {
			return nil, fmt.Errorf("could not retrieve tags: %v", err)
		}

		for _, tag := range tags {
			if !containsString(tagNames, tag.Name) {
				tagNames = append(tagNames, tag.Name)
			}
		}
	}

	return tagNames, nil
}

func (fs *TmsuFs) tagHasValues(tx *storage.Tx, tagName string) (bool, error) {
	tag, err := fs.store.TagByName(tx, tagName)
	if err != nil {
		return false, err
	}

	values, err := fs.store.ValuesByTag(tx, tag.Id)
	if err != nil {
		return false, err
	}

	return len(values) > 0, nil
}

func pathToExpression(path []string) query.Expression {
	var expression query.Expression = query.EmptyExpression{}

	for index, element := range path {
		var elementExpression query.Expression

		if element[0] == '=' {
			tagName := unescape(path[index-1])
			valueName := unescape(element[1:])

			elementExpression = query.ComparisonExpression{query.TagExpression{tagName}, "==", query.ValueExpression{valueName}}
		} else {
			if index+1 < len(path) && path[index+1][0] == '=' {
				continue
			}

			tagName := unescape(element)
			elementExpression = query.TagExpression{tagName}
		}

		expression = query.AndExpression{expression, elementExpression}
	}

	return expression
}

func fileIdToAscii(fileId entities.FileId) string {
	return strconv.FormatUint(uint64(fileId), 10)
}

func asciiToFileId(str string) (entities.FileId, error) {
	ui64, err := strconv.ParseUint(str, 10, 0)
	return entities.FileId(ui64), err
}

func containsTag(tags entities.Tags, tagName string) bool {
	for _, tag := range tags {
		if tag.Name == tagName {
			return true
		}
	}

	return false
}

func containsString(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}

	return false
}

func escape(name string) string {
	name = strings.Replace(name, `/`, "\u200B\u2215", -1)
	name = strings.Replace(name, `\`, "\u200B\u2216", -1)
	return name
}

func unescape(name string) string {
	name = strings.Replace(name, "\u200B\u2215", `/`, -1)
	name = strings.Replace(name, "\u200B\u2216", `\`, -1)
	return name
}