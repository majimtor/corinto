package redirect_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/majimtor/corinto/internal/classify"
	"github.com/majimtor/corinto/internal/ingest"
	"github.com/majimtor/corinto/internal/limits"
)

const (
	browser = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36"
	phone   = "Mozilla/5.0 (iPhone; CPU iPhone OS 18_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.6 Mobile/15E148 Safari/604.1"
)

// The table of contract/redirect.md, by its last column: which responses
// owe an event, what the event says, and which are only counted.
func TestEachResponseLeavesBehindWhatTheContractSays(t *testing.T) {
	address, behind := hostThatRecords(t, func() time.Time { return now }, "", catalogue()...)
	human := classify.Classification{
		Class: classify.ClassPresumedHuman, Rule: "presumed_human", Version: rules(t).Version(),
		Device: classify.DeviceDesktop, Browser: classify.BrowserChrome,
	}
	as := func(class classify.Class, rule string, device classify.Device, family classify.Browser) classify.Classification {
		return classify.Classification{Class: class, Rule: rule, Version: human.Version, Device: device, Browser: family}
	}
	steps := []struct {
		name    string
		method  string
		path    string
		headers []string
		status  int
		// event is what the response owes; nil when it owes none.
		event *ingest.Event
		// counted is the counter that takes the request, if one does.
		counted ingest.Counter
	}{
		{"an active link", http.MethodGet, "/k7m2xq9d", []string{"User-Agent", browser}, http.StatusFound,
			&ingest.Event{LinkID: "1", WorkspaceID: "acme", OccurredAt: now, Outcome: ingest.OutcomeRedirected, Method: "GET", Classification: human}, ""},
		{"an active link that came from a page", http.MethodGet, "/k7m2xq9d", []string{"User-Agent", browser, "Referer", "https://News.Example.com/story?id=7#comments"}, http.StatusFound,
			&ingest.Event{LinkID: "1", WorkspaceID: "acme", OccurredAt: now, Outcome: ingest.OutcomeRedirected, Method: "GET", ReferrerHost: "news.example.com", Classification: human}, ""},
		{"an active link asked for its headers", http.MethodHead, "/k7m2xq9d", []string{"User-Agent", browser}, http.StatusFound,
			&ingest.Event{LinkID: "1", WorkspaceID: "acme", OccurredAt: now, Outcome: ingest.OutcomeRedirected, Method: "HEAD",
				Classification: as(classify.ClassAutomation, "automation:head", classify.DeviceDesktop, classify.BrowserChrome)}, ""},
		{"an active link in capital letters and with a query", http.MethodGet, "/SPRING-SALE?utm_source=sentinel", []string{"User-Agent", phone}, http.StatusFound,
			&ingest.Event{LinkID: "2", WorkspaceID: "globex", OccurredAt: now, Outcome: ingest.OutcomeRedirected, Method: "GET",
				Classification: as(classify.ClassPresumedHuman, "presumed_human", classify.DeviceMobile, classify.BrowserSafari)}, ""},
		{"an active link fetched ahead", http.MethodGet, "/k7m2xq9d", []string{"User-Agent", browser, "Sec-Purpose", "prefetch"}, http.StatusFound,
			&ingest.Event{LinkID: "1", WorkspaceID: "acme", OccurredAt: now, Outcome: ingest.OutcomeRedirected, Method: "GET",
				Classification: as(classify.ClassPrefetch, "prefetch:sec-purpose", classify.DeviceDesktop, classify.BrowserChrome)}, ""},
		{"an active link asked for by a script", http.MethodGet, "/k7m2xq9d", []string{"User-Agent", "curl/8.5.0"}, http.StatusFound,
			&ingest.Event{LinkID: "1", WorkspaceID: "acme", OccurredAt: now, Outcome: ingest.OutcomeRedirected, Method: "GET",
				Classification: as(classify.ClassAutomation, "automation:^curl", classify.DeviceOther, classify.BrowserOther)}, ""},
		{"a link that expires in a second", http.MethodGet, "/expires-soon", []string{"User-Agent", browser}, http.StatusFound,
			&ingest.Event{LinkID: "3", WorkspaceID: "acme", OccurredAt: now, Outcome: ingest.OutcomeRedirected, Method: "GET", Classification: human}, ""},
		{"a link that expired", http.MethodGet, "/expired", []string{"User-Agent", browser}, http.StatusNotFound,
			&ingest.Event{LinkID: "4", WorkspaceID: "acme", OccurredAt: now, Outcome: ingest.OutcomeInactive, Method: "GET", Classification: human}, ""},
		{"a link that expires now", http.MethodGet, "/expires-now", []string{"User-Agent", browser}, http.StatusNotFound,
			&ingest.Event{LinkID: "5", WorkspaceID: "acme", OccurredAt: now, Outcome: ingest.OutcomeInactive, Method: "GET", Classification: human}, ""},
		{"a disabled link", http.MethodGet, "/disabled", []string{"User-Agent", browser}, http.StatusNotFound,
			&ingest.Event{LinkID: "6", WorkspaceID: "globex", OccurredAt: now, Outcome: ingest.OutcomeInactive, Method: "GET", Classification: human}, ""},
		{"a blocked link asked for its headers", http.MethodHead, "/blocked", []string{"User-Agent", browser}, http.StatusNotFound,
			&ingest.Event{LinkID: "7", WorkspaceID: "acme", OccurredAt: now, Outcome: ingest.OutcomeInactive, Method: "HEAD",
				Classification: as(classify.ClassAutomation, "automation:head", classify.DeviceDesktop, classify.BrowserChrome)}, ""},
		{"a link that does not exist", http.MethodGet, "/bcdfghjk", []string{"User-Agent", browser}, http.StatusNotFound, nil, ingest.CounterNotFound},
		{"a path outside the grammar", http.MethodGet, "/k7m2xq9d/more", []string{"User-Agent", browser}, http.StatusNotFound, nil, ingest.CounterNotFound},
		{"a path outside the grammar, for its headers", http.MethodHead, "/%20", []string{"User-Agent", browser}, http.StatusNotFound, nil, ingest.CounterNotFound},
		{"another method on an active link", http.MethodPost, "/k7m2xq9d", []string{"User-Agent", browser}, http.StatusMethodNotAllowed, nil, ""},
		{"another method on a link that does not exist", http.MethodDelete, "/bcdfghjk", []string{"User-Agent", browser}, http.StatusMethodNotAllowed, nil, ""},
		{"the root", http.MethodGet, "/", []string{"User-Agent", browser}, http.StatusFound, nil, ""},
		{"security.txt", http.MethodGet, "/.well-known/security.txt", []string{"User-Agent", browser}, http.StatusOK, nil, ""},
	}
	var events []ingest.Event
	counts := make(map[ingest.Counter]int)
	for _, step := range steps {
		if got := ask(t, step.method, address+step.path, step.headers...); got.status != step.status {
			t.Fatalf("%s: %s %s = %d, want %d", step.name, step.method, step.path, got.status, step.status)
		}
		if step.event != nil {
			events = append(events, *step.event)
		}
		if step.counted != "" {
			counts[step.counted]++
		}
		gotEvents, gotCounts := behind.behind()
		if len(gotEvents) != len(events) || (len(events) > 0 && gotEvents[len(gotEvents)-1] != events[len(events)-1]) {
			t.Errorf("%s: after it the host had left %d events, the last of them %+v; want %d, the last of them %+v",
				step.name, len(gotEvents), last(gotEvents), len(events), last(events))
		}
		if fmt.Sprint(gotCounts) != fmt.Sprint(counts) {
			t.Errorf("%s: after it the host had counted %v, want %v", step.name, gotCounts, counts)
		}
	}
}

