package badger

import (
	"fmt"
	"math"

	"github.com/dgraph-io/badger/v2"
	"github.com/dgraph-io/badger/v2/options"

	cmnBadger "github.com/oasisprotocol/oasis-core/go/common/badger"
	"github.com/oasisprotocol/oasis-core/go/common/cbor"
	"github.com/oasisprotocol/oasis-core/go/common/crypto/hash"
	"github.com/oasisprotocol/oasis-core/go/common/keyformat"
	"github.com/oasisprotocol/oasis-core/go/common/logging"
	"github.com/oasisprotocol/oasis-core/go/storage/mkvs/db/api"
	"github.com/oasisprotocol/oasis-core/go/storage/mkvs/node"
)

type migratorFactory func(db *badgerNodeDB, helper MigrationHelper) migration

var (
	originVersions = map[uint64]migratorFactory{
		3: func(db *badgerNodeDB, helper MigrationHelper) migration {
			return &v4Migrator{
				meta: v4MigratorMetadata{
					Roots: map[typedHash]bool{},
				},
				db:     db,
				helper: helper,
			}
		},
	}

	migrationMetaKeyFmt = keyformat.New(0xff)

	v3NodeKeyFmt                    = keyformat.New(0x00, &hash.Hash{})
	v3WriteLogKeyFmt                = keyformat.New(0x01, uint64(0), &hash.Hash{}, &hash.Hash{})
	v3RootsMetadataKeyFmt           = keyformat.New(0x02, uint64(0))
	v3RootUpdatedNodesKeyFmt        = keyformat.New(0x03, uint64(0), &hash.Hash{})
	v3MetadataKeyFmt                = keyformat.New(0x04)
	v3MultipartRestoreNodeLogKeyFmt = keyformat.New(0x05, &hash.Hash{})

	v4NodeKeyFmt                    = nodeKeyFmt
	v4WriteLogKeyFmt                = writeLogKeyFmt
	v4RootsMetadataKeyFmt           = rootsMetadataKeyFmt
	v4RootUpdatedNodesKeyFmt        = rootUpdatedNodesKeyFmt
	v4MetadataKeyFmt                = metadataKeyFmt
	v4MultipartRestoreNodeLogKeyFmt = multipartRestoreNodeLogKeyFmt
)

type v3RootsMetadata struct {
	_ struct{} `cbor:",toarray"`

	Roots map[hash.Hash][]hash.Hash
}

type v4RootsMetadata = rootsMetadata

type v3UpdatedNode struct {
	_ struct{} `cbor:",toarray"` // nolint

	Removed bool
	Hash    hash.Hash
}

type v4UpdatedNode = updatedNode

// No change in metadata format between versions 3 and 4.
type v3SerializedMetadata = serializedMetadata

type v4SerializedMetadata = serializedMetadata

type MigrationHelper interface {
	GetRootForHash(root hash.Hash, version uint64) (node.Root, error)

	ReportStatus(msg string)
	ReportProgress(msg string, current, total uint64)
}

type migration interface {
	Migrate() (uint64, error)
}

type migrationCommonMeta struct {
	// An item with this key should always exist in the metadata blob.
	// It is the original version of the database, before the migration started,
	// so the migration driver can choose the correct migration to resume with
	// even in cases where the database metadata key was already migrated.
	BaseDBVersion uint64 `json:"base_version"`
}

type v4MigratorMetadata struct {
	migrationCommonMeta

	Roots map[typedHash]bool `json:"roots"`

	InitComplete bool `json:"init_complete"`
	MetaComplete bool `json:"meta_complete"`
	TreeComplete bool `json:"tree_complete"`

	LastKey []byte `json:"last_key"`

	MetaCount        uint64 `json:"meta_count"`
	TreeCount        uint64 `json:"tree_count"`
	CurrentTreeCount uint64 `json:"current_tree_count"`
	CurrentMetaCount uint64 `json:"current_meta_count"`
}

