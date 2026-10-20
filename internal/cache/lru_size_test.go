package cache

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// A non-positive size used to panic on the first Set (pop from an empty heap).
func TestLRU_NonPositiveSizeActsAsOne(t *testing.T) {
	for _, size := range []int{0, -3} {
		c := New[string, int](size)
		assert.NotPanics(t, func() {
			c.Set("a", 1)
			c.Set("b", 2)
		})
		assert.Equal(t, 1, c.Len())
		_, ok := c.Get("a")
		assert.False(t, ok, "oldest entry evicted")
		v, ok := c.Get("b")
		assert.True(t, ok)
		assert.Equal(t, 2, v)
	}
}
