package tenant

import (
	"encoding/base64"
	"fmt"
	"strings"

	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/idgen"
)

// Pagination limits of the tenant lists.
const (
	DefaultLimit = 50
	MaxLimit     = 200
)

// Page selects one page of a list ordered by ID, oldest first. IDs are
// UUIDv7, so the order is the creation order.
type Page struct {
	Limit  int
	Cursor string
}

// After returns the limit and the ID after which the page starts. A zero
// Limit is DefaultLimit.
func (p Page) After() (int, idgen.UUID, error) {
	limit := p.Limit
	switch {
	case limit == 0:
		limit = DefaultLimit
	case limit < 0 || limit > MaxLimit:
		return 0, idgen.Nil, zserrors.New(zserrors.CodeInvalidLimit, fmt.Sprintf("The limit must be between 1 and %d.", MaxLimit))
	}
	if p.Cursor == "" {
		return limit, idgen.Nil, nil
	}
	after, err := decodeCursor(p.Cursor)
	if err != nil {
		return 0, idgen.Nil, err
	}
	return limit, after, nil
}

const cursorVersion = "v1."

// EncodeCursor returns the opaque cursor after id.
func EncodeCursor(id idgen.UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(cursorVersion + id.String()))
}

func decodeCursor(value string) (idgen.UUID, error) {
	invalid := zserrors.New(zserrors.CodeInvalidCursor, "")
	if len(value) > 128 {
		return idgen.Nil, invalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return idgen.Nil, invalid
	}
	rest, ok := strings.CutPrefix(string(raw), cursorVersion)
	if !ok {
		return idgen.Nil, invalid
	}
	id, err := idgen.ParseUUID(rest)
	if err != nil {
		return idgen.Nil, invalid
	}
	return id, nil
}
