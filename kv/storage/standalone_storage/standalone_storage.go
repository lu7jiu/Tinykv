package standalone_storage

import (
	"github.com/pingcap-incubator/tinykv/kv/config"
	"github.com/pingcap-incubator/tinykv/kv/storage"
	"github.com/pingcap-incubator/tinykv/proto/pkg/kvrpcpb"
	"github.com/pingcap-incubator/tinykv/kv/util/engine_util"
	"github.com/Connor1996/badger"
)

// StandAloneStorage is an implementation of `Storage` for a single-node TinyKV instance. It does not
// communicate with other nodes and all data is stored locally.
type StandAloneStorage struct {
	// Your Data Here (1).
	kvDB     *badger.DB
	config   *config.Config
}

func NewStandAloneStorage(conf *config.Config) *StandAloneStorage {
	// Your Code Here (1).
	kv:=engine_util.CreateDB(conf.DBPath,conf.Raft)
	return &StandAloneStorage{kvDB:kv,config:conf}
}

func (s *StandAloneStorage) Start() error {
	// Your Code Here (1).
	return nil
}

func (s *StandAloneStorage) Stop() error {
	// Your Code Here (1).
	// 关闭数据库
	if err := s.kvDB.Close(); err != nil {
			return err
		}
	return nil
}

func (s *StandAloneStorage) Reader(ctx *kvrpcpb.Context) (storage.StorageReader, error) {
	// Your Code Here (1).
	// 创建新的只读事务
	txn := s.kvDB.NewTransaction(false)
	return NewStandAloneStorageReader(txn), nil
}

func (s *StandAloneStorage) Write(ctx *kvrpcpb.Context, batch []storage.Modify) error {
	// Your Code Here (1).
	wb:=new(engine_util.WriteBatch)
	for _,modify:=range batch {
		switch modify.Data.(type) {
		case storage.Put:	// 写入操作
			wb.SetCF(modify.Data.(storage.Put).Cf,modify.Data.(storage.Put).Key,modify.Data.(storage.Put).Value)
		case storage.Delete:	// 删除操作
			wb.DeleteCF(modify.Data.(storage.Delete).Cf,modify.Data.(storage.Delete).Key)
		}
	}
	return wb.WriteToDB(s.kvDB)
}

type StandAloneStorageReader struct{
	txn *badger.Txn
}

func NewStandAloneStorageReader(newtxn *badger.Txn) *StandAloneStorageReader {
	return &StandAloneStorageReader{txn:newtxn}
}

func (sr *StandAloneStorageReader) GetCF(cf string, key []byte) ([]byte, error) {
	val,err:=engine_util.GetCFFromTxn(sr.txn,cf,key)	//获取指定列族的值
	if err==badger.ErrKeyNotFound{	//根据server_test.go可知此时error为nil
		return val,nil	
	}
	return val,nil
}

func (sr *StandAloneStorageReader) IterCF(cf string) engine_util.DBIterator {
	return engine_util.NewCFIterator(cf,sr.txn)	//创建列族感知的迭代器
}

func (sr *StandAloneStorageReader) Close() {
	sr.txn.Discard()	//关闭时丢弃事务
}

