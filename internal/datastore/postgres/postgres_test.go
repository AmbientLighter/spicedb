//go:build ci && docker
// +build ci,docker

package postgres

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	sq "github.com/Masterminds/squirrel"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"

	"github.com/authzed/spicedb/internal/datastore/common"
	pgcommon "github.com/authzed/spicedb/internal/datastore/postgres/common"
	"github.com/authzed/spicedb/internal/testfixtures"
	testdatastore "github.com/authzed/spicedb/internal/testserver/datastore"
	"github.com/authzed/spicedb/pkg/datastore"
	"github.com/authzed/spicedb/pkg/datastore/test"
	"github.com/authzed/spicedb/pkg/migrate"
	"github.com/authzed/spicedb/pkg/namespace"
	core "github.com/authzed/spicedb/pkg/proto/core/v1"
	"github.com/authzed/spicedb/pkg/tuple"
)

func TestPostgresDatastore(t *testing.T) {
	for _, config := range []struct {
		targetMigration string
		migrationPhase  string
	}{
		{"head", ""},
	} {
		config := config
		t.Run(fmt.Sprintf("%s-%s", config.targetMigration, config.migrationPhase), func(t *testing.T) {
			t.Parallel()
			b := testdatastore.RunPostgresForTesting(t, "", config.targetMigration)

			test.All(t, test.DatastoreTesterFunc(func(revisionQuantization, gcInterval, gcWindow time.Duration, watchBufferLength uint16) (datastore.Datastore, error) {
				ds := b.NewDatastore(t, func(engine, uri string) datastore.Datastore {
					ds, err := newPostgresDatastore(uri,
						RevisionQuantization(revisionQuantization),
						GCWindow(gcWindow),
						GCInterval(gcInterval),
						WatchBufferLength(watchBufferLength),
						DebugAnalyzeBeforeStatistics(),
						MigrationPhase(config.migrationPhase),
					)
					require.NoError(t, err)
					return ds
				})
				return ds, nil
			}))

			t.Run("WithSplit", func(t *testing.T) {
				// Set the split at a VERY small size, to ensure any WithUsersets queries are split.
				test.All(t, test.DatastoreTesterFunc(func(revisionQuantization, gcInterval, gcWindow time.Duration, watchBufferLength uint16) (datastore.Datastore, error) {
					ds := b.NewDatastore(t, func(engine, uri string) datastore.Datastore {
						ds, err := newPostgresDatastore(uri,
							RevisionQuantization(revisionQuantization),
							GCWindow(gcWindow),
							GCInterval(gcInterval),
							WatchBufferLength(watchBufferLength),
							DebugAnalyzeBeforeStatistics(),
							SplitAtUsersetCount(1), // 1 userset
							MigrationPhase(config.migrationPhase),
						)
						require.NoError(t, err)
						return ds
					})

					return ds, nil
				}))
			})

			t.Run("GarbageCollection", createDatastoreTest(
				b,
				GarbageCollectionTest,
				RevisionQuantization(0),
				GCWindow(1*time.Millisecond),
				WatchBufferLength(1),
				MigrationPhase(config.migrationPhase),
			))

			t.Run("TransactionTimestamps", createDatastoreTest(
				b,
				TransactionTimestampsTest,
				RevisionQuantization(0),
				GCWindow(1*time.Millisecond),
				WatchBufferLength(1),
				MigrationPhase(config.migrationPhase),
			))

			t.Run("GarbageCollectionByTime", createDatastoreTest(
				b,
				GarbageCollectionByTimeTest,
				RevisionQuantization(0),
				GCWindow(1*time.Millisecond),
				WatchBufferLength(1),
				MigrationPhase(config.migrationPhase),
			))

			t.Run("ChunkedGarbageCollection", createDatastoreTest(
				b,
				ChunkedGarbageCollectionTest,
				RevisionQuantization(0),
				GCWindow(1*time.Millisecond),
				WatchBufferLength(1),
				MigrationPhase(config.migrationPhase),
			))

			t.Run("QuantizedRevisions", func(t *testing.T) {
				QuantizedRevisionTest(t, b)
			})

			t.Run("WatchNotEnabled", func(t *testing.T) {
				WatchNotEnabledTest(t, b)
			})

			t.Run("GCQueriesServedByExpectedIndexes", func(t *testing.T) {
				GCQueriesServedByExpectedIndexes(t, b)
			})

			if config.migrationPhase == "" {
				t.Run("RevisionInversion", createDatastoreTest(
					b,
					RevisionInversionTest,
					RevisionQuantization(0),
					GCWindow(1*time.Millisecond),
					WatchBufferLength(1),
					MigrationPhase(config.migrationPhase),
				))

				t.Run("ConcurrentRevisionHead", createDatastoreTest(
					b,
					ConcurrentRevisionHeadTest,
					RevisionQuantization(0),
					GCWindow(1*time.Millisecond),
					WatchBufferLength(1),
					MigrationPhase(config.migrationPhase),
				))
			}
		})
	}
}

