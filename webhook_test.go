package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anant-barlota/girvi-plugins/sdk"
	corev1 "github.com/anant-barlota/girvi-plugins/sdk/gen/girvi/core/v1"
	deliveryv1 "github.com/anant-barlota/girvi-plugins/sdk/gen/girvi/delivery/v1"
	eventsv1 "github.com/anant-barlota/girvi-plugins/sdk/gen/girvi/events/v1"
	modelv1 "github.com/anant-barlota/girvi-plugins/sdk/gen/girvi/model/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// receiver stands in for the shop's endpoint and records the last request.
type receiver struct {
	srv    *httptest.Server
	status int
	reply  string

	method  string
	header  http.Header
	rawBody []byte
}

func newReceiver(t *testing.T) *receiver {
	r := &receiver{status: 200}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.method, r.header = req.Method, req.Header.Clone()
		r.rawBody, _ = io.ReadAll(req.Body)
		if r.status >= 300 && r.status < 400 {
			w.Header().Set("Location", "/elsewhere")
		}
		w.WriteHeader(r.status)
		_, _ = w.Write([]byte(r.reply))
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) body(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.rawBody, &m); err != nil {
		t.Fatalf("the body is not JSON: %v\n%s", err, r.rawBody)
	}
	return m
}

func configured(t *testing.T, r *receiver, firm map[string]string) *webhook {
	t.Helper()
	w := newWebhook()
	values := map[string]string{"url": r.srv.URL + "/hook"}
	for k, v := range firm {
		values[k] = v
	}
	_ = w.Configure(context.Background(), sdk.Config{Firms: map[string]sdk.FirmConfig{
		"firm-a": {Name: "Shree Jewellers", Enabled: true, Values: values},
	}})
	return w
}

var occurred = time.Date(2026, 11, 7, 5, 11, 48, 0, time.UTC)

func loanCreatedItem() *deliveryv1.Item {
	return &deliveryv1.Item{
		DeliveryId: "d-123", Capability: "events.loan.created", FirmId: "firm-a",
		OccurredAt: timestamppb.New(occurred),
		Payload: &deliveryv1.Item_LoanCreated{LoanCreated: &eventsv1.LoanCreated{
			Customer: &modelv1.Customer{Id: "c-1", DisplayName: "Ravi Kumar", Phone: "98765 43210", PhoneE164: "+919876543210"},
			Loan: &modelv1.Loan{
				Id: "l-1", Number: "F-101",
				PledgedOn:        &modelv1.Date{Iso: "2026-11-07", Display: "07 Nov 2026"},
				Principal:        &modelv1.Money{Paise: 1_500_000, Display: "₹15,000.00"},
				InterestRate:     &modelv1.Rate{BasisPoints: 1400, Per: &modelv1.Duration{Count: 1, Unit: modelv1.DurationUnit_DURATION_UNIT_YEAR}, Display: "14% p.a."},
				TotalNetWeight:   &modelv1.Weight{Milligrams: 25_000, Display: "25.000 g"},
				TotalGrossWeight: &modelv1.Weight{Milligrams: 27_500, Display: "27.500 g"},
				TotalPieces:      2, Status: modelv1.LoanStatus_LOAN_STATUS_ACTIVE,
			},
		}},
	}
}

