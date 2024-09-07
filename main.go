package main

import (
	"context"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/cespare/xxhash"
	"github.com/fiatjaf/eventstore/lmdb"
	"github.com/fiatjaf/khatru"
	"github.com/greatroar/blobloom"
	"github.com/joho/godotenv"
	"github.com/nbd-wtf/go-nostr"
)

type Config struct {
	RelayName        string
	RelayPubkey      string
	RelayDescription string
	DBPath           string
	RelayURL         string
	IndexPath        string
	StaticPath       string
}

var archivePool *nostr.SimplePool
var fetchingPool *nostr.SimplePool

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
	appendPubkey(config.RelayPubkey)

	db := lmdb.LMDBBackend{
		Path: getEnv("DB_PATH"),
	}
	if err := db.Init(); err != nil {
		panic(err)
	}

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

	mu.Lock()
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
	}
	mu.Unlock()

	go refreshTrustNetwork(relay, ctx)
	go archiveTrustedNotes(relay, ctx)

	mux := relay.Router()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		tmpl := template.Must(template.ParseFiles(os.Getenv("INDEX_PATH")))
		data := struct {
			RelayName        string
			RelayPubkey      string
			RelayDescription string
			RelayURL         string
		}{
			RelayName:        config.RelayName,
			RelayPubkey:      config.RelayPubkey,
			RelayDescription: config.RelayDescription,
			RelayURL:         config.RelayURL,
		}
		err := tmpl.Execute(w, data)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})

	mux.Handle("/favicon.ico", http.StripPrefix("/", http.FileServer(http.Dir(config.StaticPath))))

	fmt.Println("running on :3334")
	err := http.ListenAndServe(":3334", relay)
	if err != nil {
		log.Fatal(err)
	}
}

func LoadConfig() Config {
	godotenv.Load(".env")

	config := Config{
		RelayName:        getEnv("RELAY_NAME"),
		RelayPubkey:      getEnv("RELAY_PUBKEY"),
		RelayDescription: getEnv("RELAY_DESCRIPTION"),
		DBPath:           getEnv("DB_PATH"),
		RelayURL:         getEnv("RELAY_URL"),
		IndexPath:        getEnv("INDEX_PATH"),
		StaticPath:       getEnv("STATIC_PATH"),
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

func refreshTrustNetwork(relay *khatru.Relay, ctx context.Context) []string {
	fetchingPool = nostr.NewSimplePool(ctx)

	// Function to refresh the trust network
	runTrustNetworkRefresh := func() {
		timeoutCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()

		filters := []nostr.Filter{{
			Authors: []string{config.RelayPubkey},
			Kinds:   []int{nostr.KindContactList},
		}}

		for ev := range fetchingPool.SubManyEose(timeoutCtx, relays, filters) {
			for _, contact := range ev.Event.Tags.GetAll([]string{"p"}) {
				appendPubkey(contact[1])
			}
		}

		chunks := make([][]string, 0)
		for i := 0; i < len(trustNetwork); i += 100 {
			end := i + 100
			if end > len(trustNetwork) {
				end = len(trustNetwork)
			}
			chunks = append(chunks, trustNetwork[i:end])
		}

		for _, chunk := range chunks {
			threeTimeoutCtx, tenCancel := context.WithTimeout(ctx, 10*time.Second)
			defer tenCancel()

			filters = []nostr.Filter{{
				Authors: chunk,
				Kinds:   []int{nostr.KindContactList},
			}}

			for ev := range fetchingPool.SubManyEose(threeTimeoutCtx, relays, filters) {
				for _, contact := range ev.Event.Tags.GetAll([]string{"p"}) {
					if len(contact) > 1 {
						appendPubkey(contact[1])
					} else {
						fmt.Println("Skipping malformed tag: ", contact)
					}
				}
			}
		}

		getTrustNetworkProfileMetadata(relay, ctx)
	}

	runTrustNetworkRefresh()

	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		runTrustNetworkRefresh()
	}

	return trustNetwork
}

func getTrustNetworkProfileMetadata(relay *khatru.Relay, ctx context.Context) {
	chunks := make([][]string, 0)
	for i := 0; i < len(trustNetwork); i += 100 {
		end := i + 100
		if end > len(trustNetwork) {
			end = len(trustNetwork)
		}
		chunks = append(chunks, trustNetwork[i:end])
	}

	for _, chunk := range chunks {
		if len(chunk) == 0 {
			continue
		}

		timeoutCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		filters := []nostr.Filter{{
			Authors: chunk,
			Kinds:   []int{nostr.KindProfileMetadata},
		}}

		for ev := range fetchingPool.SubManyEose(timeoutCtx, relays, filters) {
			relay.AddEvent(ctx, ev.Event)
		}
		cancel()
	}
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

func archiveTrustedNotes(relay *khatru.Relay, ctx context.Context) {
	ticker := time.NewTicker(1 * time.Minute)
	archivePool = nostr.NewSimplePool(ctx)
	defer ticker.Stop()

	for range ticker.C {
		timeout, cancel := context.WithTimeout(ctx, 50*time.Second)
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
				nostr.KindTextNote,
			},
		}}

		nKeys := uint64(len(trustNetwork))
		fmt.Println("trust network size:", nKeys)
		bloomFilter := blobloom.NewOptimized(blobloom.Config{
			Capacity: nKeys,
			FPRate:   1e-4,
		})
		for _, trustedPubkey := range trustNetwork {
			bloomFilter.Add(xxhash.Sum64([]byte(trustedPubkey)))
		}

		for ev := range archivePool.SubMany(timeout, relays, filters) {

			if bloomFilter.Has(xxhash.Sum64([]byte(ev.Event.PubKey))) {
				if len(ev.Event.Tags) > 2000 {
					continue
				}
				relay.AddEvent(ctx, ev.Event)

			}
		}
		cancel()
	}
}