func last(events []ingest.Event) ingest.Event {
	if len(events) == 0 {
		return ingest.Event{}
	}
	return events[len(events)-1]
}

// An event is of the moment the clock of the host says, and a request
// over the rate limit is counted and owes no event.
func TestARequestOverTheRateLimitIsCountedAtItsMoment(t *testing.T) {
	clock := &moving{at: now}
	address, behind := hostThatRecords(t, clock.read, "", catalogue()...)
	for range limits.RedirectBurst {
		if got := ask(t, http.MethodGet, address+"/k7m2xq9d"); got.status != http.StatusFound {
			t.Fatalf("GET = %d, want 302", got.status)
		}
	}
	clock.advance(10 * time.Millisecond)
	for range 2 {
		if got := ask(t, http.MethodGet, address+"/k7m2xq9d"); got.status != http.StatusTooManyRequests {
			t.Fatalf("GET past the limit = %d, want 429", got.status)
		}
	}
	clock.advance(time.Second)
	if got := ask(t, http.MethodGet, address+"/disabled"); got.status != http.StatusNotFound {
		t.Fatalf("GET = %d, want 404", got.status)
	}

	events, counts := behind.behind()
	if len(events) != limits.RedirectBurst+1 || counts[ingest.CounterRateLimited] != 2 || len(counts) != 1 {
		t.Fatalf("the host left %d events and counted %v; want %d events and two requests over the limit", len(events), counts, limits.RedirectBurst+1)
	}
	if first, final := events[0], events[len(events)-1]; !first.OccurredAt.Equal(now) || !final.OccurredAt.Equal(now.Add(1010*time.Millisecond)) {
		t.Errorf("the first event is of %s and the last of %s", first.OccurredAt, final.OccurredAt)
	}
	behind.mu.Lock()
	defer behind.mu.Unlock()
	for _, at := range behind.counts[ingest.CounterRateLimited] {
		if !at.Equal(now.Add(10 * time.Millisecond)) {
			t.Errorf("a request over the limit was counted at %s, want %s", at, now.Add(10*time.Millisecond))
		}
	}
}