func TestPostgresDatastoreWithoutCommitTimestamps(t *testing.T) {
	b := testdatastore.RunPostgresForTestingWithCommitTimestamps(t, "", "head", false)

	// NOTE: watch API requires the commit timestamps, so we skip those tests here.
	test.AllExceptWatch(t, test.DatastoreTesterFunc(func(revisionQuantization, gcInterval, gcWindow time.Duration, watchBufferLength uint16) (datastore.Datastore, error) {
		ds := b.NewDatastore(t, func(engine, uri string) datastore.Datastore {
			ds, err := newPostgresDatastore(uri,
				RevisionQuantization(revisionQuantization),
				GCWindow(gcWindow),
				GCInterval(gcInterval),
				WatchBufferLength(watchBufferLength),
				DebugAnalyzeBeforeStatistics(),
			)
			require.NoError(t, err)
			return ds
		})
		return ds, nil
	}))
}

type datastoreTestFunc func(t *testing.T, ds datastore.Datastore)

func createDatastoreTest(b testdatastore.RunningEngineForTest, tf datastoreTestFunc, options ...Option) func(*testing.T) {
	return func(t *testing.T) {
		ds := b.NewDatastore(t, func(engine, uri string) datastore.Datastore {
			ds, err := newPostgresDatastore(uri, options...)
			require.NoError(t, err)
			return ds
		})
		defer ds.Close()

		tf(t, ds)
	}
}

