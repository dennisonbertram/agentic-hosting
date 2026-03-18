// Package cache provides a generic, thread-safe LRU cache with O(log n) eviction.
package cache

import (
	"container/heap"
	"sync"
)

// LRU is a generic least-recently-used cache with O(log n) eviction via a min-heap.
type LRU[K comparable, V any] struct {
	mu      sync.Mutex
	items   map[K]*lruEntry[K, V]
	h       lruHeap[K, V]
	maxSize int
	counter uint64 // monotonic counter for access ordering
}

type lruEntry[K comparable, V any] struct {
	key       K
	value     V
	accessOrd uint64 // monotonic ordering — lower = older
	index     int    // heap index
}

// New creates an LRU cache with the given maximum size.
func New[K comparable, V any](maxSize int) *LRU[K, V] {
	return &LRU[K, V]{
		items:   make(map[K]*lruEntry[K, V]),
		h:       make(lruHeap[K, V], 0),
		maxSize: maxSize,
	}
}

// Get returns the value for the key and true, or the zero value and false.
// Accessing a key refreshes its position (most recently used).
func (c *LRU[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.items[key]
	if !ok {
		var zero V
		return zero, false
	}
	c.counter++
	e.accessOrd = c.counter
	heap.Fix(&c.h, e.index)
	return e.value, true
}

// Set adds or updates a key-value pair. If at capacity, the least recently
// used entry is evicted.
func (c *LRU[K, V]) Set(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.counter++

	if e, ok := c.items[key]; ok {
		e.value = value
		e.accessOrd = c.counter
		heap.Fix(&c.h, e.index)
		return
	}

	if len(c.items) >= c.maxSize {
		oldest := heap.Pop(&c.h).(*lruEntry[K, V])
		delete(c.items, oldest.key)
	}

	e := &lruEntry[K, V]{
		key:       key,
		value:     value,
		accessOrd: c.counter,
	}
	heap.Push(&c.h, e)
	c.items[key] = e
}

// Delete removes a key from the cache.
func (c *LRU[K, V]) Delete(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.items[key]
	if !ok {
		return
	}
	heap.Remove(&c.h, e.index)
	delete(c.items, key)
}

// Len returns the number of entries in the cache.
func (c *LRU[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

// Range calls fn for each entry in the cache. If fn returns false, iteration stops.
// The callback receives the key and value; order is not guaranteed.
func (c *LRU[K, V]) Range(fn func(K, V) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.items {
		if !fn(k, e.value) {
			break
		}
	}
}

// DeleteFunc removes all entries for which fn returns true.
func (c *LRU[K, V]) DeleteFunc(fn func(K, V) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.items {
		if fn(k, e.value) {
			heap.Remove(&c.h, e.index)
			delete(c.items, k)
		}
	}
}

// --- min-heap implementation ---

type lruHeap[K comparable, V any] []*lruEntry[K, V]

func (h lruHeap[K, V]) Len() int           { return len(h) }
func (h lruHeap[K, V]) Less(i, j int) bool { return h[i].accessOrd < h[j].accessOrd }
func (h lruHeap[K, V]) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}

func (h *lruHeap[K, V]) Push(x any) {
	e := x.(*lruEntry[K, V])
	e.index = len(*h)
	*h = append(*h, e)
}

func (h *lruHeap[K, V]) Pop() any {
	old := *h
	n := len(old)
	e := old[n-1]
	old[n-1] = nil // avoid memory leak
	e.index = -1
	*h = old[:n-1]
	return e
}

