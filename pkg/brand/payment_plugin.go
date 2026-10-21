package brand

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/internal/components"
	"github.com/zsoftly/zs-core/internal/kbplugin"
	"github.com/zsoftly/zs-core/internal/killbill"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/idgen"
)

// Targets of the plugin and callback changes.
const (
	TargetPlugin               = "killbill.plugin"
	TargetNotificationCallback = "killbill.notification_callback"
)

// Plugin states Kill Bill reports.
const (
	pluginStateRunning = "RUNNING"
	pluginStateStopped = "STOPPED"
)

// PluginWait bounds how long apply waits for KPM to install or start a
// plugin, and pluginPoll how often it looks.
var (
	PluginWait = 5 * time.Minute
	pluginPoll = 2 * time.Second
)

// The configuration keys whose values decide whether a plugin takes real
// money. Each was read from the pinned plugin artifact. The Adyen plugin
// 0.10.0 spells its key enviroment. The correct spelling is checked too, in
// case a later release fixes it.
const (
	stripeAPIKey          = "org.killbill.billing.plugin.stripe.apiKey"
	stripeTestKeyPrefix   = "sk_test_"
	adyenEnvironmentKey   = "org.killbill.billing.plugin.adyen.enviroment"
	adyenEnvironmentKey2  = "org.killbill.billing.plugin.adyen.environment"
	adyenTestEnvironment  = "TEST"
	avataxURLKey          = "org.killbill.billing.plugin.avatax.url"
	avataxTaxRatesURLKey  = "org.killbill.billing.plugin.avatax.taxratesapi.url"
	avataxSandboxHost     = "sandbox-rest.avatax.com"
	pluginNameStripe      = "stripe"
	pluginNameAdyen       = "adyen"
	pluginNameAvaTax      = "avatax"
	maxPluginConfigLength = 64 << 10
)

func pluginUnavailable(message string) error {
	return zserrors.New(zserrors.CodePluginUnavailable, message)
}

// approvedPlugin returns the components.yaml pin of a payment plugin of
// the manifest. A plugin that is not approved, or that has no Java
// artifact, is plugin_unavailable. A manifest version other than the pin is
// refused.
func approvedPlugin(approved []components.Plugin, pp *PaymentPlugin) (components.Plugin, error) {
	return approvedPin(approved, "payment", "approved_payment_plugins", "/billing/payment_plugins", pp.Name, pp.Version)
}

// approvedPin finds a plugin in a components.yaml list. kind and list name
// the plugin kind and the list for the messages, path the manifest field.
func approvedPin(approved []components.Plugin, kind, list, path, name, version string) (components.Plugin, error) {
	if approved == nil {
		return components.Plugin{}, pluginUnavailable("The approved " + kind + " plugins are not loaded: brand apply needs components.yaml.")
	}
	for _, p := range approved {
		if p.Name != name {
			continue
		}
		if p.Status == components.PluginStatusNoJavaArtifact || p.Version == "" || p.Artifact == "" {
			return components.Plugin{}, pluginUnavailable("The " + kind + " plugin " + name + " has no Java artifact that Kill Bill 0.24 can install.")
		}
		if p.Sha256 == "" {
			return components.Plugin{}, pluginUnavailable("The " + kind + " plugin " + name + " has no sha256 in components.yaml, so Core cannot check its jar.")
		}
		if version != "" && version != p.Version {
			return components.Plugin{}, invalid([]string{path + "/version: " + version + " is not the pinned version " + p.Version + " in components.yaml"})
		}
		return p, nil
	}
	return components.Plugin{}, pluginUnavailable("The " + kind + " plugin " + name + " is not in " + list + " in components.yaml.")
}

// testModeRule is what Core knows about one payment plugin's test mode.
// forbid runs against every configuration a brand uploads, whatever its
// plugin and key, so a live key never reaches Kill Bill under another
// plugin's name. require runs against the configuration of the plugin
// itself and needs the test-mode setting to be explicit.
type testModeRule struct {
	forbid  func(properties) error
	require func(properties) error
}

var testModeRules = map[string]testModeRule{
	pluginNameStripe: {forbid: stripeForbid, require: stripeRequire},
	pluginNameAdyen:  {forbid: adyenForbid, require: adyenRequire},
	pluginNameAvaTax: {forbid: avataxForbid, require: avataxRequire},
}

// testModeRuleNames are the plugins with a test-mode rule, in a fixed order.
var testModeRuleNames = []string{pluginNameAdyen, pluginNameAvaTax, pluginNameStripe}