func GarbageCollectionTest(t *testing.T, ds datastore.Datastore) {
	require := require.New(t)

	ctx := context.Background()
	r, err := ds.ReadyState(ctx)
	require.NoError(err)
	require.True(r.IsReady)
	firstWrite, err := ds.ReadWriteTx(ctx, func(rwt datastore.ReadWriteTransaction) error {
		// Write basic namespaces.
		return rwt.WriteNamespaces(ctx, namespace.Namespace(
			"resource",
			namespace.MustRelation("reader", nil),
		), namespace.Namespace("user"))
	})
	require.NoError(err)

	// Run GC at the transaction and ensure no relationships are removed.
	pds := ds.(*pgDatastore)

	// Nothing to GC
	removed, err := pds.DeleteBeforeTx(ctx, firstWrite)
	require.NoError(err)
	require.Zero(removed.Relationships)
	require.Zero(removed.Namespaces)

	// Replace the namespace with a new one.
	updateTwoNamespaces, err := ds.ReadWriteTx(ctx, func(rwt datastore.ReadWriteTransaction) error {
		return rwt.WriteNamespaces(
			ctx,
			namespace.Namespace(
				"resource",
				namespace.MustRelation("reader", nil),
				namespace.MustRelation("unused", nil),
			),
			namespace.Namespace("user"),
		)
	})
	require.NoError(err)

	// Run GC to remove the old transaction
	removed, err = pds.DeleteBeforeTx(ctx, updateTwoNamespaces)
	require.NoError(err)
	require.Zero(removed.Relationships)
	require.Equal(int64(1), removed.Transactions) // firstWrite
	require.Equal(int64(2), removed.Namespaces)   // resource, user

	// Write a relationship.
	tpl := tuple.Parse("resource:someresource#reader@user:someuser#...")

	wroteOneRelationship, err := common.WriteTuples(ctx, ds, core.RelationTupleUpdate_CREATE, tpl)
	require.NoError(err)

	// Run GC at the transaction and ensure no relationships are removed, but 1 transaction (the previous write namespace) is.
	removed, err = pds.DeleteBeforeTx(ctx, wroteOneRelationship)
	require.NoError(err)
	require.Zero(removed.Relationships)
	require.Equal(int64(1), removed.Transactions) // updateTwoNamespaces
	require.Zero(removed.Namespaces)

	// Run GC again and ensure there are no changes.
	removed, err = pds.DeleteBeforeTx(ctx, wroteOneRelationship)
	require.NoError(err)
	require.Zero(removed.Relationships)
	require.Zero(removed.Transactions)
	require.Zero(removed.Namespaces)

	// Ensure the relationship is still present.
	tRequire := testfixtures.TupleChecker{Require: require, DS: ds}
	tRequire.TupleExists(ctx, tpl, wroteOneRelationship)

	// Overwrite the relationship.
	relOverwrittenAt, err := common.WriteTuples(ctx, ds, core.RelationTupleUpdate_TOUCH, tpl)
	require.NoError(err)

	// Run GC, which won't clean anything because we're dropping the write transaction only
	removed, err = pds.DeleteBeforeTx(ctx, relOverwrittenAt)
	require.NoError(err)
	require.Equal(int64(1), removed.Relationships) // wroteOneRelationship
	require.Equal(int64(1), removed.Transactions)  // wroteOneRelationship
	require.Zero(removed.Namespaces)

	// Run GC again and ensure there are no changes.
	removed, err = pds.DeleteBeforeTx(ctx, relOverwrittenAt)
	require.NoError(err)
	require.Zero(removed.Relationships)
	require.Zero(removed.Transactions)
	require.Zero(removed.Namespaces)

	// Ensure the relationship is still present.
	tRequire.TupleExists(ctx, tpl, relOverwrittenAt)

	// Delete the relationship.
	relDeletedAt, err := common.WriteTuples(ctx, ds, core.RelationTupleUpdate_DELETE, tpl)
	require.NoError(err)

	// Ensure the relationship is gone.
	tRequire.NoTupleExists(ctx, tpl, relDeletedAt)

	// Run GC, which will now drop the overwrite transaction only and the first tpl revision
	removed, err = pds.DeleteBeforeTx(ctx, relDeletedAt)
	require.NoError(err)
	require.Equal(int64(1), removed.Relationships)
	require.Equal(int64(1), removed.Transactions) // relOverwrittenAt
	require.Zero(removed.Namespaces)

	// Run GC again and ensure there are no changes.
	removed, err = pds.DeleteBeforeTx(ctx, relDeletedAt)
	require.NoError(err)
	require.Zero(removed.Relationships)
	require.Zero(removed.Transactions)
	require.Zero(removed.Namespaces)

	// Write a the relationship a few times.
	var relLastWriteAt datastore.Revision
	for i := 0; i < 3; i++ {
		var err error
		relLastWriteAt, err = common.WriteTuples(ctx, ds, core.RelationTupleUpdate_TOUCH, tpl)
		require.NoError(err)
	}

	// Run GC at the transaction and ensure the older copies of the relationships are removed,
	// as well as the 2 older write transactions and the older delete transaction.
	removed, err = pds.DeleteBeforeTx(ctx, relLastWriteAt)
	require.NoError(err)
	require.Equal(int64(2), removed.Relationships) // delete, old1
	require.Equal(int64(3), removed.Transactions)  // removed, write1, write2
	require.Zero(removed.Namespaces)

	// Ensure the relationship is still present.
	tRequire.TupleExists(ctx, tpl, relLastWriteAt)

	// Inject a transaction to clean up the last write
	lastRev, err := pds.ReadWriteTx(ctx, func(rwt datastore.ReadWriteTransaction) error {
		return nil
	})
	require.NoError(err)

	// Run GC to clean up the last write
	removed, err = pds.DeleteBeforeTx(ctx, lastRev)
	require.NoError(err)
	require.Zero(removed.Relationships)           // write3
	require.Equal(int64(1), removed.Transactions) // write3
	require.Zero(removed.Namespaces)
}

func TransactionTimestampsTest(t *testing.T, ds datastore.Datastore) {
	require := require.New(t)

	ctx := context.Background()
	r, err := ds.ReadyState(ctx)
	require.NoError(err)
	require.True(r.IsReady)

	// Setting db default time zone to before UTC
	pgd := ds.(*pgDatastore)
	_, err = pgd.writePool.Exec(ctx, "SET TIME ZONE 'America/New_York';")
	require.NoError(err)

	// Get timestamp in UTC as reference
	startTimeUTC, err := pgd.Now(ctx)
	require.NoError(err)

	// Transaction timestamp should not be stored in system time zone
	tx, err := pgd.writePool.Begin(ctx)
	require.NoError(err)

	txXID, _, err := createNewTransaction(ctx, tx)
	require.NoError(err)

	err = tx.Commit(ctx)
	require.NoError(err)

	var ts time.Time
	sql, args, err := psql.Select("timestamp").From(tableTransaction).Where(sq.Eq{"xid": txXID}).ToSql()
	require.NoError(err)
	err = pgd.readPool.QueryRow(ctx, sql, args...).Scan(&ts)
	require.NoError(err)

	// Transaction timestamp will be before the reference time if it was stored
	// in the default time zone and reinterpreted
	require.True(startTimeUTC.Before(ts))
}

