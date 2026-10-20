package audit

import (
	"encoding/base64"
	"strconv"
	"strings"
	"time"

	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/idgen"
)

// cursor is the position after the last event of a page. On the wire it is
// opaque: base64url of "v1.<unix microseconds>.<uuid>".
type cursor struct {
	occurredAt time.Time
	id         idgen.UUID
}

const cursorVersion = "v1"

func encodeCursor(c cursor) string {
	raw := cursorVersion + "." + strconv.FormatInt(c.occurredAt.UnixMicro(), 10) + "." + c.id.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(value string) (cursor, error) {
	invalid := zserrors.New(zserrors.CodeInvalidCursor, "")
	if len(value) > 128 {
		return cursor{}, invalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return cursor{}, invalid
	}
	parts := strings.Split(string(raw), ".")
	if len(parts) != 3 || parts[0] != cursorVersion {
		return cursor{}, invalid
	}
	micros, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || micros < 0 {
		return cursor{}, invalid
	}
	id, err := idgen.ParseUUID(parts[2])
	if err != nil {
		return cursor{}, invalid
	}
	return cursor{occurredAt: time.UnixMicro(micros).UTC(), id: id}, nil
}
