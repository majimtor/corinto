package classify

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
)

// classifier returns the rules as the binary embeds them.
func classifier(t testing.TB) *Classifier {
	t.Helper()
	rules, err := New()
	if err != nil {
		t.Fatal(err)
	}
	return rules
}

// from classifies a GET with a user agent and nothing else.
func from(rules *Classifier, userAgent string) Classification {
	return rules.Classify(http.MethodGet, http.Header{"User-Agent": {userAgent}})
}

// The classes, the devices and the browser families, written here a second
// time on purpose: they are in every stored event, and a value that changes
// here changes what is counted.
var (
	classes = []Class{"prefetch", "automation", "preview", "crawler", "presumed_human"}
	closed  = struct {
		devices  []Device
		browsers []Browser
	}{
		[]Device{"desktop", "mobile", "tablet", "other"},
		[]Browser{"chrome", "safari", "firefox", "edge", "opera", "samsung_internet", "other"},
	}
)

// wellFormed fails unless a classification has values of the enumerations,
// the version of the rules, and a rule that begins with its class.
func wellFormed(t testing.TB, rules *Classifier, got Classification) {
	t.Helper()
	if !slices.Contains(classes, got.Class) || !slices.Contains(closed.devices, got.Device) || !slices.Contains(closed.browsers, got.Browser) {
		t.Errorf("a value outside the enumerations: %+v", got)
	}
	if got.Version != rules.Version() || got.Version < 1 {
		t.Errorf("the classification carries the version %d of rules at version %d", got.Version, rules.Version())
	}
	if got.Rule != string(got.Class) && !strings.HasPrefix(got.Rule, string(got.Class)+":") {
		t.Errorf("the rule %q does not say which class it gives, %s", got.Rule, got.Class)
	}
}

func TestTheEnumerationsAreTheOnesStoredEventsCarry(t *testing.T) {
	if !slices.Equal(Classes(), classes) {
		t.Errorf("classes %v, want %v", Classes(), classes)
	}
	if !slices.Equal(Devices(), closed.devices) || !slices.Equal(Browsers(), closed.browsers) {
		t.Errorf("devices %v and browsers %v, want %v and %v", Devices(), Browsers(), closed.devices, closed.browsers)
	}
}

// The examples of the list are real user agents of crawlers, previewers and
// HTTP libraries. None of them is a person.
func TestNoExampleOfTheListIsCountedAsAPerson(t *testing.T) {
	rules := classifier(t)
	data, err := os.ReadFile("testdata/crawler-examples.json")
	if err != nil {
		t.Fatal(err)
	}
	var entries []struct {
		Pattern  string   `json:"pattern"`
		Examples []string `json:"examples"`
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatal(err)
	}
	counted := make(map[Class]int)
	examples := 0
	for _, entry := range entries {
		for _, example := range entry.Examples {
			examples++
			got := from(rules, example)
			wellFormed(t, rules, got)
			counted[got.Class]++
			if got.Class != ClassAutomation && got.Class != ClassPreview && got.Class != ClassCrawler {
				t.Errorf("an example of %q is classified %s by %q: %s", entry.Pattern, got.Class, got.Rule, example)
				continue
			}
			// The rule names a pattern, and that pattern is in the example:
			// the count can be traced to what produced it.
			_, pattern, _ := strings.Cut(got.Rule, ":")
			if matched, err := regexp.MatchString(pattern, example); err != nil || !matched {
				t.Errorf("the rule %q does not match the example it classified: %s", got.Rule, example)
			}
		}
	}
	if examples < 1900 || counted[ClassAutomation] == 0 || counted[ClassPreview] == 0 || counted[ClassCrawler] == 0 {
		t.Errorf("%d examples were read, classified %v: the file or the tags are not what they were", examples, counted)
	}
}

