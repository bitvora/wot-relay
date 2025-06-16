package main

import (
	"context"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
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
	MaxTrustNetwork  int
	MaxRelays        int
	MaxOneHopNetwork int
}

var pool *nostr.SimplePool
var wdb nostr.RelayStore
var relays []string
var relaySet = make(map[string]bool) // O(1) lookup
var config Config
var trustNetwork []string
var trustNetworkSet = make(map[string]bool) // O(1) lookup
var seedRelays []string
var booted bool
var oneHopNetwork []string
var oneHopNetworkSet = make(map[string]bool) // O(1) lookup
var trustNetworkMap map[string]bool
var pubkeyFollowerCount = make(map[string]int)
var trustedNotes uint64
var untrustedNotes uint64
var archiveEventSemaphore = make(chan struct{}, 100) // Limit concurrent goroutines

// Mutexes for thread safety
var (
	relayMutex        sync.RWMutex
	trustNetworkMutex sync.RWMutex
	oneHopMutex       sync.RWMutex
	followerMutex     sync.RWMutex
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
		// Don't reject events if we haven't booted yet or if trust network is empty
		if !booted {
			return false, ""
		}

		trustNetworkMutex.RLock()
		trusted := trustNetworkMap[event.PubKey]
		hasNetwork := len(trustNetworkMap) > 0
		trustNetworkMutex.RUnlock()

		// If we don't have a trust network yet, allow all events
		if !hasNetwork {
			return false, ""
		}

		if !trusted {
			return true, "not in web of trust"
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
	go monitorMemoryUsage() // Add memory monitoring

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

	if os.Getenv("MAX_TRUST_NETWORK") == "" {
		os.Setenv("MAX_TRUST_NETWORK", "40000")
	}

	if os.Getenv("MAX_RELAYS") == "" {
		os.Setenv("MAX_RELAYS", "1000")
	}

	if os.Getenv("MAX_ONE_HOP_NETWORK") == "" {
		os.Setenv("MAX_ONE_HOP_NETWORK", "50000")
	}

	ignoredPubkeys := []string{}
	if ignoreList := os.Getenv("IGNORE_FOLLOWS_LIST"); ignoreList != "" {
		ignoredPubkeys = splitAndTrim(ignoreList)
	}

	minimumFollowers, _ := strconv.Atoi(os.Getenv("MINIMUM_FOLLOWERS"))
	maxAgeDays, _ := strconv.Atoi(os.Getenv("MAX_AGE_DAYS"))
	maxTrustNetwork, _ := strconv.Atoi(os.Getenv("MAX_TRUST_NETWORK"))
	maxRelays, _ := strconv.Atoi(os.Getenv("MAX_RELAYS"))
	maxOneHopNetwork, _ := strconv.Atoi(os.Getenv("MAX_ONE_HOP_NETWORK"))

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
		MaxTrustNetwork:  maxTrustNetwork,
		MaxRelays:        maxRelays,
		MaxOneHopNetwork: maxOneHopNetwork,
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
	// Build new trust network in temporary variables
	newTrustNetworkMap := make(map[string]bool)
	var newTrustNetwork []string
	newTrustNetworkSet := make(map[string]bool)

	log.Println("🌐 building new trust network map")

	followerMutex.RLock()
	for pubkey, count := range pubkeyFollowerCount {
		if count >= config.MinimumFollowers {
			newTrustNetworkMap[pubkey] = true
			if !newTrustNetworkSet[pubkey] && len(pubkey) == 64 && len(newTrustNetwork) < config.MaxTrustNetwork {
				newTrustNetwork = append(newTrustNetwork, pubkey)
				newTrustNetworkSet[pubkey] = true
			}
		}
	}
	followerMutex.RUnlock()

	// Now atomically replace the active trust network
	trustNetworkMutex.Lock()
	trustNetworkMap = newTrustNetworkMap
	trustNetwork = newTrustNetwork
	trustNetworkSet = newTrustNetworkSet
	trustNetworkMutex.Unlock()

	log.Println("🌐 trust network map updated with", len(newTrustNetwork), "keys")

	// Cleanup follower count map periodically to prevent unbounded growth
	followerMutex.Lock()
	if len(pubkeyFollowerCount) > config.MaxOneHopNetwork*2 {
		log.Println("🧹 cleaning follower count map")
		newFollowerCount := make(map[string]int)
		for pubkey, count := range pubkeyFollowerCount {
			if count >= config.MinimumFollowers || newTrustNetworkMap[pubkey] {
				newFollowerCount[pubkey] = count
			}
		}
		oldCount := len(pubkeyFollowerCount)
		pubkeyFollowerCount = newFollowerCount
		log.Printf("🧹 cleaned follower count map: %d -> %d entries", oldCount, len(newFollowerCount))
	}
	followerMutex.Unlock()
}

func refreshProfiles(ctx context.Context) {
	// Get a snapshot of current trust network to avoid holding locks during network operations
	trustNetworkMutex.RLock()
	currentTrustNetwork := make([]string, len(trustNetwork))
	copy(currentTrustNetwork, trustNetwork)
	trustNetworkMutex.RUnlock()

	for i := 0; i < len(currentTrustNetwork); i += 200 {
		timeout, cancel := context.WithTimeout(ctx, 4*time.Second)

		end := i + 200
		if end > len(currentTrustNetwork) {
			end = len(currentTrustNetwork)
		}

		filters := []nostr.Filter{{
			Authors: currentTrustNetwork[i:end],
			Kinds:   []int{nostr.KindProfileMetadata},
		}}

		for ev := range pool.SubManyEose(timeout, seedRelays, filters) {
			wdb.Publish(ctx, *ev.Event)
		}

		cancel() // Cancel after each iteration
	}
	log.Println("👤 profiles refreshed: ", len(currentTrustNetwork))
}

func refreshTrustNetwork(ctx context.Context, relay *khatru.Relay) {
	runTrustNetworkRefresh := func() {
		// Build new networks in temporary variables to avoid disrupting the active network
		var newOneHopNetwork []string
		newOneHopNetworkSet := make(map[string]bool)
		newPubkeyFollowerCount := make(map[string]int)

		// Copy existing follower counts to preserve data
		followerMutex.RLock()
		for k, v := range pubkeyFollowerCount {
			newPubkeyFollowerCount[k] = v
		}
		followerMutex.RUnlock()

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
				newPubkeyFollowerCount[contact[1]]++

				// Add to new one-hop network
				if !newOneHopNetworkSet[contact[1]] && len(contact[1]) == 64 && len(newOneHopNetwork) < config.MaxOneHopNetwork {
					newOneHopNetwork = append(newOneHopNetwork, contact[1])
					newOneHopNetworkSet[contact[1]] = true
				}
			}
		}

		log.Println("🌐 building web of trust graph")
		for i := 0; i < len(newOneHopNetwork); i += 100 {
			timeout, cancel := context.WithTimeout(ctx, 4*time.Second)

			end := i + 100
			if end > len(newOneHopNetwork) {
				end = len(newOneHopNetwork)
			}

			filters = []nostr.Filter{{
				Authors: newOneHopNetwork[i:end],
				Kinds:   []int{nostr.KindFollowList, nostr.KindRelayListMetadata, nostr.KindProfileMetadata},
			}}

			for ev := range pool.SubManyEose(timeout, seedRelays, filters) {
				for _, contact := range ev.Event.Tags.GetAll([]string{"p"}) {
					if len(contact) > 1 {
						newPubkeyFollowerCount[contact[1]]++
					}
				}

				for _, relay := range ev.Event.Tags.GetAll([]string{"r"}) {
					appendRelay(relay[1])
				}

				if ev.Event.Kind == nostr.KindProfileMetadata {
					wdb.Publish(ctx, *ev.Event)
				}
			}
			cancel() // Cancel after each iteration
		}

		// Now atomically replace the active data structures
		oneHopMutex.Lock()
		oneHopNetwork = newOneHopNetwork
		oneHopNetworkSet = newOneHopNetworkSet
		oneHopMutex.Unlock()

		followerMutex.Lock()
		pubkeyFollowerCount = newPubkeyFollowerCount
		followerMutex.Unlock()

		log.Println("🫂  total network size:", len(newPubkeyFollowerCount))
		relayMutex.RLock()
		log.Println("🔗 relays discovered:", len(relays))
		relayMutex.RUnlock()
	}

	ticker := time.NewTicker(time.Duration(config.RefreshInterval) * time.Hour)
	defer ticker.Stop()

	// Run initial refresh
	log.Println("🚀 performing initial trust network build...")
	runTrustNetworkRefresh()
	updateTrustNetworkFilter()

	// Mark as booted after initial trust network is built
	booted = true
	log.Println("✅ trust network initialized, relay is now active")

	deleteOldNotes(relay)
	archiveTrustedNotes(ctx, relay)

	// Then run on timer
	for {
		select {
		case <-ticker.C:
			log.Println("🔄 refreshing trust network in background...")
			runTrustNetworkRefresh()
			updateTrustNetworkFilter()
			deleteOldNotes(relay)
			archiveTrustedNotes(ctx, relay)
			log.Println("✅ trust network refresh completed")
		case <-ctx.Done():
			return
		}
	}
}

