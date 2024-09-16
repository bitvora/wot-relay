package main

import (
	"context"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/fiatjaf/eventstore"
	"github.com/fiatjaf/khatru"
	"github.com/fiatjaf/khatru/policies"
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
	MinimumFollowers int
	TrustDepth       int
}

var pool *nostr.SimplePool
var wdb nostr.RelayStore
var relays []string
var config Config

var trustNetwork [][]string

var seedRelays []string
var booted bool
var trustNetworkMap map[string]bool
var pubkeyFollowerCount = make(map[string]int)
var trustedNotes uint64
var untrustedNotes uint64

func main() {
	nostr.InfoLogger = log.New(io.Discard, "", 0)
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
8888P   Y8888 Y88..88P 888          888  T88b  Y8b.    888 888  888 Y88b 888 
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
	trustNetworkMap = make(map[string]bool)

	relay.Info.Name = config.RelayName
	relay.Info.PubKey = config.RelayPubkey
	relay.Info.Description = config.RelayDescription
	trustNetwork = make([][]string, config.TrustDepth)
	appendPubkey(config.RelayPubkey)

	db := getDB()
	if err := db.Init(); err != nil {
		panic(err)
	}
	wdb = eventstore.RelayWrapper{Store: &db}

	relay.RejectEvent = append(relay.RejectEvent,
		policies.RejectEventsWithBase64Media,
		policies.EventIPRateLimiter(5, time.Minute*1, 30),
	)

	relay.RejectFilter = append(relay.RejectFilter,
		policies.NoEmptyFilters,
		policies.NoComplexFilters,
	)

	relay.RejectConnection = append(relay.RejectConnection,
		policies.ConnectionRateLimiter(10, time.Minute*2, 30),
	)

	relay.StoreEvent = append(relay.StoreEvent, db.SaveEvent)
	relay.QueryEvents = append(relay.QueryEvents, db.QueryEvents)
	relay.DeleteEvent = append(relay.DeleteEvent, db.DeleteEvent)
	relay.RejectEvent = append(relay.RejectEvent, func(ctx context.Context, event *nostr.Event) (bool, string) {
		for _, pubkeys := range trustNetwork {
			for _, pk := range pubkeys {
				if pk == event.PubKey {
					return false, ""
				}
			}
		}
		return true, "you are not in the web of trust"
	})

	seedRelays = []string{
		"wss://nos.lol",
		"wss://nostr.mom",
		"wss://purplepag.es",
		"wss://purplerelay.com",
		"wss://relay.damus.io",
		"wss://relay.nostr.band",
		"wss://relay.snort.social",
		"wss://relayable.org",
		"wss://relay.primal.net",
		"wss://relay.nostr.bg",
		"wss://no.str.cr",
		"wss://nostr21.com",
		"wss://nostrue.com",
		"wss://relay.siamstr.com",
	}

	go refreshTrustNetwork(ctx, relay)

	mux := relay.Router()
	static := http.FileServer(http.Dir(config.StaticPath))

	mux.Handle("GET /static/", http.StripPrefix("/static/", static))
	mux.Handle("GET /favicon.ico", http.StripPrefix("/", static))

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

	log.Println("🎉 relay running on port :3334")
	err := http.ListenAndServe(":3334", relay)
	if err != nil {
		log.Fatal(err)
	}
}

