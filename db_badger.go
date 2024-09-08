// +build badger

package main

import (
    "github.com/fiatjaf/eventstore/badger"
)

func getDB() badger.BadgerBackend {
    return badger.BadgerBackend{
        Path: getEnv("DB_PATH"),
    }
}