// The class of an entry of the list comes from its tags, and the classes
// are tried in their order.
func TestWhatDeclaresItselfIsClassifiedByWhatItIs(t *testing.T) {
	rules := classifier(t)
	cases := []struct {
		name      string
		userAgent string
		want      Class
	}{
		{"a command-line client", "curl/8.5.0", ClassAutomation},
		{"an HTTP library", "python-requests/2.32.3", ClassAutomation},
		{"another HTTP library", "Go-http-client/2.0", ClassAutomation},
		{"an automated browser", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) HeadlessChrome/142.0.0.0 Safari/537.36", ClassAutomation},
		{"a social previewer", "facebookexternalhit/1.1 (+http://www.facebook.com/externalhit_uatext.php)", ClassPreview},
		{"a messaging previewer", "WhatsApp/2.23.20.0", ClassPreview},
		{"a chat previewer", "Slackbot-LinkExpanding 1.0 (+https://api.slack.com/robots)", ClassPreview},
		{"a search engine", "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)", ClassCrawler},
		{"another search engine", "Mozilla/5.0 (compatible; bingbot/2.0; +http://www.bing.com/bingbot.htm)", ClassCrawler},
		{"a monitor", "Mozilla/5.0+(compatible; UptimeRobot/2.0; http://www.uptimerobot.com/)", ClassCrawler},
		{"a crawler that builds a public archive of the web", "CCBot/2.0 (https://commoncrawl.org/faq/)", ClassCrawler},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := from(rules, c.userAgent)
			wellFormed(t, rules, got)
			if got.Class != c.want {
				t.Errorf("classified %s by %q, want %s", got.Class, got.Rule, c.want)
			}
		})
	}
}

// The classes are tried in their order: something that names an HTTP
// library and a crawler is automation, and a previewer that names a crawler
// is a preview.
func TestTheClassesAreTriedInTheirOrder(t *testing.T) {
	rules := classifier(t)
	cases := []struct {
		name      string
		userAgent string
		want      Class
	}{
		{"a library that says it is a crawler", "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html) python-requests/2.32.3", ClassAutomation},
		{"a previewer that says it is a crawler", "WhatsApp/2.23.20.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)", ClassPreview},
		{"a library that says it is a previewer", "curl/8.5.0 facebookexternalhit/1.1", ClassAutomation},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := from(rules, c.userAgent); got.Class != c.want {
				t.Errorf("classified %s by %q, want %s", got.Class, got.Rule, c.want)
			}
		})
	}
}

// Rules that cannot be read whole are refused: a classifier with part of
// its rules would count wrongly without saying so.
func TestRulesThatCannotBeReadAreRefused(t *testing.T) {
	const rules = `{"version": 1, "prefetch_headers": ["Sec-Purpose"], "automation_tags": ["http-library"], "preview_tags": [],
		"browsers": [{"value": "chrome", "any": ["Chrome/"]}], "devices": [{"value": "mobile", "any": ["Mobi"]}]}`
	const list = `[{"pattern": "curl", "tags": ["http-library"]}, {"pattern": "Googlebot", "tags": ["search-engine"]}]`
	cases := []struct {
		name  string
		files fstest.MapFS
		want  string
	}{
		{"no file of the project's rules", fstest.MapFS{"crawlers.json": {Data: []byte(list)}}, "rules.json"},
		{"no list", fstest.MapFS{"rules.json": {Data: []byte(rules)}}, "crawlers.json"},
		{"rules that are not JSON", fstest.MapFS{"rules.json": {Data: []byte("{")}, "crawlers.json": {Data: []byte(list)}}, "rules.json"},
		{"rules without a version", fstest.MapFS{"rules.json": {Data: []byte(strings.Replace(rules, `"version": 1`, `"version": 0`, 1))}, "crawlers.json": {Data: []byte(list)}}, "no version"},
		{"a browser outside the families", fstest.MapFS{"rules.json": {Data: []byte(strings.Replace(rules, `"chrome"`, `"netscape"`, 1))}, "crawlers.json": {Data: []byte(list)}}, `browser "netscape"`},
		{"a device outside the enumeration", fstest.MapFS{"rules.json": {Data: []byte(strings.Replace(rules, `"mobile"`, `"watch"`, 1))}, "crawlers.json": {Data: []byte(list)}}, `device "watch"`},
		{"a pattern that is not one", fstest.MapFS{"rules.json": {Data: []byte(rules)}, "crawlers.json": {Data: []byte(`[{"pattern": "Google(bot", "tags": []}]`)}}, "pattern that cannot be read"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if loaded, err := load(c.files); err == nil || loaded != nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want one that says %q", err, c.want)
			}
		})
	}

	// The same two files, whole, are rules.
	loaded, err := load(fstest.MapFS{"rules.json": {Data: []byte(rules)}, "crawlers.json": {Data: []byte(list)}})
	if err != nil {
		t.Fatal(err)
	}
	if got := from(loaded, "curl/8.5.0"); got.Class != ClassAutomation || got.Rule != "automation:curl" {
		t.Errorf("classified %s by %q, want automation by automation:curl", got.Class, got.Rule)
	}
	if got := from(loaded, "Googlebot/2.1"); got.Class != ClassCrawler || got.Rule != "crawler:Googlebot" {
		t.Errorf("classified %s by %q, want crawler by crawler:Googlebot", got.Class, got.Rule)
	}
}

