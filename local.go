package cache

import (
	"sync"
	"sync/atomic"
	"time"
)

const (
	// Default maximum number of cache entries.
	maximumCapacity = 1 << 30
	// Buffer size of entry channels
	chanBufSize = 16
	// Maximum number of entries to be drained in a single clean up.
	drainMax = 16
	// Number of cache access operations that will trigger clean up.
	drainThreshold = 64
)

// currentTime is an alias for time.Now, used for testing.
var currentTime = time.Now

// localCache is an asynchronous LRU cache.
type localCache struct {
	// user configurations
	policyName        string
	expireAfterAccess time.Duration
	expireAfterWrite  time.Duration
	refreshAfterWrite time.Duration

	onInsertion Func
	onRemoval   Func

	loader LoaderFunc
	exec   ExecutorFunc
	stats  StatsCounter

	// internal data structure
	cache cache
	cap   int

	entries policy
	// Channels for processEntries
	addEntry    chan *entry
	hitEntry    chan *entry
	deleteEntry chan *entry

	// readCount is a counter of the number of reads since the last write.
	readCount int32

	// for closing routines created by this cache.
	closeOnce sync.Once
	closeCh   chan struct{}
}

// newLocalCache returns a default localCache.
// init must be called before this cache can be used.
func newLocalCache() *localCache {
	return &localCache{
		cap:   maximumCapacity,
		cache: cache{},
		stats: &statsCounter{},
	}
}

// init initializes cache replacement policy after all user configuration properties are set.
func (c *localCache) init() {
	c.entries = newPolicy(c.policyName)
	c.entries.init(&c.cache, c.cap)

	c.addEntry = make(chan *entry, chanBufSize)
	c.hitEntry = make(chan *entry, chanBufSize)
	c.deleteEntry = make(chan *entry, chanBufSize)

	c.closeCh = make(chan struct{})
	go c.processEntries()
}

// Close implements io.Closer and always returns a nil error.
// Caller would ensure the cache is not being used (reading and writing) before closing.
func (c *localCache) Close() error {
	c.closeOnce.Do(c.close)
	return nil
}

// GetIfPresent gets cached value from entries list and updates
// last access time for the entry if it is found.
func (c *localCache) GetIfPresent(k Key) (Value, bool) {
	en := c.cache.get(k, sum(k))
	if en == nil {
		c.stats.RecordMisses(1)
		return nil, false
	}
	now := currentTime()
	if c.isExpired(en, now) {
		c.stats.RecordMisses(1)
		c.deleteEntry <- en
		return nil, false
	}
	c.stats.RecordHits(1)
	en.setAccessTime(now.UnixNano())
	c.hitEntry <- en
	return en.getValue(), true
}

// Put adds new entry to entries list.
func (c *localCache) Put(k Key, v Value) {
	h := sum(k)
	en := c.cache.get(k, h)
	now := currentTime()
	if en == nil {
		en = newEntry(k, v, h)
		en.setWriteTime(now.UnixNano())
		en.setAccessTime(now.UnixNano())
		// Add to the cache directly so the new value is available immediately.
		// However, only do this within the cache capacity (approximately).
		if c.cap == 0 || c.cache.len() < c.cap {
			cen := c.cache.getOrSet(en)
			if cen != nil {
				cen.setValue(v)
				en = cen
			}
		}
	} else {
		// Update value and send notice
		en.setValue(v)
		en.setWriteTime(now.UnixNano())
	}
	c.addEntry <- en
}

// Invalidate removes the entry associated with key k.
func (c *localCache) Invalidate(k Key) {
	en := c.cache.get(k, sum(k))
	if en != nil {
		en.setInvalidated(true)
		c.deleteEntry <- en
	}
}

// InvalidateAll resets entries list.
func (c *localCache) InvalidateAll() {
	c.cache.walk(func(en *entry) {
		en.setInvalidated(true)
	})
	c.deleteEntry <- nil
}

// Get returns value associated with k or call underlying loader to retrieve value
// if it is not in the cache. The returned value is only cached when loader returns
// nil error.
func (c *localCache) Get(k Key) (Value, error) {
	en := c.cache.get(k, sum(k))
	if en == nil {
		c.stats.RecordMisses(1)
		return c.load(k)
	}
	// Check if this entry needs to be refreshed
	now := currentTime()
	if c.isExpired(en, now) {
		c.stats.RecordMisses(1)
		if c.loader == nil {
			c.deleteEntry <- en
		} else {
			// For loading cache, we do not delete entry but leave it to
			// the eviction policy, so users still can get the old value.
			en.setAccessTime(now.UnixNano())
			c.refreshAsync(en)
		}
	} else {
		c.stats.RecordHits(1)
		en.setAccessTime(now.UnixNano())
		c.hitEntry <- en
	}
	return en.getValue(), nil
}

