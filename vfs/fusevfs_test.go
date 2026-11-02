package vfs

import (
	"strings"
	"testing"
	"time"
	"tmsu/common/fingerprint"
	"tmsu/entities"
)

// escape / unescape round-trip — catches breakage in unicode escaping of / and \

func TestEscapeUnescapeRoundTrip(test *testing.T) {
	names := []string{`simple`, `a/b`, `a\b`, `complex/with\both`, `///`, `\\\`}
	for _, name := range names {
		escaped := escape(name)
		unescaped := unescape(escaped)
		if unescaped != name {
			test.Fatalf("Round-trip failed for '%v': '%v' -> '%v'",
				name, escaped, unescaped)
		}
	}
}

// getLinkName — verifies symlink naming in the VFS

func makeFile(id uint64, dir string, name string) *entities.File {
	return &entities.File{
		Id:          entities.FileId(id),
		Directory:   dir,
		Name:        name,
		Fingerprint: fingerprint.Fingerprint(""),
		ModTime:     time.Time{},
		Size:        0,
		IsDir:       false,
	}
}

func TestGetLinkNameBasic(test *testing.T) {
	fs := NewTmsuFs(nil, "", "/mnt")
	file := makeFile(42, "/photos", "vacation.jpg")
	name := fs.getLinkName(file)
	if name != "vacation.42.jpg" {
		test.Fatalf("Expected 'vacation.42.jpg' but was '%v'", name)
	}
}

func TestGetLinkNameDoubleExtension(test *testing.T) {
	// Only the LAST extension is stripped: .tar.gz → base is "photos.tar"
	fs := NewTmsuFs(nil, "", "/mnt")
	file := makeFile(7, "/archives", "photos.tar.gz")
	name := fs.getLinkName(file)
	if name != "photos.tar.7.gz" {
		test.Fatalf("Expected 'photos.tar.7.gz' but was '%v'", name)
	}
}

func TestGetLinkNameHiddenFile(test *testing.T) {
	// filepath.Ext(".gitignore") treats the whole name as extension
	fs := NewTmsuFs(nil, "", "/mnt")
	file := makeFile(9, "/config", ".gitignore")
	name := fs.getLinkName(file)
	if name != ".9.gitignore" {
		test.Fatalf("Expected '.9.gitignore' but was '%v'", name)
	}
}

func TestGetLinkNameTruncation(test *testing.T) {
	// Total link name must not exceed 255 chars (ext is preserved)
	fs := NewTmsuFs(nil, "", "/mnt")
	longName := strings.Repeat("a", 250) + ".txt"
	file := makeFile(1, "/", longName)

	name := fs.getLinkName(file)
	if len(name) > 255 {
		test.Fatalf("Link name exceeds 255 chars: %v", len(name))
	}
	if name[len(name)-6:] != ".1.txt" {
		test.Fatalf("Expected suffix '.1.txt' but got '%v'", name[len(name)-6:])
	}
}