// Ordinary browsers must not be flagged, and each says its device and its
// family.
func TestOrdinaryBrowsersArePresumedHuman(t *testing.T) {
	rules := classifier(t)
	const webkit = "AppleWebKit/537.36 (KHTML, like Gecko)"
	cases := []struct {
		name      string
		userAgent string
		device    Device
		browser   Browser
	}{
		{"Chrome on Windows", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) " + webkit + " Chrome/142.0.0.0 Safari/537.36", DeviceDesktop, BrowserChrome},
		{"Chrome on macOS", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) " + webkit + " Chrome/142.0.0.0 Safari/537.36", DeviceDesktop, BrowserChrome},
		{"Chrome on Linux", "Mozilla/5.0 (X11; Linux x86_64) " + webkit + " Chrome/142.0.0.0 Safari/537.36", DeviceDesktop, BrowserChrome},
		{"Chrome on ChromeOS", "Mozilla/5.0 (X11; CrOS x86_64 14541.0.0) " + webkit + " Chrome/142.0.0.0 Safari/537.36", DeviceDesktop, BrowserChrome},
		{"Chrome on an Android phone", "Mozilla/5.0 (Linux; Android 10; K) " + webkit + " Chrome/142.0.0.0 Mobile Safari/537.36", DeviceMobile, BrowserChrome},
		{"Chrome on an Android tablet", "Mozilla/5.0 (Linux; Android 10; K) " + webkit + " Chrome/142.0.0.0 Safari/537.36", DeviceTablet, BrowserChrome},
		{"Chrome on an iPhone", "Mozilla/5.0 (iPhone; CPU iPhone OS 18_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/142.0.0.0 Mobile/15E148 Safari/604.1", DeviceMobile, BrowserChrome},
		{"Safari on macOS", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.6 Safari/605.1.15", DeviceDesktop, BrowserSafari},
		{"Safari on an iPhone", "Mozilla/5.0 (iPhone; CPU iPhone OS 18_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.6 Mobile/15E148 Safari/604.1", DeviceMobile, BrowserSafari},
		{"Safari on an iPad that says so", "Mozilla/5.0 (iPad; CPU OS 18_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.6 Mobile/15E148 Safari/604.1", DeviceTablet, BrowserSafari},
		{"Firefox on Windows", "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:145.0) Gecko/20100101 Firefox/145.0", DeviceDesktop, BrowserFirefox},
		{"Firefox on Linux", "Mozilla/5.0 (X11; Linux x86_64; rv:145.0) Gecko/20100101 Firefox/145.0", DeviceDesktop, BrowserFirefox},
		{"Firefox on an Android phone", "Mozilla/5.0 (Android 15; Mobile; rv:145.0) Gecko/145.0 Firefox/145.0", DeviceMobile, BrowserFirefox},
		{"Firefox on an iPhone", "Mozilla/5.0 (iPhone; CPU iPhone OS 18_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) FxiOS/145.0 Mobile/15E148 Safari/605.1.15", DeviceMobile, BrowserFirefox},
		{"Edge on Windows", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) " + webkit + " Chrome/142.0.0.0 Safari/537.36 Edg/142.0.0.0", DeviceDesktop, BrowserEdge},
		{"Edge on an Android phone", "Mozilla/5.0 (Linux; Android 10; K) " + webkit + " Chrome/142.0.0.0 Mobile Safari/537.36 EdgA/142.0.0.0", DeviceMobile, BrowserEdge},
		{"Opera on Windows", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) " + webkit + " Chrome/142.0.0.0 Safari/537.36 OPR/126.0.0.0", DeviceDesktop, BrowserOpera},
		{"Samsung Internet on a phone", "Mozilla/5.0 (Linux; Android 10; K) " + webkit + " SamsungBrowser/29.0 Chrome/136.0.0.0 Mobile Safari/537.36", DeviceMobile, BrowserSamsungInternet},
		{"Silk on a Kindle", "Mozilla/5.0 (Linux; Android 11; KFTRWI) " + webkit + " Silk/140.2.1 like Chrome/140.0.0.0 Safari/537.36", DeviceTablet, BrowserChrome},
		{"an application nobody declared", "LinkReader/3.2 (build 418)", DeviceOther, BrowserOther},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := from(rules, c.userAgent)
			wellFormed(t, rules, got)
			if got.Class != ClassPresumedHuman || got.Rule != "presumed_human" {
				t.Errorf("classified %s by %q, want presumed_human", got.Class, got.Rule)
			}
			if got.Device != c.device || got.Browser != c.browser {
				t.Errorf("device %s and browser %s, want %s and %s", got.Device, got.Browser, c.device, c.browser)
			}
		})
	}
}

