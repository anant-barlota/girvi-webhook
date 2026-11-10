package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/anant-barlota/girvi-plugins/sdk"
	corev1 "github.com/anant-barlota/girvi-plugins/sdk/gen/girvi/core/v1"
	deliveryv1 "github.com/anant-barlota/girvi-plugins/sdk/gen/girvi/delivery/v1"
)

const version = "0.1.1"

type webhook struct {
	client *http.Client
	now    func() time.Time

	mu  sync.RWMutex
	cfg sdk.Config
}

func newWebhook() *webhook {
	return &webhook{
		now: time.Now,
		client: &http.Client{
			Timeout: 15 * time.Second,
			// A redirect to POST would turn into a GET and drop the body, so it is
			// reported as a problem with the URL instead of being followed.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func (w *webhook) Info() sdk.Info {
	firm := corev1.SettingScope_SETTING_SCOPE_FIRM
	return sdk.Info{
		ID:          "anant-barlota/webhook",
		Name:        "Webhook",
		Author:      "Anant Barlota",
		Description: "Posts loan events to a URL you choose: loans created, payments received and loans redeemed.",
		Version:     version,
		Capabilities: []*corev1.Capability{
			{Name: "events.loan.created", Version: 1},
			{Name: "events.payment.received", Version: 1},
			{Name: "events.loan.redeemed", Version: 1},
		},
		Requires: []string{"network", "storage.private"},
		Settings: []*corev1.SettingField{
			{Key: "url", Title: "Webhook URL", Description: "Where to post events. Use https unless it is on your own network.", Type: "string", Required: true, Scope: firm},
			{Key: "signing_secret", Title: "Signing secret", Description: "Optional. If set, every post is signed so the receiver can check it came from Girvi.", Type: "string", Secret: true, Scope: firm},
			{Key: "include_customer_details", Title: "Include customer names and phone numbers", Description: "Turn off to send only customer IDs.", Type: "boolean", DefaultValue: "true", Scope: firm},
			{Key: "auth_header", Title: "Authorization header", Description: "Optional, sent as it is, for example Bearer abc123.", Type: "string", Secret: true, Scope: firm, Advanced: true},
			{Key: "allow_plain_http", Title: "Allow plain http outside my network", Description: "Off by default, because customer details would travel unencrypted.", Type: "boolean", DefaultValue: "false", Scope: firm, Advanced: true},
		},
	}
}

func (w *webhook) Configure(_ context.Context, cfg sdk.Config) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cfg = cfg
	return nil
}

func (w *webhook) firm(firmID string) (values map[string]string, name string, enabled bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	f, ok := w.cfg.Firms[firmID]
	if !ok {
		return nil, "", false
	}
	values = make(map[string]string, len(f.Values))
	for k, v := range f.Values {
		values[k] = v
	}
	return values, f.Name, f.Enabled
}

func isTrue(v string) bool { return strings.EqualFold(strings.TrimSpace(v), "true") }

// target checks the URL a firm configured. A problem the shop can fix is
// reported as blocked, so the event waits and is sent once the URL is right.
func target(values map[string]string) (*url.URL, error) {
	raw := strings.TrimSpace(values["url"])
	if raw == "" {
		return nil, sdk.Blocked(deliveryv1.ErrorClass_ERROR_CLASS_CONFIG, "enter the webhook URL in the settings")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, sdk.Blocked(deliveryv1.ErrorClass_ERROR_CLASS_CONFIG, "the webhook URL must start with https:// or http://")
	}
	if u.Scheme == "http" && !onOwnNetwork(u.Hostname()) && !isTrue(values["allow_plain_http"]) {
		return nil, sdk.Blocked(deliveryv1.ErrorClass_ERROR_CLASS_CONFIG,
			"the webhook URL uses plain http, so customer details would travel unencrypted. Use https, or turn on \"Allow plain http\" under Advanced")
	}
	return u, nil
}

// onOwnNetwork reports whether a host is this computer or a private address.
func onOwnNetwork(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".local") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate())
}

// safeURL is a URL without the parts that can hold secrets, for messages.
func safeURL(u *url.URL) string {
	return u.Scheme + "://" + u.Host + u.Path
}

func (w *webhook) Handle(ctx context.Context, item *deliveryv1.Item) (sdk.Result, error) {
	values, firmName, enabled := w.firm(item.GetFirmId())
	if !enabled {
		return sdk.Result{}, sdk.Permanent(deliveryv1.ErrorClass_ERROR_CLASS_CONFIG, "this plugin is not turned on for the firm")
	}
	u, err := target(values)
	if err != nil {
		return sdk.Result{}, err
	}

	env, err := build(item, firmName, !strings.EqualFold(strings.TrimSpace(values["include_customer_details"]), "false"))
	if err != nil {
		return sdk.Result{}, sdk.Permanent(deliveryv1.ErrorClass_ERROR_CLASS_UNSUPPORTED, "%v", err)
	}
	body, err := json.Marshal(env)
	if err != nil {
		return sdk.Result{}, sdk.Permanent(deliveryv1.ErrorClass_ERROR_CLASS_INTERNAL, "could not build the payload: %v", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return sdk.Result{}, sdk.Blocked(deliveryv1.ErrorClass_ERROR_CLASS_CONFIG, "the webhook URL is not valid")
	}
	timestamp := strconv.FormatInt(w.now().Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Girvi-Webhook/"+version)
	req.Header.Set("X-Girvi-Delivery", env.ID)
	req.Header.Set("X-Girvi-Event", env.Type)
	req.Header.Set("X-Girvi-Timestamp", timestamp)
	if secret := values["signing_secret"]; secret != "" {
		req.Header.Set("X-Girvi-Signature", sign(secret, timestamp, body))
	}
	if auth := strings.TrimSpace(values["auth_header"]); auth != "" {
		req.Header.Set("Authorization", auth)
	}

	resp, err := w.client.Do(req)
	if err != nil {
		return sdk.Result{}, networkError(err, u)
	}
	defer resp.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))

	// What the receiver says is shown to the user and stored, so it must not be able to
	// hand back the credentials it was sent.
	return classify(resp.StatusCode, u, scrub(snippet, values["signing_secret"], values["auth_header"]))
}

