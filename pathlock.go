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

// refDestination resolves dest to its canonical lock key and returns the
// referenced entry. Every successful call must be balanced by exactly one
// releaseDestinationRef.
func refDestination(dest string) (string, *pathLockEntry, error) {
	key, err := filepath.Abs(dest)
	if err != nil {
		return "", nil, err
	}
	// Resolve directory symlinks (e.g. /tmp vs /private/tmp) so path
	// aliases of one destination share a lock. Best-effort: the download
	// needs the directory to exist anyway.
	if dir, rerr := filepath.EvalSymlinks(filepath.Dir(key)); rerr == nil {
		key = filepath.Join(dir, filepath.Base(key))
	}

	destinationLocks.mu.Lock()
	defer destinationLocks.mu.Unlock()
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
	return key, entry, nil
}

func unlockFunc(key string, entry *pathLockEntry) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			entry.token <- struct{}{}
			releaseDestinationRef(key, entry)
		})
	}
}

// acquireDestination waits for exclusive ownership of dest while respecting
// ctx. The returned function must be called exactly once.
func acquireDestination(ctx context.Context, dest string) (func(), error) {
	key, entry, err := refDestination(dest)
	if err != nil {
		return nil, err
	}
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
	return unlockFunc(key, entry), nil
}

// tryAcquireDestination takes the destination lock only if it is free right
// now. On success it returns the unlock function (call exactly once) and
// contended=false. When another holder owns the lock it returns
// contended=true and a nil unlock. Cancellation and key-resolution failures
// surface as err — distinct from contention — with the token and reference
// rolled back exactly as acquireDestination does, so a caller whose context
// died during election can never proceed to touch staging.
func tryAcquireDestination(ctx context.Context, dest string) (unlock func(), contended bool, err error) {
	key, entry, err := refDestination(dest)
	if err != nil {
		return nil, false, err
	}
	select {
	case <-entry.token:
		if err := ctx.Err(); err != nil {
			entry.token <- struct{}{}
			releaseDestinationRef(key, entry)
			return nil, false, err
		}
		return unlockFunc(key, entry), false, nil
	default:
		releaseDestinationRef(key, entry)
		return nil, true, nil
	}
}

func releaseDestinationRef(key string, entry *pathLockEntry) {
	destinationLocks.mu.Lock()
	defer destinationLocks.mu.Unlock()
	entry.refs--
	if entry.refs == 0 && destinationLocks.locks[key] == entry {
		delete(destinationLocks.locks, key)
	}
}