func (m *v4MigratorMetadata) load(db *badger.DB) error {
	txn := db.NewTransactionAt(tsMetadata, false)
	defer txn.Discard()

	item, err := txn.Get(migrationMetaKeyFmt.Encode())
	if err != nil {
		return err
	}

	return item.Value(func(data []byte) error {
		return cbor.Unmarshal(data, m)
	})
}

func (m *v4MigratorMetadata) save(db *badger.DB) error {
	txn := db.NewTransactionAt(tsMetadata, true)
	defer txn.Discard()

	err := txn.Set(
		migrationMetaKeyFmt.Encode(),
		cbor.Marshal(m),
	)
	if err != nil {
		return err
	}
	return txn.Commit()
}

func (m *v4MigratorMetadata) remove(db *badger.DB) error {
	txn := db.NewTransactionAt(tsMetadata, true)
	defer txn.Discard()

	err := txn.Delete(migrationMetaKeyFmt.Encode())
	if err != nil {
		return err
	}
	return txn.Commit()
}

type v4Migrator struct {
	db     *badgerNodeDB
	helper MigrationHelper

	readTxn     *badger.Txn
	changeBatch *badger.WriteBatch

	meta v4MigratorMetadata
}

func (v4 *v4Migrator) walkTree(rootType node.RootType, nodeHash hash.Hash) error {
	defer func() {
		// The count of currently processed nodes here might over- or undershoot since
		// we have a read transaction that's stuck in the past. But node hashes aren't
		// supposed to come up more than once, so it should be approximately correct.
		v4.helper.ReportProgress("updated tree nodes", v4.meta.CurrentTreeCount, v4.meta.TreeCount)
	}()

	th := typedHashFromParts(rootType, nodeHash)

	// Trivial: see if this node appears in an on-going multipart restore.
	multipartKey := v3MultipartRestoreNodeLogKeyFmt.Encode(&nodeHash)
	if item, err := v4.readTxn.Get(multipartKey); err == nil {
		err = v4.changeBatch.DeleteAt(multipartKey, item.Version())
		if err != nil {
			return err
		}
		var value []byte
		_ = item.Value(func(data []byte) error {
			value = data
			return nil
		})
		entry := badger.NewEntry(
			v4MultipartRestoreNodeLogKeyFmt.Encode(&th),
			value,
		)
		err = v4.changeBatch.SetEntryAt(entry, item.Version())
		if err != nil {
			return err
		}

		v4.meta.CurrentTreeCount++
	}

	// Node processing and tree walk.
	nodeKey := v3NodeKeyFmt.Encode(&nodeHash)
	if item, err := v4.readTxn.Get(nodeKey); err == nil {
		var n node.Node
		var value []byte
		err = item.Value(func(data []byte) error {
			var nerr error
			value = data
			n, nerr = node.UnmarshalBinary(data)
			return nerr
		})
		if err != nil {
			return err
		}

		err = v4.changeBatch.DeleteAt(nodeKey, item.Version())
		if err != nil {
			return err
		}
		entry := badger.NewEntry(
			v4NodeKeyFmt.Encode(&th),
			value,
		)
		err = v4.changeBatch.SetEntryAt(entry, item.Version())
		if err != nil {
			return err
		}

		switch nn := n.(type) {
		case *node.LeafNode:
			// Nothing to do here.

		case *node.InternalNode:
			children := make([]hash.Hash, 0, 3)
			if nn.LeafNode != nil {
				children = append(children, nn.LeafNode.Hash)
			}
			if nn.Left != nil {
				children = append(children, nn.Left.Hash)
			}
			if nn.Right != nil {
				children = append(children, nn.Right.Hash)
			}
			for _, child := range children {
				err = v4.walkTree(rootType, child)
				if err != nil {
					return err
				}
			}
		}

		v4.meta.CurrentTreeCount++
	}

	return nil
}

func (v4 *v4Migrator) seek(it *badger.Iterator) {
	it.Rewind()
	if len(v4.meta.LastKey) == 0 {
		return
	}
	it.Seek(v4.meta.LastKey)
	// This key would've been done already, we need the next one.
	it.Next()
}

