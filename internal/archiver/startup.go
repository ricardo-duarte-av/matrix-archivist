package archiver

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// CheckDatabase refuses to start with a database that doesn't exist yet while the storage
// directory already holds archived repositories: that is almost always a wrong database path
// (for example after changing volume mounts), and starting from an empty list would re-add
// sources' repositories in their default mode and treat hand-added full archives as light
// ones. To really start a new list over existing storage, create an empty file at the
// database path first.
func CheckDatabase(database, storageDir string) error {
	if _, err := os.Stat(database); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	entries, err := os.ReadDir(storageDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var found []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			found = append(found, e.Name())
		}
	}
	if len(found) == 0 {
		return nil
	}
	return fmt.Errorf("database %s does not exist, but %s already holds archived repositories (%s); "+
		"check the database path in the config and your volume mounts. To really start a new, empty "+
		"repository list over this storage, create an empty file at %s first",
		database, storageDir, strings.Join(found, ", "), database)
}