// An event carries nothing that identifies a visitor: not the address, not
// the user agent, not the query string, and no more of the referrer than
// its host (PLAN.md section 5).
func TestAnEventCarriesNothingOfTheVisitor(t *testing.T) {
	address, behind := hostThatRecords(t, func() time.Time { return now }, "127.0.0.0/8", catalogue()...)
	for _, path := range []string{"/k7m2xq9d?token=sentinel-query", "/disabled?token=sentinel-query"} {
		ask(t, http.MethodGet, address+path,
			"User-Agent", "sentinel-agent/1.0", "X-Forwarded-For", "203.0.113.77", "Cookie", "session=sentinel-cookie",
			"Referer", "https://sentinel-user:sentinel-secret@blog.example.org:8443/sentinel-path?q=sentinel-search#sentinel-part")
	}
	events, _ := behind.behind()
	if len(events) != 2 {
		t.Fatalf("the host left %d events, want 2", len(events))
	}
	for _, event := range events {
		if written := fmt.Sprintf("%+v", event); strings.Contains(written, "sentinel") || strings.Contains(written, "203.0.113") || strings.Contains(written, "127.0.0") {
			t.Errorf("the event carries something of the visitor: %s", written)
		}
		if event.ReferrerHost != "blog.example.org" {
			t.Errorf("the event keeps %q of the referrer, want its host", event.ReferrerHost)
		}
	}
}

// broken is a recorder that fails every time.
type broken struct{}

func (broken) Record(ingest.Event)             { panic("the buffer is broken") }
func (broken) Count(ingest.Counter, time.Time) { panic("the buffer is broken") }