func applyRejected(message string) error {
	return zserrors.New(zserrors.CodeBrandApplyRejected, message)
}

func stripeForbid(p properties) error {
	for _, v := range p.all(stripeAPIKey) {
		if !strings.HasPrefix(v, stripeTestKeyPrefix) {
			return applyRejected("The configuration sets a Stripe API key that is not a test-mode key. v1 accepts only keys that start with " + stripeTestKeyPrefix + ", under any plugin.")
		}
	}
	return nil
}

func stripeRequire(p properties) error {
	if len(p.all(stripeAPIKey)) == 0 {
		return applyRejected("The Stripe configuration must set " + stripeAPIKey + " to a test-mode key that starts with " + stripeTestKeyPrefix + ". A key from the environment of the Kill Bill node is not accepted.")
	}
	return nil
}

func adyenForbid(p properties) error {
	for _, key := range []string{adyenEnvironmentKey, adyenEnvironmentKey2} {
		for _, v := range p.all(key) {
			if v != adyenTestEnvironment {
				return applyRejected("The configuration sets an Adyen environment other than TEST. v1 accepts only the Adyen test environment, under any plugin.")
			}
		}
	}
	return nil
}

// avataxForbid refuses an AvaTax endpoint other than the AvaTax sandbox,
// https://sandbox-rest.avatax.com, the endpoint the plugin's documentation
// gives for tests. A production endpoint records real tax documents.
func avataxForbid(p properties) error {
	for _, key := range []string{avataxURLKey, avataxTaxRatesURLKey} {
		for _, v := range p.all(key) {
			u, err := url.Parse(strings.TrimSpace(v))
			if err != nil || u.Scheme != "https" || u.Hostname() != avataxSandboxHost || u.User != nil {
				return applyRejected("The configuration sets an AvaTax endpoint other than the AvaTax sandbox. v1 accepts only https://" + avataxSandboxHost + ", under any plugin.")
			}
		}
	}
	return nil
}

func avataxRequire(p properties) error {
	if len(p.all(avataxURLKey)) == 0 {
		return applyRejected("The AvaTax configuration must set " + avataxURLKey + " to the AvaTax sandbox, https://" + avataxSandboxHost + "/api/v2.")
	}
	return nil
}

func adyenRequire(p properties) error {
	if len(p.all(adyenEnvironmentKey)) == 0 {
		return applyRejected("The Adyen configuration must set " + adyenEnvironmentKey + " to TEST. An unset environment is not accepted.")
	}
	return nil
}

// CheckTestMode refuses a plugin configuration that would take real money.
// The configuration is read as Kill Bill reads it, as a Java properties
// text, after the checks of checkPluginConfigText. Every known plugin's
// forbid rule runs against it: a Stripe key that does not start with
// sk_test_ or an Adyen environment other than TEST is refused whatever the
// plugin. Then the plugin's own require rule runs: Stripe needs its API key
// set, Adyen needs its environment set to TEST. A plugin with no test-mode
// rule is plugin_unavailable. The error never holds a configuration value.
func CheckTestMode(plugin, config string) error {
	rule, ok := testModeRules[plugin]
	if !ok {
		return pluginUnavailable("The plugin " + plugin + " has no test-mode rule in Core, so v1 cannot enable it.")
	}
	if len(config) > maxPluginConfigLength {
		return applyRejected("The payment plugin configuration is larger than 64 KiB.")
	}
	if err := checkPluginConfigText(config); err != nil {
		return err
	}
	props, err := parseProperties(config)
	if err != nil {
		return err
	}
	for _, name := range testModeRuleNames {
		if err := testModeRules[name].forbid(props); err != nil {
			return err
		}
	}
	return rule.require(props)
}

// checkPluginConfigText refuses the parts of the Java properties format
// where a reader that differs from Java's could see another value than Kill
// Bill does. These are not needed by any plugin configuration, so Core
// refuses them outright, as defence in depth behind the parser.
func checkPluginConfigText(config string) error {
	for i := 0; i < len(config); i++ {
		switch c := config[i]; {
		case c >= 0x80:
			return applyRejected("The payment plugin configuration holds a byte that is not ASCII. Core accepts ASCII only.")
		case c == '\r' && (i+1 == len(config) || config[i+1] != '\n'):
			return applyRejected("The payment plugin configuration holds a carriage return that does not end a line. Core accepts lines that end with a line feed or a carriage return and a line feed.")
		}
	}
	if strings.Contains(config, `\u`) {
		return applyRejected(`The payment plugin configuration holds a \u escape. Core refuses Unicode escapes.`)
	}
	for _, line := range strings.Split(config, "\n") {
		if continues(strings.TrimSuffix(line, "\r")) {
			return applyRejected("The payment plugin configuration continues a line with a backslash at its end. Core refuses continuation lines.")
		}
	}
	return nil
}

