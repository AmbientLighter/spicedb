package proxy

import (
	"context"
	"errors"
	"sync"
	"testing"
	"unsafe"

	"github.com/authzed/spicedb/pkg/util"

	"github.com/authzed/spicedb/pkg/schemadsl/compiler"

	"github.com/dustin/go-humanize"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/singleflight"

	"github.com/authzed/spicedb/pkg/cache"
	"github.com/authzed/spicedb/pkg/datastore"
	core "github.com/authzed/spicedb/pkg/proto/core/v1"
)

// *DefinitionSizeVTMultiplier are the mulitipliers to be used for
// estimating the in-memory cost of a SchemaDefinition based on its
// on-wire size, as returned by SizeVT. This was determined by testing
// all existing definitions found in consistency tests and is
// enforced via the estimatedsize_test.
const (
	namespaceDefinitionSizeVTMultiplier = 10
	namespaceDefinitionMinimumSize      = 150

	caveatDefinitionSizeVTMultiplier = 10
	caveatDefinitionMinimumSize      = 150
)

// DatastoreProxyTestCache returns a cache used for testing.
func DatastoreProxyTestCache(t testing.TB) cache.Cache {
	cache, err := cache.NewCache(&cache.Config{
		NumCounters: 1000,
		MaxCost:     1 * humanize.MiByte,
	})
	require.Nil(t, err)
	return cache
}

// NewCachingDatastoreProxy creates a new datastore proxy which caches definitions that
// are loaded at specific datastore revisions.
func NewCachingDatastoreProxy(delegate datastore.Datastore, c cache.Cache) datastore.Datastore {
	if c == nil {
		c = cache.NoopCache()
	}
	return &definitionCachingProxy{
		Datastore: delegate,
		c:         c,
	}
}

type schemaDefinition interface {
	compiler.SchemaDefinition
	SizeVT() int
}

type definitionCachingProxy struct {
	datastore.Datastore
	c         cache.Cache
	readGroup singleflight.Group
}

func (p *definitionCachingProxy) Close() error {
	p.c.Close()
	return p.Datastore.Close()
}

func (p *definitionCachingProxy) SnapshotReader(rev datastore.Revision) datastore.Reader {
	delegateReader := p.Datastore.SnapshotReader(rev)
	return &definitionCachingReader{delegateReader, rev, p}
}

func (p *definitionCachingProxy) ReadWriteTx(
	ctx context.Context,
	f datastore.TxUserFunc,
) (datastore.Revision, error) {
	return p.Datastore.ReadWriteTx(ctx, func(delegateRWT datastore.ReadWriteTransaction) error {
		rwt := &definitionCachingRWT{delegateRWT, &sync.Map{}}
		return f(rwt)
	})
}

const (
	namespaceCacheKeyPrefix = "n"
	caveatCacheKeyPrefix    = "c"
)

type definitionCachingReader struct {
	datastore.Reader
	rev datastore.Revision
	p   *definitionCachingProxy
}

func (r *definitionCachingReader) ReadNamespaceByName(
	ctx context.Context,
	name string,
) (*core.NamespaceDefinition, datastore.Revision, error) {
	return readAndCache[*core.NamespaceDefinition](ctx, r, namespaceCacheKeyPrefix, name,
		func(ctx context.Context, name string) (*core.NamespaceDefinition, datastore.Revision, error) {
			return r.Reader.ReadNamespaceByName(ctx, name)
		},
		estimatedNamespaceDefinitionSize)
}

func (r *definitionCachingReader) LookupNamespacesWithNames(
	ctx context.Context,
	nsNames []string,
) ([]datastore.RevisionedNamespace, error) {
	return listAndCache[*core.NamespaceDefinition](ctx, r, namespaceCacheKeyPrefix, nsNames,
		func(ctx context.Context, names []string) ([]datastore.RevisionedNamespace, error) {
			return r.Reader.LookupNamespacesWithNames(ctx, names)
		},
		estimatedNamespaceDefinitionSize)
}