// The redirect always answers; when something fails, analytics give way.
func TestTheAnswerDoesNotDependOnWhatItLeavesBehind(t *testing.T) {
	written := &lockedWriter{}
	server := httptest.NewServer(handlerOver(t, func() time.Time { return now }, "", broken{}, written, catalogue()...))
	t.Cleanup(server.Close)

	if got := ask(t, http.MethodGet, server.URL+"/k7m2xq9d?token=sentinel-query", "User-Agent", "sentinel-agent"); got.status != http.StatusFound || got.header.Get("Location") == "" {
		t.Errorf("an active link = %d to %q, want 302 to its destination", got.status, got.header.Get("Location"))
	}
	if got := ask(t, http.MethodGet, server.URL+"/disabled"); got.status != http.StatusNotFound || len(got.body) == 0 {
		t.Errorf("a disabled link = %d with a body of %d bytes, want 404 with its page", got.status, len(got.body))
	}
	if got := ask(t, http.MethodGet, server.URL+"/bcdfghjk"); got.status != http.StatusNotFound || len(got.body) == 0 {
		t.Errorf("an unknown link = %d with a body of %d bytes, want 404 with its page", got.status, len(got.body))
	}
	for range limits.RedirectBurst - 3 {
		ask(t, http.MethodGet, server.URL+"/")
	}
	if got := ask(t, http.MethodGet, server.URL+"/k7m2xq9d"); got.status != http.StatusTooManyRequests || got.header.Get("Retry-After") == "" {
		t.Errorf("a request over the limit = %d, want 429 with Retry-After", got.status)
	}
	server.Close()

	log := written.String()
	if failures := strings.Count(log, "what a response leaves behind could not be handed over"); failures != 4 {
		t.Errorf("the log has %d failures to hand over, want the 4: %s", failures, log)
	}
	if strings.Contains(log, "a redirect panicked") || strings.Contains(log, "sentinel") || strings.Contains(log, "127.0.0.1") || strings.Contains(log, "k7m2xq9d") {
		t.Errorf("the log carries something of a request, or says the redirect failed: %s", log)
	}
}

// lockedWriter is a log that a test reads after a server has written it.
type lockedWriter struct {
	mu   sync.Mutex
	text bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.text.Write(p)
}

func (l *lockedWriter) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.text.String()
}

// stuck is an inbox whose writes do not come back until a test lets them.
type stuck struct {
	release chan struct{}

	mu       sync.Mutex
	asked    int
	accepted []ingest.Event
	served   int64
}

func (s *stuck) Count(_ context.Context, counts []ingest.Count) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, count := range counts {
		s.served += count.Value
	}
	return len(counts), nil
}

func (s *stuck) Accept(ctx context.Context, events []ingest.Event) (int, error) {
	s.mu.Lock()
	s.asked++
	s.mu.Unlock()
	select {
	case <-s.release:
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accepted = append(s.accepted, events...)
	return len(events), nil
}

func (s *stuck) state() (asked, accepted int, served int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.asked, len(s.accepted), s.served
}

// Statement 1 of docs/delivery-semantics.md: the response never waits for
// the event. The host answers while the writer behind the buffer is held
// by an inbox that does not come back.
func TestTheResponseDoesNotWaitForTheEvent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		into := &stuck{release: make(chan struct{})}
		logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
		buffer := ingest.NewBuffer(into, logger)
		ctx, cancel := context.WithCancel(t.Context())
		handler := handlerOver(t, func() time.Time { return now }, "", buffer, io.Discard, catalogue()...)
		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			buffer.Run(ctx)
		}()
		defer func() {
			cancel()
			<-stopped
		}()

		serve := func() int {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/k7m2xq9d", nil))
			return response.Code
		}
		if status := serve(); status != http.StatusFound {
			t.Fatalf("GET = %d, want 302", status)
		}
		// The interval passes and the writer takes the event to the inbox,
		// which does not answer.
		time.Sleep(ingest.FlushEvery)
		synctest.Wait()
		if asked, accepted, _ := into.state(); asked != 1 || accepted != 0 {
			t.Fatalf("the inbox was asked %d times and took %d events, want a write that has not come back", asked, accepted)
		}
		// The host goes on answering. A response that waited would never
		// come: every goroutine here would be waiting for another.
		for range 30 {
			if status := serve(); status != http.StatusFound {
				t.Fatalf("GET with the writer held = %d, want 302", status)
			}
		}
		if _, accepted, _ := into.state(); accepted != 0 {
			t.Fatalf("the inbox took %d events while it was held", accepted)
		}

		close(into.release)
		time.Sleep(ingest.FlushEvery)
		synctest.Wait()
		if _, accepted, served := into.state(); accepted != 31 || served != 31 {
			t.Errorf("once the inbox answers it has %d events and %d responses counted as served, want 31 of each", accepted, served)
		}
	})
}