// What a request says of itself comes before what its user agent is.
func TestTheRequestIsLookedAtBeforeTheUserAgent(t *testing.T) {
	rules := classifier(t)
	const browser = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36"
	const crawler = "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)"
	cases := []struct {
		name      string
		method    string
		header    http.Header
		wantClass Class
		wantRule  string
	}{
		{"a browser that fetches ahead", http.MethodGet, http.Header{"User-Agent": {browser}, "Sec-Purpose": {"prefetch"}}, ClassPrefetch, "prefetch:sec-purpose"},
		{"a browser that renders ahead", http.MethodGet, http.Header{"User-Agent": {browser}, "Sec-Purpose": {"prefetch;prerender"}}, ClassPrefetch, "prefetch:sec-purpose"},
		{"the older header", http.MethodGet, http.Header{"User-Agent": {browser}, "Purpose": {"prefetch"}}, ClassPrefetch, "prefetch:purpose"},
		{"the header of another browser", http.MethodGet, http.Header{"User-Agent": {browser}, "X-Moz": {"Prefetch"}}, ClassPrefetch, "prefetch:x-moz"},
		{"a crawler that fetches ahead", http.MethodGet, http.Header{"User-Agent": {crawler}, "Sec-Purpose": {"prefetch"}}, ClassPrefetch, "prefetch:sec-purpose"},
		{"a prefetch by its headers alone", http.MethodHead, http.Header{"User-Agent": {browser}, "Sec-Purpose": {"prefetch"}}, ClassPrefetch, "prefetch:sec-purpose"},
		{"a purpose that is not a prefetch", http.MethodGet, http.Header{"User-Agent": {browser}, "Sec-Purpose": {"navigate"}}, ClassPresumedHuman, "presumed_human"},
		{"a browser that asks for the headers alone", http.MethodHead, http.Header{"User-Agent": {browser}}, ClassAutomation, "automation:head"},
		{"a previewer that asks for the headers alone", http.MethodHead, http.Header{"User-Agent": {"WhatsApp/2.23.20.0"}}, ClassAutomation, "automation:head"},
		{"no user agent", http.MethodGet, http.Header{}, ClassAutomation, "automation:no-user-agent"},
		{"an empty user agent", http.MethodGet, http.Header{"User-Agent": {""}}, ClassAutomation, "automation:no-user-agent"},
		{"a user agent of spaces", http.MethodGet, http.Header{"User-Agent": {"   "}}, ClassAutomation, "automation:no-user-agent"},
		{"a browser", http.MethodGet, http.Header{"User-Agent": {browser}}, ClassPresumedHuman, "presumed_human"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := rules.Classify(c.method, c.header)
			wellFormed(t, rules, got)
			if got.Class != c.wantClass || got.Rule != c.wantRule {
				t.Errorf("classified %s by %q, want %s by %q", got.Class, got.Rule, c.wantClass, c.wantRule)
			}
		})
	}
	// The device and the family are what the user agent says, whatever the
	// class.
	ahead := rules.Classify(http.MethodGet, http.Header{"User-Agent": {browser}, "Sec-Purpose": {"prefetch"}})
	if ahead.Device != DeviceDesktop || ahead.Browser != BrowserChrome {
		t.Errorf("a prefetch from a desktop Chrome says %s and %s", ahead.Device, ahead.Browser)
	}
}

// The rules have a version, and the events stored carry it. A change to the
// list or to the project's own rules without a new version would make two
// periods look comparable when they are not: VERSIONS holds the checksums
// of both files for each version, and this fails until a changed file comes
// with a new line.
func TestTheRulesDoNotChangeWithoutANewVersion(t *testing.T) {
	recorded, err := os.ReadFile("VERSIONS")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(recorded)), "\n")
	for i, line := range lines {
		if fields := strings.Fields(line); len(fields) != 3 || fields[0] != strconv.Itoa(i+1) {
			t.Fatalf("line %d of VERSIONS is %q, want the version %d and two checksums", i+1, line, i+1)
		}
	}
	sum := func(name string) string {
		t.Helper()
		data, err := embedded.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(data)
		return hex.EncodeToString(hash[:])
	}
	last := strings.Fields(lines[len(lines)-1])
	if version := classifier(t).Version(); last[0] != strconv.Itoa(version) {
		t.Errorf("rules.json is at version %d and the last line of VERSIONS at %s", version, last[0])
	}
	if last[1] != sum("rules.json") || last[2] != sum("crawlers.json") {
		t.Error("rules.json or crawlers.json changed: raise the version in rules.json and add its line to VERSIONS")
	}
}