func LoadConfig() Config {
	godotenv.Load(".env")

	if os.Getenv("REFRESH_INTERVAL_HOURS") == "" {
		os.Setenv("REFRESH_INTERVAL_HOURS", "3")
	}

	refreshInterval, _ := strconv.Atoi(os.Getenv("REFRESH_INTERVAL_HOURS"))
	log.Println("🔄 refresh interval set to", refreshInterval, "hours")

	if os.Getenv("MINIMUM_FOLLOWERS") == "" {
		os.Setenv("MINIMUM_FOLLOWERS", "1")
	}

	minimumFollowers, _ := strconv.Atoi(os.Getenv("MINIMUM_FOLLOWERS"))

	if os.Getenv("TRUST_DEPTH") == "" {
		os.Setenv("TRUST_DEPTH", "1")
	}

	trustDepth, _ := strconv.Atoi(os.Getenv("TRUST_DEPTH"))

	config := Config{
		RelayName:        getEnv("RELAY_NAME"),
		RelayPubkey:      getEnv("RELAY_PUBKEY"),
		RelayDescription: getEnv("RELAY_DESCRIPTION"),
		DBPath:           getEnv("DB_PATH"),
		RelayURL:         getEnv("RELAY_URL"),
		IndexPath:        getEnv("INDEX_PATH"),
		StaticPath:       getEnv("STATIC_PATH"),
		RefreshInterval:  refreshInterval,
		MinimumFollowers: minimumFollowers,
		TrustDepth:       trustDepth,
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
	trustNetworkMap = make(map[string]bool)

	log.Println("🌐 updating trust network map")
	for _, depthKeys := range trustNetwork {
		for _, pubkey := range depthKeys {
			if pubkeyFollowerCount[pubkey] >= config.MinimumFollowers {
				trustNetworkMap[pubkey] = true
			}
		}
	}

	totalKeys := 0
	for _, depthKeys := range trustNetwork {
		totalKeys += len(depthKeys)
	}
	log.Println("🌐 trust network map updated with", totalKeys, "total keys")
}

func refreshProfiles(ctx context.Context) {
	for depth, pubkeysAtDepth := range trustNetwork {
		if len(pubkeysAtDepth) == 0 {
			log.Printf("No pubkeys to refresh at depth %d", depth+1)
			continue
		}

		for i := 0; i < len(pubkeysAtDepth); i += 200 {
			timeout, cancel := context.WithTimeout(ctx, 4*time.Second)
			defer cancel()

			end := i + 200
			if end > len(pubkeysAtDepth) {
				end = len(pubkeysAtDepth)
			}

			validPubkeys := []string{}
			for _, pubkey := range pubkeysAtDepth[i:end] {
				if isValidPubkey(pubkey) {
					validPubkeys = append(validPubkeys, pubkey)
				} else {
					//log.Printf("Skipping invalid pubkey at depth %d: %s", depth+1, pubkey)
				}
			}

			if len(validPubkeys) == 0 {
				log.Printf("No valid pubkeys at depth %d in batch %d-%d", depth+1, i, end)
				continue
			}

			filters := []nostr.Filter{{
				Authors: validPubkeys,
				Kinds:   []int{nostr.KindProfileMetadata},
			}}

			for ev := range pool.SubManyEose(timeout, seedRelays, filters) {
				wdb.Publish(ctx, *ev.Event)
			}
		}

		log.Printf("👤 profiles refreshed at depth %d: %d pubkeys", depth+1, len(pubkeysAtDepth))
	}
}

func fetchFollowers(ctx context.Context, pubkey string, depth int) {
	if depth > config.TrustDepth {
		return
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()

	filters := []nostr.Filter{{
		Authors: []string{pubkey},
		Kinds:   []int{nostr.KindContactList},
	}}

	for ev := range pool.SubManyEose(timeoutCtx, seedRelays, filters) {
		for _, contact := range ev.Event.Tags.GetAll([]string{"p"}) {
			followerPubkey := contact[1]

			if !pubkeyExistsAtDepth(followerPubkey, depth) {
				trustNetwork[depth-1] = append(trustNetwork[depth-1], followerPubkey)
			}
		}
		log.Printf("🌐 trust network updated at depth %d with %d keys", depth, len(trustNetwork[depth-1]))
	}
}

func pubkeyExistsAtDepth(pubkey string, depth int) bool {
	for _, pk := range trustNetwork[depth-1] {
		if pk == pubkey {
			return true
		}
	}
	return false
}

func refreshTrustNetwork(ctx context.Context, relay *khatru.Relay) {
	runTrustNetworkRefresh := func() {
		log.Println("🔍 fetching owner's follows")
		trustNetwork = make([][]string, config.TrustDepth)
		trustNetwork[0] = []string{config.RelayPubkey}

		fetchFollowers(ctx, config.RelayPubkey, 1)

		for depth := 2; depth <= config.TrustDepth; depth++ {
			log.Printf("🔍 fetching depth %d follows", depth)
			trustNetwork[depth-1] = []string{}
			previousDepthPubkeys := trustNetwork[depth-2]

			for _, pubkey := range previousDepthPubkeys {
				log.Printf("👤 fetching followers for pubkey: %s at depth: %d", pubkey, depth)
				fetchFollowers(ctx, pubkey, depth)
			}
		}

		updateTrustNetworkFilter()
		archiveTrustedNotes(ctx, relay)

		for i, depthKeys := range trustNetwork {
			log.Printf("Depth %d: %d pubkeys", i+1, len(depthKeys))
		}
	}

	for {
		runTrustNetworkRefresh()
	}
}

func appendRelay(relay string) {
	for _, r := range relays {
		if r == relay {
			return
		}
	}
	relays = append(relays, relay)
}

func isValidPubkey(pubkey string) bool {
    if len(pubkey) != 64 {
        return false
    }

    for _, c := range pubkey {
        if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
            return false
        }
    }

    return true
}

func appendPubkey(pubkey string) {
    if _, exists := trustNetworkMap[pubkey]; exists {
        return
    }

    if !isValidPubkey(pubkey) {
        log.Printf("Invalid pubkey: %s", pubkey)
        return
    }

    trustNetwork[0] = append(trustNetwork[0], pubkey)
    trustNetworkMap[pubkey] = true
}

func archiveTrustedNotes(ctx context.Context, relay *khatru.Relay) {
	timeout, cancel := context.WithTimeout(ctx, time.Duration(config.RefreshInterval)*time.Hour)
	defer cancel()
	go refreshProfiles(ctx)

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

	for ev := range pool.SubMany(timeout, seedRelays, filters) {
		go archiveEvent(ctx, relay, *ev.Event)
	}

	log.Println("📦 archived", trustedNotes, "trusted notes and discarded ", untrustedNotes, "untrusted notes")
}

func archiveEvent(ctx context.Context, relay *khatru.Relay, ev nostr.Event) {
	if trustNetworkMap[ev.PubKey] {
		wdb.Publish(ctx, ev)
		relay.BroadcastEvent(&ev)
		trustedNotes++
	} else {
		untrustedNotes++
	}
}