func GarbageCollectionByTimeTest(t *testing.T, ds datastore.Datastore) {
	require := require.New(t)

	ctx := context.Background()
	r, err := ds.ReadyState(ctx)
	require.NoError(err)
	require.True(r.IsReady)
	// Write basic namespaces.
	_, err = ds.ReadWriteTx(ctx, func(rwt datastore.ReadWriteTransaction) error {
		return rwt.WriteNamespaces(ctx, namespace.Namespace(
			"resource",
			namespace.MustRelation("reader", nil),
		), namespace.Namespace("user"))
	})
	require.NoError(err)

	pds := ds.(*pgDatastore)

	// Sleep 1ms to ensure GC will delete the previous transaction.
	time.Sleep(1 * time.Millisecond)

	// Write a relationship.
	tpl := tuple.Parse("resource:someresource#reader@user:someuser#...")
	relLastWriteAt, err := common.WriteTuples(ctx, ds, core.RelationTupleUpdate_CREATE, tpl)
	require.NoError(err)

	// Run GC and ensure only transactions were removed.
	afterWrite, err := pds.Now(ctx)
	require.NoError(err)

	afterWriteTx, err := pds.TxIDBefore(ctx, afterWrite)
	require.NoError(err)

	removed, err := pds.DeleteBeforeTx(ctx, afterWriteTx)
	require.NoError(err)
	require.Zero(removed.Relationships)
	require.True(removed.Transactions > 0)
	require.Zero(removed.Namespaces)

	// Ensure the relationship is still present.
	tRequire := testfixtures.TupleChecker{Require: require, DS: ds}
	tRequire.TupleExists(ctx, tpl, relLastWriteAt)

	// Sleep 1ms to ensure GC will delete the previous write.
	time.Sleep(1 * time.Millisecond)

	// Delete the relationship.
	relDeletedAt, err := common.WriteTuples(ctx, ds, core.RelationTupleUpdate_DELETE, tpl)
	require.NoError(err)

	// Inject a revision to sweep up the last revision
	_, err = pds.ReadWriteTx(ctx, func(rwt datastore.ReadWriteTransaction) error {
		return nil
	})
	require.NoError(err)

	// Run GC and ensure the relationship is not removed.
	afterDelete, err := pds.Now(ctx)
	require.NoError(err)

	afterDeleteTx, err := pds.TxIDBefore(ctx, afterDelete)
	require.NoError(err)

	removed, err = pds.DeleteBeforeTx(ctx, afterDeleteTx)
	require.NoError(err)
	require.Equal(int64(1), removed.Relationships)
	require.Equal(int64(2), removed.Transactions) // relDeletedAt, injected
	require.Zero(removed.Namespaces)

	// Ensure the relationship is still not present.
	tRequire.NoTupleExists(ctx, tpl, relDeletedAt)
}

const chunkRelationshipCount = 2000

func ChunkedGarbageCollectionTest(t *testing.T, ds datastore.Datastore) {
	require := require.New(t)

	ctx := context.Background()
	r, err := ds.ReadyState(ctx)
	require.NoError(err)
	require.True(r.IsReady)
	// Write basic namespaces.
	_, err = ds.ReadWriteTx(ctx, func(rwt datastore.ReadWriteTransaction) error {
		return rwt.WriteNamespaces(ctx, namespace.Namespace(
			"resource",
			namespace.MustRelation("reader", nil),
		), namespace.Namespace("user"))
	})
	require.NoError(err)

	pds := ds.(*pgDatastore)

	// Prepare relationships to write.
	var tpls []*core.RelationTuple
	for i := 0; i < chunkRelationshipCount; i++ {
		tpl := tuple.Parse(fmt.Sprintf("resource:resource-%d#reader@user:someuser#...", i))
		tpls = append(tpls, tpl)
	}

	// Write a large number of relationships.
	writtenAt, err := common.WriteTuples(ctx, ds, core.RelationTupleUpdate_CREATE, tpls...)
	require.NoError(err)

	// Ensure the relationships were written.
	tRequire := testfixtures.TupleChecker{Require: require, DS: ds}
	for _, tpl := range tpls {
		tRequire.TupleExists(ctx, tpl, writtenAt)
	}

	// Run GC and ensure only transactions were removed.
	afterWrite, err := pds.Now(ctx)
	require.NoError(err)

	afterWriteTx, err := pds.TxIDBefore(ctx, afterWrite)
	require.NoError(err)

	removed, err := pds.DeleteBeforeTx(ctx, afterWriteTx)
	require.NoError(err)
	require.Zero(removed.Relationships)
	require.True(removed.Transactions > 0)
	require.Zero(removed.Namespaces)

	// Sleep to ensure the relationships will GC.
	time.Sleep(1 * time.Millisecond)

	// Delete all the relationships.
	deletedAt, err := common.WriteTuples(ctx, ds, core.RelationTupleUpdate_DELETE, tpls...)
	require.NoError(err)

	// Inject a revision to sweep up the last revision
	_, err = pds.ReadWriteTx(ctx, func(rwt datastore.ReadWriteTransaction) error {
		return nil
	})
	require.NoError(err)

	// Ensure the relationships were deleted.
	for _, tpl := range tpls {
		tRequire.NoTupleExists(ctx, tpl, deletedAt)
	}

	// Sleep to ensure GC.
	time.Sleep(1 * time.Millisecond)

	// Run GC and ensure all the stale relationships are removed.
	afterDelete, err := pds.Now(ctx)
	require.NoError(err)

	afterDeleteTx, err := pds.TxIDBefore(ctx, afterDelete)
	require.NoError(err)

	removed, err = pds.DeleteBeforeTx(ctx, afterDeleteTx)
	require.NoError(err)
	require.Equal(int64(chunkRelationshipCount), removed.Relationships)
	require.Equal(int64(2), removed.Transactions)
	require.Zero(removed.Namespaces)
}