// What is remembered of the user agents is bounded, and remembering changes
// no answer.
func TestTheCacheIsBoundedAndChangesNothing(t *testing.T) {
	rules := classifier(t)
	const crawler = "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)"
	first := from(rules, crawler)
	held := func() int {
		rules.mu.RLock()
		defer rules.mu.RUnlock()
		return len(rules.cached)
	}
	// The cache is filled by hand: classifying ten thousand user agents
	// against the whole list is what it exists to avoid.
	rules.mu.Lock()
	for i := range maxCached - 1 {
		rules.cached["LinkReader/3.2 (build "+strconv.Itoa(i)+")"] = agent{
			class: ClassPresumedHuman, rule: string(ClassPresumedHuman), device: DeviceOther, browser: BrowserOther,
		}
	}
	rules.mu.Unlock()
	if held() != maxCached {
		t.Fatalf("the cache holds %d user agents, want %d", held(), maxCached)
	}
	if again := from(rules, crawler); again != first || held() != maxCached {
		t.Errorf("a user agent already seen is classified %+v, with %d held; want %+v and %d", again, held(), first, maxCached)
	}
	// One more, and the cache starts again.
	from(rules, "LinkReader/3.2 (one more)")
	if held() != 1 {
		t.Errorf("after one user agent too many the cache holds %d, want 1", held())
	}
	if again := from(rules, crawler); again != first {
		t.Errorf("after the cache was emptied the crawler is classified %+v, want %+v", again, first)
	}
}

// A user agent is read up to a length: a request cannot buy work with a
// long header, and what is past the length does not count.
func TestALongUserAgentIsClassifiedByItsBeginning(t *testing.T) {
	rules := classifier(t)
	padding := strings.Repeat("a", maxUserAgent)
	if got := from(rules, "Googlebot/2.1 "+padding); got.Class != ClassCrawler {
		t.Errorf("a crawler at the beginning of a long user agent is %s", got.Class)
	}
	if got := from(rules, padding+" Googlebot/2.1"); got.Class != ClassPresumedHuman {
		t.Errorf("a crawler past the length that is read is %s by %q", got.Class, got.Rule)
	}
	rules.mu.RLock()
	defer rules.mu.RUnlock()
	for userAgent := range rules.cached {
		if len(userAgent) > maxUserAgent {
			t.Errorf("the cache keeps a user agent of %d bytes", len(userAgent))
		}
	}
}

// Requests are classified from many goroutines at once. The race detector
// is the assertion.
func TestClassifyingAtTheSameTime(t *testing.T) {
	rules := classifier(t)
	var wait sync.WaitGroup
	for worker := range 8 {
		wait.Go(func() {
			for i := range 500 {
				got := from(rules, fmt.Sprintf("LinkReader/3.2 (build %d)", (worker*i)%50))
				if got.Class != ClassPresumedHuman {
					t.Errorf("classified %s", got.Class)
					return
				}
			}
		})
	}
	wait.Wait()
}

// Whatever a request carries, it gets one class of the enumeration and a
// rule that says which.
func FuzzClassify(f *testing.F) {
	rules := classifier(f)
	f.Add("GET", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36", "")
	f.Add("HEAD", "curl/8.5.0", "prefetch")
	f.Add("GET", "", "prefetch;prerender")
	f.Add("POST", strings.Repeat("Googlebot", 100), "x")
	f.Fuzz(func(t *testing.T, method, userAgent, purpose string) {
		got := rules.Classify(method, http.Header{"User-Agent": {userAgent}, "Sec-Purpose": {purpose}})
		wellFormed(t, rules, got)
	})
}

// BenchmarkClassify measures the classification of a browser user agent
// that was never seen, which is tried against every entry of the list, and
// of one that was. Run it with
//
//	go test -run '^$' -bench Classify ./internal/classify/
func BenchmarkClassify(b *testing.B) {
	rules := classifier(b)
	const browser = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36"
	b.Run("never seen", func(b *testing.B) {
		i := 0
		for b.Loop() {
			from(rules, browser+" build/"+strconv.Itoa(i))
			i++
		}
	})
	b.Run("seen", func(b *testing.B) {
		for b.Loop() {
			from(rules, browser)
		}
	})
}
