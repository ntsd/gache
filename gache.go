package gache

import (
	"context"
	"encoding/gob"
	"io"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/kpango/fastime"
	"github.com/cornelk/hashmap"
	"github.com/zeebo/xxh3"
	"golang.org/x/sync/singleflight"
)

type (
	// Gache is base interface type
	Gache[V any] interface {
		Clear()
		Delete(string) (bool)
		DeleteExpired(context.Context) uint64
		DisableExpiredHook() Gache[V]
		EnableExpiredHook() Gache[V]
		Range(context.Context, func(string, V, int64) bool) Gache[V]
		Get(string) (V, bool)
		GetWithExpire(string) (V, int64, bool)
		Read(io.Reader) error
		Set(string, V)
		SetDefaultExpire(time.Duration) Gache[V]
		SetExpiredHook(f func(context.Context, string)) Gache[V]
		SetWithExpire(string, V, time.Duration)
		StartExpired(context.Context, time.Duration) Gache[V]
		Len() int
		ToMap(context.Context) *sync.Map
		ToRawMap(context.Context) map[string]V
		Write(context.Context, io.Writer) error
		Stop()

		// TODO Future works below
		// func ExtendExpire(string, addExp time.Duration){}
		// func (g *gache)ExtendExpire(string, addExp time.Duration){}
		// func GetRefresh(string)(V, bool){}
		// func (g *gache)GetRefresh(string)(V, bool){}
		// func GetRefreshWithDur(string, time.Duration)(V, bool){}
		// func (g *gache)GetRefreshWithDur(string, time.Duration)(V, bool){}
		// func GetWithIgnoredExpire(string)(V, bool){}
		// func (g *gache)GetWithIgnoredExpire(string)(V, bool){}
		// func Keys(context.Context)[]string{}
		// func (g *gache)Keys(context.Context)[]string{}
		// func Pop(string)(V, bool) // Get & Delete{}
		// func (g *gache)Pop(string)(V, bool) // Get & Delete{}
		// func SetIfNotExists(string, V){}
		// func (g *gache)SetIfNotExists(string, V){}
		// func SetWithExpireIfNotExists(string, V, time.Duration){}
		// func (g *gache)SetWithExpireIfNotExists(string, V, time.Duration){}
	}

	// gache is base instance type
	gache[V any] struct {
		expFuncEnabled bool
		expire         int64
		l              uint64
		cancel         atomic.Value
		expGroup       singleflight.Group
		expChan        chan string
		expFunc        func(context.Context, string)
		shards         [slen]*hashmap.Map[string, value[V]]
	}

	value[V any] struct {
		expire int64
		val    V
	}
)

const (
	// slen is shards length
	slen = 512
	// slen = 4096
	// mask is slen-1 Hex value
	mask = 0x1FF
	// mask = 0xFFF

	// NoTTL can be use for disabling ttl cache expiration
	NoTTL time.Duration = -1
)

// New returns Gache (*gache) instance
func New[V any](opts ...Option[V]) Gache[V] {
	g := new(gache[V])
	for _, opt := range append([]Option[V]{
		WithDefaultExpiration[V](time.Second * 30),
	}, opts...) {
		opt(g)
	}
	for i := range g.shards {
		g.shards[i] = newMap[V]()
	}
	g.expChan = make(chan string, len(g.shards)*10)
	return g
}

func newMap[V any]() (m *hashmap.Map[string, value[V]]) {
	m = hashmap.New[string, value[V]]()
	m.SetHasher(func(k string) uintptr {
		return uintptr(xxh3.HashString(k))
	})
	return m
}

// isValid checks expiration of value
func (v *value[V]) isValid() bool {
	return v.expire <= 0 || fastime.UnixNanoNow() <= v.expire
}

// SetDefaultExpire set expire duration
func (g *gache[V]) SetDefaultExpire(ex time.Duration) Gache[V] {
	atomic.StoreInt64(&g.expire, *(*int64)(unsafe.Pointer(&ex)))
	return g
}

// EnableExpiredHook enables expired hook function
func (g *gache[V]) EnableExpiredHook() Gache[V] {
	g.expFuncEnabled = true
	return g
}

// DisableExpiredHook disables expired hook function
func (g *gache[V]) DisableExpiredHook() Gache[V] {
	g.expFuncEnabled = false
	return g
}

// SetExpiredHook set expire hooked function
func (g *gache[V]) SetExpiredHook(f func(context.Context, string)) Gache[V] {
	g.expFunc = f
	return g
}

// StartExpired starts delete expired value daemon
func (g *gache[V]) StartExpired(ctx context.Context, dur time.Duration) Gache[V] {
	go func() {
		tick := time.NewTicker(dur)
		ctx, cancel := context.WithCancel(ctx)
		g.cancel.Store(cancel)
		for {
			select {
			case <-ctx.Done():
				tick.Stop()
				return
			case <-tick.C:
				g.DeleteExpired(ctx)
				runtime.Gosched()
			case key := <-g.expChan:
				go g.expFunc(ctx, key)
			}
		}
	}()
	return g
}

