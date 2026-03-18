package cache

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLRU_BasicGetSet(t *testing.T) {
	c := New[string, int](10)

	v, ok := c.Get("missing")
	assert.False(t, ok)
	assert.Equal(t, 0, v)

	c.Set("a", 1)
	v, ok = c.Get("a")
	assert.True(t, ok)
	assert.Equal(t, 1, v)

	// Overwrite
	c.Set("a", 2)
	v, ok = c.Get("a")
	assert.True(t, ok)
	assert.Equal(t, 2, v)
	assert.Equal(t, 1, c.Len())
}

func TestLRU_Delete(t *testing.T) {
	c := New[string, string](10)
	c.Set("x", "hello")
	c.Delete("x")

	_, ok := c.Get("x")
	assert.False(t, ok)
	assert.Equal(t, 0, c.Len())

	// Delete non-existent key is a no-op
	c.Delete("nonexistent")
}

func TestLRU_EvictionAtCapacity(t *testing.T) {
	c := New[string, int](3)

	c.Set("a", 1)
	c.Set("b", 2)
	c.Set("c", 3)
	assert.Equal(t, 3, c.Len())

	// Adding 4th entry should evict oldest ("a")
	c.Set("d", 4)
	assert.Equal(t, 3, c.Len())

	_, ok := c.Get("a")
	assert.False(t, ok, "oldest entry 'a' should have been evicted")

	for _, key := range []string{"b", "c", "d"} {
		_, ok := c.Get(key)
		assert.True(t, ok, "key %s should still be present", key)
	}
}

func TestLRU_GetUpdatesAccessTime(t *testing.T) {
	c := New[string, int](3)

	c.Set("a", 1)
	c.Set("b", 2)
	c.Set("c", 3)

	// Access "a" to refresh it
	_, _ = c.Get("a")

	// Now "b" is oldest — adding "d" should evict "b", not "a"
	c.Set("d", 4)

	_, ok := c.Get("b")
	assert.False(t, ok, "'b' should be evicted as least recently used")

	_, ok = c.Get("a")
	assert.True(t, ok, "'a' should still be present after access refresh")
}

func TestLRU_SetUpdatesAccessTime(t *testing.T) {
	c := New[string, int](3)

	c.Set("a", 1)
	c.Set("b", 2)
	c.Set("c", 3)

	// Re-set "a" with new value — refreshes access time
	c.Set("a", 100)

	// "b" is now oldest
	c.Set("d", 4)

	_, ok := c.Get("b")
	assert.False(t, ok, "'b' should be evicted")

	v, ok := c.Get("a")
	assert.True(t, ok)
	assert.Equal(t, 100, v)
}

func TestLRU_MaxSizeOne(t *testing.T) {
	c := New[string, string](1)

	c.Set("a", "first")
	assert.Equal(t, 1, c.Len())

	c.Set("b", "second")
	assert.Equal(t, 1, c.Len())

	_, ok := c.Get("a")
	assert.False(t, ok)

	v, ok := c.Get("b")
	assert.True(t, ok)
	assert.Equal(t, "second", v)
}

func TestLRU_ConcurrentAccess(t *testing.T) {
	c := New[int, int](100)
	var wg sync.WaitGroup

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				c.Set(base*100+j, j)
			}
		}(i)
	}

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				c.Get(base*100 + j)
			}
		}(i)
	}

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				c.Delete(base*50 + j)
			}
		}(i)
	}

	wg.Wait()
	assert.True(t, c.Len() >= 0)
	assert.True(t, c.Len() <= 100)
}

func TestLRU_EvictionOrder(t *testing.T) {
	c := New[int, int](5)

	for i := 0; i < 5; i++ {
		c.Set(i, i)
	}
	for i := 5; i < 10; i++ {
		c.Set(i, i)
	}

	// Keys 0-4 should all be evicted
	for i := 0; i < 5; i++ {
		_, ok := c.Get(i)
		assert.False(t, ok, "key %d should be evicted", i)
	}
	// Keys 5-9 should be present
	for i := 5; i < 10; i++ {
		v, ok := c.Get(i)
		assert.True(t, ok, "key %d should be present", i)
		assert.Equal(t, i, v)
	}
}

func TestLRU_DeleteThenReinsert(t *testing.T) {
	c := New[string, int](3)

	c.Set("a", 1)
	c.Set("b", 2)
	c.Delete("a")
	assert.Equal(t, 1, c.Len())

	c.Set("a", 10)
	assert.Equal(t, 2, c.Len())

	v, ok := c.Get("a")
	require.True(t, ok)
	assert.Equal(t, 10, v)
}

func TestLRU_Range(t *testing.T) {
	c := New[string, int](10)
	c.Set("a", 1)
	c.Set("b", 2)
	c.Set("c", 3)

	got := make(map[string]int)
	c.Range(func(k string, v int) bool {
		got[k] = v
		return true
	})
	assert.Equal(t, map[string]int{"a": 1, "b": 2, "c": 3}, got)
}

func TestLRU_DeleteFunc(t *testing.T) {
	c := New[string, int](10)
	c.Set("a", 1)
	c.Set("b", 2)
	c.Set("c", 3)

	c.DeleteFunc(func(k string, v int) bool {
		return k == "a" || k == "c"
	})
	assert.Equal(t, 1, c.Len())
	_, ok := c.Get("b")
	assert.True(t, ok)
}

// --- Benchmarks ---

func BenchmarkLinearScanEviction(b *testing.B) {
	const size = 5000
	entries := make(map[int]time.Time, size)
	for i := 0; i < size; i++ {
		entries[i] = time.Now().Add(time.Duration(i) * time.Millisecond)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var oldestKey int
		var oldestTime time.Time
		first := true
		for k, v := range entries {
			if first || v.Before(oldestTime) {
				oldestKey = k
				oldestTime = v
				first = false
			}
		}
		delete(entries, oldestKey)
		entries[size+i] = time.Now()
	}
}

func BenchmarkHeapEviction(b *testing.B) {
	const size = 5000
	c := New[int, int](size)
	for i := 0; i < size; i++ {
		c.Set(i, i)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Set(size+i, i)
	}
}

func BenchmarkLRU_Get(b *testing.B) {
	c := New[string, int](5000)
	for i := 0; i < 5000; i++ {
		c.Set(fmt.Sprintf("key-%d", i), i)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Get(fmt.Sprintf("key-%d", i%5000))
	}
}
