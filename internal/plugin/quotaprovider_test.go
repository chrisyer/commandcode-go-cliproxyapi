package plugin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"commandcode-go-cliproxyapi/internal/config"
)

// quotaProviderFixture answers the three account endpoints for one key and
// records the Authorization headers it saw, so a test can assert which key the
// quota provider resolved.
type quotaProviderFixture struct {
	key    string
	auths  []string
	client *fakeCaller
}

const quotaProviderYAML = "api-keys:\n  - value: quota-provider-key\n" +
	"catalog-url: https://catalog.test/models\n"

func newQuotaProviderFixture(t *testing.T, key string) (*Manager, *quotaProviderFixture) {
	t.Helper()
	fx := &quotaProviderFixture{key: key}
	fx.client = &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			if method == pluginabi.MethodHostAuthList {
				return hostOK(hostAuthListResponse{}), nil
			}
			return hostOK(map[string]any{}), nil
		}
		var wire struct {
			URL     string      `json:"url"`
			Headers http.Header `json:"headers"`
		}
		if err := json.Unmarshal(payload, &wire); err != nil {
			t.Fatalf("bad host.http.do payload: %s", payload)
		}
		fx.auths = append(fx.auths, wire.Headers.Get("Authorization"))
		switch wire.URL {
		case "https://quota.test/alpha/billing/credits":
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(
				`{"credits":{"monthlyCredits":51.5,"purchasedCredits":2},"windowLimits":{"limited":true,` +
					`"fiveHour":{"used":0.5,"cap":14,"exceeded":false,"resetAt":1789657174807},` +
					`"weekly":{"used":18.5,"cap":35,"exceeded":false,"resetAt":1790136440498}}}`)}), nil
		case "https://quota.test/alpha/billing/subscriptions":
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(
				`{"success":true,"data":{"planId":"individual-goat","status":"active","currentPeriodEnd":"2026-10-16T04:03:37.000Z"}}`)}), nil
		case "https://quota.test/alpha/whoami?limits=1":
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(
				`{"success":true,"user":{"email":"dev@example.test"},"org":null}`)}), nil
		case "https://catalog.test/models":
			// Registration refreshes the catalog; an empty list keeps this
			// fixture focused on the account endpoints.
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"data":[]}`)}), nil
		}
		t.Fatalf("unexpected quota URL %q", wire.URL)
		return nil, nil
	}}
	m := NewManager(NewHostBridge(fx.client.call))
	m.cfg = config.Config{
		BaseURL:        "https://quota.test/provider/v1",
		RequestTimeout: config.DefaultRequestTimeout,
		APIKeys:        []config.APIKey{{Value: key}},
	}
	return m, fx
}

// The host calls quota.* on any plugin advertising the capability; the
// identifier must be this plugin's provider key so credentials map onto it
// without a DescribeQuota round-trip.
func TestQuotaProviderIdentifierAndDescribe(t *testing.T) {
	m, _ := newQuotaProviderFixture(t, "quota-provider-key")
	var id struct{ Identifier string }
	decodeResult(t, mustHandle(t, m, methodQuotaIdentifier, []byte(`{}`)), &id)
	if id.Identifier != ProviderID {
		t.Fatalf("quota identifier = %q, want %q", id.Identifier, ProviderID)
	}
	var desc quotaDescribeReply
	decodeResult(t, mustHandle(t, m, methodQuotaDescribe, []byte(`{}`)), &desc)
	if len(desc.SupportedProviders) != 1 || desc.SupportedProviders[0] != ProviderID || desc.SupportsReset {
		t.Fatalf("describe = %+v", desc)
	}
	if desc.DisplayName == "" {
		t.Fatal("describe must carry a display name")
	}
}

// quota.fetch normalizes the account snapshot into the panel's remaining-
// fraction buckets: the panel renders "% remaining", which is the inverse of
// the used percent the plugin's own page shows.
func TestQuotaProviderFetchNormalizesWindows(t *testing.T) {
	const key = "quota-provider-key"
	m, fx := newQuotaProviderFixture(t, key)
	id, _ := quotaIdentity(key)
	body, _ := json.Marshal(quotaFetchRequest{
		AuthID:     id,
		Provider:   ProviderID,
		Attributes: map[string]string{"api_key": key},
	})
	var got quotaFetchResponse
	decodeResult(t, mustHandle(t, m, methodQuotaFetch, body), &got)

	if got.Subscription == nil || got.Subscription.Plan != "GOAT" {
		t.Fatalf("subscription = %+v, want the resolved plan", got.Subscription)
	}
	if len(got.Groups) != 1 || got.Groups[0].DisplayName != "dev@example.test" {
		t.Fatalf("groups = %+v", got.Groups)
	}
	byWindow := map[string]quotaBucket{}
	for _, b := range got.Groups[0].Buckets {
		byWindow[b.Window] = b
	}
	// five-hour: used 0.5/14 -> 13.5/14 remaining.
	if b := byWindow["5h"]; b.RemainingFraction < 0.96 || b.RemainingFraction > 0.97 {
		t.Errorf("5h remaining = %v, want ~0.964", b.RemainingFraction)
	}
	if b := byWindow["weekly"]; b.RemainingFraction < 0.46 || b.RemainingFraction > 0.48 {
		t.Errorf("weekly remaining = %v, want ~0.471", b.RemainingFraction)
	}
	if b := byWindow["monthly"]; b.RemainingFraction < 0.74 || b.RemainingFraction > 0.75 {
		t.Errorf("monthly remaining = %v, want ~0.743", b.RemainingFraction)
	}
	if b := byWindow["5h"]; b.ResetTime != time.UnixMilli(1789657174807).UTC().Format(time.RFC3339) {
		t.Errorf("5h reset = %q", b.ResetTime)
	}
	// The summary keeps the raw credit counts, which the panel prints verbatim.
	if len(got.Summary) != 2 || got.Summary[0].Key != "credits_left" || got.Summary[0].Value != 51.5 || got.Summary[1].Value != 2 {
		t.Fatalf("summary = %+v", got.Summary)
	}
	if len(fx.auths) == 0 {
		t.Fatal("quota fetch did not call upstream")
	}
	for _, auth := range fx.auths {
		if auth != "Bearer "+key {
			t.Fatalf("upstream auth = %q, want the credential key", auth)
		}
	}
}

// A host that omits attributes must still work: the key falls back to the
// storage JSON and then to the configured key matching the credential ID.
func TestQuotaProviderFetchResolvesKeyWithoutAttributes(t *testing.T) {
	const key = "quota-provider-key"
	id, _ := quotaIdentity(key)
	cases := map[string]quotaFetchRequest{
		"from storage json": {AuthID: id, StorageJSON: []byte(`{"type":"commandcode","api_key":"` + key + `"}`)},
		"from auth id":      {AuthID: id},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			m, fx := newQuotaProviderFixture(t, key)
			body, _ := json.Marshal(req)
			var got quotaFetchResponse
			decodeResult(t, mustHandle(t, m, methodQuotaFetch, body), &got)
			if len(got.Groups) != 1 {
				t.Fatalf("groups = %+v", got.Groups)
			}
			for _, auth := range fx.auths {
				if auth != "Bearer "+key {
					t.Fatalf("upstream auth = %q, want the resolved key", auth)
				}
			}
		})
	}
}

// An unresolved credential must fail closed with a redacted error: no key
// material may reach the panel, and no upstream call may be attempted.
func TestQuotaProviderFetchUnknownCredentialIsRedacted(t *testing.T) {
	const key = "quota-provider-key"
	m, fx := newQuotaProviderFixture(t, key)
	body, _ := json.Marshal(quotaFetchRequest{AuthID: "commandcode-key-unknown"})
	resp := mustHandle(t, m, methodQuotaFetch, body)
	env := decodeEnv(t, resp)
	if env.OK || env.Error == nil {
		t.Fatalf("expected an error envelope, got %s", resp)
	}
	if strings.Contains(string(resp), key) || len(fx.auths) != 0 {
		t.Fatalf("unknown credential leaked or called upstream: %s auths=%v", resp, fx.auths)
	}
}

// commandcode credits cannot be reset by a client, so reset is reported
// unsupported rather than silently succeeding.
func TestQuotaProviderResetIsUnsupported(t *testing.T) {
	m, _ := newQuotaProviderFixture(t, "quota-provider-key")
	env := decodeEnv(t, mustHandle(t, m, methodQuotaReset, []byte(`{}`)))
	if env.OK || env.Error == nil || env.Error.Code != "unsupported_protocol_or_parameter" {
		t.Fatalf("reset envelope = %s", mustHandle(t, m, methodQuotaReset, []byte(`{}`)))
	}
}

// Registration must advertise the quota capability so hosts with the feature
// surface this credential's quota natively.
func TestRegistrationAdvertisesQuotaProvider(t *testing.T) {
	m, _ := newQuotaProviderFixture(t, "quota-provider-key")
	var reg registrationResult
	decodeResult(t, mustHandle(t, m, pluginabi.MethodPluginRegister, lifecycleRequestBody(quotaProviderYAML)), &reg)
	if !reg.Capabilities.QuotaProvider {
		t.Fatalf("capabilities = %+v, want quota_provider", reg.Capabilities)
	}
}

// A window with no denominator (unknown plan or uncapped upstream) must be
// omitted, not rendered as a fabricated 0% bar.
func TestBucketFromWindowOmitsUndenominated(t *testing.T) {
	if _, ok := bucketFromWindow(quotaWindow{Used: 3, Cap: 0, Status: "unlimited"}, "5h", "5-hour window"); ok {
		t.Fatal("uncapped window must be omitted")
	}
	b, ok := bucketFromWindow(quotaWindow{Used: 10, Cap: 5, Status: "exceeded"}, "weekly", "Weekly window")
	if !ok || b.RemainingFraction != 0 {
		t.Fatalf("over-cap bucket = %+v ok=%t, want remaining 0", b, ok)
	}
}
