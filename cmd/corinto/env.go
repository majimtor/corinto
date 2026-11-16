package main

import (
	"fmt"
	"strings"
)

// environment reads the configuration that differs between installations:
// addresses, hosts and credentials. A value that is missing stops the start,
// and the error names every variable that is missing, not only the first.
type environment struct {
	getenv  func(string) string
	missing []string
}

// require returns the value of a variable and remembers its name when the
// value is empty.
func (e *environment) require(name string) string {
	value := e.getenv(name)
	if value == "" {
		e.missing = append(e.missing, name)
	}
	return value
}

// err reports the variables that were required and not set.
func (e *environment) err() error {
	if len(e.missing) == 0 {
		return nil
	}
	return fmt.Errorf("missing environment variables: %s", strings.Join(e.missing, ", "))
}
