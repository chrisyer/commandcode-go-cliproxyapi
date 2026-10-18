package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"

	"commandcode-go-cliproxyapi/internal/config"
	"commandcode-go-cliproxyapi/internal/errclass"
)

// Quota-provider ABI method names (quota.identifier/describe/fetch/reset): the
// surface a host panel calls to render one credential's quota natively, the
// same one codex/antigravity credentials use. Aliased locally so the dispatch
// site reads as this plugin's quota arm rather than a bare SDK constant.
const (
	methodQuotaIdentifier = pluginabi.MethodQuotaIdentifier
	methodQuotaDescribe   = pluginabi.MethodQuotaDescribe
	methodQuotaFetch      = pluginabi.MethodQuotaFetch
	methodQuotaReset      = pluginabi.MethodQuotaReset
)

// Quota wire shapes come straight from the SDK so the plugin cannot drift from
// the host contract. The panel renders each bucket as a REMAINING fraction
// (1 = untouched), the inverse of the "used" percent this plugin's own page
// shows; the SDK's UnmarshalJSON also tolerates the snake_case spelling, but
// this plugin only ever marshals the camelCase tags.
type (
	quotaFetchRequest    = pluginapi.QuotaFetchRequest
	quotaFetchResponse   = pluginapi.QuotaFetchResponse
	quotaDescribeRequest = pluginapi.QuotaDescribeRequest
	quotaDescribeReply   = pluginapi.QuotaDescribeResponse
	quotaBucket          = pluginapi.QuotaBucket
	quotaGroup           = pluginapi.QuotaGroup
	quotaMetric          = pluginapi.QuotaMetric
	quotaSubscription    = pluginapi.QuotaSubscription
)

// quotaProviderIdentifier is the provider key this quota provider serves. It
// matches ProviderID so the host maps each commandcode credential onto this
// plugin without a DescribeQuota round-trip.
const quotaProviderIdentifier = ProviderID

