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
	"strings"
	"time"

	"github.com/fiatjaf/eventstore"
	"github.com/fiatjaf/eventstore/badger"
	"github.com/fiatjaf/khatru"
	"github.com/fiatjaf/khatru/policies"
	"github.com/joho/godotenv"
	"github.com/nbd-wtf/go-nostr"
)

var (
	version string
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
	ArchivalSync     bool
	RelayContact     string
	RelayIcon        string
	MaxAgeDays       int
	ArchiveReactions bool
	IgnoredPubkeys   []string
}

var pool *nostr.SimplePool
var wdb nostr.RelayStore
var relays []string
var config Config
var trustNetwork []string
var seedRelays []string
var booted bool
var oneHopNetwork []string
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
	relay.Info.Icon = config.RelayIcon
	relay.Info.Contact = config.RelayContact
	relay.Info.Description = config.RelayDescription
	relay.Info.Software = "https://github.com/bitvora/wot-relay"
	relay.Info.Version = version

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
		if !trustNetworkMap[event.PubKey] {
			return true, "we are rebuilding the trust network, please try again later"
		}
		if event.Kind == nostr.KindEncryptedDirectMessage {
			return true, "only gift wrapped DMs are allowed"
		}

		return false, ""
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

	if os.Getenv("ARCHIVAL_SYNC") == "" {
		os.Setenv("ARCHIVAL_SYNC", "TRUE")
	}

	if os.Getenv("RELAY_ICON") == "" {
		os.Setenv("RELAY_ICON", "https://pfp.nostr.build/56306a93a88d4c657d8a3dfa57b55a4ed65b709eee927b5dafaab4d5330db21f.png")
	}

	if os.Getenv("RELAY_CONTACT") == "" {
		os.Setenv("RELAY_CONTACT", getEnv("RELAY_PUBKEY"))
	}

	if os.Getenv("MAX_AGE_DAYS") == "" {
		os.Setenv("MAX_AGE_DAYS", "0")
	}

	if os.Getenv("ARCHIVE_REACTIONS") == "" {
		os.Setenv("ARCHIVE_REACTIONS", "FALSE")
	}

	ignoredPubkeys := []string{}
	if ignoreList := os.Getenv("IGNORE_FOLLOWS_LIST"); ignoreList != "" {
		ignoredPubkeys = splitAndTrim(ignoreList)
		log.Printf("🚫 Loaded %d ignored pubkeys: %v", len(ignoredPubkeys), ignoredPubkeys)
	}

	minimumFollowers, _ := strconv.Atoi(os.Getenv("MINIMUM_FOLLOWERS"))
	maxAgeDays, _ := strconv.Atoi(os.Getenv("MAX_AGE_DAYS"))

	config := Config{
		RelayName:        getEnv("RELAY_NAME"),
		RelayPubkey:      getEnv("RELAY_PUBKEY"),
		RelayDescription: getEnv("RELAY_DESCRIPTION"),
		RelayContact:     getEnv("RELAY_CONTACT"),
		RelayIcon:        getEnv("RELAY_ICON"),
		DBPath:           getEnv("DB_PATH"),
		RelayURL:         getEnv("RELAY_URL"),
		IndexPath:        getEnv("INDEX_PATH"),
		StaticPath:       getEnv("STATIC_PATH"),
		RefreshInterval:  refreshInterval,
		MinimumFollowers: minimumFollowers,
		ArchivalSync:     getEnv("ARCHIVAL_SYNC") == "TRUE",
		MaxAgeDays:       maxAgeDays,
		ArchiveReactions: getEnv("ARCHIVE_REACTIONS") == "TRUE",
		IgnoredPubkeys:   ignoredPubkeys,
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

func refreshTrustNetwork(ctx context.Context, relay *khatru.Relay) {

	runTrustNetworkRefresh := func() {
		timeoutCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()

		filters := []nostr.Filter{{
			Authors: []string{config.RelayPubkey},
			Kinds:   []int{nostr.KindFollowList},
		}}

		log.Println("🔍 fetching owner's follows")
		for ev := range pool.SubManyEose(timeoutCtx, seedRelays, filters) {
			for _, contact := range ev.Event.Tags.GetAll([]string{"p"}) {
				pubkey := contact[1]
				if isIgnored(pubkey, config.IgnoredPubkeys) {
					fmt.Println("ignoring follows from pubkey: ", pubkey)
					continue
				}
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
				Kinds:   []int{nostr.KindFollowList, nostr.KindRelayListMetadata, nostr.KindProfileMetadata},
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
		deleteOldNotes(relay)
		deleteIgnoredNotes(relay)
		archiveTrustedNotes(ctx, relay)
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

func archiveTrustedNotes(ctx context.Context, relay *khatru.Relay) {
	timeout, cancel := context.WithTimeout(ctx, time.Duration(config.RefreshInterval)*time.Hour)
	defer cancel()

	done := make(chan struct{})

	go func() {
		if config.ArchivalSync {
			go refreshProfiles(ctx)

			var filters []nostr.Filter
			if config.ArchiveReactions {

				filters = []nostr.Filter{{
					Kinds: []int{
						nostr.KindArticle,
						nostr.KindDeletion,
						nostr.KindFollowList,
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
			} else {
				filters = []nostr.Filter{{
					Kinds: []int{
						nostr.KindArticle,
						nostr.KindDeletion,
						nostr.KindFollowList,
						nostr.KindEncryptedDirectMessage,
						nostr.KindMuteList,
						nostr.KindRelayListMetadata,
						nostr.KindRepost,
						nostr.KindZapRequest,
						nostr.KindZap,
						nostr.KindTextNote,
					},
				}}
			}

			log.Println("📦 archiving trusted notes...")

			for ev := range pool.SubMany(timeout, seedRelays, filters) {
				go archiveEvent(ctx, relay, *ev.Event)
			}

			log.Println("📦 archived", trustedNotes, "trusted notes and discarded", untrustedNotes, "untrusted notes")
		} else {
			log.Println("🔄 web of trust will refresh in", config.RefreshInterval, "hours")
			select {
			case <-timeout.Done():
			}
		}

		close(done)
	}()

	select {
	case <-timeout.Done():
		log.Println("restarting process")
	case <-done:
		log.Println("📦 archiving process completed")
	}
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

func deleteOldNotes(relay *khatru.Relay) error {
	ctx := context.TODO()

	if config.MaxAgeDays <= 0 {
		log.Printf("MAX_AGE_DAYS disabled")
		return nil
	}

	maxAgeSecs := nostr.Timestamp(config.MaxAgeDays * 86400)
	oldAge := nostr.Now() - maxAgeSecs
	if oldAge <= 0 {
		log.Printf("MAX_AGE_DAYS too large")
		return nil
	}

	filter := nostr.Filter{
		Until: &oldAge,
		Kinds: []int{
			nostr.KindArticle,
			nostr.KindDeletion,
			nostr.KindFollowList,
			nostr.KindEncryptedDirectMessage,
			nostr.KindMuteList,
			nostr.KindReaction,
			nostr.KindRelayListMetadata,
			nostr.KindRepost,
			nostr.KindZapRequest,
			nostr.KindZap,
			nostr.KindTextNote,
		},
	}

	ch, err := relay.QueryEvents[0](ctx, filter)
	if err != nil {
		log.Printf("query error %s", err)
		return err
	}

	events := make([]*nostr.Event, 0)

	for evt := range ch {
		events = append(events, evt)
	}

	if len(events) < 1 {
		log.Println("0 old notes found")
		return nil
	}

	for num_evt, del_evt := range events {
		for _, del := range relay.DeleteEvent {
			if err := del(ctx, del_evt); err != nil {
				log.Printf("error deleting note %d of %d. event id: %s", num_evt, len(events), del_evt.ID)
				return err
			}
		}
	}

	log.Printf("%d old (until %d) notes deleted", len(events), oldAge)
	return nil
}

func deleteIgnoredNotes(relay *khatru.Relay) error {
	ctx := context.TODO()

	if len(config.IgnoredPubkeys) == 0 {
		log.Printf("No ignored pubkeys configured")
		return nil
	}

	log.Printf("🔍 Searching for notes from %d ignored pubkeys: %v", 
		len(config.IgnoredPubkeys), config.IgnoredPubkeys)

	for {
		filter := nostr.Filter{
			Authors: config.IgnoredPubkeys,
		}

		ch, err := relay.QueryEvents[0](ctx, filter)
		if err != nil {
			log.Printf("query error %s", err)
			return err
		}

		events := make([]*nostr.Event, 0)

		for evt := range ch {
			events = append(events, evt)
		}

		if len(events) == 0 {
			log.Println("✨ All notes from ignored pubkeys have been deleted")
			return nil
		}

		log.Printf("📝 Found %d events to delete from ignored pubkeys", len(events))
		for _, evt := range events {
			log.Printf("  - Event %s from pubkey %s", evt.ID, evt.PubKey)
		}

		for num_evt, del_evt := range events {
			for _, del := range relay.DeleteEvent {
				if err := del(ctx, del_evt); err != nil {
					log.Printf("error deleting note %d of %d. event id: %s", num_evt, len(events), del_evt.ID)
					return err
				}
			}
		}

		log.Printf("🗑️ Deleted batch of %d notes from ignored pubkeys", len(events))
	}
}

func getDB() badger.BadgerBackend {
	return badger.BadgerBackend{
		Path: getEnv("DB_PATH"),
	}
}

func splitAndTrim(input string) []string {
	items := strings.Split(input, ",")
	for i, item := range items {
		items[i] = strings.TrimSpace(item)
	}
	return items
}

func isIgnored(pubkey string, ignoredPubkeys []string) bool {
	for _, ignored := range ignoredPubkeys {
		if pubkey == ignored {
			return true
		}
	}
	return false
}
