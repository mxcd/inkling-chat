package service

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/mxcd/inkling-chat/internal/model"
)

// maximumListedDirectories bounds a listing.
//
// shortcut: a folder with more subfolders shows the first 500 by name; page the listing if a
// user ever needs the rest (typing the path still works).
const maximumListedDirectories = 500

// ListDirectories is the directory at path for a user who picks the tools' directory: path must
// be an existing directory in one of the roots, the first root when empty. Hidden folders (".",
// "_") are left out, as the tools hide them. A refusal says the user what is wrong.
func (s *ChatService) ListDirectories(path string) (model.DirectoryListing, error) {
	if path == "" {
		if len(s.options.WorkspaceRoots) == 0 {
			return model.DirectoryListing{}, &model.InvalidInputError{Message: "There are no folders to choose from: WORKSPACE_ROOTS names none."}
		}
		path = s.options.WorkspaceRoots[0]
	}
	directory, err := s.directory(path)
	if err != nil {
		return model.DirectoryListing{}, err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return model.DirectoryListing{}, &model.InvalidInputError{Message: fmt.Sprintf("Cannot read the folder %s.", directory)}
	}
	listing := model.DirectoryListing{Path: directory, Roots: slices.Clone(s.options.WorkspaceRoots), Directories: []string{}}
	if !slices.Contains(s.options.WorkspaceRoots, directory) {
		listing.Parent = filepath.Dir(directory)
	}
	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") && !strings.HasPrefix(entry.Name(), "_") {
			listing.Directories = append(listing.Directories, entry.Name())
		}
		if len(listing.Directories) == maximumListedDirectories {
			break
		}
	}
	return listing, nil
}
