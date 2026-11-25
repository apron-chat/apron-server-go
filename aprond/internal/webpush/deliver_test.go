package webpush

import (
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRefusesInternalAddresses(t *testing.T) {
	var hits atomic.Int32
	relay := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer relay.Close()
	deliverer := NewDeliverer(false)
	called := false
	deliverer.Deliver(Target{URL: relay.URL}, []byte("{}"), "normal", "", func(status int) { called = status != 0 })
	deliverer.Wait()
	if hits.Load() != 0 || called {
		t.Fatalf("delivered to a loopback address: hits=%d called=%v", hits.Load(), called)
	}
	if _, problem := CheckURL("http://relay.example/p", false); problem == "" {
		t.Fatal("http push URL accepted unless insecure")
	}
	// Internal address literals, localhost, and hosts ending in a dot are
	// refused at registration.
	for _, endpoint := range []string{"https://10.0.0.1/p", "https://[::1]/p", "https://[fd00::1]:8443/p", "https://169.254.169.254/p", "https://localhost/p", "https://push.localhost/p", "https://192.0.2.1/p", "https://push.example.net./p", "mailto:a@b.example"} {
		if _, problem := CheckURL(endpoint, false); problem == "" {
			t.Errorf("%s accepted", endpoint)
		}
	}
	// Endpoints are kept in one form: scheme and host lowercased, no
	// default port.
	for endpoint, want := range map[string]string{
		"https://93.184.215.14/p":                   "https://93.184.215.14/p",
		"https://fcm.googleapis.com/fcm/send/x":     "https://fcm.googleapis.com/fcm/send/x",
		"HTTPS://FCM.GoogleAPIs.com:443/fcm/send/x": "https://fcm.googleapis.com/fcm/send/x",
		"https://push.example.net:8443/A?b=C":       "https://push.example.net:8443/A?b=C",
		"https://[2606:4700::1111]:443/p":           "https://[2606:4700::1111]/p",
	} {
		if got, problem := CheckURL(endpoint, false); got != want {
			t.Errorf("%s normalized to %q (%s), want %q", endpoint, got, problem, want)
		}
	}
}

// Badge deliveries for one registration that have not started give way to
// the latest; lanes are per host, ignoring case.
func TestDeliveriesCoalesceBadges(t *testing.T) {
	release := make(chan struct{})
	var bodies []string
	var mu sync.Mutex
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()
		<-release
	}))
	defer relay.Close()
	deliverer := NewDeliverer(true)
	// Fill the host's lane so later deliveries wait.
	local := strings.Replace(relay.URL, "127.0.0.1", "localhost", 1)
	for range maxPushPerHost {
		deliverer.Deliver(Target{UserID: "filler", URL: local + "/busy"}, []byte("{}"), "normal", "", func(int) {})
	}
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		mu.Lock()
		busy := len(bodies) == maxPushPerHost
		mu.Unlock()
		if busy {
			break
		}
	}
	upper := strings.Replace(relay.URL, "127.0.0.1", "LocalHost", 1)
	for i := range 3 {
		deliverer.Deliver(Target{UserID: "u", URL: upper + "/badge"}, []byte(strconv.Itoa(i)), "low", "u "+upper+"/badge", func(int) {})
	}
	deliverer.mu.Lock()
	lanes, queued := len(deliverer.hosts), deliverer.users["u"]
	deliverer.mu.Unlock()
	close(release)
	deliverer.Wait()
	if lanes != 1 || queued != 1 {
		t.Fatalf("lanes %d, queued badge deliveries %d", lanes, queued)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Contains(bodies, "2") || slices.Contains(bodies, "0") || slices.Contains(bodies, "1") {
		t.Fatalf("delivered bodies: %v", bodies)
	}
}

