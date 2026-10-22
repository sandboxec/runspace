package sandbox

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Sandbox is one entry of ls.
//
//nolint:tagliatelle // The JSON names are the documented result fields of ls.
type Sandbox struct {
	Name               string     `json:"name"`
	State              string     `json:"state"`
	Repository         string     `json:"repository"`
	Machine            string     `json:"machine"`
	CreatedAt          time.Time  `json:"created_at"`
	RetentionExpiresAt *time.Time `json:"retention_expires_at"`
}

// List returns every sandbox runspace created for this identity.
func (s *Sandboxes) List(ctx context.Context, _ ListOptions) ([]Sandbox, error) {
	all, err := s.api.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	sandboxes := []Sandbox{}

	for i := range all {
		codespace := &all[i]
		if !isRunspace(codespace) {
			continue
		}

		entry := Sandbox{
			Name:               codespace.Name,
			State:              codespace.State,
			Repository:         codespace.Repository.FullName,
			Machine:            "",
			CreatedAt:          codespace.CreatedAt,
			RetentionExpiresAt: codespace.RetentionExpiresAt,
		}
		if codespace.Machine != nil {
			entry.Machine = codespace.Machine.Name
		}

		sandboxes = append(sandboxes, entry)
	}

	return sandboxes, nil
}

// Delete deletes the named sandboxes, or every one with opts.All. It closes
// any cached Conn first, keeps going after a failure, and returns the names
// it deleted.
func (s *Sandboxes) Delete(ctx context.Context, opts DeleteOptions) ([]string, error) {
	err := opts.Validate()
	if err != nil {
		return nil, err
	}

	var (
		names []string
		errs  []error
	)

	if opts.All {
		names, err = s.allNames(ctx)
		if err != nil {
			return nil, err
		}
	} else {
		names, errs = s.taggedNames(ctx, opts.Names)
	}

	deleted := []string{}

	for _, name := range names {
		s.forget(name)

		err = s.api.Delete(ctx, name)
		if err != nil {
			errs = append(errs, err)

			continue
		}

		deleted = append(deleted, name)
	}

	return deleted, errors.Join(errs...)
}

// allNames returns the names of every sandbox runspace created.
func (s *Sandboxes) allNames(ctx context.Context) ([]string, error) {
	sandboxes, err := s.List(ctx, ListOptions{})
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(sandboxes))
	for _, entry := range sandboxes {
		names = append(names, entry.Name)
	}

	return names, nil
}

// taggedNames returns those of names that runspace created, and an error
// for each other.
func (s *Sandboxes) taggedNames(ctx context.Context, names []string) ([]string, []error) {
	var (
		tagged []string
		errs   []error
	)

	for _, name := range names {
		_, err := s.tagged(ctx, name)
		if err != nil {
			errs = append(errs, err)

			continue
		}

		tagged = append(tagged, name)
	}

	return tagged, errs
}