// scrub hides anything this plugin was given as a secret from text that came back
// from the receiver. For an authorization header such as "Bearer abc123" the credential
// after the scheme is hidden too, because a receiver may echo only that part. Values
// shorter than four characters are left alone: hiding them would mangle ordinary text.
func scrub(text []byte, secrets ...string) []byte {
	out := string(text)
	for _, secret := range secrets {
		secret = strings.TrimSpace(secret)
		parts := []string{secret}
		if _, credential, found := strings.Cut(secret, " "); found {
			parts = append(parts, strings.TrimSpace(credential))
		}
		for _, p := range parts {
			if len(p) >= 4 {
				out = strings.ReplaceAll(out, p, "[hidden]")
			}
		}
	}
	return []byte(out)
}

// sign is the signature of one post: HMAC-SHA256, keyed with the secret, over the
// timestamp, a dot and the exact body. Receivers should also reject old
// timestamps, so a captured post cannot be replayed.
func sign(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return "v1=" + hex.EncodeToString(mac.Sum(nil))
}

// networkError says what went wrong without repeating the URL's query string,
// which can carry a secret.
func networkError(err error, u *url.URL) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		err = uerr.Err
	}
	return fmt.Errorf("could not reach %s: %v", safeURL(u), err)
}

// classify decides what the receiver's answer means.
//   - 2xx: delivered. The receiver confirmed it, which is more than most
//     channels can say.
//   - 401, 403, 404, 410 and redirects: something the shop has to fix, so the
//     event waits for the fix.
//   - Other 4xx: the receiver refused this event, so it fails.
//   - Timeouts, 429 and 5xx: try again later.
func classify(status int, u *url.URL, body []byte) (sdk.Result, error) {
	note := fmt.Sprintf("HTTP %d from %s", status, u.Host)
	switch {
	case status >= 200 && status < 300:
		return sdk.Result{Note: note}, nil

	case status >= 300 && status < 400:
		return sdk.Result{}, sdk.Blocked(deliveryv1.ErrorClass_ERROR_CLASS_CONFIG,
			"the webhook URL redirects (HTTP %d). Enter the address it redirects to", status)
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return sdk.Result{}, sdk.Blocked(deliveryv1.ErrorClass_ERROR_CLASS_CONFIG,
			"the receiver refused the request (HTTP %d). Check the signing secret and the authorization header%s", status, reason(body))
	case status == http.StatusNotFound, status == http.StatusGone:
		return sdk.Result{}, sdk.Blocked(deliveryv1.ErrorClass_ERROR_CLASS_CONFIG,
			"the receiver says that address does not exist (HTTP %d). Check the webhook URL", status)

	case status == http.StatusRequestTimeout, status == http.StatusTooEarly, status == http.StatusTooManyRequests, status >= 500:
		return sdk.Result{}, fmt.Errorf("the receiver is busy or unavailable (HTTP %d)", status)

	default:
		return sdk.Result{}, sdk.Permanent(deliveryv1.ErrorClass_ERROR_CLASS_INVALID_INPUT,
			"the receiver refused this event (HTTP %d)%s", status, reason(body))
	}
}

// reason adds the receiver's own words, trimmed and stripped of control characters.
func reason(body []byte) string {
	s := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, string(body))
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	if s == "" {
		return ""
	}
	return ": " + s
}

// Health checks each firm's settings. It makes no request to the receiver, so
// nothing is posted just to see whether it is there.
func (w *webhook) Health(_ context.Context) sdk.Health {
	w.mu.RLock()
	cfg := w.cfg
	w.mu.RUnlock()

	ids := make([]string, 0, len(cfg.Firms))
	for id, f := range cfg.Firms {
		if f.Enabled {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)

	out := sdk.Health{Status: corev1.HealthStatus_HEALTH_STATUS_OK}
	if len(ids) == 0 {
		out.Message = "Running, not turned on for any firm"
		return out
	}
	var lines []string
	for _, id := range ids {
		f := cfg.Firms[id]
		status, msg := corev1.HealthStatus_HEALTH_STATUS_OK, ""
		if u, err := target(f.Values); err != nil {
			var blocked *sdk.BlockedError
			if errors.As(err, &blocked) {
				status, msg = corev1.HealthStatus_HEALTH_STATUS_DEGRADED, "Needs attention: "+blocked.Message
			}
		} else {
			msg = "Posts to " + u.Host
		}
		out.Firms = append(out.Firms, &corev1.FirmHealth{FirmId: id, Status: status, Message: msg})
		if status > out.Status {
			out.Status = status
		}
		if len(ids) == 1 {
			lines = append(lines, msg)
		} else {
			lines = append(lines, f.Name+": "+msg)
		}
	}
	out.Message = strings.Join(lines, " | ")
	return out
}
