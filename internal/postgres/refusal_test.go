package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestARefusalIsAnAnswerAboutWhatTheDatabaseWasGiven(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want Refusal
		ok   bool
	}{
		{"a value the database cannot read", &pgconn.PgError{Code: "22P02", Message: `invalid input syntax for type uuid: "sentinel"`}, Refusal{Code: "22P02"}, true},
		{"a broken check", &pgconn.PgError{Code: "23514", ConstraintName: "event_inbox_outcome_check", Detail: "Failing row contains (sentinel)."}, Refusal{Code: "23514", Constraint: "event_inbox_outcome_check"}, true},
		{"a broken reference, wrapped", fmt.Errorf("insert an event: %w", &pgconn.PgError{Code: "23503", ConstraintName: "event_inbox_link_id_fkey"}), Refusal{Code: "23503", Constraint: "event_inbox_link_id_fkey"}, true},
		{"a deadlock", &pgconn.PgError{Code: "40P01"}, Refusal{}, false},
		{"a transaction that has to be tried again", &pgconn.PgError{Code: "40001"}, Refusal{}, false},
		{"a statement that ran out of time", &pgconn.PgError{Code: "57014"}, Refusal{}, false},
		{"a session the database ended", &pgconn.PgError{Code: "25P03"}, Refusal{}, false},
		{"too many connections", &pgconn.PgError{Code: "53300"}, Refusal{}, false},
		{"a database that is away", errors.New("dial tcp 127.0.0.1:5432: connect: connection refused"), Refusal{}, false},
		{"a caller that stopped waiting", context.DeadlineExceeded, Refusal{}, false},
		{"no error", nil, Refusal{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := Refused(c.err)
			if got != c.want || ok != c.ok {
				t.Errorf("Refused = %+v, %t; want %+v, %t", got, ok, c.want, c.ok)
			}
		})
	}
}
