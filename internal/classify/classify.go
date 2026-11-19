// Package classify gives every request to a short link one traffic class,
// by rules in a fixed order where the first match wins
// (docs/adr/0009-traffic-classification-by-versioned-rules.md). The rules
// are data: a list of crawlers kept by others, and a short file of the
// project's own. Together they have a version, and every classification
// names the rule that produced it.
package classify

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// Class is what a request is taken for.
type Class string

// The classes, in the order in which their rules are tried
// (PLAN.md section 5).
const (
	// ClassPrefetch is a request that says it fetches a page ahead of a
	// visit that may not happen.
	ClassPrefetch Class = "prefetch"
	// ClassAutomation is a request without a body to read, without a user
	// agent, or from an HTTP library or an automated browser.
	ClassAutomation Class = "automation"
	// ClassPreview is a messaging or social application drawing the card
	// of a link.
	ClassPreview Class = "preview"
	// ClassCrawler is a search engine, a monitor, a scanner, an archiver or
	// any other crawler that says what it is.
	ClassCrawler Class = "crawler"
	// ClassPresumedHuman is everything else. It is an estimate by excess:
	// whatever presents itself as a browser counts.
	ClassPresumedHuman Class = "presumed_human"
)

// Device is the kind of device a user agent says it runs on.
type Device string

// The devices. The enumeration is closed: a value can be added and none
// taken away, because the events already stored carry them.
const (
	DeviceDesktop Device = "desktop"
	DeviceMobile  Device = "mobile"
	DeviceTablet  Device = "tablet"
	DeviceOther   Device = "other"
)

// Browser is the family of browsers a user agent says it is.
type Browser string

// The browser families. Closed like the devices.
const (
	BrowserChrome          Browser = "chrome"
	BrowserSafari          Browser = "safari"
	BrowserFirefox         Browser = "firefox"
	BrowserEdge            Browser = "edge"
	BrowserOpera           Browser = "opera"
	BrowserSamsungInternet Browser = "samsung_internet"
	BrowserOther           Browser = "other"
)

// Classification is what the rules say of one request.
type Classification struct {
	Class Class
	// Rule names the rule that matched, so that any count can be traced to
	// it: the class, and after a colon what in the request it matched.
	Rule string
	// Version is the version of the rules that classified the request.
	Version int
	Device  Device
	Browser Browser
}

const (
	// maxUserAgent is how much of a user agent is read. The longest one in
	// the list of crawlers has 285 bytes; a longer one is classified by its
	// beginning, so that a request cannot buy work with the length of a
	// header.
	maxUserAgent = 512
	// maxCached bounds the user agents whose classification is remembered.
	// When it is reached the cache is emptied whole: most traffic comes
	// from a few user agents, which fill it again at once, and nothing has
	// to be kept about which one is the oldest.
	maxCached = 10_000
)

// The rules. crawlers.json is derived from a list kept by others; its
// notice is next to it.
//
//go:embed rules.json crawlers.json
var embedded embed.FS

// own is the file of the project's own rules.
type own struct {
	Version         int      `json:"version"`
	PrefetchHeaders []string `json:"prefetch_headers"`
	AutomationTags  []string `json:"automation_tags"`
	PreviewTags     []string `json:"preview_tags"`
	Browsers        []trait  `json:"browsers"`
	Devices         []trait  `json:"devices"`
}

// trait is one rule about what a user agent says of itself: it holds when
// the user agent contains any of some texts and none of others.
type trait struct {
	Value string   `json:"value"`
	Any   []string `json:"any"`
	None  []string `json:"none"`
}

func (t trait) holds(userAgent string) bool {
	contains := func(text string) bool { return strings.Contains(userAgent, text) }
	return slices.ContainsFunc(t.Any, contains) && !slices.ContainsFunc(t.None, contains)
}

// declared is one entry of the list of crawlers, ready to be tried.
type declared struct {
	class   Class
	rule    string
	pattern *regexp.Regexp
}

// agent is what the rules say of a user agent, whatever the request.
type agent struct {
	class   Class
	rule    string
	device  Device
	browser Browser
}

// Classifier holds the rules, compiled, and remembers what it said of the
// user agents it has seen.
type Classifier struct {
	version  int
	prefetch []string
	// declared holds the entries of the list in the order in which they
	// are tried: automation, then previews, then crawlers.
	declared []declared
	browsers []trait
	devices  []trait

	mu     sync.RWMutex
	cached map[string]agent
}

// New reads the rules that the binary embeds.
func New() (*Classifier, error) {
	return load(embedded)
}

