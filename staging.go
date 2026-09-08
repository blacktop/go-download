package download

import (
	"fmt"
	"os"
)

// openStaging never truncates on open and refuses links and special files
// before callers can write. openStagingFile supplies the platform's no-follow
// open; checking the pathname also rejects a replacement during the open.
func openStaging(path string, flag int) (*os.File, error) {
	f, err := openStagingFile(path, flag)
	if err != nil {
		return nil, err
	}
	opened, err := f.Stat()
	if err == nil {
		var current os.FileInfo
		current, err = os.Lstat(path)
		if err == nil && (!opened.Mode().IsRegular() || !current.Mode().IsRegular() || !os.SameFile(opened, current)) {
			err = fmt.Errorf("staging path is not the opened regular file: %s", path)
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
