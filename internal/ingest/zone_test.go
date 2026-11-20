package ingest

import (
	"testing"
	"time"
)

// Every test of this package runs eight hours behind UTC: the hours and
// the days the code works out are the ones of UTC wherever the process is,
// and a zone that is not UTC is what shows when they are not.
//
// The zone is set here, once, before any test and any goroutine. A test
// that set it for itself, and set it back when it ended, wrote a variable
// that the connection pools of the other tests read.
func init() {
	time.Local = time.FixedZone("UTC-8", -8*60*60)
}

func TestTheTestsOfThisPackageRunBehindUTC(t *testing.T) {
	if name, offset := time.Now().Zone(); offset != -8*60*60 {
		t.Fatalf("the tests run in %s, %d seconds from UTC; want eight hours behind it", name, offset)
	}
}
