package mysql

import (
	"database/sql"

	"github.com/golang/glog"
	"github.com/golang/protobuf/proto"
	"github.com/google/trillian"
	"github.com/google/trillian/merkle"
	"github.com/google/trillian/storage"
	"github.com/google/trillian/storage/cache"
)

const insertMapHeadSQL string = `INSERT INTO MapHead(TreeId, MapHeadTimestamp, RootHash, MapRevision, RootSignature, MapperData)
	VALUES(?, ?, ?, ?, ?, ?)`

const selectLatestSignedMapRootSQL string = `SELECT MapHeadTimestamp, RootHash, MapRevision, RootSignature, MapperData
		 FROM MapHead WHERE TreeId=?
		 ORDER BY MapHeadTimestamp DESC LIMIT 1`

const insertMapLeafSQL string = `INSERT INTO MapLeaf(TreeId, KeyHash, MapRevision, TheData) VALUES (?, ?, ?, ?)`

// Note that MapRevision is stored negated, hence the odd equality check below:
const selectMapLeafSQL string = `SELECT KeyHash, MAX(MapRevision), TheData
	 FROM MapLeaf
	 WHERE KeyHash IN (` + placeholderSQL + `) AND
	       TreeId = ? AND
				 MapRevision >= ?
	 GROUP BY KeyHash`

var defaultMapStrata = []int{8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 176}

type mySQLMapStorage struct {
	*mySQLTreeStorage

	mapID trillian.MapID
}

func (m *mySQLMapStorage) MapID() trillian.MapID {
	return m.mapID
}

// NewMapStorage creates a mySQLMapStorage instance for the specified MySQL URL.
func NewMapStorage(id trillian.MapID, dbURL string) (storage.MapStorage, error) {
	// TODO(al): pass this through/configure from DB
	th := merkle.NewRFC6962TreeHasher(trillian.NewSHA256())
	ts, err := newTreeStorage(id.TreeID, dbURL, th.Size(), defaultMapStrata, cache.PopulateMapSubtreeNodes(th))
	if err != nil {
		glog.Warningf("Couldn't create a new treeStorage: %s", err)
		return nil, err
	}

	s := mySQLMapStorage{
		mySQLTreeStorage: ts,
		mapID:            id,
	}

	if err != nil {
		glog.Warningf("Couldn't create a new treeStorage: %s", err)
		return nil, err
	}

	return &s, nil
}

func (m *mySQLMapStorage) Begin() (storage.MapTX, error) {
	ttx, err := m.beginTreeTx()
	if err != nil {
		return nil, err
	}
	ret := &mapTX{
		treeTX: ttx,
		ms:     m,
	}

	root, err := ret.LatestSignedMapRoot()
	if err != nil {
		return nil, err
	}

	ret.treeTX.writeRevision = root.MapRevision + 1

	return ret, nil
}

func (m *mySQLMapStorage) Snapshot() (storage.ReadOnlyMapTX, error) {
	tx, err := m.Begin()
	if err != nil {
		return nil, err
	}
	return tx.(storage.ReadOnlyMapTX), err
}

type mapTX struct {
	treeTX
	ms *mySQLMapStorage
}

func (m *mapTX) WriteRevision() int64 {
	return m.treeTX.writeRevision
}

func (m *mapTX) Set(keyHash trillian.Hash, value trillian.MapLeaf) error {
	// TODO(al): consider storing some sort of value which represents the group of keys being set in this Tx.
	//           That way, if this attempt partially fails (i.e. because some subset of the in-the-future merkle
	//           nodes do get written), we can enforce that future map update attempts are a complete replay of
	//           the failed set.
	flatValue, err := proto.Marshal(&value)
	if err != nil {
		return nil
	}

	stmt, err := m.tx.Prepare(insertMapLeafSQL)
	if err != nil {
		return err
	}
	defer stmt.Close()

	// Note: MapRevision is stored negated:
	_, err = stmt.Exec(m.ms.mapID.TreeID, []byte(keyHash), -m.writeRevision, flatValue)
	return err
}