func TestPostsTheEventAsJSONWithItsHeaders(t *testing.T) {
	r := newReceiver(t)
	w := configured(t, r, map[string]string{"auth_header": "Bearer abc123"})

	res, err := w.Handle(context.Background(), loanCreatedItem())
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if res.Unconfirmed {
		t.Error("a 2xx from the receiver is a confirmed delivery, not an unconfirmed send")
	}
	if r.method != "POST" || r.header.Get("Content-Type") != "application/json" {
		t.Errorf("request = %s %q", r.method, r.header.Get("Content-Type"))
	}
	for k, want := range map[string]string{
		"X-Girvi-Delivery": "d-123", "X-Girvi-Event": "loan.created", "Authorization": "Bearer abc123",
	} {
		if got := r.header.Get(k); got != want {
			t.Errorf("%s = %q; want %q", k, got, want)
		}
	}
	if !strings.HasPrefix(r.header.Get("User-Agent"), "Girvi-Webhook/") {
		t.Errorf("user agent = %q", r.header.Get("User-Agent"))
	}

	b := r.body(t)
	if b["id"] != "d-123" || b["type"] != "loan.created" || b["version"] != float64(1) || b["occurred_at"] != "2026-11-07T05:11:48Z" {
		t.Errorf("envelope wrong: %v", b)
	}
	if f := b["firm"].(map[string]any); f["id"] != "firm-a" || f["name"] != "Shree Jewellers" {
		t.Errorf("firm wrong: %v", f)
	}
	data := b["data"].(map[string]any)
	if c := data["customer"].(map[string]any); c["name"] != "Ravi Kumar" || c["phone_e164"] != "+919876543210" {
		t.Errorf("customer wrong: %v", c)
	}
	l := data["loan"].(map[string]any)
	// Numbers must be JSON numbers, not strings: protobuf's own JSON would quote them.
	if p := l["principal"].(map[string]any); p["paise"] != float64(1_500_000) || p["display"] != "₹15,000.00" {
		t.Errorf("principal wrong: %v", p)
	}
	if rt := l["interest_rate"].(map[string]any); rt["basis_points"] != float64(1400) || rt["per"].(map[string]any)["unit"] != "year" {
		t.Errorf("rate wrong: %v", rt)
	}
	if wt := l["total_net_weight"].(map[string]any); wt["milligrams"] != float64(25_000) {
		t.Errorf("weight wrong: %v", wt)
	}
	if l["status"] != "active" || l["total_pieces"] != float64(2) {
		t.Errorf("loan wrong: %v", l)
	}
}

func TestEverySupportedEventHasItsOwnShape(t *testing.T) {
	r := newReceiver(t)
	w := configured(t, r, nil)

	payment := &deliveryv1.Item{DeliveryId: "d-2", FirmId: "firm-a", OccurredAt: timestamppb.New(occurred),
		Payload: &deliveryv1.Item_PaymentReceived{PaymentReceived: &eventsv1.PaymentReceived{
			Customer:             &modelv1.Customer{Id: "c-1", DisplayName: "Ravi Kumar"},
			Loan:                 &modelv1.LoanRef{Id: "l-1", Number: "F-101"},
			Amount:               &modelv1.Money{Paise: 500_000, Display: "₹5,000.00"},
			Date:                 &modelv1.Date{Iso: "2026-11-07", Display: "07 Nov 2026"},
			OutstandingPrincipal: &modelv1.Money{Paise: 1_000_000, Display: "₹10,000.00"},
		}}}
	if _, err := w.Handle(context.Background(), payment); err != nil {
		t.Fatal(err)
	}
	b := r.body(t)
	d := b["data"].(map[string]any)
	if b["type"] != "payment.received" || d["amount"].(map[string]any)["paise"] != float64(500_000) ||
		d["outstanding_principal"].(map[string]any)["paise"] != float64(1_000_000) || d["loan"].(map[string]any)["number"] != "F-101" {
		t.Errorf("payment.received wrong: %v", b)
	}

	redeemed := &deliveryv1.Item{DeliveryId: "d-3", FirmId: "firm-a", OccurredAt: timestamppb.New(occurred),
		Payload: &deliveryv1.Item_LoanRedeemed{LoanRedeemed: &eventsv1.LoanRedeemed{
			Customer:       &modelv1.Customer{Id: "c-1"},
			Loan:           &modelv1.LoanRef{Id: "l-1", Number: "F-101"},
			RedeemedOn:     &modelv1.Date{Iso: "2026-11-07", Display: "07 Nov 2026"},
			ReturnedWeight: &modelv1.Weight{Milligrams: 25_000, Display: "25.000 g"},
		}}}
	if _, err := w.Handle(context.Background(), redeemed); err != nil {
		t.Fatal(err)
	}
	b = r.body(t)
	d = b["data"].(map[string]any)
	if b["type"] != "loan.redeemed" || d["returned_weight"].(map[string]any)["milligrams"] != float64(25_000) || d["redeemed_on"].(map[string]any)["iso"] != "2026-11-07" {
		t.Errorf("loan.redeemed wrong: %v", b)
	}
}

