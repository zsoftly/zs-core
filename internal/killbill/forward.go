package killbill

import (
	"context"
	"strings"

	"github.com/zsoftly/zs-core/internal/upstream"
)

// Forward sends one request to Kill Bill with the tenant credentials and,
// for a write, the audit headers, and returns the answer. path starts with
// /1.0/kb/ and may carry a query. The caller decides which requests may be
// forwarded. A 4xx is returned as *Error and a 5xx as *Unavailable, so no
// Kill Bill error text is passed on.
func (c *Client) Forward(ctx context.Context, creds TenantCredentials, cc CallContext, method, path, contentType string, body []byte) (upstream.Response, error) {
	r := request{op: "forward " + strings.ToLower(method), method: method, path: path, tenant: &creds, contentType: contentType, body: body}
	if method != "GET" && method != "HEAD" {
		r.call = &cc
	}
	return c.do(ctx, r)
}