func (r *definitionCachingReader) ReadCaveatByName(
	ctx context.Context,
	name string,
) (*core.CaveatDefinition, datastore.Revision, error) {
	return readAndCache[*core.CaveatDefinition](ctx, r, caveatCacheKeyPrefix, name,
		func(ctx context.Context, name string) (*core.CaveatDefinition, datastore.Revision, error) {
			return r.Reader.ReadCaveatByName(ctx, name)
		},
		estimatedCaveatDefinitionSize)
}

func (r *definitionCachingReader) LookupCaveatsWithNames(
	ctx context.Context,
	caveatNames []string,
) ([]datastore.RevisionedCaveat, error) {
	return listAndCache[*core.CaveatDefinition](ctx, r, caveatCacheKeyPrefix, caveatNames,
		func(ctx context.Context, names []string) ([]datastore.RevisionedCaveat, error) {
			return r.Reader.LookupCaveatsWithNames(ctx, names)
		},
		estimatedCaveatDefinitionSize)
}

func listAndCache[T schemaDefinition](
	ctx context.Context,
	r *definitionCachingReader,
	prefix string,
	names []string,
	reader func(ctx context.Context, names []string) ([]datastore.RevisionedDefinition[T], error),
	estimator func(sizeVT int) int64,
) ([]datastore.RevisionedDefinition[T], error) {
	if len(names) == 0 {
		return nil, nil
	}

	// Check the cache for each entry.
	remainingToLoad := util.NewSet[string]()
	remainingToLoad.Extend(names)

	foundDefs := make([]datastore.RevisionedDefinition[T], 0, len(names))
	for _, name := range names {
		cacheRevisionKey := prefix + ":" + name + "@" + r.rev.String()
		loadedRaw, found := r.p.c.Get(cacheRevisionKey)
		if !found {
			continue
		}

		remainingToLoad.Remove(name)
		loaded := loadedRaw.(*cacheEntry)
		foundDefs = append(foundDefs, datastore.RevisionedDefinition[T]{
			Definition:          loaded.definition.(T),
			LastWrittenRevision: loaded.updated,
		})
	}

	if !remainingToLoad.IsEmpty() {
		// Load and cache the remaining names.
		loadedDefs, err := reader(ctx, remainingToLoad.AsSlice())
		if err != nil {
			return nil, err
		}

		for _, def := range loadedDefs {
			foundDefs = append(foundDefs, def)

			cacheRevisionKey := prefix + ":" + def.Definition.GetName() + "@" + r.rev.String()
			estimatedDefinitionSize := estimator(def.Definition.SizeVT())
			entry := &cacheEntry{def.Definition, def.LastWrittenRevision, estimatedDefinitionSize, err}
			r.p.c.Set(cacheRevisionKey, entry, entry.Size())
		}

		// We have to call wait here or else Ristretto may not have the key(s)
		// available to a subsequent caller.
		r.p.c.Wait()
	}

	return foundDefs, nil
}

func readAndCache[T schemaDefinition](
	ctx context.Context,
	r *definitionCachingReader,
	prefix string,
	name string,
	reader func(ctx context.Context, name string) (T, datastore.Revision, error),
	estimator func(sizeVT int) int64,
) (T, datastore.Revision, error) {
	// Check the cache.
	cacheRevisionKey := prefix + ":" + name + "@" + r.rev.String()
	loadedRaw, found := r.p.c.Get(cacheRevisionKey)
	if !found {
		// We couldn't use the cached entry, load one
		var err error
		loadedRaw, err, _ = r.p.readGroup.Do(cacheRevisionKey, func() (any, error) {
			// sever the context so that another branch doesn't cancel the
			// single-flighted read
			loaded, updatedRev, err := reader(SeparateContextWithTracing(ctx), name)
			if err != nil && !errors.Is(err, &datastore.ErrNamespaceNotFound{}) && !errors.Is(err, &datastore.ErrCaveatNameNotFound{}) {
				// Propagate this error to the caller
				return nil, err
			}

			estimatedDefinitionSize := estimator(loaded.SizeVT())
			entry := &cacheEntry{loaded, updatedRev, estimatedDefinitionSize, err}
			r.p.c.Set(cacheRevisionKey, entry, entry.Size())

			// We have to call wait here or else Ristretto may not have the key
			// available to a subsequent caller.
			r.p.c.Wait()

			return entry, nil
		})
		if err != nil {
			return *new(T), datastore.NoRevision, err
		}
	}

	loaded := loadedRaw.(*cacheEntry)
	return loaded.definition.(T), loaded.updated, loaded.notFound
}