// Refresh asynchronously reloads value for Key if it existed, otherwise
// it will synchronously load and block until it value is loaded.
func (c *localCache) Refresh(k Key) {
	if c.loader == nil {
		return
	}
	en := c.cache.get(k, sum(k))
	if en == nil {
		c.load(k)
	} else {
		c.refreshAsync(en)
	}
}

// Stats copies cache stats to t.
func (c *localCache) Stats(t *Stats) {
	c.stats.Snapshot(t)
}

func (c *localCache) processEntries() {
	defer close(c.closeCh)
	for {
		select {
		case <-c.closeCh:
			c.removeAll()
			return
		case en := <-c.addEntry:
			c.add(en)
			c.postWriteCleanup()
		case en := <-c.hitEntry:
			c.hit(en)
			c.postReadCleanup()
		case en := <-c.deleteEntry:
			if en == nil {
				c.removeAll()
			} else {
				c.remove(en)
			}
			c.postReadCleanup()
		}
	}
}

// This function must only be called from processEntries goroutine.
func (c *localCache) add(en *entry) {
	remEn := c.entries.add(en)
	if c.onInsertion != nil {
		c.onInsertion(en.key, en.getValue())
	}
	if remEn != nil {
		// An entry has been evicted
		c.stats.RecordEviction()
		if c.onRemoval != nil {
			c.onRemoval(remEn.key, remEn.getValue())
		}
	}
}

// removeAll remove all entries in the cache.
// This function must only be called from processEntries goroutine.
func (c *localCache) removeAll() {
	if c.onRemoval == nil {
		c.cache.walk(func(en *entry) {
			c.cache.delete(en)
		})
	} else {
		c.cache.walk(func(en *entry) {
			c.cache.delete(en)
			c.onRemoval(en.key, en.getValue())
		})
	}
}

// remove removes the given element from the cache and entries list.
// It also calls onRemoval callback if it is set.
func (c *localCache) remove(en *entry) {
	en = c.entries.remove(en)
	if en != nil && c.onRemoval != nil {
		c.onRemoval(en.key, en.getValue())
	}
}

// hit moves the given element to the top of the entries list.
// This function must only be called from processEntries goroutine.
func (c *localCache) hit(en *entry) {
	c.entries.hit(en)
}

// load uses current loader to synchronously retrieve value for k and adds new
// entry to the cache only if loader returns a nil error.
func (c *localCache) load(k Key) (Value, error) {
	if c.loader == nil {
		panic("cache loader function must be set")
	}
	// TODO: Poll the value instead when the entry is loading.
	start := currentTime()
	v, err := c.loader(k)
	now := currentTime()
	loadTime := now.Sub(start)
	if err != nil {
		c.stats.RecordLoadError(loadTime)
		return nil, err
	}
	c.stats.RecordLoadSuccess(loadTime)
	en := newEntry(k, v, sum(k))
	en.setWriteTime(now.UnixNano())
	en.setAccessTime(now.UnixNano())
	c.addEntry <- en
	return v, nil
}

func (c *localCache) refreshAsync(en *entry) {
	if c.loader == nil {
		panic("cache loader function must be set")
	}
	if en.setLoading(true) {
		// Only do refresh if it isn't running.
		if c.exec == nil {
			go c.refresh(en)
		} else {
			c.exec(func() { c.refresh(en) })
		}
	}
}

// refresh reloads value for the given key. If loader returns an error,
// that error will be omitted and current value will be returned.
// Otherwise, the function will returns new value and updates the current
// cache entry.
func (c *localCache) refresh(en *entry) {
	defer en.setLoading(false)

	start := currentTime()
	v, err := c.loader(en.key)
	now := currentTime()
	loadTime := now.Sub(start)
	if err == nil {
		c.stats.RecordLoadSuccess(loadTime)
		en.setValue(v)
		en.setWriteTime(now.UnixNano())
		c.addEntry <- en
	} else {
		// TODO: Log error
		c.stats.RecordLoadError(loadTime)
	}
}

// postReadCleanup is run after entry access/delete event.
// This function must only be called from processEntries goroutine.
func (c *localCache) postReadCleanup() {
	if atomic.AddInt32(&c.readCount, 1) > drainThreshold {
		atomic.StoreInt32(&c.readCount, 0)
		c.expireEntries()
	}
}

