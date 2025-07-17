package mvcc

import (
	"bytes"

	"github.com/pingcap-incubator/tinykv/kv/util/engine_util"
)

// Scanner is used for reading multiple sequential key/value pairs from the storage layer. It is aware of the implementation
// of the storage layer and returns results suitable for users.
// Invariant: either the scanner is finished and cannot be used, or it is ready to return a value immediately.
type Scanner struct {
	// Your Data Here (4C).
	nextkey []byte
	mvcctxn *MvccTxn
	iter    engine_util.DBIterator
}

// NewScanner creates a new scanner ready to read from the snapshot in txn.
func NewScanner(startKey []byte, txn *MvccTxn) *Scanner {
	// Your Code Here (4C).
	newiter := txn.Reader.IterCF(engine_util.CfWrite)
	return &Scanner{
		nextkey: startKey,
		mvcctxn: txn,
		iter:    newiter,
	}
}

func (scan *Scanner) Close() {
	// Your Code Here (4C).
	scan.iter.Close()
}

// Next returns the next key/value pair from the scanner. If the scanner is exhausted, then it will return `nil, nil, nil`.
func (scan *Scanner) Next() ([]byte, []byte, error) {
	// Your Code Here (4C).
	if !scan.iter.Valid() {
		return nil, nil, nil
	}
	key := scan.nextkey
	//若key在txn.startts之前没有记录，则nextkey可能会指向无记录的key，需修改为实际的nextkey!!!否则读取数据小于预期数量
	scan.iter.Seek(EncodeKey(key, scan.mvcctxn.StartTS))
	if !scan.iter.Valid() {
		return nil, nil, nil
	}
	item := scan.iter.Item()
	userkey := DecodeUserKey(item.KeyCopy(nil))
	if !bytes.Equal(userkey, key) {
		scan.nextkey = userkey
		return scan.Next()
	}
	//更新nextkey(在所有记录中查找key不同的为新的nextkey，可能读取到的key在本事务中不可见)
	for {
		scan.iter.Next()
		if !scan.iter.Valid() {
			break
		}
		item := scan.iter.Item()
		userkey := DecodeUserKey(item.KeyCopy(nil))
		if !bytes.Equal(userkey, key) {
			scan.nextkey = userkey
			break
		}
	}
	val, err := scan.mvcctxn.GetValue(key)
	if err != nil {
		return key, nil, err
	}
	return key, val, nil
}
