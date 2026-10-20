package auth

import (
	"container/list"
	"crypto/sha256"
	"time"

	"github.com/therealshek/llmgate/internal/store"
)

type cacheEntry struct {
	hash      [sha256.Size]byte
	key       store.Key
	expiresAt time.Time
}

// Cache operations run under the authenticator's mutex, which also owns admission
// and revocation. Expired entries are removed on access; capacity bounds idle entries.
func (a *Authenticator) cachedKey(hash [sha256.Size]byte, now time.Time) (store.Key, bool) {
	element := a.cache[hash]
	if element == nil {
		return store.Key{}, false
	}

	entry := element.Value.(cacheEntry)
	if !now.Before(entry.expiresAt) {
		a.removeCached(element)
		return store.Key{}, false
	}

	return entry.key, true
}

func (a *Authenticator) cacheKey(hash [sha256.Size]byte, key store.Key, expiresAt, now time.Time) {
	if _, cached := a.cachedKey(hash, now); cached || !now.Before(expiresAt) {
		return
	}

	if len(a.cache) >= a.cacheCapacity {
		a.removeCached(a.cacheOrder.Front())
	}

	entry := cacheEntry{hash: hash, key: key, expiresAt: expiresAt}
	a.cache[hash] = a.cacheOrder.PushBack(entry)
}

func (a *Authenticator) removeCached(element *list.Element) {
	delete(a.cache, element.Value.(cacheEntry).hash)
	a.cacheOrder.Remove(element)
}
