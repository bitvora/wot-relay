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

var pool *nostr.SimplePool
var relays []string
var config Config
var trustNetwork []string
var mu sync.Mutex
var trustNetworkFilter *blobloom.Filter
var trustNetworkFilterMu sync.Mutex
var seedRelays []string

func main() {
	green := "\033[32m"
	reset := "\033[0m"

	art := `                               
 __      __      ___.             _____  ___________                      __   
/  \    /  \ ____\_ |__     _____/ ____\ \__    ___/______ __ __  _______/  |_ 
\   \/\/   // __ \| __ \   /  _ \   __\    |    |  \_  __ \  |  \/  ___/\   __\
 \        /\  ___/| \_\ \ (  <_> )  |      |    |   |  | \/  |  /\___ \  |  |  
  \__/\  /  \___  >___  /  \____/|__|      |____|   |__|  |____//____  > |__|  
       \/       \/    \/                                             \/            
		`

	fmt.Println(green + art + reset)
	log.Println("🚀 booting up web of trust relay")
	relay := khatru.NewRelay()
	ctx := context.Background()
	pool = nostr.NewSimplePool(ctx)
	config = LoadConfig()

	relay.Info.Name = config.RelayName
	relay.Info.PubKey = config.RelayPubkey
	relay.Info.Description = config.RelayDescription
	appendPubkey(config.RelayPubkey)

	db := getDB()
	if err := db.Init(); err != nil {
		panic(err)
	}

	relay.StoreEvent = append(relay.StoreEvent, db.SaveEvent)
	relay.QueryEvents = append(relay.QueryEvents, db.QueryEvents)
	relay.DeleteEvent = append(relay.DeleteEvent, db.DeleteEvent)
	relay.RejectEvent = append(relay.RejectEvent, func(ctx context.Context, event *nostr.Event) (bool, string) {
		for _, pk := range trustNetwork {
			if pk == event.PubKey {
				return false, ""
			}
		}
		return true, "you are not in the web of trust"
	})

	mu.Lock()
	seedRelays = []string{
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

	log.Println("🎉 relay running on port :3334")
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

func updateTrustNetworkFilter() {
	trustNetworkFilterMu.Lock()
	defer trustNetworkFilterMu.Unlock()

	nKeys := uint64(len(trustNetwork))
	log.Println("🌐 updating trust network filter with", nKeys, "keys")
	trustNetworkFilter = blobloom.NewOptimized(blobloom.Config{
		Capacity: nKeys,
		FPRate:   1e-4,
	})
	for _, trustedPubkey := range trustNetwork {
		trustNetworkFilter.Add(xxhash.Sum64([]byte(trustedPubkey)))
	}
}

func refreshTrustNetwork(relay *khatru.Relay, ctx context.Context) []string {

	runTrustNetworkRefresh := func() {
		timeoutCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()

		filters := []nostr.Filter{{
			Authors: []string{config.RelayPubkey},
			Kinds:   []int{nostr.KindContactList},
		}}

		log.Println("🔍 fetching owner's follows")
		for ev := range pool.SubManyEose(timeoutCtx, seedRelays, filters) {
			for _, contact := range ev.Event.Tags.GetAll([]string{"p"}) {
				appendPubkey(contact[1])
			}
		}

		follows := make([]string, len(trustNetwork))
		copy(follows, trustNetwork)

		log.Println("🌐 building web of trust graph")
		for i := 0; i < len(follows); i += 200 {
			timeout, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()

			end := i + 200
			if end > len(follows) {
				end = len(follows)
			}

			filters = []nostr.Filter{{
				Authors: follows[i:end],
				Kinds:   []int{nostr.KindContactList, nostr.KindRelayListMetadata, nostr.KindProfileMetadata},
			}}

			for ev := range pool.SubManyEose(timeout, seedRelays, filters) {
				for _, contact := range ev.Event.Tags.GetAll([]string{"p"}) {
					if len(contact) > 1 {
						appendPubkey(contact[1])
					}
				}

				for _, relay := range ev.Event.Tags.GetAll([]string{"r"}) {
					appendRelay(relay[1])
				}

				if ev.Event.Kind == nostr.KindProfileMetadata {
					relay.AddEvent(ctx, ev.Event)
				}
			}

		}
		log.Println("🫂  network size:", len(trustNetwork))
		log.Println("🔗 relays discovered:", len(relays))
	}

	runTrustNetworkRefresh()
	updateTrustNetworkFilter()

	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()

	for range ticker.C {
		runTrustNetworkRefresh()
		updateTrustNetworkFilter()
	}

	return trustNetwork
}

func appendRelay(relay string) {
	mu.Lock()
	defer mu.Unlock()

	for _, r := range relays {
		if r == relay {
			return
		}
	}
	relays = append(relays, relay)
}

func appendPubkey(pubkey string) {
	mu.Lock()
	defer mu.Unlock()

	for _, pk := range trustNetwork {
		if pk == pubkey {
			return
		}
	}

	if len(pubkey) != 64 {
		return
	}

	trustNetwork = append(trustNetwork, pubkey)
}

func archiveTrustedNotes(relay *khatru.Relay, ctx context.Context) {
	log.Println("⏳ waiting for trust network to be populated")
	time.Sleep(1 * time.Minute)
	timeout, cancel := context.WithTimeout(ctx, 24*time.Hour)
	defer cancel()

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

	log.Println("📦 archiving trusted notes...")
	var i int64
	trustNetworkFilterMu.Lock()
	for ev := range pool.SubMany(timeout, seedRelays, filters) {
		if trustNetworkFilter.Has(xxhash.Sum64([]byte(ev.Event.PubKey))) {
			if len(ev.Event.Tags) > 2000 {
				continue
			}
			relay.AddEvent(ctx, ev.Event)
			i++
		}
	}
	trustNetworkFilterMu.Unlock()
	fmt.Println("📦 archived", i, "trusted notes")
}
