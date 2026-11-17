// Package limits holds the initial limits and quotas of PLAN.md section 9,
// one constant per value. They are code and not configuration: they do not
// differ between installations, so a change to one is a change that is
// reviewed, and nothing can start with a value nobody chose.
package limits

// The quotas a workspace is created with. They are the initial values: the
// operator can give a workspace others when creating it.
const (
	// MaxLinks is how many links a workspace holds, deleted ones not
	// counted. It bounds what one workspace adds to the copy of the links
	// that the redirect keeps in memory.
	MaxLinks = 1000
	// MaxLinksDemo is the same quota for a demo workspace, which anyone can
	// enter without a secret: what a stranger can create is kept small.
	MaxLinksDemo = 100
	// MaxKeys is how many keys a workspace holds, revoked ones not counted.
	// It bounds the credentials in force that a workspace has to keep track
	// of.
	MaxKeys = 10
)
