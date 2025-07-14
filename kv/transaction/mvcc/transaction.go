package mvcc

import (
	"encoding/binary"

	"github.com/pingcap-incubator/tinykv/kv/storage"
	"github.com/pingcap-incubator/tinykv/kv/util/codec"
	"github.com/pingcap-incubator/tinykv/kv/util/engine_util"
	"github.com/pingcap-incubator/tinykv/proto/pkg/kvrpcpb"
	"github.com/pingcap-incubator/tinykv/scheduler/pkg/tsoutil"

	"bytes"
)

// // KeyError is a wrapper type so we can implement the `error` interface.
type KeyError struct {
	kvrpcpb.KeyError
}

func (ke *KeyError) Error() string {
	return ke.String()
}

// MvccTxn groups together writes as part of a single transaction. It also provides an abstraction over low-level
// storage, lowering the concepts of timestamps, writes, and locks into plain keys and values.
type MvccTxn struct {
	StartTS uint64
	Reader  storage.StorageReader
	writes  []storage.Modify
}

func NewMvccTxn(reader storage.StorageReader, startTs uint64) *MvccTxn {
	return &MvccTxn{
		Reader:  reader,
		StartTS: startTs,
	}
}

// Writes returns all changes added to this transaction.
func (txn *MvccTxn) Writes() []storage.Modify {
	return txn.writes
}

// PutWrite records a write at key and ts.
func (txn *MvccTxn) PutWrite(key []byte, ts uint64, write *Write) {
	// Your Code Here (4A).
	//存储位置：Write 列族 (CF)
	//在txn.writes中添加一个CfWrite类型的数据
	cf := engine_util.CfWrite
	newkey := EncodeKey(key, ts) //用户键和时间戳组合成一个编码键
	value := write.ToBytes()
	txn.writes = append(txn.writes, storage.Modify{
		Data: storage.Put{
			Cf:    cf,
			Key:   newkey,
			Value: value,
		},
	})
}

// GetLock returns a lock if key is locked. It will return (nil, nil) if there is no lock on key, and (nil, err)
// if an error occurs during lookup.
func (txn *MvccTxn) GetLock(key []byte) (*Lock, error) {
	// Your Code Here (4A).
	//查找key上的锁
	reader := txn.Reader
	//GetCF(cf string, key []byte) ([]byte, error)
	val, err := reader.GetCF(engine_util.CfLock, key)
	if err != nil {
		return nil, err
	}
	if val == nil {
		return nil, nil
	}
	//func ParseLock(input []byte) (*Lock, error)
	lock, err := ParseLock(val)
	if err != nil {
		return nil, err
	}
	return lock, err
}

// PutLock adds a key/lock to this transaction.
func (txn *MvccTxn) PutLock(key []byte, lock *Lock) {
	// Your Code Here (4A).
	//在txn.writes中添加一个CfLock类型的数据
	cf := engine_util.CfLock
	val := lock.ToBytes()
	txn.writes = append(txn.writes, storage.Modify{
		Data: storage.Put{
			Cf:    cf,
			Key:   key,
			Value: val,
		},
	})
}

// DeleteLock adds a delete lock to this transaction.
func (txn *MvccTxn) DeleteLock(key []byte) {
	// Your Code Here (4A).
	//在txn.writes中删除一个CfLock类型的数据
	cf := engine_util.CfLock
	txn.writes = append(txn.writes, storage.Modify{
		Data: storage.Delete{
			Cf:  cf,
			Key: key,
		},
	})
}

// GetValue finds the value for key, valid at the start timestamp of this transaction.
// I.e., the most recent value committed before the start of this transaction.
func (txn *MvccTxn) GetValue(key []byte) ([]byte, error) {
	// Your Code Here (4A).
	//查找commit_ts<=txn.start_ts的write，根据write的start_ts寻找default对应的value
	iter := txn.Reader.IterCF(engine_util.CfWrite)
	defer iter.Close()
	iter.Seek(EncodeKey(key, txn.StartTS)) //查找commit_ts<=txn.start_ts的最新的write
	if !iter.Valid() {                     //判断是否有效！
		return nil, nil
	}
	item := iter.Item()
	//判断找到的 key 是不是目标key
	if !bytes.Equal(DecodeUserKey(item.KeyCopy(nil)), key) {
		return nil, nil
	}
	val, err := item.ValueCopy(nil)
	if err != nil {
		return nil, err
	}
	write, err := ParseWrite(val) //字符串转换为write格式
	if err != nil {
		return nil, err
	}
	//判断write.Kind是否为WriteKindPut类型
	if write.Kind != WriteKindPut {
		return nil, nil
	}
	startts := write.StartTS
	value, err := txn.Reader.GetCF(engine_util.CfDefault, EncodeKey(key, startts))
	if err != nil {
		return nil, err
	}
	return value, nil
}

