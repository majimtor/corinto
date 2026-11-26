package panel

import (
	"testing"
)

func TestTheFormOfTheCookieIsTheOneWithSecureUnlessTheOtherIsAskedFor(t *testing.T) {
	for written, want := range map[string]CookieForm{"": CookieSecure, "secure": CookieSecure, "loopback": CookieLoopback} {
		if got, err := ParseCookieForm(written); err != nil || got != want {
			t.Errorf("ParseCookieForm(%q) = %q, %v; want %q", written, got, err, want)
		}
	}
	for _, written := range []string{"Secure", "LOOPBACK", "insecure", "plain", "none", " secure", "true", "0"} {
		if got, err := ParseCookieForm(written); err == nil {
			t.Errorf("ParseCookieForm(%q) = %q, want an error", written, got)
		}
	}
	// A panel is not built with a form that is neither.
	for _, form := range []CookieForm{"", "plain"} {
		if _, err := New(t.Context(), Settings{Cookie: form}); err == nil {
			t.Errorf("a panel was built with the form %q of the cookie", form)
		}
	}
}

func TestOnlyLocalhostAndTheLoopbackAddressesAreLoopbackNames(t *testing.T) {
	for host, want := range map[string]bool{
		"localhost":             true,
		"localhost:8080":        true,
		"LOCALHOST:8080":        true,
		"127.0.0.1":             true,
		"127.0.0.1:8080":        true,
		"127.8.9.10:80":         true,
		"[::1]":                 true,
		"[::1]:8080":            true,
		"[::ffff:127.0.0.1]:80": true,

		"":                           false,
		"example.com":                false,
		"example.com:8080":           false,
		"localhost.example.com":      false,
		"localhost.example.com:8080": false,
		"notlocalhost":               false,
		"192.0.2.10":                 false,
		"192.0.2.10:8080":            false,
		"10.0.0.1:8080":              false,
		"0.0.0.0:8080":               false,
		"[2001:db8::1]:8080":         false,
		"[::]:8080":                  false,
		"127.0.0.1.example.com":      false,
		"127.0.0.1:8080@localhost":   false,
		"127.0.0.1:":                 false,
		"localhost:http":             false,
	} {
		if got := loopback(host); got != want {
			t.Errorf("loopback(%q) = %t, want %t", host, got, want)
		}
	}
}

func TestTheLanguageOfTheDoorIsTheOneTheBrowserAsksForTheMost(t *testing.T) {
	for header, want := range map[string]language{
		"":                               spanish,
		"es":                             spanish,
		"en":                             english,
		"en-US,en;q=0.9":                 english,
		"es-MX,es;q=0.9,en;q=0.8":        spanish,
		"en;q=0.8,es":                    spanish,
		"es;q=0.5,en;q=0.9":              english,
		"fr-FR,fr;q=0.9":                 spanish,
		"fr,en;q=0.3":                    english,
		"de, en-GB;q=0.7, es;q=0.7":      english,
		"*":                              spanish,
		"EN":                             english,
		" en ; q=0.4 , es ; q=0.2":       english,
		"en;q=0,es;q=0":                  spanish,
		"en;q=0":                         spanish,
		"en;q=nonsense":                  spanish,
		"en;q=7":                         spanish,
		"english":                        spanish,
		"es;q=0.3,en;q=nonsense,fr;q=1":  spanish,
		"fr;q=1,en;q=0.2,es;q=0.1,de":    english,
		"en-GB-oxendict":                 english,
		",,;q=1,en":                      english,
		"en;level=1":                     english,
		"es;q=0.9;other=1,en;q=0.8":      spanish,
		"es;q=0.9 ,en;q=0.95, en;q=0.1 ": english,
	} {
		if got := preferred(header); got != want {
			t.Errorf("preferred(%q) = %q, want %q", header, got, want)
		}
	}
}