func TestTheSignatureLetsAReceiverCheckThePost(t *testing.T) {
	r := newReceiver(t)
	w := configured(t, r, map[string]string{"signing_secret": "s3cret"})
	w.now = func() time.Time { return time.Unix(1_794_000_000, 0) }

	if _, err := w.Handle(context.Background(), loanCreatedItem()); err != nil {
		t.Fatal(err)
	}
	ts := r.header.Get("X-Girvi-Timestamp")
	if ts != "1794000000" {
		t.Errorf("timestamp = %q", ts)
	}

	// What a receiver does: HMAC-SHA256 over timestamp + "." + the exact body.
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write([]byte(ts + "." + string(r.rawBody)))
	want := "v1=" + hex.EncodeToString(mac.Sum(nil))
	if got := r.header.Get("X-Girvi-Signature"); got != want {
		t.Errorf("signature = %q; a receiver computing it as documented gets %q", got, want)
	}

	// A different secret does not verify.
	other := hmac.New(sha256.New, []byte("wrong"))
	other.Write([]byte(ts + "." + string(r.rawBody)))
	if r.header.Get("X-Girvi-Signature") == "v1="+hex.EncodeToString(other.Sum(nil)) {
		t.Error("a signature must depend on the secret")
	}
}

func TestWithoutASecretNothingIsSigned(t *testing.T) {
	r := newReceiver(t)
	w := configured(t, r, nil)
	_, _ = w.Handle(context.Background(), loanCreatedItem())
	if r.header.Get("X-Girvi-Signature") != "" {
		t.Error("no secret, no signature")
	}
}

func TestCustomerDetailsCanBeLeftOut(t *testing.T) {
	r := newReceiver(t)
	w := configured(t, r, map[string]string{"include_customer_details": "false"})
	if _, err := w.Handle(context.Background(), loanCreatedItem()); err != nil {
		t.Fatal(err)
	}
	c := r.body(t)["data"].(map[string]any)["customer"].(map[string]any)
	if c["id"] != "c-1" {
		t.Errorf("the customer id must stay: %v", c)
	}
	for _, k := range []string{"name", "phone", "phone_e164"} {
		if _, has := c[k]; has {
			t.Errorf("%s must be left out when the shop turns details off", k)
		}
	}
	if strings.Contains(string(r.rawBody), "Ravi") || strings.Contains(string(r.rawBody), "9876543210") {
		t.Error("no trace of the customer's details may remain in the body")
	}
}

func kind(err error) string {
	var perm *sdk.PermanentError
	var blocked *sdk.BlockedError
	switch {
	case err == nil:
		return "delivered"
	case errors.As(err, &perm):
		return "permanent"
	case errors.As(err, &blocked):
		return "blocked"
	}
	return "retry"
}

func TestWhatEachAnswerMeans(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{200, "delivered"}, {201, "delivered"}, {204, "delivered"},
		{301, "blocked"}, {302, "blocked"}, {307, "blocked"},
		{401, "blocked"}, {403, "blocked"}, {404, "blocked"}, {410, "blocked"},
		{400, "permanent"}, {409, "permanent"}, {422, "permanent"},
		{408, "retry"}, {425, "retry"}, {429, "retry"}, {500, "retry"}, {502, "retry"}, {503, "retry"},
	}
	for _, c := range cases {
		r := newReceiver(t)
		r.status = c.status
		w := configured(t, r, nil)
		_, err := w.Handle(context.Background(), loanCreatedItem())
		if got := kind(err); got != c.want {
			t.Errorf("HTTP %d: %s (%v); want %s", c.status, got, err, c.want)
		}
	}
}