// properties is a parsed Java properties text: the value Kill Bill reads
// for each key, which is the last one, and every value each key was given,
// in order.
type properties struct {
	last   map[string]string
	values map[string][]string
}

// all returns every value a key was given, in order.
func (p properties) all(key string) []string { return p.values[key] }

// parseProperties reads a Java properties text as java.util.Properties.load
// does, which is how Kill Bill reads a per-tenant plugin configuration. It
// follows the line terminators (\n, \r and \r\n), comment lines (# and !),
// the key separators (=, : or blanks), continuation lines, and the escapes
// \t, \n, \r, \f, \uXXXX and backslash before any other character. A later
// value of a key replaces an earlier one. A malformed \u escape is refused, as
// Java refuses it.
func parseProperties(text string) (properties, error) {
	p := properties{last: map[string]string{}, values: map[string][]string{}}
	r := lineReader{in: []rune(text)}
	for {
		line, ok := r.next()
		if !ok {
			return p, nil
		}
		key, value, err := splitProperty(line)
		if err != nil {
			return properties{}, err
		}
		p.last[key] = value
		p.values[key] = append(p.values[key], value)
	}
}

// lineReader returns the logical lines of a properties text, as the
// LineReader of java.util.Properties in Java 11, Kill Bill's runtime, does:
// leading blanks removed, comment and blank lines skipped, continuation
// lines joined without the backslash, the line terminator and the leading
// blanks of the next line. A # or ! is a comment while the logical line is
// still empty, even after a continuation.
type lineReader struct {
	in  []rune
	pos int
}

func isBlank(c rune) bool { return c == ' ' || c == '\t' || c == '\f' }

func (r *lineReader) next() ([]rune, bool) {
	var (
		line                       []rune
		skipWhite                  = true
		appended, skipLF, escaping bool
	)
	for {
		if r.pos >= len(r.in) {
			if len(line) == 0 {
				return nil, false
			}
			if escaping {
				line = line[:len(line)-1]
			}
			return line, true
		}
		c := r.in[r.pos]
		r.pos++
		if skipLF {
			skipLF = false
			if c == '\n' {
				continue
			}
		}
		if skipWhite {
			if isBlank(c) || (!appended && (c == '\r' || c == '\n')) {
				continue
			}
			skipWhite, appended = false, false
		}
		if len(line) == 0 && (c == '#' || c == '!') {
			for r.pos < len(r.in) {
				c = r.in[r.pos]
				r.pos++
				if c == '\r' || c == '\n' {
					break
				}
			}
			skipWhite = true
			continue
		}
		if c != '\n' && c != '\r' {
			line = append(line, c)
			escaping = c == '\\' && !escaping
			continue
		}
		if len(line) == 0 {
			skipWhite = true
			continue
		}
		if r.pos >= len(r.in) {
			if escaping {
				line = line[:len(line)-1]
			}
			return line, true
		}
		if !escaping {
			if c == '\r' && r.in[r.pos] == '\n' {
				r.pos++
			}
			return line, true
		}
		line = line[:len(line)-1]
		skipWhite, appended, escaping = true, true, false
		skipLF = c == '\r'
	}
}

// splitProperty splits a logical line into its key and value, as
// Properties.load does, and converts the escapes of both.
func splitProperty(line []rune) (string, string, error) {
	keyLen, valueStart := 0, len(line)
	hasSep, backslash := false, false
	for keyLen < len(line) {
		c := line[keyLen]
		if (c == '=' || c == ':') && !backslash {
			valueStart, hasSep = keyLen+1, true
			break
		}
		if isBlank(c) && !backslash {
			valueStart = keyLen + 1
			break
		}
		backslash = c == '\\' && !backslash
		keyLen++
	}
	for valueStart < len(line) {
		c := line[valueStart]
		if !isBlank(c) {
			if hasSep || (c != '=' && c != ':') {
				break
			}
			hasSep = true
		}
		valueStart++
	}
	key, err := convertEscapes(line[:keyLen])
	if err != nil {
		return "", "", err
	}
	value, err := convertEscapes(line[valueStart:])
	if err != nil {
		return "", "", err
	}
	return key, value, nil
}

