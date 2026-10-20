package databases

import (
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	"github.com/stretchr/testify/assert"
)

func TestEnginePort(t *testing.T) {
	assert.Equal(t, 5432, EnginePort("postgres"))
	assert.Equal(t, 6379, EnginePort("redis"))
	assert.Equal(t, 0, EnginePort("mysql"))
}

func TestBuildConnectionString(t *testing.T) {
	id := "db0123456789abcdef0123456789abcd"
	pg := buildConnectionString("postgres", id, "ah", "pw", "ah")
	assert.Equal(t, "postgres://ah:pw@"+docker.DatabaseAlias(id)+":5432/ah?sslmode=disable", pg)
	rd := buildConnectionString("redis", id, "", "pw", "")
	assert.Equal(t, "redis://:pw@"+docker.DatabaseAlias(id)+":6379/0", rd)
}

func TestRewriteLoopbackAddress(t *testing.T) {
	got, ok := rewriteLoopbackAddress("postgres://ah:pw@127.0.0.1:5456/ah?sslmode=disable", "127.0.0.1:5456", "ah-db-x:5432")
	assert.True(t, ok)
	assert.Equal(t, "postgres://ah:pw@ah-db-x:5432/ah?sslmode=disable", got)

	same, ok := rewriteLoopbackAddress("already-new", "127.0.0.1:5456", "ah-db-x:5432")
	assert.False(t, ok)
	assert.Equal(t, "already-new", same)
}

func TestExposeEngineAddress(t *testing.T) {
	d := &Database{ID: "db1", Type: "postgres", Host: "127.0.0.1", Port: 5456}
	exposeEngineAddress(d)
	assert.Equal(t, docker.DatabaseAlias("db1"), d.Host)
	assert.Equal(t, 5432, d.Port)
}
