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
// References are namespaced per brand. An env:// reference of brand <slug>
// must start with BRAND_<SLUG>__, the slug upper-cased with "-" replaced by
// "_" and followed by a double underscore. A slug is lowercase words of
// letters and digits joined by single hyphens, so <SLUG> never holds "__",
// never starts or ends with "_", and two brands never share a prefix or have
// one prefix inside the other. A file:// reference must resolve inside
// <dir>/<slug>/, where dir is ZS_SECRETS_DIR; without that directory file
// references are refused. Check enforces the rule at validation and Resolve
// enforces it again before it reads anything.
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

// ErrNotFound is returned when a valid reference does not resolve: the
// variable is unset or empty, or the file is missing, unreadable, empty or
// too large. Every such case gives the same error.
var ErrNotFound = errors.New("secret not found")

// ErrNotAllowed is returned for a reference outside the brand's namespace.
var ErrNotAllowed = errors.New("secret reference outside the brand's namespace")

// Slug lengths. The bounds match the brands table.
const (
	MinSlugLength = 2
	MaxSlugLength = 40
)

var (
	envName  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	slugForm = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
)

// ValidSlug reports whether slug is a brand slug: 2 to 40 characters,
// lowercase letters and digits in words joined by single hyphens. No
// leading, trailing or double hyphen, which keeps the env:// prefixes of two
// brands apart.
func ValidSlug(slug string) bool {
	return len(slug) >= MinSlugLength && len(slug) <= MaxSlugLength && slugForm.MatchString(slug)
}

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
		if !strings.HasPrefix(path, "/") || path == "/" || filepath.Clean(path) != path || strings.ContainsRune(path, 0) {
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

// EnvPrefix returns the prefix every env:// reference of the brand must
// carry: BRAND_ZS_REFERENCE__ for zs-reference and BRAND_ZS__ for zs. For
// valid slugs the mapping is one to one and no prefix starts another, because
// the "__" terminator cannot occur inside an upper-cased slug.
func EnvPrefix(slug string) string {
	return "BRAND_" + strings.ToUpper(strings.ReplaceAll(slug, "-", "_")) + "__"
}

// Check reports whether ref is a reference brand slug may use, given dir, the
// secrets directory (ZS_SECRETS_DIR, empty when unset). It reads nothing. The
// error names the rule, never a value.
func Check(slug, dir, ref string) error {
	_, err := check(slug, dir, ref)
	return err
}

// check returns the parsed reference and, for a file reference, the path
// relative to <dir>/<slug>.
func check(slug, dir, ref string) (Ref, error) {
	r, err := Parse(ref)
	if err != nil {
		return Ref{}, err
	}
	if !ValidSlug(slug) {
		return Ref{}, fmt.Errorf("%w: invalid brand slug", ErrNotAllowed)
	}
	switch r.Scheme {
	case "env":
		prefix := EnvPrefix(slug)
		if !strings.HasPrefix(r.Name, prefix) || len(r.Name) == len(prefix) {
			return Ref{}, fmt.Errorf("%w: env:// references of brand %s must be env://%s<NAME>", ErrNotAllowed, slug, prefix)
		}
		return r, nil
	default:
		if dir == "" || !filepath.IsAbs(dir) {
			return Ref{}, fmt.Errorf("%w: file:// references need ZS_SECRETS_DIR", ErrNotAllowed)
		}
		base := filepath.Join(filepath.Clean(dir), slug)
		rel, err := filepath.Rel(base, r.Name)
		if err != nil || !filepath.IsLocal(rel) || rel == "." {
			return Ref{}, fmt.Errorf("%w: file:// references of brand %s must be inside ZS_SECRETS_DIR/%s/", ErrNotAllowed, slug, slug)
		}
		return Ref{Scheme: r.Scheme, Name: rel}, nil
	}
}

// Resolver checks and resolves the references of one brand.
type Resolver interface {
	// Check reports whether brand slug may use ref. It reads nothing.
	Check(slug, ref string) error
	// Resolve checks ref and returns its value. A reference that fails Check
	// returns an error wrapping ErrNotAllowed; one that passes and does not
	// resolve returns ErrNotFound.
	Resolve(ctx context.Context, slug, ref string) (string, error)
}

// Default resolves references against the process environment and the
// secrets directory. LookupEnv defaults to os.LookupEnv. Dir is
// ZS_SECRETS_DIR; when it is empty, file references are refused.
type Default struct {
	LookupEnv func(string) (string, bool)
	Dir       string
}

// Check implements Resolver.
func (d Default) Check(slug, ref string) error { return Check(slug, d.Dir, ref) }

// Resolve returns the value behind ref. The value is never part of an error,
// and every failure after the namespace check returns the same ErrNotFound.
func (d Default) Resolve(_ context.Context, slug, ref string) (string, error) {
	r, err := check(slug, d.Dir, ref)
	if err != nil {
		return "", err
	}
	notFound := fmt.Errorf("%w: %s", ErrNotFound, ref)
	if r.Scheme == "env" {
		lookup := d.LookupEnv
		if lookup == nil {
			lookup = os.LookupEnv
		}
		value, ok := lookup(r.Name)
		if !ok || value == "" {
			return "", notFound
		}
		return value, nil
	}
	// The brand's directory is opened as a root, so a symbolic link cannot
	// lead the read outside it.
	root, err := os.OpenRoot(filepath.Join(filepath.Clean(d.Dir), slug))
	if err != nil {
		return "", notFound
	}
	defer root.Close()
	f, err := root.Open(r.Name)
	if err != nil {
		return "", notFound
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", notFound
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil || len(data) > MaxFileBytes {
		return "", notFound
	}
	value := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	if value == "" {
		return "", notFound
	}
	return value, nil
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