func (v4 *v4Migrator) migrateMeta(it *badger.Iterator) error {
	v4.helper.ReportStatus("migrating roots and metadata")
	for v4.seek(it); it.Valid(); it.Next() {
		key := it.Item().Key()

		var h1, h2 hash.Hash
		var th1, th2 typedHash
		var version uint64

		switch {
		case v3NodeKeyFmt.Decode(key, &h1):
			// Tree nodes will be updated once we have all roots.

		case v3WriteLogKeyFmt.Decode(key, &version, &h1, &h2):
			var val []byte
			_ = it.Item().Value(func(data []byte) error {
				val = data
				return nil
			})

			r1, err := v4.helper.GetRootForHash(h1, version)
			if err != nil {
				return fmt.Errorf("mkvs/badger/migrate: error getting writelog root %v: %w", h1, err)
			}
			th1.FromParts(r1.Type, h1)
			th2.FromParts(r1.Type, h2)

			entry := badger.NewEntry(v4WriteLogKeyFmt.Encode(&version, &th1, &th2), val)
			err = v4.changeBatch.SetEntryAt(entry, it.Item().Version())
			if err != nil {
				return fmt.Errorf("mkvs/badger/migrate: error setting updated writelog key: %w", err)
			}
			err = v4.changeBatch.DeleteAt(key, it.Item().Version())
			if err != nil {
				return fmt.Errorf("mkvs/badger/migrate: error removing old writelog key: %w", err)
			}

		case v3RootsMetadataKeyFmt.Decode(key, &version):
			var rootsMeta v3RootsMetadata
			err := it.Item().Value(func(data []byte) error {
				return cbor.Unmarshal(data, &rootsMeta)
			})
			if err != nil {
				return fmt.Errorf("mkvs/badger/migrate: error deserializing roots metadata: %w", err)
			}

			var newRoots v4RootsMetadata
			newRoots.Roots = map[typedHash][]typedHash{}
			for k, v := range rootsMeta.Roots {
				arr := make([]typedHash, 0, len(v))
				var root node.Root
				for _, r := range v {
					root, err = v4.helper.GetRootForHash(r, version)
					if err != nil {
						return fmt.Errorf("mkvs/badger/migrate: error getting root for %v: %w", r, err)
					}
					th := typedHashFromRoot(root)
					v4.meta.Roots[th] = true
					arr = append(arr, th)
				}
				root, err = v4.helper.GetRootForHash(k, version)
				if err != nil {
					return fmt.Errorf("mkvs/badger/migrate: error getting root for %v: %w", k, err)
				}
				th := typedHashFromRoot(root)
				v4.meta.Roots[th] = true
				newRoots.Roots[th] = arr
			}

			entry := badger.NewEntry(v4RootsMetadataKeyFmt.Encode(&version), cbor.Marshal(newRoots))
			err = v4.changeBatch.SetEntryAt(entry, it.Item().Version())
			if err != nil {
				return fmt.Errorf("mkvs/badger/migrate: error storing updated root metadata: %w", err)
			}
			err = v4.changeBatch.DeleteAt(key, it.Item().Version())
			if err != nil {
				return fmt.Errorf("mkvs/badger/migrate: error removing old root metadata: %w", err)
			}

		case v3RootUpdatedNodesKeyFmt.Decode(key, &version, &h1):
			var oldUpdatedNodes []v3UpdatedNode
			err := it.Item().Value(func(data []byte) error {
				return cbor.UnmarshalTrusted(data, &oldUpdatedNodes)
			})
			if err != nil {
				return fmt.Errorf("mkvs/badger/migrate: error decoding updated nodes list for root %v:%v: %w", h1, version, err)
			}

			root, err := v4.helper.GetRootForHash(h1, version)
			if err != nil {
				return fmt.Errorf("mkvs/badger/migrate: error getting root %v:%v for updated nodes list: %w", h1, version, err)
			}
			v4.meta.Roots[typedHashFromRoot(root)] = true

			newUpdatedNodes := make([]v4UpdatedNode, 0, len(oldUpdatedNodes))
			for _, up := range oldUpdatedNodes {
				newUpdatedNodes = append(newUpdatedNodes, v4UpdatedNode{
					Removed: up.Removed,
					Hash:    typedHashFromParts(root.Type, up.Hash),
				})
			}

			th := typedHashFromRoot(root)
			entry := badger.NewEntry(
				v4RootUpdatedNodesKeyFmt.Encode(version, &th),
				cbor.Marshal(newUpdatedNodes),
			)
			err = v4.changeBatch.SetEntryAt(entry, it.Item().Version())
			if err != nil {
				return fmt.Errorf("mkvs/badger/migrate: error storing updated nodes list for root %v: %w", root, err)
			}
			err = v4.changeBatch.DeleteAt(key, it.Item().Version())
			if err != nil {
				return fmt.Errorf("mkvs/badger/migrate: error deleting old nodes nodes list for root %v: %w", root, err)
			}

		case v3MetadataKeyFmt.Decode(key):
			var meta3 v3SerializedMetadata
			err := it.Item().Value(func(data []byte) error {
				return cbor.UnmarshalTrusted(data, &meta3)
			})
			if err != nil {
				return fmt.Errorf("mkvs/badger/migrate: error decoding database metadata: %w", err)
			}

			var meta4 v4SerializedMetadata
			meta4 = meta3
			meta4.Version = 4

			entry := badger.NewEntry(
				key,
				cbor.Marshal(meta4),
			)
			err = v4.changeBatch.SetEntryAt(entry, it.Item().Version())
			if err != nil {
				return fmt.Errorf("mkvs/badger/migrate: error storing updated database metadata: %w", err)
			}

		case v3MultipartRestoreNodeLogKeyFmt.Decode(key, &h1):
			// Tree nodes will be updated once we have all roots.
		}

		v4.meta.CurrentMetaCount++
		v4.helper.ReportProgress("updated keys", v4.meta.CurrentMetaCount, v4.meta.MetaCount)
		v4.meta.LastKey = it.Item().Key()
		if err := v4.meta.save(v4.db.db); err != nil {
			return err
		}
	}

	return nil
}