// handleQuota dispatches the quota-provider RPC surface. Every arm is
// best-effort from the host's perspective: an unsupported or failing call is a
// redacted error envelope, never a panic across the FFI boundary.
func (m *Manager) handleQuota(method string, request []byte) ([]byte, error) {
	switch method {
	case methodQuotaIdentifier:
		return okEnvelope(map[string]string{"identifier": quotaProviderIdentifier}), nil
	case methodQuotaDescribe:
		return okEnvelope(quotaDescribeReply{
			SupportedProviders: []string{quotaProviderIdentifier},
			DisplayName:        "CommandCode",
			// Account credits cannot be reset by a client, so the panel must
			// not offer a reset action.
			SupportsReset: false,
		}), nil
	case methodQuotaReset:
		return classEnvelope(&errclass.Error{
			Class:   errclass.ClassUnsupported,
			Message: "commandcode quota cannot be reset by a client",
		}), nil
	case methodQuotaFetch:
		var req quotaFetchRequest
		if len(request) > 0 && json.Unmarshal(request, &req) != nil {
			return ErrEnvelope("invalid_request", "malformed quota fetch request body"), nil
		}
		resp, err := m.fetchCredentialQuota(context.Background(), req)
		if err != nil {
			return ErrEnvelope("quota_failure", err.Error()), nil
		}
		return okEnvelope(resp), nil
	default:
		return ErrEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// fetchCredentialQuota resolves the credential's API key, reads its account
// snapshot, and normalizes it for the panel. The key is taken from the auth
// attributes the host supplies (populated by ParseAuth), falling back to the
// auth storage JSON and finally to the configured key whose derived auth ID
// matches the requested credential.
func (m *Manager) fetchCredentialQuota(ctx context.Context, req quotaFetchRequest) (quotaFetchResponse, error) {
	m.mu.RLock()
	baseURL, timeout := m.cfg.BaseURL, m.cfg.RequestTimeout
	keys := append([]config.APIKey(nil), m.cfg.APIKeys...)
	m.mu.RUnlock()

	key := quotaRequestKey(req, keys)
	if key == "" {
		return quotaFetchResponse{}, fmt.Errorf("quota unavailable: credential key could not be resolved")
	}
	usage, label, email, err := fetchQuota(ctx, m.bridge, baseURL, timeout, key)
	if err != nil {
		return quotaFetchResponse{}, err
	}
	name := email
	if name == "" {
		name = label
	}
	// The host's own auth entry is titled from the mailbox; mirror that as the
	// group title only when the account reported one, so the quota block is
	// identified by account rather than by an opaque hash.
	if name == "" {
		name = "CommandCode"
	}
	return quotaToFetchResponse(usage, name), nil
}

// quotaRequestKey picks the upstream API key for one quota.fetch. Preference
// order is attributes (the host's canonical routing value) → storage JSON →
// the configured key matching the credential ID, which keeps working when a
// host omits attributes.
func quotaRequestKey(req quotaFetchRequest, keys []config.APIKey) string {
	if v := strings.TrimSpace(req.Attributes["api_key"]); v != "" {
		return v
	}
	if len(req.StorageJSON) > 0 && gjson.ValidBytes(req.StorageJSON) {
		if v := strings.TrimSpace(gjson.GetBytes(req.StorageJSON, "api_key").String()); v != "" {
			return v
		}
	}
	id := strings.TrimSpace(req.AuthID)
	if id == "" {
		return ""
	}
	for _, k := range keys {
		if got, _ := quotaIdentity(k.Value); got == id {
			return k.Value
		}
	}
	return ""
}

// quotaToFetchResponse maps the account snapshot onto the host's normalized
// quota shape. The panel renders each bucket as "% remaining", so every window
// is inverted here; a window without a denominator (unknown plan, unlimited
// upstream) is omitted rather than shown as a misleading 0%.
func quotaToFetchResponse(usage quotaUsage, groupName string) quotaFetchResponse {
	out := quotaFetchResponse{
		Subscription: &quotaSubscription{Plan: usage.Plan},
	}
	if usage.Plan == "" {
		out.Subscription.Plan = "CommandCode"
	}
	out.Summary = []quotaMetric{{
		Key:    "credits_left",
		Label:  "Credits left",
		Value:  usage.CreditsLeft,
		Format: "number",
	}}
	if usage.PurchasedCredits > 0 {
		out.Summary = append(out.Summary, quotaMetric{
			Key:    "purchased_credits",
			Label:  "Purchased credits",
			Value:  usage.PurchasedCredits,
			Format: "number",
		})
	}

	group := quotaGroup{DisplayName: groupName}
	for _, w := range []struct {
		window quotaWindow
		name   string
		label  string
	}{
		{usage.FiveHour, "5h", "5-hour window"},
		{usage.Weekly, "weekly", "Weekly window"},
	} {
		if bucket, ok := bucketFromWindow(w.window, w.name, w.label); ok {
			group.Buckets = append(group.Buckets, bucket)
		}
	}
	if usage.Month != nil {
		if bucket, ok := bucketFromWindow(*usage.Month, "monthly", "Monthly credits"); ok {
			group.Buckets = append(group.Buckets, bucket)
		}
	}
	if len(group.Buckets) > 0 {
		out.Groups = []quotaGroup{group}
	}
	return out
}

// bucketFromWindow converts one used/cap window into the panel's
// remaining-fraction bucket. Windows with no cap carry no denominator, so they
// are reported as absent instead of as a fabricated 0% (or unlimited) bar.
func bucketFromWindow(w quotaWindow, window, label string) (quotaBucket, bool) {
	if w.Cap <= 0 {
		return quotaBucket{}, false
	}
	remaining := (w.Cap - w.Used) / w.Cap
	if remaining < 0 {
		remaining = 0
	}
	if remaining > 1 {
		remaining = 1
	}
	return quotaBucket{
		Window:            window,
		RemainingFraction: remaining,
		ResetTime:         w.ResetsAt,
		Description:       label,
	}, true
}
