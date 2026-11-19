package postgres

import (
	"errors"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// Refusal is the answer of the database that it will not take what it was
// given: a value it cannot read, or one that breaks a constraint. Trying
// the same again gets the same answer, which tells it from a database that
// is away, a deadlock or a statement that ran out of time.
type Refusal struct {
	// Code is the SQLSTATE of the answer.
	Code string
	// Constraint is the constraint the answer names, when it names one.
	Constraint string
}

// Refused returns the refusal that an error carries, if it carries one:
// an answer of the database with a SQLSTATE of the class of the data
// exceptions, 22, or of the integrity constraint violations, 23. The text
// and the detail of the answer are left out on purpose: they quote the row.
func Refused(err error) (Refusal, bool) {
	var answer *pgconn.PgError
	if !errors.As(err, &answer) {
		return Refusal{}, false
	}
	if !strings.HasPrefix(answer.Code, "22") && !strings.HasPrefix(answer.Code, "23") {
		return Refusal{}, false
	}
	return Refusal{Code: answer.Code, Constraint: answer.ConstraintName}, true
}
