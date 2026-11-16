package link

// reserved holds the words no workspace can take as an alias. The grammar of
// an identifier already keeps an alias from colliding with a path the
// redirect host serves, so the list guards against two other things: a path
// the host may serve one day, and a word that on a short domain reads as a
// page of the service itself. It is short on purpose; adding a word later
// breaks nothing.
var reserved = set(
	// Paths the redirect host serves or may come to serve.
	"admin", "api", "assets", "favicon", "health", "livez", "metrics",
	"readyz", "robots", "sitemap", "static", "status", "well-known",

	// Words that read as a page of the service, where a visitor would trust
	// a form.
	"account", "auth", "billing", "help", "login", "logout", "password",
	"payment", "register", "security", "signin", "signup", "support",
	"verify",

	// The project and the documents a site is expected to have.
	"about", "abuse", "contact", "corinto", "docs", "legal", "privacy",
	"report", "terms", "www",
)

func set(words ...string) map[string]struct{} {
	entries := make(map[string]struct{}, len(words))
	for _, word := range words {
		entries[word] = struct{}{}
	}
	return entries
}