// load reads the two files of rules. A rule that cannot be read is an
// error: a classifier with part of its rules would count wrongly without
// saying so.
func load(files fs.FS) (*Classifier, error) {
	var rules own
	if err := read(files, "rules.json", &rules); err != nil {
		return nil, err
	}
	var list []struct {
		Pattern string   `json:"pattern"`
		Tags    []string `json:"tags"`
	}
	if err := read(files, "crawlers.json", &list); err != nil {
		return nil, err
	}
	if rules.Version < 1 {
		return nil, errors.New("rules.json has no version")
	}

	classifier := &Classifier{
		version: rules.Version, prefetch: rules.PrefetchHeaders,
		browsers: rules.Browsers, devices: rules.Devices,
		cached: make(map[string]agent),
	}
	for _, browser := range rules.Browsers {
		if !slices.Contains(browsers(), Browser(browser.Value)) {
			return nil, fmt.Errorf("rules.json names the browser %q, which is not one of the families", browser.Value)
		}
	}
	for _, device := range rules.Devices {
		if !slices.Contains(devices(), Device(device.Value)) {
			return nil, fmt.Errorf("rules.json names the device %q, which is not one of the devices", device.Value)
		}
	}
	// The classes are tried in their order, so the entries are too: one
	// that is an HTTP library is automation before it is a crawler.
	byClass := make(map[Class][]declared)
	for _, entry := range list {
		pattern, err := regexp.Compile(entry.Pattern)
		if err != nil {
			return nil, fmt.Errorf("crawlers.json has a pattern that cannot be read: %w", err)
		}
		class := ClassCrawler
		switch {
		case slices.ContainsFunc(entry.Tags, func(tag string) bool { return slices.Contains(rules.AutomationTags, tag) }):
			class = ClassAutomation
		case slices.ContainsFunc(entry.Tags, func(tag string) bool { return slices.Contains(rules.PreviewTags, tag) }):
			class = ClassPreview
		}
		byClass[class] = append(byClass[class], declared{class: class, rule: string(class) + ":" + entry.Pattern, pattern: pattern})
	}
	classifier.declared = slices.Concat(byClass[ClassAutomation], byClass[ClassPreview], byClass[ClassCrawler])
	return classifier, nil
}

func read(files fs.FS, name string, into any) error {
	data, err := fs.ReadFile(files, name)
	if err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}
	if err := json.Unmarshal(data, into); err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}
	return nil
}

// browsers and devices return the two closed enumerations.
func browsers() []Browser {
	return []Browser{BrowserChrome, BrowserSafari, BrowserFirefox, BrowserEdge, BrowserOpera, BrowserSamsungInternet, BrowserOther}
}

func devices() []Device {
	return []Device{DeviceDesktop, DeviceMobile, DeviceTablet, DeviceOther}
}

// Version returns the version of the rules.
func (c *Classifier) Version() int { return c.version }

// Classify says what a request is, from its method and its headers alone.
// The user agent is read here and kept nowhere but as the key of a bounded
// cache.
func (c *Classifier) Classify(method string, header http.Header) Classification {
	userAgent := header.Get("User-Agent")
	if len(userAgent) > maxUserAgent {
		userAgent = userAgent[:maxUserAgent]
	}
	said := c.agent(userAgent)
	classified := Classification{
		Class: said.class, Rule: said.rule, Version: c.version, Device: said.device, Browser: said.browser,
	}
	// What the request says of itself comes before what its user agent is:
	// a browser that fetches ahead is a prefetch, and anything that asks
	// only for the headers is not a person reading a page.
	for _, name := range c.prefetch {
		if strings.Contains(strings.ToLower(header.Get(name)), "prefetch") {
			classified.Class, classified.Rule = ClassPrefetch, string(ClassPrefetch)+":"+strings.ToLower(name)
			return classified
		}
	}
	if method == http.MethodHead {
		classified.Class, classified.Rule = ClassAutomation, string(ClassAutomation)+":head"
	}
	return classified
}

// agent says what a user agent is, from the cache when it has been seen.
func (c *Classifier) agent(userAgent string) agent {
	c.mu.RLock()
	said, seen := c.cached[userAgent]
	c.mu.RUnlock()
	if seen {
		return said
	}

	said = agent{class: ClassPresumedHuman, rule: string(ClassPresumedHuman), device: DeviceOther, browser: BrowserOther}
	for _, device := range c.devices {
		if device.holds(userAgent) {
			said.device = Device(device.Value)
			break
		}
	}
	for _, browser := range c.browsers {
		if browser.holds(userAgent) {
			said.browser = Browser(browser.Value)
			break
		}
	}
	if strings.TrimSpace(userAgent) == "" {
		said.class, said.rule = ClassAutomation, string(ClassAutomation)+":no-user-agent"
	} else {
		for _, entry := range c.declared {
			if entry.pattern.MatchString(userAgent) {
				said.class, said.rule = entry.class, entry.rule
				break
			}
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.cached) >= maxCached {
		c.cached = make(map[string]agent)
	}
	c.cached[userAgent] = said
	return said
}
