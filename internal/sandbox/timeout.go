package sandbox

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// TimeoutOption is the deadline of one remote command: exec's and run's
// command, cp's remote tar, or diff's script. Commands embed it, so
// the field, its default, and its meaning are written once.
//
//nolint:lll // A struct tag is one literal that cannot be wrapped, and the schema description is long.
type TimeoutOption struct {
	Timeout string `flag:"timeout" json:"timeout,omitempty" jsonschema:"deadline for the remote command as a Go duration (default 2h); 0 disables it"`

	timeout time.Duration
}

// errTimeout is the cause of a context whose timeout expired. Only it maps
// to 124; other deadlines, such as the availability wait, are failures.
var errTimeout = errors.New("timeout expired")

// parse applies the default and checks the syntax.
func (o *TimeoutOption) parse() error {
	var err error

	o.timeout, err = duration("timeout", o.Timeout, DefaultTimeout)

	return err
}

// deadline returns ctx bounded by the timeout. Expiry has the cause
// errTimeout.
func (o *TimeoutOption) deadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if o.timeout <= 0 {
		return context.WithCancel(ctx)
	}

	return context.WithTimeoutCause(ctx, o.timeout, errTimeout)
}

// explain returns err, or, when err is the timeout's, err with a message
// that what, such as "the diff", did not finish in time.
func (o *TimeoutOption) explain(what string, err error) error {
	if !errors.Is(err, errTimeout) {
		return err
	}

	return fmt.Errorf("%s did not finish within the %s timeout: %w", what, FormatDuration(o.timeout), err)
}
