package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/fiatjaf/eventstore/lmdb"
	"github.com/fiatjaf/khatru"
	"github.com/joho/godotenv"
	"github.com/nbd-wtf/go-nostr"
)

type Config struct {
	RelayName        string
	RelayPubkey      string
	RelayDescription string
	DBPath           string
}

var pool *nostr.SimplePool
var relays []string
var config Config
var trustNetwork []string
var mu sync.Mutex

func main() {
	fmt.Println("starting")
	relay := khatru.NewRelay()
	ctx := context.Background()

	config = LoadConfig()

	relay.Info.Name = config.RelayName
	relay.Info.PubKey = config.RelayPubkey
	relay.Info.Description = config.RelayDescription
	trustNetwork = append(trustNetwork, config.RelayPubkey)

	db := lmdb.LMDBBackend{
		Path: getEnv("DB_PATH"),
	}
	if err := db.Init(); err != nil {
		panic(err)
	}

	go refreshTrustNetwork()
	go archiveTrustedNotes(relay)

	relay.StoreEvent = append(relay.StoreEvent, db.SaveEvent)
	relay.QueryEvents = append(relay.QueryEvents, db.QueryEvents)
	relay.RejectEvent = append(relay.RejectEvent, func(ctx context.Context, event *nostr.Event) (bool, string) {
		for _, pk := range trustNetwork {
			if pk == event.PubKey {
				return false, ""
			}
		}
		return true, "you are not in the web of trust"
	})

	relays = []string{
		"wss://nos.lol",
		"wss://nostr.mom",
		"wss://purplepag.es",
		"wss://purplerelay.com",
		"wss://relay.damus.io",
		"wss://relay.mostr.pub",
		"wss://relay.nos.social",
		"wss://relay.nostr.band",
		"wss://relay.snort.social",
		"wss://relayable.org",
		"wss://pyramid.fiatjaf.com",
		"wss://relay.primal.net",
		"wss://relay.nostr.bg",
		"wss://no.str.cr",
		"wss://nostr21.com",
		"wss://nostrue.com",
		"wss://relay.siamstr.com",
		"wss://nostrarchives.com",
	}

	pool = nostr.NewSimplePool(ctx)

	fmt.Println("running on :3334")
	http.ListenAndServe(":3334", relay)
}

func LoadConfig() Config {
	err := godotenv.Load(".env")
	if err != nil {
		log.Fatalf("Error loading .env file")
	}

	config := Config{
		RelayName:        getEnv("RELAY_NAME"),
		RelayPubkey:      getEnv("RELAY_PUBKEY"),
		RelayDescription: getEnv("RELAY_DESCRIPTION"),
		DBPath:           getEnv("DB_PATH"),
	}

	return config
}

func getEnv(key string) string {
	value, exists := os.LookupEnv(key)
	if !exists {
		log.Fatalf("Environment variable %s not set", key)
	}
	return value
}

func refreshTrustNetwork() []string {
	ctx := context.Background()
	ticker := time.NewTicker(1 * time.Minute)
	for range ticker.C {

		timeoutCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
		defer cancel()

		filters := []nostr.Filter{{
			Authors: []string{config.RelayPubkey},
			Kinds:   []int{nostr.KindContactList},
		}}

		for ev := range pool.SubManyEose(timeoutCtx, relays, filters) {
			for _, contact := range ev.Event.Tags.GetAll([]string{"p"}) {
				appendPubkey(contact[1])
			}
		}

		fmt.Println("trust network size:", len(trustNetwork))

		chunks := make([][]string, 0)
		for i := 0; i < len(trustNetwork); i += 100 {
			end := i + 100
			if end > len(trustNetwork) {
				end = len(trustNetwork)
			}
			chunks = append(chunks, trustNetwork[i:end])
		}

		for _, chunk := range chunks {
			threeTimeoutCtx, tenCancel := context.WithTimeout(ctx, 3*time.Second)
			defer tenCancel()

			filters = []nostr.Filter{{
				Authors: chunk,
				Kinds:   []int{nostr.KindContactList},
			}}

			for ev := range pool.SubManyEose(threeTimeoutCtx, relays, filters) {
				for _, contact := range ev.Event.Tags.GetAll([]string{"p"}) {
					if len(contact) > 1 {
						appendPubkey(contact[1])
					} else {
						fmt.Println("Skipping malformed tag: ", contact)
					}
				}
			}
		}

		fmt.Println("trust network size:", len(trustNetwork))
	}

	return trustNetwork
}

func appendPubkey(pubkey string) {
	mu.Lock()
	defer mu.Unlock()

	for _, pk := range trustNetwork {
		if pk == pubkey {
			return
		}
	}
	trustNetwork = append(trustNetwork, pubkey)
}

func archiveTrustedNotes(relay *khatru.Relay) {
	ctx := context.Background()
	filters := []nostr.Filter{{
		Kinds: []int{
			nostr.KindArticle,
			nostr.KindDeletion,
			nostr.KindContactList,
			nostr.KindEncryptedDirectMessage,
			nostr.KindMuteList,
			nostr.KindReaction,
			nostr.KindRelayListMetadata,
			nostr.KindRepost,
			nostr.KindZapRequest,
			nostr.KindZap,
		},
	}}

	for ev := range pool.SubMany(ctx, relays, filters) {
		for _, trustedPubkey := range trustNetwork {
			if ev.Event.PubKey == trustedPubkey {
				relay.AddEvent(ctx, ev.Event)
			}
		}
	}
}
