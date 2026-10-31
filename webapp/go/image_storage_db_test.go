package isureal

import (
	"testing"
)

// 画像本体の列を圧縮しない設定（#119）を、実際の PostgreSQL で確かめる。ISUREAL_TEST_DSN を指定したときだけ実行する。
//
//	ISUREAL_TEST_DSN='postgres://isureal:isureal@127.0.0.1:55419/isureal?sslmode=disable' go test -run 'TestImageStorageDB' -v ./...

// nonExternalImageDataRelations は、image_objects とそのパーティションのうち、data 列が external でないものの名前を返す。
func nonExternalImageDataRelations(t *testing.T, env *lockTestEnv) []string {
	t.Helper()
	var names []string
	if err := env.db.Select(&names, `
  SELECT c.relname FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid
  WHERE a.attname = 'data' AND a.attstorage <> 'e'
    AND a.attrelid IN (
      SELECT 'image_objects'::regclass
      UNION ALL SELECT inhrelid FROM pg_inherits WHERE inhparent = 'image_objects'::regclass
    )
  ORDER BY c.relname
 `); err != nil {
		t.Fatal(err)
	}
	return names
}

// migrate.sql の後は、親と今あるパーティションの data 列がすべて external になっている。
func TestImageStorageDBExternal(t *testing.T) {
	env := newLockTestEnv(t)
	var partitions int
	if err := env.db.Get(&partitions, `SELECT count(*) FROM pg_inherits WHERE inhparent = 'image_objects'::regclass`); err != nil {
		t.Fatal(err)
	}
	if partitions == 0 {
		t.Fatal("image_objects has no partitions")
	}
	if names := nonExternalImageDataRelations(t, env); len(names) != 0 {
		t.Fatalf("data is not external in %v", names)
	}
}

// 後から PARTITION OF で作るパーティションも、親の external を引き継ぐ。作ったパーティションはロールバックで消す。
func TestImageStorageDBNewPartitionInherits(t *testing.T) {
	env := newLockTestEnv(t)
	tx, err := env.db.Beginx()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	tx.MustExec(`CREATE TABLE image_objects_test_119 PARTITION OF image_objects FOR VALUES FROM ('2099-01-01 00:00:00+00') TO ('2099-02-01 00:00:00+00')`)
	var storage string
	if err := tx.Get(&storage, `SELECT attstorage::text FROM pg_attribute WHERE attrelid = 'image_objects_test_119'::regclass AND attname = 'data'`); err != nil {
		t.Fatal(err)
	}
	if storage != "e" {
		t.Fatalf("attstorage of a new partition = %q, want e", storage)
	}
}
