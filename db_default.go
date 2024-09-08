// +build !badger

package main

import (
    "github.com/fiatjaf/eventstore/lmdb"
)

func getDB() lmdb.LMDBBackend {
    return lmdb.LMDBBackend{
        Path: getEnv("DB_PATH"),
    }
}