// postWriteCleanup is run after entry add event.
// This function must only be called from processEntries goroutine.
func (c *localCache) postWriteCleanup() {
	atomic.StoreInt32(&c.readCount, 0)
	c.expireEntries()
}

// expireEntries removes expired entries.
func (c *localCache) expireEntries() {
	if c.expireAfterAccess <= 0 {
		return
	}
	expire := currentTime().Add(-c.expireAfterAccess).UnixNano()
	remain := drainMax
	c.entries.walkAccess(func(en *entry) bool {
		if en.getAccessTime() >= expire {
			// Can stop as the entries are sorted by access time.
			return false
		}
		// accessTime + expiry passed
		c.remove(en)
		c.stats.RecordEviction()
		remain--
		return remain > 0
	})
}

func (c *localCache) isExpired(en *entry, now time.Time) bool {
	if en.getInvalidated() {
		return true
	}
	if c.expireAfterAccess > 0 && en.getAccessTime() < now.Add(-c.expireAfterAccess).UnixNano() {
		// accessTime + expiry passed
		return true
	}
	if c.expireAfterWrite > 0 && en.getWriteTime() < now.Add(-c.expireAfterWrite).UnixNano() {
		// writeTime + expiry passed
		return true
	}
	return false
}

func (c *localCache) needRefresh(en *entry, now time.Time) bool {
	if en.getLoading() {
		return false
	}
	if c.refreshAfterWrite > 0 {
		tm := en.getWriteTime()
		if tm > 0 && tm < now.Add(-c.refreshAfterWrite).UnixNano() {
			// writeTime + refresh passed
			return true
		}
	}
	return false
}

// close asks processEntries to stop.
func (c *localCache) close() {
	c.closeCh <- struct{}{}
	// Wait for the goroutine to close this channel
	// (should use sync.WaitGroup or a new channel instead?)
	<-c.closeCh
}

// New returns a local in-memory Cache.
func New(options ...Option) Cache {
	c := newLocalCache()
	for _, opt := range options {
		opt(c)
	}
	c.init()
	return c
}

// NewLoadingCache returns a new LoadingCache with given loader function
// and cache options.
func NewLoadingCache(loader LoaderFunc, options ...Option) LoadingCache {
	c := newLocalCache()
	c.loader = loader
	for _, opt := range options {
		opt(c)
	}
	c.init()
	return c
}

// Option add options for default Cache.
type Option func(c *localCache)

// WithMaximumSize returns an Option which sets maximum size for the cache.
// Any non-positive numbers is considered as unlimited.
func WithMaximumSize(size int) Option {
	if size < 0 {
		size = 0
	}
	if size > maximumCapacity {
		size = maximumCapacity
	}
	return func(c *localCache) {
		c.cap = size
	}
}

// WithRemovalListener returns an Option to set cache to call onRemoval for each
// entry evicted from the cache.
func WithRemovalListener(onRemoval Func) Option {
	return func(c *localCache) {
		c.onRemoval = onRemoval
	}
}

// WithExpireAfterAccess returns an option to expire a cache entry after the
// given duration without being accessed.
func WithExpireAfterAccess(d time.Duration) Option {
	return func(c *localCache) {
		c.expireAfterAccess = d
	}
}

// WithExpireAfterWrite returns an option to expire a cache entry after the
// given duration from creation.
func WithExpireAfterWrite(d time.Duration) Option {
	return func(c *localCache) {
		c.expireAfterWrite = d
	}
}

// WithRefreshAfterWrite returns an option to refresh a cache entry after the
// given duration. This option is only applicable for LoadingCache.
func WithRefreshAfterWrite(d time.Duration) Option {
	return func(c *localCache) {
		c.refreshAfterWrite = d
	}
}

// WithStatsCounter returns an option which overrides default cache stats counter.
func WithStatsCounter(st StatsCounter) Option {
	return func(c *localCache) {
		c.stats = st
	}
}

// WithPolicy returns an option which sets cache policy associated to the given name.
// Supported policies are: lru, slru, tinylfu.
func WithPolicy(name string) Option {
	return func(c *localCache) {
		c.policyName = name
	}
}

// WithExecutor returns an option which sets executor for cache loader.
// By default, each asynchronous reload is run in a go routine.
// This option is only applicable for LoadingCache.
func WithExecutor(executor ExecutorFunc) Option {
	return func(c *localCache) {
		c.exec = executor
	}
}

// withInsertionListener is used for testing.
func withInsertionListener(onInsertion Func) Option {
	return func(c *localCache) {
		c.onInsertion = onInsertion
	}
}
