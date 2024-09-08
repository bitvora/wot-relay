package main

import (
	"context"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"strconv"
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
	RefreshInterval  int
}

var pool *nostr.SimplePool
var relays []string
var config Config
var trustNetwork []string
var mu sync.Mutex
var trustNetworkFilter *blobloom.Filter
var trustNetworkFilterMu sync.Mutex
var seedRelays []string
var booted bool
var oneHopNetwork []string

func main() {
	booted = false
	green := "\033[32m"
	reset := "\033[0m"

	art := `
888       888      88888888888      8888888b.          888                   
888   o   888          888          888   Y88b         888                   
888  d8b  888          888          888    888         888                   
888 d888b 888  .d88b.  888          888   d88P .d88b.  888  8888b.  888  888 
888d88888b888 d88""88b 888          8888888P" d8P  Y8b 888     "88b 888  888 
88888P Y88888 888  888 888          888 T88b  88888888 888 .d888888 888  888 
8888P   Y8888 Y88..88P 888          888  T88b Y8b.     888 888  888 Y88b 888 
888P     Y888  "Y88P"  888          888   T88b "Y8888  888 "Y888888  "Y88888 
                                                                         888 
                                                                    Y8b d88P 
                                               powered by: khatru     "Y88P"  
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

	if os.Getenv("REFRESH_INTERVAL_HOURS") == "" {
		os.Setenv("REFRESH_INTERVAL_HOURS", "24")
	}

	refreshInterval, _ := strconv.Atoi(os.Getenv("REFRESH_INTERVAL_HOURS"))
	log.Println("🔄 refresh interval set to", refreshInterval, "hours")

	config := Config{
		RelayName:        getEnv("RELAY_NAME"),
		RelayPubkey:      getEnv("RELAY_PUBKEY"),
		RelayDescription: getEnv("RELAY_DESCRIPTION"),
		DBPath:           getEnv("DB_PATH"),
		RelayURL:         getEnv("RELAY_URL"),
		IndexPath:        getEnv("INDEX_PATH"),
		StaticPath:       getEnv("STATIC_PATH"),
		RefreshInterval:  refreshInterval,
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

func refreshTrustNetwork(relay *khatru.Relay, ctx context.Context) {

	runTrustNetworkRefresh := func() {
		timeoutCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
		defer cancel()

		filters := []nostr.Filter{{
			Authors: []string{config.RelayPubkey},
			Kinds:   []int{nostr.KindContactList},
		}}

		log.Println("🔍 fetching owner's follows")
		for ev := range pool.SubManyEose(timeoutCtx, seedRelays, filters) {
			for _, contact := range ev.Event.Tags.GetAll([]string{"p"}) {
				appendOneHopNetwork(contact[1])
			}
		}

		log.Println("🌐 building web of trust graph")
		for i := 0; i < len(oneHopNetwork); i += 100 {
			timeout, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()

			end := i + 100
			if end > len(oneHopNetwork) {
				end = len(oneHopNetwork)
			}

			filters = []nostr.Filter{{
				Authors: oneHopNetwork[i:end],
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

	for {
		runTrustNetworkRefresh()
		updateTrustNetworkFilter()
		archiveTrustedNotes(relay, ctx)
	}
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

func appendOneHopNetwork(pubkey string) {
	mu.Lock()
	defer mu.Unlock()

	for _, pk := range oneHopNetwork {
		if pk == pubkey {
			return
		}
	}

	if len(pubkey) != 64 {
		return
	}

	oneHopNetwork = append(oneHopNetwork, pubkey)
}

func archiveTrustedNotes(relay *khatru.Relay, ctx context.Context) {
	timeout, cancel := context.WithTimeout(ctx, time.Duration(config.RefreshInterval)*time.Hour)
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
	var trustedNotes uint64
	var untrustedNotes uint64
	trustNetworkFilterMu.Lock()
	for ev := range pool.SubMany(timeout, seedRelays, filters) {
		if trustNetworkFilter.Has(xxhash.Sum64([]byte(ev.Event.PubKey))) {
			if len(ev.Event.Tags) > 2000 {
				continue
			}
			relay.AddEvent(ctx, ev.Event)
			trustedNotes++
		} else {
			untrustedNotes++
		}
	}
	trustNetworkFilterMu.Unlock()
	log.Println("📦 archived", trustedNotes, "trusted notes and discarded", untrustedNotes, "untrusted notes")
}