// convertEscapes converts the escapes of a key or value as
// Properties.loadConvert does.
func convertEscapes(in []rune) (string, error) {
	var out strings.Builder
	for i := 0; i < len(in); i++ {
		c := in[i]
		if c != '\\' || i+1 == len(in) {
			out.WriteRune(c)
			continue
		}
		i++
		switch c = in[i]; c {
		case 't':
			out.WriteRune('\t')
		case 'n':
			out.WriteRune('\n')
		case 'r':
			out.WriteRune('\r')
		case 'f':
			out.WriteRune('\f')
		case 'u':
			if i+4 >= len(in) {
				return "", applyRejected(`The payment plugin configuration holds a malformed \u escape.`)
			}
			var v rune
			for _, h := range in[i+1 : i+5] {
				d := hexDigit(h)
				if d < 0 {
					return "", applyRejected(`The payment plugin configuration holds a malformed \u escape.`)
				}
				v = v<<4 | d
			}
			out.WriteRune(v)
			i += 4
		default:
			out.WriteRune(c)
		}
	}
	return out.String(), nil
}

func hexDigit(c rune) rune {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10
	}
	return -1
}

// continues reports whether a line ends with an odd number of backslashes.
func continues(line string) bool {
	n := 0
	for i := len(line) - 1; i >= 0 && line[i] == '\\'; i-- {
		n++
	}
	return n%2 == 1
}

// ArtifactURL is the address of a plugin jar in a Maven repository. See
// kbplugin.ArtifactURL.
func ArtifactURL(repository string, p components.Plugin) (string, error) {
	return kbplugin.ArtifactURL(repository, p)
}

// CheckPluginRepository refuses a plugin repository that is not an https
// URL, so a plugin jar is never fetched in clear.
func CheckPluginRepository(repository string) error { return kbplugin.CheckRepository(repository) }

// maxPluginJarBytes caps the download of a plugin jar.
const maxPluginJarBytes = kbplugin.MaxJarBytes

// verifyPluginJar checks access to the configured mirror and compares its
// answer with the pin before Core asks KPM to install. KPM then downloads
// from the same verified mirror, which hashes and serves a fresh immutable
// snapshot for every request. A repository URL is never an install source.
func verifyPluginJar(ctx context.Context, client *http.Client, uri string, pin components.Plugin) error {
	if client == nil {
		client = &http.Client{Timeout: PluginWait}
	}
	hc := *client
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return pluginUnavailable("The plugin " + pin.Name + " cannot be downloaded: its address is not valid.")
	}
	resp, err := hc.Do(req)
	if err != nil {
		// Transport errors can include the request URL and its jar token.
		return &killbill.Unavailable{Op: "download plugin " + pin.Name, Err: errors.New("the plugin mirror request failed")}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &killbill.Unavailable{Op: "download plugin " + pin.Name, Status: resp.StatusCode, Err: fmt.Errorf("the plugin mirror answered HTTP %d", resp.StatusCode)}
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(resp.Body, maxPluginJarBytes+1))
	if err != nil {
		return &killbill.Unavailable{Op: "download plugin " + pin.Name, Err: errors.New("the plugin mirror response could not be read")}
	}
	if n > maxPluginJarBytes {
		return zserrors.New(zserrors.CodeBrandApplyRejected, "The plugin "+pin.Name+" jar is larger than 256 MiB.")
	}
	if got := hex.EncodeToString(h.Sum(nil)); !hmac.Equal([]byte(got), []byte(pin.Sha256)) {
		return zserrors.New(zserrors.CodeBrandApplyRejected, "The plugin "+pin.Name+" jar does not match the sha256 in components.yaml. Core did not install it.")
	}
	return nil
}