func (v4 *v4Migrator) migrateTree(it *badger.Iterator) error {
	v4.helper.ReportStatus("migrating tree nodes")
	v4.meta.CurrentTreeCount = 0
	// TODO make this seekable
	for root := range v4.meta.Roots {
		if err := v4.walkTree(root.Type(), root.Hash()); err != nil {
			return fmt.Errorf("mkvs/badger/migrate: error walking node tree under %v: %w", root, err)
		}
	}

	return nil
}

func (v4 *v4Migrator) Migrate() (rversion uint64, rerr error) {
	v4.readTxn = v4.db.db.NewTransactionAt(math.MaxUint64, false)
	defer v4.readTxn.Discard()
	v4.changeBatch = v4.db.db.NewWriteBatchAt(tsMetadata)
	defer v4.changeBatch.Cancel()

	// Load migration metadata and set up saving on function return.
	err := v4.meta.load(v4.db.db)
	if err != nil && err != badger.ErrKeyNotFound {
		return 0, err
	}
	v4.meta.BaseDBVersion = 3

	defer func() {
		if rerr != nil {
			v4.meta.save(v4.db.db)
		} else if !v4.meta.TreeComplete || !v4.meta.MetaComplete {
			rerr = v4.meta.save(v4.db.db)
		} else {
			rerr = v4.meta.remove(v4.db.db)
		}
	}()

	it := v4.readTxn.NewIterator(badger.DefaultIteratorOptions)
	defer it.Close()

	// Count keys first, so we can report some sensible progress to the user.
	// Badger says this should be fast.
	if !v4.meta.InitComplete {
		v4.meta.TreeCount = 0
		v4.meta.MetaCount = 0
		for it.Rewind(); it.Valid(); it.Next() {
			prefix := it.Item().Key()[0]
			if prefix == v3NodeKeyFmt.Prefix() || prefix == v3MultipartRestoreNodeLogKeyFmt.Prefix() {
				v4.meta.TreeCount++
			} else {
				v4.meta.MetaCount++
			}
		}
		v4.meta.InitComplete = true
		if err := v4.meta.save(v4.db.db); err != nil {
			return 0, err
		}
	}

	// Migrate!

	if !v4.meta.MetaComplete {
		if err := v4.migrateMeta(it); err != nil {
			return 0, err
		}
		v4.meta.MetaComplete = true
		if err := v4.meta.save(v4.db.db); err != nil {
			return 0, err
		}
	}

	if !v4.meta.TreeComplete {
		if err := v4.migrateTree(it); err != nil {
			return 0, err
		}
		v4.meta.TreeComplete = true
		if err := v4.meta.save(v4.db.db); err != nil {
			return 0, err
		}
	}

	// All done, flush and clean up. The metadata blob will be removed
	// in the defer handler.
	if err := v4.changeBatch.Flush(); err != nil {
		return 0, fmt.Errorf("mkvs/badger/migrate: error writing changes to the database: %w", err)
	}
	return 4, nil
}