func TestTheReceiversOwnWordsAreShownButTrimmed(t *testing.T) {
	r := newReceiver(t)
	r.status = 422
	r.reply = "missing field\nin row\t7 " + strings.Repeat("x", 300)
	w := configured(t, r, nil)
	_, err := w.Handle(context.Background(), loanCreatedItem())
	if kind(err) != "permanent" || !strings.Contains(err.Error(), "missing field in row 7") {
		t.Fatalf("err = %v", err)
	}
	if strings.ContainsAny(err.Error(), "\n\t") || len(err.Error()) > 220 {
		t.Errorf("the message must be one tidy line, got %d characters: %q", len(err.Error()), err.Error())
	}
}

func TestTheURLIsCheckedBeforeAnythingIsSent(t *testing.T) {
	cases := []struct {
		name   string
		values map[string]string
		want   string // "" means allowed
	}{
		{"missing", map[string]string{}, "enter the webhook URL"},
		{"not a url", map[string]string{"url": "not a url"}, "must start with"},
		{"wrong scheme", map[string]string{"url": "ftp://example.com/x"}, "must start with"},
		{"https is fine", map[string]string{"url": "https://hooks.example.com/x"}, ""},
		{"plain http to the internet", map[string]string{"url": "http://hooks.example.com/x"}, "plain http"},
		{"plain http allowed by the shop", map[string]string{"url": "http://hooks.example.com/x", "allow_plain_http": "true"}, ""},
		{"plain http to this computer", map[string]string{"url": "http://127.0.0.1:9090/x"}, ""},
		{"plain http on the lan", map[string]string{"url": "http://192.168.1.20/x"}, ""},
		{"plain http to localhost", map[string]string{"url": "http://localhost:9090/x"}, ""},
	}
	for _, c := range cases {
		_, err := target(c.values)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v; want it allowed", c.name, err)
		case c.want != "" && (kind(err) != "blocked" || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: %v; want blocked with %q", c.name, err, c.want)
		}
	}
}

func TestNothingIsSentWhileTheSettingsAreIncomplete(t *testing.T) {
	r := newReceiver(t)
	w := configured(t, r, map[string]string{"url": ""})
	if _, err := w.Handle(context.Background(), loanCreatedItem()); kind(err) != "blocked" {
		t.Errorf("= %v; want blocked", err)
	}
	if r.method != "" {
		t.Error("no request may be made without a URL")
	}
}

func TestAFirmThatIsOffIsNeverPostedFor(t *testing.T) {
	r := newReceiver(t)
	w := newWebhook()
	_ = w.Configure(context.Background(), sdk.Config{Firms: map[string]sdk.FirmConfig{
		"firm-a": {Enabled: false, Values: map[string]string{"url": r.srv.URL}},
	}})
	if _, err := w.Handle(context.Background(), loanCreatedItem()); kind(err) != "permanent" {
		t.Errorf("= %v; want permanent", err)
	}
	if r.method != "" {
		t.Error("nothing may be posted for a firm the plugin is off for")
	}
}

func TestAnEventItDoesNotKnowIsAPermanentFailure(t *testing.T) {
	r := newReceiver(t)
	w := configured(t, r, nil)
	_, err := w.Handle(context.Background(), &deliveryv1.Item{DeliveryId: "x", FirmId: "firm-a", Capability: "notify"})
	if kind(err) != "permanent" || r.method != "" {
		t.Errorf("= %v (posted: %q); want permanent and nothing posted", err, r.method)
	}
}

