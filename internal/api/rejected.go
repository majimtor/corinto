package api

import (
	"errors"
	"regexp"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
)

// keywords says why a value is rejected when it fails a keyword of its
// schema in the contract. A keyword that is not here is "invalid".
var keywords = map[string]Reason{
	"required":    ReasonRequired,
	"properties":  ReasonUnknown,
	"minLength":   ReasonTooShort,
	"minItems":    ReasonTooShort,
	"maxLength":   ReasonTooLong,
	"maxItems":    ReasonTooMany,
	"uniqueItems": ReasonDuplicate,
}

// unsupported is how the library that reads the contract names a property
// the schema does not have. The name is nowhere else in its error. If the
// wording changes the entry still says "unknown", of the object instead of
// the property, and a test notices.
var unsupported = regexp.MustCompile(`^property "(.*)" is unsupported$`)

// rejections turns what the validation of the contract found in a request
// into the entries of the error. Only the place and the keyword of each
// fault are read: the messages of the library repeat the value that was
// sent, and nothing the client sent goes back out or into a log.
func rejections(err error) []rejection {
	var found []rejection
	for _, fault := range flatten(err) {
		var request *openapi3filter.RequestError
		if !errors.As(fault, &request) {
			continue
		}
		switch {
		case request.Parameter != nil && request.Parameter.In == openapi3.ParameterInQuery:
			for _, reason := range reasonsOf(request.Err) {
				found = append(found, rejection{In: inQuery, Field: request.Parameter.Name, Code: reason.code})
			}
		case request.RequestBody != nil:
			for _, reason := range reasonsOf(request.Err) {
				found = append(found, rejection{In: inBody, Field: reason.pointer, Code: reason.code})
			}
		}
	}
	if len(found) == 0 {
		// Something the contract refuses and this cannot place: the whole
		// request is what is wrong.
		return []rejection{{In: inBody, Field: "", Code: ReasonInvalid}}
	}
	return found
}

// reason is why a value failed, and where it is inside what was validated.
type reason struct {
	pointer string
	code    Reason
}

// reasonsOf reads the faults of one parameter or of the body.
func reasonsOf(err error) []reason {
	var found []reason
	for _, fault := range flatten(err) {
		var schema *openapi3.SchemaError
		switch {
		case errors.Is(fault, openapi3filter.ErrInvalidRequired):
			found = append(found, reason{code: ReasonRequired})
		case errors.As(fault, &schema):
			found = append(found, schemaReason(schema))
		default:
			// It could not be read as what the contract says it is.
			found = append(found, reason{code: ReasonInvalid})
		}
	}
	return found
}

func schemaReason(fault *openapi3.SchemaError) reason {
	path := fault.JSONPointer()
	code, known := keywords[fault.SchemaField]
	if !known {
		code = ReasonInvalid
	}
	if code == ReasonUnknown {
		if property := unsupported.FindStringSubmatch(fault.Reason); property != nil {
			path = append(path, property[1])
		}
	}
	return reason{pointer: pointer(path), code: code}
}

// pointer writes a path as a JSON Pointer (RFC 6901).
func pointer(path []string) string {
	var written strings.Builder
	for _, part := range path {
		written.WriteByte('/')
		written.WriteString(strings.NewReplacer("~", "~0", "/", "~1").Replace(part))
	}
	return written.String()
}

// flatten returns the faults inside an error of the library, which nests
// lists of them.
func flatten(err error) []error {
	//nolint:errorlint // only a list itself is opened: an error that wraps a list keeps its place
	list, isList := err.(openapi3.MultiError)
	if !isList {
		if err == nil {
			return nil
		}
		return []error{err}
	}
	var faults []error
	for _, inner := range list {
		faults = append(faults, flatten(inner)...)
	}
	return faults
}