// planPlugin plans the KPM install and start of a plugin the brand uses. It
// returns no change when the pinned version is RUNNING on every node.
func planPlugin(ctx context.Context, deps Deps, u pluginUse) ([]Change, error) {
	pin := u.pin
	kb := deps.KillBill
	nodes, err := kb.NodesInfo(ctx)
	if err != nil {
		return nil, err
	}
	if len(nodes) == 0 {
		return nil, zserrors.New(zserrors.CodeBrandApplyRejected, "Kill Bill reports no nodes.")
	}
	install, start := false, false
	before := "absent"
	for _, n := range nodes {
		info, found := findPlugin(n, pin.Name)
		switch {
		case !found:
			install = true
		case info.Version != pin.Version:
			return nil, zserrors.New(zserrors.CodeBrandApplyRejected, fmt.Sprintf(
				"Kill Bill runs %s plugin %s %s, but components.yaml pins %s. Follow docs/upgrades/killbill.md to change plugin versions.", u.kind, pin.Name, info.Version, pin.Version))
		case info.State != pluginStateRunning:
			start = true
			before = strings.ToLower(info.State)
		}
	}
	if !install && !start {
		return nil, nil
	}
	src := installSource{}
	if install {
		if src, err = pluginSource(deps, u); err != nil {
			return nil, err
		}
	}
	action := ActionUpdate
	field := u.field
	fields := []FieldChange{{Field: field + ".state", Before: before, After: "running"}}
	if install {
		action = ActionCreate
		fields = append([]FieldChange{{Field: field + ".version", After: pin.Version}, {Field: field + ".source", After: src.name}}, fields...)
	}
	return []Change{{
		Target: TargetPlugin, Name: pin.Name, Action: action, Fields: fields,
		run: func(ctx context.Context, st *runState) error {
			return installPlugin(ctx, st, pin, src, install)
		},
	}}, nil
}

func findPlugin(n killbill.NodeInfo, key string) (killbill.PluginInfo, bool) {
	for _, p := range n.PluginsInfo {
		if p.PluginKey == key {
			return p, true
		}
	}
	return killbill.PluginInfo{}, false
}

// installSource is the verified mirror KPM downloads a jar from. name is
// shown in the diff. The URL holds a token and is never shown.
type installSource struct {
	name   string
	uri    string
	mirror bool
}

// pluginSource requires the Core mirror so KPM only receives verified bytes.
func pluginSource(deps Deps, u pluginUse) (installSource, error) {
	if deps.PluginMirrorURL == "" {
		return installSource{}, pluginUnavailable("Installing a plugin requires ZS_PLUGIN_MIRROR_URL and ZS_PLUGIN_MIRROR_SECRET for Core's verified mirror. Sync the mirror before applying the brand.")
	}
	uri, err := kbplugin.MirrorURL(deps.PluginMirrorURL, deps.PluginMirrorSecret, u.pin)
	if err != nil {
		return installSource{}, pluginUnavailable("The " + u.kind + " plugin " + u.pin.Name + " cannot be installed from the mirror: " + err.Error() + ".")
	}
	return installSource{name: "mirror", uri: uri, mirror: true}, nil
}

// installPlugin sends the KPM commands, waits for the plugin to run on every
// node and records it in killbill_plugin_installs.
func installPlugin(ctx context.Context, st *runState, pin components.Plugin, src installSource, install bool) error {
	kb := st.deps.KillBill
	cc := st.callContext()
	installedAt := time.Now().UTC()
	if install {
		want, err := pluginSource(st.deps, pluginUse{pin: pin})
		if err != nil {
			return err
		}
		if !src.mirror || !secretEqual(src.uri, want.uri) {
			return pluginUnavailable("The plugin install source must be Core's configured verified mirror.")
		}
		if err := verifyPluginJar(ctx, st.deps.PluginHTTP, src.uri, pin); err != nil {
			return err
		}
		// The bundle builds the download address with a doubled slash
		// that Maven Central refuses, so the address is passed in pluginUri.
		if err := kb.SendNodeCommand(ctx, cc, killbill.NodeCommand{
			NodeCommandType: killbill.NodeCommandInstallPlugin, IsSystemCommandType: true,
			NodeCommandProperties: []killbill.NodeCommandProperty{
				{Key: "pluginKey", Value: pin.Name}, {Key: "pluginVersion", Value: pin.Version}, {Key: "pluginUri", Value: src.uri},
			},
		}); err != nil {
			return err
		}
		if _, err := waitForPlugin(ctx, kb, pin, func(p killbill.PluginInfo) bool { return true }); err != nil {
			return err
		}
	}
	if err := kb.SendNodeCommand(ctx, cc, killbill.NodeCommand{
		NodeCommandType: killbill.NodeCommandStartPlugin,
		NodeCommandProperties: []killbill.NodeCommandProperty{
			{Key: "pluginKey", Value: pin.Name}, {Key: "pluginVersion", Value: pin.Version},
		},
	}); err != nil {
		return err
	}
	nodes, err := waitForPlugin(ctx, kb, pin, func(p killbill.PluginInfo) bool { return p.State == pluginStateRunning })
	if err != nil {
		return err
	}
	startedAt := time.Now().UTC()
	return inTx(ctx, st.deps.DB, func(tx pgx.Tx) error {
		for _, n := range nodes {
			if _, err := tx.Exec(ctx, `INSERT INTO killbill_plugin_installs (id, plugin, version, node, brand_id, installed_at, started_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (plugin, version, node) DO UPDATE SET started_at = EXCLUDED.started_at`,
				idgen.MustNewV7(), pin.Name, pin.Version, n, st.brandID, installedAt, startedAt); err != nil {
				return zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
			}
		}
		return nil
	})
}