func QuantizedRevisionTest(t *testing.T, b testdatastore.RunningEngineForTest) {
	testCases := []struct {
		testName      string
		quantization  time.Duration
		relativeTimes []time.Duration
		numLower      uint64
		numHigher     uint64
	}{
		{
			"DefaultRevision",
			1 * time.Second,
			[]time.Duration{},
			0, 0,
		},
		{
			"OnlyPastRevisions",
			1 * time.Second,
			[]time.Duration{-2 * time.Second},
			1, 0,
		},
		{
			"OnlyFutureRevisions",
			1 * time.Second,
			[]time.Duration{2 * time.Second},
			0, 1,
		},
		{
			"QuantizedLower",
			2 * time.Second,
			[]time.Duration{-4 * time.Second, -1 * time.Nanosecond, 0},
			1, 2,
		},
		{
			"QuantizationDisabled",
			1 * time.Nanosecond,
			[]time.Duration{-2 * time.Second, -1 * time.Nanosecond, 0},
			3, 0,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.testName, func(t *testing.T) {
			require := require.New(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			var conn *pgx.Conn
			ds := b.NewDatastore(t, func(engine, uri string) datastore.Datastore {
				var err error
				conn, err = pgx.Connect(ctx, uri)
				RegisterTypes(conn.TypeMap())
				require.NoError(err)

				ds, err := newPostgresDatastore(
					uri,
					RevisionQuantization(5*time.Second),
					GCWindow(24*time.Hour),
					WatchBufferLength(1),
				)
				require.NoError(err)

				return ds
			})
			defer ds.Close()

			// set a random time zone to ensure the queries are unaffected by tz
			_, err := conn.Exec(ctx, fmt.Sprintf("SET TIME ZONE -%d", rand.Intn(8)+1))
			require.NoError(err)

			var dbNow time.Time
			err = conn.QueryRow(ctx, "SELECT (NOW() AT TIME ZONE 'utc')").Scan(&dbNow)
			require.NoError(err)

			if len(tc.relativeTimes) > 0 {
				psql := sq.StatementBuilder.PlaceholderFormat(sq.Dollar)
				insertTxn := psql.Insert(tableTransaction).Columns(colTimestamp)

				for _, offset := range tc.relativeTimes {
					sql, args, err := insertTxn.Values(dbNow.Add(offset)).ToSql()
					require.NoError(err)

					_, err = conn.Exec(ctx, sql, args...)
					require.NoError(err)
				}
			}

			queryRevision := fmt.Sprintf(
				querySelectRevision,
				colXID,
				tableTransaction,
				colTimestamp,
				tc.quantization.Nanoseconds(),
				colSnapshot,
			)

			var revision xid8
			var snapshot pgSnapshot
			var validFor time.Duration
			err = conn.QueryRow(ctx, queryRevision).Scan(&revision, &snapshot, &validFor)
			require.NoError(err)

			queryFmt := "SELECT COUNT(%[1]s) FROM %[2]s WHERE pg_visible_in_snapshot(%[1]s, $1) = %[3]s;"
			numLowerQuery := fmt.Sprintf(queryFmt, colXID, tableTransaction, "true")
			numHigherQuery := fmt.Sprintf(queryFmt, colXID, tableTransaction, "false")

			var numLower, numHigher uint64
			require.NoError(conn.QueryRow(ctx, numLowerQuery, snapshot).Scan(&numLower), "%s - %s", revision, snapshot)
			require.NoError(conn.QueryRow(ctx, numHigherQuery, snapshot).Scan(&numHigher), "%s - %s", revision, snapshot)

			// Subtract one from numLower because of the artificially injected first transaction row
			require.Equal(tc.numLower, numLower-1)
			require.Equal(tc.numHigher, numHigher)
		})
	}
}

// ConcurrentRevisionHeadTest uses goroutines and channels to intentionally set up a pair of
// revisions that are concurrently applied and then ensures a call to HeadRevision reflects
// the changes found in *both* revisions.
func ConcurrentRevisionHeadTest(t *testing.T, ds datastore.Datastore) {
	require := require.New(t)

	ctx := context.Background()
	r, err := ds.ReadyState(ctx)
	require.NoError(err)
	require.True(r.IsReady)
	// Write basic namespaces.
	_, err = ds.ReadWriteTx(ctx, func(rwt datastore.ReadWriteTransaction) error {
		return rwt.WriteNamespaces(ctx, namespace.Namespace(
			"resource",
			namespace.MustRelation("reader", nil),
		), namespace.Namespace("user"))
	})
	require.NoError(err)

	g := errgroup.Group{}

	waitToStart := make(chan struct{})
	waitToFinish := make(chan struct{})

	var commitLastRev, commitFirstRev datastore.Revision
	g.Go(func() error {
		var err error
		commitLastRev, err = ds.ReadWriteTx(ctx, func(rwt datastore.ReadWriteTransaction) error {
			rtu := tuple.Touch(&core.RelationTuple{
				ResourceAndRelation: &core.ObjectAndRelation{
					Namespace: "resource",
					ObjectId:  "123",
					Relation:  "reader",
				},
				Subject: &core.ObjectAndRelation{
					Namespace: "user",
					ObjectId:  "456",
					Relation:  "...",
				},
			})
			err = rwt.WriteRelationships(ctx, []*core.RelationTupleUpdate{rtu})
			require.NoError(err)

			close(waitToStart)
			<-waitToFinish

			return err
		})
		require.NoError(err)
		return nil
	})

	<-waitToStart

	commitFirstRev, err = ds.ReadWriteTx(ctx, func(rwt datastore.ReadWriteTransaction) error {
		rtu := tuple.Touch(&core.RelationTuple{
			ResourceAndRelation: &core.ObjectAndRelation{
				Namespace: "resource",
				ObjectId:  "789",
				Relation:  "reader",
			},
			Subject: &core.ObjectAndRelation{
				Namespace: "user",
				ObjectId:  "456",
				Relation:  "...",
			},
		})
		return rwt.WriteRelationships(ctx, []*core.RelationTupleUpdate{rtu})
	})
	close(waitToFinish)

	require.NoError(err)
	require.NoError(g.Wait())

	// Ensure the revisions do not compare.
	require.False(commitFirstRev.GreaterThan(commitLastRev))
	require.False(commitFirstRev.Equal(commitLastRev))

	// Ensure a call to HeadRevision now reflects both sets of data applied.
	headRev, err := ds.HeadRevision(ctx)
	require.NoError(err)

	reader := ds.SnapshotReader(headRev)
	it, err := reader.QueryRelationships(ctx, datastore.RelationshipsFilter{
		ResourceType: "resource",
	})
	require.NoError(err)
	defer it.Close()

	found := []*core.RelationTuple{}
	for tpl := it.Next(); tpl != nil; tpl = it.Next() {
		require.NoError(it.Err())
		found = append(found, tpl)
	}

	require.Equal(2, len(found), "missing relationships in %v", found)
}

// RevisionInversionTest uses goroutines and channels to intentionally set up a pair of
// revisions that might compare incorrectly.
func RevisionInversionTest(t *testing.T, ds datastore.Datastore) {
	require := require.New(t)

	ctx := context.Background()
	r, err := ds.ReadyState(ctx)
	require.NoError(err)
	require.True(r.IsReady)
	// Write basic namespaces.
	_, err = ds.ReadWriteTx(ctx, func(rwt datastore.ReadWriteTransaction) error {
		return rwt.WriteNamespaces(ctx, namespace.Namespace(
			"resource",
			namespace.MustRelation("reader", nil),
		), namespace.Namespace("user"))
	})
	require.NoError(err)

	g := errgroup.Group{}

	waitToStart := make(chan struct{})
	waitToFinish := make(chan struct{})

	var commitLastRev, commitFirstRev datastore.Revision
	g.Go(func() error {
		var err error
		commitLastRev, err = ds.ReadWriteTx(ctx, func(rwt datastore.ReadWriteTransaction) error {
			rtu := tuple.Touch(&core.RelationTuple{
				ResourceAndRelation: &core.ObjectAndRelation{
					Namespace: "resource",
					ObjectId:  "123",
					Relation:  "reader",
				},
				Subject: &core.ObjectAndRelation{
					Namespace: "user",
					ObjectId:  "456",
					Relation:  "...",
				},
			})
			err = rwt.WriteRelationships(ctx, []*core.RelationTupleUpdate{rtu})
			require.NoError(err)

			close(waitToStart)
			<-waitToFinish

			return err
		})
		require.NoError(err)
		return nil
	})

	<-waitToStart

	commitFirstRev, err = ds.ReadWriteTx(ctx, func(rwt datastore.ReadWriteTransaction) error {
		rtu := tuple.Touch(&core.RelationTuple{
			ResourceAndRelation: &core.ObjectAndRelation{
				Namespace: "resource",
				ObjectId:  "789",
				Relation:  "reader",
			},
			Subject: &core.ObjectAndRelation{
				Namespace: "user",
				ObjectId:  "ten",
				Relation:  "...",
			},
		})
		return rwt.WriteRelationships(ctx, []*core.RelationTupleUpdate{rtu})
	})
	close(waitToFinish)

	require.NoError(err)
	require.NoError(g.Wait())
	require.False(commitFirstRev.GreaterThan(commitLastRev))
	require.False(commitFirstRev.Equal(commitLastRev))
}

func WatchNotEnabledTest(t *testing.T, _ testdatastore.RunningEngineForTest) {
	require := require.New(t)

	ds := testdatastore.RunPostgresForTestingWithCommitTimestamps(t, "", migrate.Head, false).NewDatastore(t, func(engine, uri string) datastore.Datastore {
		ds, err := newPostgresDatastore(uri,
			RevisionQuantization(0),
			GCWindow(time.Millisecond*1),
			WatchBufferLength(1),
		)
		require.NoError(err)
		return ds
	})
	defer ds.Close()

	ds, revision := testfixtures.StandardDatastoreWithData(ds, require)
	_, errChan := ds.Watch(
		context.Background(),
		revision,
	)
	err := <-errChan
	require.NotNil(err)
	require.Contains(err.Error(), "track_commit_timestamp=on")
}

func BenchmarkPostgresQuery(b *testing.B) {
	req := require.New(b)

	ds := testdatastore.RunPostgresForTesting(b, "", migrate.Head).NewDatastore(b, func(engine, uri string) datastore.Datastore {
		ds, err := newPostgresDatastore(uri,
			RevisionQuantization(0),
			GCWindow(time.Millisecond*1),
			WatchBufferLength(1),
		)
		require.NoError(b, err)
		return ds
	})
	defer ds.Close()
	ds, revision := testfixtures.StandardDatastoreWithData(ds, req)

	b.Run("benchmark checks", func(b *testing.B) {
		require := require.New(b)

		for i := 0; i < b.N; i++ {
			iter, err := ds.SnapshotReader(revision).QueryRelationships(context.Background(), datastore.RelationshipsFilter{
				ResourceType: testfixtures.DocumentNS.Name,
			})
			require.NoError(err)

			defer iter.Close()

			for tpl := iter.Next(); tpl != nil; tpl = iter.Next() {
				require.Equal(testfixtures.DocumentNS.Name, tpl.ResourceAndRelation.Namespace)
			}
			require.NoError(iter.Err())
		}
	})
}

func datastoreWithInterceptorAndTestData(t *testing.T, interceptor pgcommon.QueryInterceptor) datastore.Datastore {
	require := require.New(t)

	ds := testdatastore.RunPostgresForTestingWithCommitTimestamps(t, "", migrate.Head, false).NewDatastore(t, func(engine, uri string) datastore.Datastore {
		ds, err := newPostgresDatastore(uri,
			RevisionQuantization(0),
			GCWindow(time.Millisecond*1),
			WatchBufferLength(1),
			WithQueryInterceptor(interceptor),
		)
		require.NoError(err)
		return ds
	})
	t.Cleanup(func() {
		ds.Close()
	})

	ds, _ = testfixtures.StandardDatastoreWithData(ds, require)

	// Write namespaces and a few thousand relationships.
	ctx := context.Background()
	for i := 0; i < 1000; i++ {
		_, err := ds.ReadWriteTx(ctx, func(rwt datastore.ReadWriteTransaction) error {
			err := rwt.WriteNamespaces(ctx, namespace.Namespace(
				fmt.Sprintf("resource%d", i),
				namespace.MustRelation("reader", nil)))
			if err != nil {
				return err
			}

			// Write some relationships.
			rtu := tuple.Touch(&core.RelationTuple{
				ResourceAndRelation: &core.ObjectAndRelation{
					Namespace: testfixtures.DocumentNS.Name,
					ObjectId:  fmt.Sprintf("doc%d", i),
					Relation:  "reader",
				},
				Subject: &core.ObjectAndRelation{
					Namespace: "user",
					ObjectId:  "456",
					Relation:  "...",
				},
			})

			rtu2 := tuple.Touch(&core.RelationTuple{
				ResourceAndRelation: &core.ObjectAndRelation{
					Namespace: fmt.Sprintf("resource%d", i),
					ObjectId:  "123",
					Relation:  "reader",
				},
				Subject: &core.ObjectAndRelation{
					Namespace: "user",
					ObjectId:  "456",
					Relation:  "...",
				},
			})

			rtu3 := tuple.Touch(&core.RelationTuple{
				ResourceAndRelation: &core.ObjectAndRelation{
					Namespace: fmt.Sprintf("resource%d", i),
					ObjectId:  "123",
					Relation:  "writer",
				},
				Subject: &core.ObjectAndRelation{
					Namespace: "user",
					ObjectId:  "456",
					Relation:  "...",
				},
			})

			return rwt.WriteRelationships(ctx, []*core.RelationTupleUpdate{rtu, rtu2, rtu3})
		})
		require.NoError(err)
	}

	// Delete some relationships.
	for i := 990; i < 1000; i++ {
		_, err := ds.ReadWriteTx(ctx, func(rwt datastore.ReadWriteTransaction) error {
			rtu := tuple.Delete(&core.RelationTuple{
				ResourceAndRelation: &core.ObjectAndRelation{
					Namespace: testfixtures.DocumentNS.Name,
					ObjectId:  fmt.Sprintf("doc%d", i),
					Relation:  "reader",
				},
				Subject: &core.ObjectAndRelation{
					Namespace: "user",
					ObjectId:  "456",
					Relation:  "...",
				},
			})

			rtu2 := tuple.Delete(&core.RelationTuple{
				ResourceAndRelation: &core.ObjectAndRelation{
					Namespace: fmt.Sprintf("resource%d", i),
					ObjectId:  "123",
					Relation:  "reader",
				},
				Subject: &core.ObjectAndRelation{
					Namespace: "user",
					ObjectId:  "456",
					Relation:  "...",
				},
			})
			return rwt.WriteRelationships(ctx, []*core.RelationTupleUpdate{rtu, rtu2})
		})
		require.NoError(err)
	}

	// Write some more relationships.
	for i := 1000; i < 1100; i++ {
		_, err := ds.ReadWriteTx(ctx, func(rwt datastore.ReadWriteTransaction) error {
			// Write some relationships.
			rtu := tuple.Touch(&core.RelationTuple{
				ResourceAndRelation: &core.ObjectAndRelation{
					Namespace: testfixtures.DocumentNS.Name,
					ObjectId:  fmt.Sprintf("doc%d", i),
					Relation:  "reader",
				},
				Subject: &core.ObjectAndRelation{
					Namespace: "user",
					ObjectId:  "456",
					Relation:  "...",
				},
			})

			rtu2 := tuple.Touch(&core.RelationTuple{
				ResourceAndRelation: &core.ObjectAndRelation{
					Namespace: fmt.Sprintf("resource%d", i),
					ObjectId:  "123",
					Relation:  "reader",
				},
				Subject: &core.ObjectAndRelation{
					Namespace: "user",
					ObjectId:  "456",
					Relation:  "...",
				},
			})
			return rwt.WriteRelationships(ctx, []*core.RelationTupleUpdate{rtu, rtu2})
		})
		require.NoError(err)
	}

	return ds
}

func GCQueriesServedByExpectedIndexes(t *testing.T, _ testdatastore.RunningEngineForTest) {
	require := require.New(t)
	interceptor := &withQueryInterceptor{explanations: make(map[string]string, 0)}
	ds := datastoreWithInterceptorAndTestData(t, interceptor)

	// Get the head revision.
	ctx := context.Background()
	revision, err := ds.HeadRevision(ctx)
	require.NoError(err)

	for {
		wds, ok := ds.(datastore.UnwrappableDatastore)
		if !ok {
			break
		}
		ds = wds.Unwrap()
	}

	casted, ok := ds.(common.GarbageCollector)
	require.True(ok)

	_, err = casted.DeleteBeforeTx(context.Background(), revision)
	require.NoError(err)

	require.NotEmpty(interceptor.explanations, "expected queries to be executed")

	// Ensure we have indexes representing each query in the GC workflow.
	for _, explanation := range interceptor.explanations {
		switch {
		case strings.HasPrefix(explanation, "Delete on relation_tuple_transaction"):
			fallthrough

		case strings.HasPrefix(explanation, "Delete on namespace_config"):
			fallthrough

		case strings.HasPrefix(explanation, "Delete on relation_tuple"):
			require.Contains(explanation, "Index Scan")

		default:
			require.Failf("unknown GC query: %s", explanation)
		}
	}
}
