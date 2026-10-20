package doctor

import (
	"context"
	"fmt"
	"net/http"

	"github.com/zsoftly/zs-core/internal/components"
)

// checkKAUI covers reachability only. KAUI is optional: with no URL the check
// is skipped. The version is not exposed over HTTP, so matches-pin stays
// unknown and the pin is reported for reference.
func checkKAUI(ctx context.Context, opts Options) (ComponentReport, []Finding) {
	const name = components.KAUI
	report := ComponentReport{Name: name}
	pin, _ := opts.Manifest.Component(name)
	report.Expected = pin.Version
	if opts.KAUI.URL == "" {
		return report, []Finding{{name, "reachable", OK, "skipped: ZS_KAUI_URL is not set"}}
	}
	// KAUI redirects the root to the sign-in page; the client follows it.
	status, err := getStatus(ctx, opts.HTTPClient, opts.KAUI.URL+"/")
	if err != nil {
		return report, []Finding{unreachable(name, err)}
	}
	if status != http.StatusOK {
		return report, []Finding{{name, "reachable", Fail, fmt.Sprintf("GET / returned HTTP %d", status)}}
	}
	report.Reachable = true
	return report, []Finding{{name, "reachable", OK, fmt.Sprintf("reachable at %s (HTTP 200); the version is not exposed, pin is %s", redactURL(opts.KAUI.URL), pin.Version)}}
}

func getStatus(ctx context.Context, client *http.Client, url string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	return do(client, req, nil)
}