// waitForPlugin polls the nodes until the plugin is on every node and ok
// holds for it, and returns the node names.
func waitForPlugin(ctx context.Context, kb *killbill.Client, pin components.Plugin, ok func(killbill.PluginInfo) bool) ([]string, error) {
	return waitForPluginNodes(ctx, kb, pin, nil, func(_ string, p killbill.PluginInfo) bool { return ok(p) })
}

// waitForPluginNodes optionally fixes the node set for an operation, so a
// disappearing node cannot turn an incomplete restart into a success.
func waitForPluginNodes(ctx context.Context, kb *killbill.Client, pin components.Plugin, expected map[string]bool, ok func(string, killbill.PluginInfo) bool) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, PluginWait)
	defer cancel()
	for {
		nodes, err := kb.NodesInfo(ctx)
		if err == nil && expected != nil {
			seen := map[string]bool{}
			for _, n := range nodes {
				if !expected[n.NodeName] || seen[n.NodeName] {
					return nil, &killbill.Unavailable{Op: "wait for plugin " + pin.Name, Err: errors.New("the Kill Bill node set changed during the operation")}
				}
				seen[n.NodeName] = true
			}
			if len(seen) != len(expected) {
				return nil, &killbill.Unavailable{Op: "wait for plugin " + pin.Name, Err: errors.New("a target Kill Bill node disappeared during the operation")}
			}
		}
		if err == nil && len(nodes) > 0 {
			var names []string
			for _, n := range nodes {
				if p, found := findPlugin(n, pin.Name); found && p.Version == pin.Version && ok(n.NodeName, p) {
					names = append(names, n.NodeName)
				}
			}
			if len(names) == len(nodes) {
				return names, nil
			}
		}
		select {
		case <-ctx.Done():
			return nil, &killbill.Unavailable{Op: "wait for plugin " + pin.Name, Err: errors.New("the plugin did not reach the expected state in time")}
		case <-time.After(pluginPoll):
		}
	}
}

// WebhookToken is the per-brand token in the path of the Kill Bill push
// notification callback: the HMAC-SHA256 of the brand ID under the
// deployment's ZS_KILLBILL_WEBHOOK_SECRET, in hex. A brand's Kill Bill
// tenant learns its own token only, so it cannot post as another brand.
func WebhookToken(secret []byte, brand idgen.UUID) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("zs-core.killbill-webhook.v1:"))
	mac.Write([]byte(brand.String()))
	return hex.EncodeToString(mac.Sum(nil))
}

// WebhookPath is the path of a brand's callback on Core.
func WebhookPath(secret []byte, brand idgen.UUID) string {
	return "/v1/billing/webhooks/killbill/" + idgen.Format(idgen.Brand, brand) + "/" + WebhookToken(secret, brand)
}

// planCallback plans the registration of the brand's push notification
// callback. Kill Bill keeps one callback per tenant, so registering replaces
// any other. Without a webhook base URL and secret nothing is planned.
func planCallback(ctx context.Context, deps Deps, creds killbill.TenantCredentials, brandID idgen.UUID, exists bool) ([]Change, error) {
	if deps.WebhookBaseURL == "" || len(deps.WebhookSecret) == 0 {
		return nil, nil
	}
	want := strings.TrimRight(deps.WebhookBaseURL, "/") + WebhookPath(deps.WebhookSecret, brandID)
	var have []string
	if exists {
		var err error
		if have, err = deps.KillBill.NotificationCallbacks(ctx, creds); err != nil {
			return nil, err
		}
		if len(have) == 1 && secretEqual(have[0], want) {
			return nil, nil
		}
	}
	action := ActionUpdate
	if len(have) == 0 {
		action = ActionCreate
	}
	return []Change{{
		Target: TargetNotificationCallback, Name: creds.APIKey, Action: action,
		Fields: []FieldChange{{Field: "billing.notification_callback", Redacted: true}},
		run: func(ctx context.Context, st *runState) error {
			return deps.KillBill.RegisterNotificationCallback(ctx, creds, st.callContext(), want)
		},
	}}, nil
}
