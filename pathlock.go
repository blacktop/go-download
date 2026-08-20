package download

import (
	"context"
	"path/filepath"
	"sync"
)

// pathLockRegistry serializes staging-file ownership across all Downloader
// instances in this process. Entries are reference-counted so arbitrary
// destination names do not accumulate forever.
type pathLockRegistry struct {
	mu    sync.Mutex
	locks map[string]*pathLockEntry
}

type pathLockEntry struct {
	token chan struct{}
	refs  int
}

var destinationLocks pathLockRegistry

// acquireDestination waits for exclusive ownership of dest while respecting
// ctx. The returned function must be called exactly once.
func acquireDestination(ctx context.Context, dest string) (func(), error) {
	key, err := filepath.Abs(dest)
	if err != nil {
		return nil, err
	}

	destinationLocks.mu.Lock()
	if destinationLocks.locks == nil {
		destinationLocks.locks = make(map[string]*pathLockEntry)
	}
	entry := destinationLocks.locks[key]
	if entry == nil {
		entry = &pathLockEntry{token: make(chan struct{}, 1)}
		entry.token <- struct{}{}
		destinationLocks.locks[key] = entry
	}
	entry.refs++
	destinationLocks.mu.Unlock()

	select {
	case <-ctx.Done():
		releaseDestinationRef(key, entry)
		return nil, ctx.Err()
	case <-entry.token:
		if err := ctx.Err(); err != nil {
			entry.token <- struct{}{}
			releaseDestinationRef(key, entry)
			return nil, err
		}
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			entry.token <- struct{}{}
			releaseDestinationRef(key, entry)
		})
	}, nil
}

func releaseDestinationRef(key string, entry *pathLockEntry) {
	destinationLocks.mu.Lock()
	defer destinationLocks.mu.Unlock()
	entry.refs--
	if entry.refs == 0 && destinationLocks.locks[key] == entry {
		delete(destinationLocks.locks, key)
	}
}