func appendRelay(relay string) {
	relayMutex.Lock()
	defer relayMutex.Unlock()

	if len(relays) >= config.MaxRelays {
		return // Prevent unbounded growth
	}

	if relaySet[relay] {
		return // Already exists
	}

	relays = append(relays, relay)
	relaySet[relay] = true
}

func appendPubkey(pubkey string) {
	trustNetworkMutex.Lock()
	defer trustNetworkMutex.Unlock()

	if len(trustNetwork) >= config.MaxTrustNetwork {
		return // Prevent unbounded growth
	}

	if trustNetworkSet[pubkey] {
		return // Already exists
	}

	if len(pubkey) != 64 {
		return
	}

	trustNetwork = append(trustNetwork, pubkey)
	trustNetworkSet[pubkey] = true
}

func archiveTrustedNotes(ctx context.Context, relay *khatru.Relay) {
	timeout, cancel := context.WithTimeout(ctx, time.Duration(config.RefreshInterval)*time.Hour)
	defer cancel()

	done := make(chan struct{})

	go func() {
		defer close(done)
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
				// Use semaphore to limit concurrent goroutines
				select {
				case archiveEventSemaphore <- struct{}{}:
					go func(event nostr.Event) {
						defer func() { <-archiveEventSemaphore }()
						archiveEvent(ctx, relay, event)
					}(*ev.Event)
				case <-timeout.Done():
					return
				}
			}

			log.Println("📦 archived", trustedNotes, "trusted notes and discarded", untrustedNotes, "untrusted notes")
		} else {
			log.Println("🔄 web of trust will refresh in", config.RefreshInterval, "hours")
			select {
			case <-timeout.Done():
			}
		}
	}()

	select {
	case <-timeout.Done():
		log.Println("restarting process")
	case <-done:
		log.Println("📦 archiving process completed")
	}
}