func TestAQueryStringSecretNeverAppearsInAnError(t *testing.T) {
	r := newReceiver(t)
	url := r.srv.URL + "/hook?token=SUPER-SECRET"
	w := configured(t, r, map[string]string{"url": url})
	r.srv.Close() // nothing is listening now
	_, err := w.Handle(context.Background(), loanCreatedItem())
	if kind(err) != "retry" {
		t.Fatalf("an unreachable receiver is retried, got %s (%v)", kind(err), err)
	}
	if strings.Contains(err.Error(), "SUPER-SECRET") {
		t.Errorf("the query string leaked into %q", err.Error())
	}
}

func TestHealthSaysWhatTheSettingsLookLikeWithoutCallingTheReceiver(t *testing.T) {
	r := newReceiver(t)
	w := configured(t, r, nil)
	h := w.Health(context.Background())
	if h.Status != corev1.HealthStatus_HEALTH_STATUS_OK || !strings.HasPrefix(h.Message, "Posts to ") {
		t.Errorf("healthy: %+v", h)
	}
	if r.method != "" {
		t.Error("health must not post anything to the receiver")
	}

	w = configured(t, r, map[string]string{"url": ""})
	if h = w.Health(context.Background()); h.Status != corev1.HealthStatus_HEALTH_STATUS_DEGRADED || !strings.Contains(h.Message, "enter the webhook URL") {
		t.Errorf("missing url: %+v", h)
	}

	if h = newWebhook().Health(context.Background()); h.Status != corev1.HealthStatus_HEALTH_STATUS_OK || !strings.Contains(h.Message, "not turned on") {
		t.Errorf("nothing enabled: %+v", h)
	}
}

func TestSecretSettingsAreMarkedSecret(t *testing.T) {
	secrets := map[string]bool{}
	for _, f := range newWebhook().Info().Settings {
		secrets[f.Key] = f.Secret
	}
	if !secrets["signing_secret"] || !secrets["auth_header"] || secrets["url"] {
		t.Errorf("secret flags wrong: %v", secrets)
	}
}

func TestARefusalPointsAtBothCredentials(t *testing.T) {
	r := newReceiver(t)
	r.status, r.reply = 401, "signature does not match"
	w := configured(t, r, map[string]string{"signing_secret": "wrong"})
	_, err := w.Handle(context.Background(), loanCreatedItem())
	for _, want := range []string{"HTTP 401", "signing secret", "authorization header", "signature does not match"} {
		if kind(err) != "blocked" || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v; want a blocked message containing %q", err, want)
		}
	}
}

// A receiver that echoes the credentials it was sent must not get them stored and shown.
func TestAReceiverCannotHandCredentialsBack(t *testing.T) {
	r := newReceiver(t)
	r.status, r.reply = 401, "Invalid token: Bearer abc12345XYZ for secret s3cretvalue99 (also abc12345XYZ)"
	w := configured(t, r, map[string]string{"signing_secret": "s3cretvalue99", "auth_header": "Bearer abc12345XYZ"})
	_, err := w.Handle(context.Background(), loanCreatedItem())
	if err == nil {
		t.Fatal("a 401 must be an error")
	}
	for _, leaked := range []string{"abc12345XYZ", "s3cretvalue99"} {
		if strings.Contains(err.Error(), leaked) {
			t.Errorf("%q was echoed back into the message: %v", leaked, err)
		}
	}
	if !strings.Contains(err.Error(), "[hidden]") || !strings.Contains(err.Error(), "HTTP 401") {
		t.Errorf("the message should still say what happened: %v", err)
	}
}

func TestScrubLeavesOrdinaryTextAndShortValuesAlone(t *testing.T) {
	if got := string(scrub([]byte("bad request: field a is missing"), "a", "")); got != "bad request: field a is missing" {
		t.Errorf("a short or empty secret must not mangle text: %q", got)
	}
	if got := string(scrub([]byte("token=tok_live_123"), "Bearer tok_live_123")); got != "token=[hidden]" {
		t.Errorf("the credential after the scheme is hidden: %q", got)
	}
}
