// Package secrets resolves secret references such as env://NAME and
// file:///path into their values.
//
// Brand manifests and the brands table hold references, never values. v1
// supports two schemes and refuses every other one, so a value pasted where a
// reference belongs is rejected instead of being stored:
//
//	env://NAME        the environment variable NAME
//	file:///path      the content of an absolute file path, with one trailing
//	                  newline removed
//
// Errors name the reference but never the value.
package secrets

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// MaxFileBytes caps the size of a file secret.
const MaxFileBytes = 64 << 10

// ErrUnsupported is returned for a reference that is not env:// or file://.
var ErrUnsupported = errors.New("unsupported secret reference: use env://NAME or file:///absolute/path")

// ErrNotFound is returned when the variable is unset or empty, or the file is
// missing or empty.
var ErrNotFound = errors.New("secret not found")

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// Ref is a parsed secret reference.
type Ref struct {
	Scheme string // "env" or "file"
	Name   string // variable name or absolute path
}

// String returns the reference in its canonical form.
func (r Ref) String() string {
	if r.Scheme == "file" {
		return "file://" + r.Name
	}
	return r.Scheme + "://" + r.Name
}

// Parse validates a reference without resolving it.
func Parse(ref string) (Ref, error) {
	switch {
	case strings.HasPrefix(ref, "env://"):
		name := strings.TrimPrefix(ref, "env://")
		if !envName.MatchString(name) {
			return Ref{}, fmt.Errorf("secret reference %q: invalid environment variable name", redact(ref))
		}
		return Ref{Scheme: "env", Name: name}, nil
	case strings.HasPrefix(ref, "file://"):
		path := strings.TrimPrefix(ref, "file://")
		if !strings.HasPrefix(path, "/") || filepath.Clean(path) != path || strings.ContainsRune(path, 0) {
			return Ref{}, fmt.Errorf("secret reference %q: file references need a clean absolute path, file:///path", redact(ref))
		}
		return Ref{Scheme: "file", Name: path}, nil
	default:
		return Ref{}, ErrUnsupported
	}
}

// IsReference reports whether ref parses as a supported reference.
func IsReference(ref string) bool {
	_, err := Parse(ref)
	return err == nil
}

// Resolver resolves references.
type Resolver interface {
	Resolve(ctx context.Context, ref string) (string, error)
}

// Default resolves references against the process environment and the local
// file system. LookupEnv defaults to os.LookupEnv.
type Default struct {
	LookupEnv func(string) (string, bool)
}

// Resolve returns the value behind ref. The value is never part of an error.
func (d Default) Resolve(_ context.Context, ref string) (string, error) {
	r, err := Parse(ref)
	if err != nil {
		return "", err
	}
	switch r.Scheme {
	case "env":
		lookup := d.LookupEnv
		if lookup == nil {
			lookup = os.LookupEnv
		}
		value, ok := lookup(r.Name)
		if !ok || value == "" {
			return "", fmt.Errorf("%w: %s", ErrNotFound, r)
		}
		return value, nil
	default:
		f, err := os.Open(r.Name)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return "", fmt.Errorf("%w: %s", ErrNotFound, r)
			}
			return "", fmt.Errorf("secret %s: cannot open file", r)
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			return "", fmt.Errorf("secret %s: not a regular file", r)
		}
		data, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
		if err != nil {
			return "", fmt.Errorf("secret %s: cannot read file", r)
		}
		if len(data) > MaxFileBytes {
			return "", fmt.Errorf("secret %s: file is larger than %d bytes", r, MaxFileBytes)
		}
		value := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
		if value == "" {
			return "", fmt.Errorf("%w: %s", ErrNotFound, r)
		}
		return value, nil
	}
}

// redact keeps a malformed reference recognisable in an error without
// echoing more than its scheme and length.
func redact(ref string) string {
	scheme, _, ok := strings.Cut(ref, "://")
	if !ok {
		return fmt.Sprintf("<%d characters>", len(ref))
	}
	return fmt.Sprintf("%s://<%d characters>", scheme, len(ref)-len(scheme)-3)
}