func archiveEvent(ctx context.Context, relay *khatru.Relay, ev nostr.Event) {
	trustNetworkMutex.RLock()
	trusted := trustNetworkMap[ev.PubKey]
	trustNetworkMutex.RUnlock()

	if trusted {
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
		Limit: 1000, // Process in batches to avoid memory issues
	}

	ch, err := relay.QueryEvents[0](ctx, filter)
	if err != nil {
		log.Printf("query error %s", err)
		return err
	}

	// Process events in batches to avoid memory issues
	batchSize := 100
	events := make([]*nostr.Event, 0, batchSize)
	count := 0

	for evt := range ch {
		events = append(events, evt)
		count++

		if len(events) >= batchSize {
			// Delete this batch
			for num_evt, del_evt := range events {
				for _, del := range relay.DeleteEvent {
					if err := del(ctx, del_evt); err != nil {
						log.Printf("error deleting note %d of batch. event id: %s", num_evt, del_evt.ID)
						return err
					}
				}
			}
			events = events[:0] // Reset slice but keep capacity
		}
	}

	// Delete remaining events
	if len(events) > 0 {
		for num_evt, del_evt := range events {
			for _, del := range relay.DeleteEvent {
				if err := del(ctx, del_evt); err != nil {
					log.Printf("error deleting note %d of final batch. event id: %s", num_evt, del_evt.ID)
					return err
				}
			}
		}
	}

	if count == 0 {
		log.Println("0 old notes found")
	} else {
		log.Printf("%d old (until %d) notes deleted", count, oldAge)
	}

	return nil
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

// Add memory monitoring
func monitorMemoryUsage() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			var m runtime.MemStats
			runtime.ReadMemStats(&m)

			relayMutex.RLock()
			relayCount := len(relays)
			relayMutex.RUnlock()

			trustNetworkMutex.RLock()
			trustNetworkCount := len(trustNetwork)
			trustNetworkMutex.RUnlock()

			oneHopMutex.RLock()
			oneHopCount := len(oneHopNetwork)
			oneHopMutex.RUnlock()

			followerMutex.RLock()
			followerCount := len(pubkeyFollowerCount)
			followerMutex.RUnlock()

			log.Printf("📊 Memory: Alloc=%d KB, Sys=%d KB, NumGC=%d",
				m.Alloc/1024, m.Sys/1024, m.NumGC)
			log.Printf("📊 Data structures: Relays=%d, TrustNetwork=%d, OneHop=%d, Followers=%d",
				relayCount, trustNetworkCount, oneHopCount, followerCount)
		}
	}
}