// PutValue adds a key/value write to this transaction.
func (txn *MvccTxn) PutValue(key []byte, value []byte) {
	// Your Code Here (4A).
	//存储位置：Default 列族 (CF)
	//在txn.writes中添加一个CfDefault类型的数据
	cf := engine_util.CfDefault
	newkey := EncodeKey(key, txn.StartTS)
	txn.writes = append(txn.writes, storage.Modify{
		Data: storage.Put{
			Cf:    cf,
			Key:   newkey,
			Value: value,
		},
	})
}

// DeleteValue removes a key/value pair in this transaction.
func (txn *MvccTxn) DeleteValue(key []byte) {
	// Your Code Here (4A).
	//在txn.writes中添加删除一个CfDefault类型数据的Modify
	cf := engine_util.CfDefault
	newkey := EncodeKey(key, txn.StartTS)
	txn.writes = append(txn.writes, storage.Modify{
		Data: storage.Delete{
			Cf:  cf,
			Key: newkey,
		},
	})
}

// CurrentWrite searches for a write with this transaction's start timestamp. It returns a Write from the DB and that
// write's commit timestamp, or an error.
func (txn *MvccTxn) CurrentWrite(key []byte) (*Write, uint64, error) {
	// Your Code Here (4A).
	//查找与当前事务相关的记录（Write.StartTS == txn.StartTS）
	iter := txn.Reader.IterCF(engine_util.CfWrite)
	defer iter.Close()
	iter.Seek(EncodeKey(key, ^uint64(0))) //按ts从大到小存放
	for ; iter.Valid(); iter.Next() {
		item := iter.Item()
		//判断是否为目标key
		userkey := DecodeUserKey(item.KeyCopy(nil))
		if !bytes.Equal(userkey, key) {
			return nil, 0, nil
		}
		val, err := item.ValueCopy(nil)
		if err != nil {
			return nil, 0, err
		}
		write, err := ParseWrite(val)
		if err != nil {
			return nil, 0, err
		}
		//判断是否为当前事务的StartTS
		if write.StartTS == txn.StartTS {
			return write, decodeTimestamp(item.KeyCopy(nil)), nil
		}
	}
	return nil, 0, nil
}

// MostRecentWrite finds the most recent write with the given key. It returns a Write from the DB and that
// write's commit timestamp, or an error.
func (txn *MvccTxn) MostRecentWrite(key []byte) (*Write, uint64, error) {
	// Your Code Here (4A).
	//查找所有事务中对指定键的最新写记录
	iter := txn.Reader.IterCF(engine_util.CfWrite)
	defer iter.Close()
	iter.Seek(EncodeKey(key, ^uint64(0))) //获得最新写记录
	if !iter.Valid() {
		return nil, 0, nil
	}
	item := iter.Item()
	//判断是否为目标key
	userkey := DecodeUserKey(item.KeyCopy(nil))
	if !bytes.Equal(userkey, key) {
		return nil, 0, nil
	}

	val, err := item.ValueCopy(nil)
	if err != nil {
		return nil, 0, err
	}
	write, err := ParseWrite(val)
	if err != nil {
		return nil, 0, err
	}

	return write, decodeTimestamp(item.KeyCopy(nil)), nil
}

// EncodeKey encodes a user key and appends an encoded timestamp to a key. Keys and timestamps are encoded so that
// timestamped keys are sorted first by key (ascending), then by timestamp (descending). The encoding is based on
// https://github.com/facebook/mysql-5.6/wiki/MyRocks-record-format#memcomparable-format.
func EncodeKey(key []byte, ts uint64) []byte {
	encodedKey := codec.EncodeBytes(key)
	newKey := append(encodedKey, make([]byte, 8)...)
	binary.BigEndian.PutUint64(newKey[len(encodedKey):], ^ts)
	return newKey
}

// DecodeUserKey takes a key + timestamp and returns the key part.
func DecodeUserKey(key []byte) []byte {
	_, userKey, err := codec.DecodeBytes(key)
	if err != nil {
		panic(err)
	}
	return userKey
}

// decodeTimestamp takes a key + timestamp and returns the timestamp part.
func decodeTimestamp(key []byte) uint64 {
	left, _, err := codec.DecodeBytes(key)
	if err != nil {
		panic(err)
	}
	return ^binary.BigEndian.Uint64(left)
}

// PhysicalTime returns the physical time part of the timestamp.

func PhysicalTime(ts uint64) uint64 {
	return ts >> tsoutil.PhysicalShiftBits
}