type definitionCachingRWT struct {
	datastore.ReadWriteTransaction
	definitionCache *sync.Map
}

type rwtCacheEntry struct {
	loaded   schemaDefinition
	updated  datastore.Revision
	notFound error
}

func (rwt *definitionCachingRWT) ReadNamespaceByName(
	ctx context.Context,
	nsName string,
) (*core.NamespaceDefinition, datastore.Revision, error) {
	return readAndCacheInTransaction[*core.NamespaceDefinition](
		ctx, rwt, "namespace", nsName, func(ctx context.Context, name string) (*core.NamespaceDefinition, datastore.Revision, error) {
			return rwt.ReadWriteTransaction.ReadNamespaceByName(ctx, name)
		})
}

func (rwt *definitionCachingRWT) ReadCaveatByName(
	ctx context.Context,
	nsName string,
) (*core.CaveatDefinition, datastore.Revision, error) {
	return readAndCacheInTransaction[*core.CaveatDefinition](
		ctx, rwt, "caveat", nsName, func(ctx context.Context, name string) (*core.CaveatDefinition, datastore.Revision, error) {
			return rwt.ReadWriteTransaction.ReadCaveatByName(ctx, name)
		})
}

func readAndCacheInTransaction[T schemaDefinition](
	ctx context.Context,
	rwt *definitionCachingRWT,
	prefix string,
	name string,
	reader func(ctx context.Context, name string) (T, datastore.Revision, error),
) (T, datastore.Revision, error) {
	key := prefix + ":" + name
	untypedEntry, ok := rwt.definitionCache.Load(key)

	var entry rwtCacheEntry
	if ok {
		entry = untypedEntry.(rwtCacheEntry)
	} else {
		loaded, updatedRev, err := reader(ctx, name)
		if err != nil && !errors.As(err, &datastore.ErrNamespaceNotFound{}) && !errors.As(err, &datastore.ErrCaveatNameNotFound{}) {
			// Propagate this error to the caller
			return *new(T), datastore.NoRevision, err
		}

		entry = rwtCacheEntry{loaded, updatedRev, err}
		rwt.definitionCache.Store(key, entry)
	}

	return entry.loaded.(T), entry.updated, entry.notFound
}

func (rwt *definitionCachingRWT) WriteNamespaces(ctx context.Context, newConfigs ...*core.NamespaceDefinition) error {
	if err := rwt.ReadWriteTransaction.WriteNamespaces(ctx, newConfigs...); err != nil {
		return err
	}

	for _, nsDef := range newConfigs {
		rwt.definitionCache.Delete("namespace:" + nsDef.Name)
	}

	return nil
}

func (rwt *definitionCachingRWT) WriteCaveats(ctx context.Context, newConfigs []*core.CaveatDefinition) error {
	if err := rwt.ReadWriteTransaction.WriteCaveats(ctx, newConfigs); err != nil {
		return err
	}

	for _, caveatDef := range newConfigs {
		rwt.definitionCache.Delete("caveat:" + caveatDef.Name)
	}

	return nil
}

type cacheEntry struct {
	definition              schemaDefinition
	updated                 datastore.Revision
	estimatedDefinitionSize int64
	notFound                error
}

func (c *cacheEntry) Size() int64 {
	return c.estimatedDefinitionSize + int64(unsafe.Sizeof(c))
}

var (
	_ datastore.Datastore = &definitionCachingProxy{}
	_ datastore.Reader    = &definitionCachingReader{}
)

func estimatedNamespaceDefinitionSize(sizevt int) int64 {
	size := int64(sizevt * namespaceDefinitionSizeVTMultiplier)
	if size < namespaceDefinitionMinimumSize {
		return namespaceDefinitionMinimumSize
	}
	return size
}

func estimatedCaveatDefinitionSize(sizevt int) int64 {
	size := int64(sizevt * caveatDefinitionSizeVTMultiplier)
	if size < caveatDefinitionMinimumSize {
		return caveatDefinitionMinimumSize
	}
	return size
}
