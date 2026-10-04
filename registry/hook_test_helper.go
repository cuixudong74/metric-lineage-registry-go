package registry

import (
	"sync/atomic"
	"testing"
)

// putHook 让 Recompute 的测试钩子能访问当前 registry 实例。
var putHook atomic.Pointer[Registry]

func rPut(t *testing.T) *Registry {
	t.Helper()
	return putHook.Load()
}