// ToMap returns All Cache Key-Value sync.Map
func (g *gache[V]) ToMap(ctx context.Context) *sync.Map {
	m := new(sync.Map)
	g.Range(ctx, func(key string, val V, exp int64) bool {
		go m.Store(key, val)
		return true
	})

	return m
}

// ToRawMap returns All Cache Key-Value map
func (g *gache[V]) ToRawMap(ctx context.Context) map[string]V {
	m := make(map[string]V, g.Len())
	mu := new(sync.Mutex)
	g.Range(ctx, func(key string, val V, exp int64) bool {
		mu.Lock()
		m[key] = val
		mu.Unlock()
		return true
	})
	return m
}

// get returns value & exists from key
func (g *gache[V]) get(key string) (V, int64, bool) {
	var val V
	v, ok := g.shards[xxh3.HashString(key)&mask].Get(key)
	if !ok {
		return val, 0, false
	}

	if v.isValid() {
		val = v.val
		return val, v.expire, true
	}

	g.expiration(key)
	return val, v.expire, false
}

// Get returns value & exists from key
func (g *gache[V]) Get(key string) (V, bool) {
	v, _, ok := g.get(key)
	return v, ok
}

// GetWithExpire returns value & expire & exists from key
func (g *gache[V]) GetWithExpire(key string) (V, int64, bool) {
	return g.get(key)
}

// set sets key-value & expiration to Gache
func (g *gache[V]) set(key string, val V, expire int64) {
	if expire > 0 {
		expire = fastime.UnixNanoNow() + expire
	}
	atomic.AddUint64(&g.l, 1)
	g.shards[xxh3.HashString(key)&mask].Set(key, value[V]{
		expire: expire,
		val:    val,
	})
}

// SetWithExpire sets key-value & expiration to Gache
func (g *gache[V]) SetWithExpire(key string, val V, expire time.Duration) {
	g.set(key, val, *(*int64)(unsafe.Pointer(&expire)))
}

// Set sets key-value to Gache using default expiration
func (g *gache[V]) Set(key string, val V) {
	g.set(key, val, atomic.LoadInt64(&g.expire))
}

// Delete deletes value from Gache using key
func (g *gache[V]) Delete(key string) (loaded bool) {
	atomic.AddUint64(&g.l, ^uint64(0))
	return g.shards[xxh3.HashString(key)&mask].Del(key)
}

func (g *gache[V]) expiration(key string) {
	g.expGroup.Do(key, func() (interface{}, error) {
		g.Delete(key)
		if g.expFuncEnabled {
			g.expChan <- key
		}
		return nil, nil
	})
}

// DeleteExpired deletes expired value from Gache it can be cancel using context
func (g *gache[V]) DeleteExpired(ctx context.Context) uint64 {
	wg := new(sync.WaitGroup)
	var rows uint64
	for i := range g.shards {
		wg.Add(1)
		go func(c context.Context, idx int) {
			g.shards[idx].Range(func(k string, v value[V]) bool {
				select {
				case <-c.Done():
					return false
				default:
					if !v.isValid() {
						g.expiration(k)
						atomic.AddUint64(&rows, 1)
						runtime.Gosched()
					}
					return true
				}
			})
			wg.Done()
		}(ctx, i)
	}
	wg.Wait()
	return atomic.LoadUint64(&rows)
}

// Range calls f sequentially for each key and value present in the Gache.
func (g *gache[V]) Range(ctx context.Context, f func(string, V, int64) bool) Gache[V] {
	wg := new(sync.WaitGroup)
	for i := range g.shards {
		wg.Add(1)
		go func(c context.Context, idx int) {
			g.shards[idx].Range(func(k string, v value[V]) bool {
				select {
				case <-c.Done():
					return false
				default:
					if v.isValid() {
						return f(k, v.val, v.expire)
					}
					runtime.Gosched()
					g.expiration(k)
					return true
				}
			})
			wg.Done()
		}(ctx, i)
	}
	wg.Wait()
	return g
}

// Len returns stored object length
func (g *gache[V]) Len() int {
	l := atomic.LoadUint64(&g.l)
	return *(*int)(unsafe.Pointer(&l))
}

// Write writes all cached data to writer
func (g *gache[V]) Write(ctx context.Context, w io.Writer) error {
	mu := new(sync.Mutex)
	m := make(map[string]V, g.Len())

	g.Range(ctx, func(key string, val V, exp int64) bool {
		gob.Register(val)
		mu.Lock()
		m[key] = val
		mu.Unlock()
		return true
	})
	gob.Register(map[string]V{})

	return gob.NewEncoder(w).Encode(&m)
}

// Read reads reader data to cache
func (g *gache[V]) Read(r io.Reader) error {
	var m map[string]V
	gob.Register(map[string]V{})
	err := gob.NewDecoder(r).Decode(&m)
	if err != nil {
		return err
	}
	for k, v := range m {
		g.Set(k, v)
	}
	return nil
}

// Stop kills expire daemon
func (g *gache[V]) Stop() {
	if c := g.cancel.Load(); c != nil {
		if cancel, ok := c.(context.CancelFunc); ok && cancel != nil {
			cancel()
		}
	}
}

// Clear deletes all key and value present in the Gache.
func (g *gache[V]) Clear() {
	for i := range g.shards {
		g.shards[i] = newMap[V]()
	}
}
