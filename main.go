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
}

var (
	pool                *nostr.SimplePool
	wdb                 nostr.RelayStore
	relays              []string
	config              Config
	trustNetwork        []string
	seedRelays          []string
	booted              bool
	oneHopNetwork       []string
	trustNetworkMap     map[string]bool
	pubkeyFollowerCount = make(map[string]int)
)

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
	wdb = eventstore.RelayWrapper{Store: &db}

	relay.RejectEvent = append(relay.RejectEvent,
		policies.RejectEventsWithBase64Media,
		policies.EventIPRateLimiter(5, time.Minute*2, 30),
	)

	relay.RejectFilter = append(relay.RejectFilter,
		policies.NoEmptyFilters,
		policies.NoComplexFilters,
		// policies.FilterIPRateLimiter(50, time.Minute, 250),
	)

	relay.RejectConnection = append(relay.RejectConnection,
		policies.ConnectionRateLimiter(10, time.Minute*1, 30),
	)

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

	go refreshTrustNetwork(ctx)

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
	for pubkey, count := range pubkeyFollowerCount {
		if count >= config.MinimumFollowers {
			trustNetworkMap[pubkey] = true
			appendPubkey(pubkey)
		}
	}

	log.Println("🌐 trust network map updated with", len(trustNetwork), "keys")
}

func refreshProfiles(ctx context.Context) {
	for i := 0; i < len(trustNetwork); i += 200 {
		timeout, cancel := context.WithTimeout(ctx, 4*time.Second)
		defer cancel()

		end := i + 200
		if end > len(trustNetwork) {
			end = len(trustNetwork)
		}

		filters := []nostr.Filter{{
			Authors: trustNetwork[i:end],
			Kinds:   []int{nostr.KindProfileMetadata},
		}}

		for ev := range pool.SubManyEose(timeout, seedRelays, filters) {
			wdb.Publish(ctx, *ev.Event)
		}
	}
	log.Println("👤 profiles refreshed: ", len(trustNetwork))
}

func refreshTrustNetwork(ctx context.Context) {
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
				pubkeyFollowerCount[contact[1]]++ // Increment follower count for the pubkey
				appendOneHopNetwork(contact[1])
			}
		}

		log.Println("🌐 building web of trust graph")
		for i := 0; i < len(oneHopNetwork); i += 100 {
			timeout, cancel := context.WithTimeout(ctx, 4*time.Second)
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
						pubkeyFollowerCount[contact[1]]++ // Increment follower count for the pubkey
					}
				}

				for _, relay := range ev.Event.Tags.GetAll([]string{"r"}) {
					appendRelay(relay[1])
				}

				if ev.Event.Kind == nostr.KindProfileMetadata {
					wdb.Publish(ctx, *ev.Event)
				}
			}
		}
		log.Println("🫂  total network size:", len(pubkeyFollowerCount))
		log.Println("🔗 relays discovered:", len(relays))
	}

	for {
		runTrustNetworkRefresh()
		updateTrustNetworkFilter()
		archiveTrustedNotes(ctx)
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

func appendPubkey(pubkey string) {
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

func archiveTrustedNotes(ctx context.Context) {
	timeout := time.After(time.Second * 3)
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
	var trustedNotes uint64
	var untrustedNotes uint64

	eventChan := pool.SubMany(ctx, seedRelays, filters)

	for {
		select {
		case <-timeout:
			log.Println("⏰ Archive process terminated due to timeout")
			log.Println("📦 archived", trustedNotes, "trusted notes and discarded", untrustedNotes, "untrusted notes")
			return

		case <-ctx.Done():
			log.Println("⏰ Archive process terminated due to context cancellation")
			log.Println("📦 archived", trustedNotes, "trusted notes and discarded", untrustedNotes, "untrusted notes")
			return

		case ev, ok := <-eventChan:
			if !ok {
				log.Println("📦 subscription channel closed")
				log.Println("📦 archived", trustedNotes, "trusted notes and discarded", untrustedNotes, "untrusted notes")
				return
			}

			if trustNetworkMap[ev.Event.PubKey] {
				if len(ev.Event.Tags) > 3000 {
					continue
				}

				wdb.Publish(ctx, *ev.Event)
				trustedNotes++
				// log.Println("📦 archived note: ", ev.Event.ID)
			} else {
				untrustedNotes++
			}
		}
	}
}