func Migrate(cfg *api.Config, helper MigrationHelper) (uint64, error) {
	db := &badgerNodeDB{
		logger:           logging.GetLogger("mkvs/db/badger/migrate"),
		namespace:        cfg.Namespace,
		discardWriteLogs: cfg.DiscardWriteLogs,
	}

	opts := badger.DefaultOptions(cfg.DB)
	opts = opts.WithLogger(cmnBadger.NewLogAdapter(db.logger))
	opts = opts.WithSyncWrites(!cfg.NoFsync)
	// Allow value log truncation if required (this is needed to recover the
	// value log file which can get corrupted in crashes).
	opts = opts.WithTruncate(true)
	opts = opts.WithCompression(options.Snappy)
	opts = opts.WithBlockCacheSize(cfg.MaxCacheSize)
	opts = opts.WithDetectConflicts(false)

	var err error

	if db.db, err = badger.OpenManaged(opts); err != nil {
		return 0, fmt.Errorf("mkvs/badger/migrate: failed to open database: %w", err)
	}

	// Make sure that we can discard any deleted/invalid metadata.
	db.db.SetDiscardTs(tsMetadata)

	db.gc = cmnBadger.NewGCWorker(db.logger, db.db)
	defer db.Close()

	// Load metadata.
	lastVersion, err := func() (uint64, error) {
		tx := db.db.NewTransactionAt(tsMetadata, false)
		defer tx.Discard()

		var migMeta migrationCommonMeta
		item, rerr := tx.Get(migrationMetaKeyFmt.Encode())
		if rerr == nil {
			rerr = item.Value(func(data []byte) error {
				return cbor.Unmarshal(data, &migMeta)
			})
			if rerr != nil {
				return 0, fmt.Errorf("mkvs/badger/migrate: error decoding metadata from a previous migration: %w", rerr)
			}
			return migMeta.BaseDBVersion, nil
		}

		item, rerr = tx.Get(metadataKeyFmt.Encode())
		if rerr != nil {
			return 0, rerr
		}

		var meta metadata

		rerr = item.Value(func(data []byte) error {
			return cbor.UnmarshalTrusted(data, &meta.value)
		})
		if rerr != nil {
			return 0, rerr
		}

		return meta.value.Version, nil
	}()
	if err != nil {
		return 0, fmt.Errorf("mkvs/badger/migrate: error probing current database version: %w", err)
	}

	// Main upgrade loop.
	for lastVersion != dbVersion {
		migratorFactory := originVersions[lastVersion]
		if migratorFactory == nil {
			return 0, fmt.Errorf("mkvs/badger/migrate: unsupported version %d", lastVersion)
		}
		migrator := migratorFactory(db, helper)

		newVersion, err := migrator.Migrate()
		if err != nil {
			return 0, fmt.Errorf("mkvs/badger/migrate: error while migrating from version %d: %w", lastVersion, err)
		}
		lastVersion = newVersion
	}

	return lastVersion, nil
}
