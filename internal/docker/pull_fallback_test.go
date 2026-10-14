package docker

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPullOrUseLocal(t *testing.T) {
	pullErr := errors.New("registry unreachable")
	pull := func(context.Context, string) error { return pullErr }
	ok := func(context.Context, string) error { return nil }
	local := func(context.Context, string) (bool, error) { return true, nil }
	absent := func(context.Context, string) (bool, error) { return false, nil }

	assert.NoError(t, pullOrUseLocal(context.Background(), "i", ok, absent))
	assert.NoError(t, pullOrUseLocal(context.Background(), "i", pull, local), "local image tolerates pull failure")
	assert.ErrorIs(t, pullOrUseLocal(context.Background(), "i", pull, absent), pullErr)
}