func TestLanesIsolateStalledRelays(t *testing.T) {
	release := make(chan struct{})
	var stalled atomic.Int32
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stalled.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	delivered := make(chan struct{}, 1)
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		delivered <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer fast.Close()
	deliverer := NewDeliverer(true)
	defer func() {
		close(release)
		deliverer.Wait()
	}()

	// One user queues at most maxPushQueuedPerUser deliveries.
	for range maxPushQueuedPerUser + 5 {
		deliverer.Deliver(Target{UserID: "spammer", URL: slow.URL + "/slow"}, []byte("{}"), "normal", "", func(int) {})
	}
	deliverer.mu.Lock()
	queued := deliverer.users["spammer"]
	deliverer.mu.Unlock()
	if queued != maxPushQueuedPerUser {
		t.Fatalf("queued deliveries for one user: %d", queued)
	}
	for i := range maxConcurrentPushPOST {
		deliverer.Deliver(Target{UserID: "user" + strconv.Itoa(i), URL: slow.URL + "/slow"}, []byte("{}"), "normal", "", func(int) {})
	}
	deadline := time.Now().Add(time.Second)
	for stalled.Load() < maxPushPerHost && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := stalled.Load(); got != maxPushPerHost {
		t.Fatalf("concurrent deliveries to one host: %d", got)
	}
	// Another push host has its own lane.
	deliverer.Deliver(Target{UserID: "victim", URL: fast.URL + "/fast"}, []byte("{}"), "normal", "", func(int) {})
	select {
	case <-delivered:
	case <-time.After(2 * time.Second):
		t.Fatal("a stalled push host blocked delivery to another host")
	}
}

func TestRefusesNonPublicRanges(t *testing.T) {
	for host, want := range map[string]bool{
		"93.184.215.14":     true,
		"2606:4700::1111":   true,
		"127.0.0.1":         false,
		"10.1.2.3":          false,
		"169.254.169.254":   false,
		"100.64.0.1":        false,
		"100.100.100.200":   false,
		"198.18.0.1":        false,
		"192.0.0.1":         false,
		"240.0.0.1":         false,
		"::ffff:100.64.0.1": false,
		"64:ff9b::a00:1":    false,
		"2002:a00:1::1":     false,
		"fd00::1":           false,
		"192.0.2.1":         false,
		"198.51.100.1":      false,
		"203.0.113.1":       false,
		"::a00:1":           false,
		"100::1":            false,
		"2001:db8::1":       false,
		"fec0::1":           false,
		"3fff::1":           false,
		"::1":               false,
	} {
		if got := PublicAddress(host); got != want {
			t.Errorf("PublicAddress(%s) = %v, want %v", host, got, want)
		}
	}
}

// A badge delivery that replaces one still queued goes with the
// registration as it is now, its token included, not as it was when the
// first was queued.
func TestCoalescedBadgeUsesLatestRegistration(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	tokens := map[string]string{}
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		tokens[string(body)] = r.Header.Get("Authorization")
		mu.Unlock()
		<-release
	}))
	defer relay.Close()
	deliverer := NewDeliverer(true)
	for range maxPushPerHost {
		deliverer.Deliver(Target{UserID: "filler", URL: relay.URL + "/busy"}, []byte("{}"), "normal", "", func(int) {})
	}
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		mu.Lock()
		busy := len(tokens) > 0
		mu.Unlock()
		if busy {
			break
		}
	}
	key := "u " + relay.URL + "/badge"
	deliverer.Deliver(Target{UserID: "u", URL: relay.URL + "/badge", Token: "old"}, []byte("1"), "low", key, func(int) {})
	deliverer.Deliver(Target{UserID: "u", URL: relay.URL + "/badge", Token: "new"}, []byte("2"), "low", key, func(int) {})
	close(release)
	deliverer.Wait()
	mu.Lock()
	defer mu.Unlock()
	if _, sent := tokens["1"]; sent || tokens["2"] != "Bearer new" {
		t.Fatalf("badge deliveries: %#v", tokens)
	}
}