func (m *mapTX) Get(revision int64, keyHashes []trillian.Hash) ([]trillian.MapLeaf, error) {
	stmt, err := m.ms.getStmt(selectMapLeafSQL, len(keyHashes), "?", "?")
	if err != nil {
		return nil, err
	}
	stx := m.tx.Stmt(stmt)
	defer stx.Close()

	args := make([]interface{}, 0, len(keyHashes)+2)
	for _, k := range keyHashes {
		args = append(args, []byte(k[:]))
	}
	args = append(args, m.ms.mapID.TreeID)
	// Note: MapRevision is negated when stored to cause more recent revisions to
	// appear earlier in query results.
	args = append(args, -revision)

	glog.Infof("args size %d", len(args))

	rows, err := stx.Query(args...)
	// It's possible there are no values for any of these keys yet
	if err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, err
	}

	ret := make([]trillian.MapLeaf, 0, len(keyHashes))
	nr := 0
	er := 0
	for rows.Next() {
		var mapKeyHash trillian.Hash
		var mapRevision int64
		var flatData []byte
		err = rows.Scan(&mapKeyHash, &mapRevision, &flatData)
		if err != nil {
			return nil, err
		}
		if len(flatData) == 0 {
			er++
			continue
		}
		var mapLeaf trillian.MapLeaf
		err = proto.Unmarshal(flatData, &mapLeaf)
		if err != nil {
			return nil, err
		}
		mapLeaf.KeyHash = mapKeyHash
		ret = append(ret, mapLeaf)
		nr++
	}
	glog.Infof("%d rows, %d empty", nr, er)
	return ret, nil
}

func (m *mapTX) LatestSignedMapRoot() (trillian.SignedMapRoot, error) {
	var timestamp, mapRevision int64
	var rootHash, rootSignatureBytes []byte
	var rootSignature trillian.DigitallySigned
	var mapperMetaBytes []byte
	var mapperMeta *trillian.MapperMetadata

	stmt, err := m.tx.Prepare(selectLatestSignedMapRootSQL)
	if err != nil {
		return trillian.SignedMapRoot{}, err
	}
	defer stmt.Close()

	err = stmt.QueryRow(m.ms.mapID.TreeID).Scan(
		&timestamp, &rootHash, &mapRevision, &rootSignatureBytes, &mapperMetaBytes)

	// It's possible there are no roots for this tree yet
	if err == sql.ErrNoRows {
		return trillian.SignedMapRoot{}, nil
	}

	err = proto.Unmarshal(rootSignatureBytes, &rootSignature)
	if err != nil {
		glog.Warningf("Failed to unmarshal root signature: %v", err)
		return trillian.SignedMapRoot{}, err
	}

	if mapperMetaBytes != nil && len(mapperMetaBytes) != 0 {
		mapperMeta = &trillian.MapperMetadata{}
		if err := proto.Unmarshal(mapperMetaBytes, mapperMeta); err != nil {
			glog.Warningf("Failed to unmarshal Metadata; %v", err)
			return trillian.SignedMapRoot{}, err
		}
	}

	ret := trillian.SignedMapRoot{
		RootHash:       rootHash,
		TimestampNanos: timestamp,
		MapRevision:    mapRevision,
		Signature:      &rootSignature,
		MapId:          m.ms.mapID.MapID,
		Metadata:       mapperMeta,
	}

	return ret, nil
}

func (m *mapTX) StoreSignedMapRoot(root trillian.SignedMapRoot) error {
	signatureBytes, err := proto.Marshal(root.Signature)
	if err != nil {
		glog.Warningf("Failed to marshal root signature: %v %v", root.Signature, err)
		return err
	}

	var mapperMetaBytes []byte

	if root.Metadata != nil {
		mapperMetaBytes, err = proto.Marshal(root.Metadata)
		if err != nil {
			glog.Warning("Failed to marshal MetaData: %v %v", root.Metadata, err)
			return err
		}
	}

	stmt, err := m.tx.Prepare(insertMapHeadSQL)
	if err != nil {
		return err
	}
	defer stmt.Close()

	// TODO(al): store transactionLogHead too
	res, err := stmt.Exec(m.ms.mapID.TreeID, root.TimestampNanos, root.RootHash, root.MapRevision, signatureBytes, mapperMetaBytes)

	if err != nil {
		glog.Warningf("Failed to store signed map root: %s", err)
	}

	return checkResultOkAndRowCountIs(res, err, 1)
}
