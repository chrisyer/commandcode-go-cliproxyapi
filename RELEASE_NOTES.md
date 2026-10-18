# Release Notes

## v0.2.0 — Native per-credential quota

CommandCode credentials now report quota the way codex/antigravity credentials
do, with no plugin page and no panel extension.

**Added**

- **Quota provider capability**: the plugin advertises `quota_provider`, so the
  panel renders each CommandCode credential's plan and remaining quota inside
  its own credential list, refreshed from the credential row, next to the
  built-in codex/antigravity entries. The normalized response reports remaining
  fractions (`1.0` = untouched) plus the raw remaining and purchased credit
  counts.
- **Panel-agnostic quota page**: the standalone `CommandCode Quota` page now
  recovers the management key from whichever panel hosts it — `enc::v1::` (the
  official Management Center), `enc::v2::` (CPA-Manager-Plus), scoped
  `cli-proxy-auth*` entries, or plain JSON — and, when none is readable, asks
  for the key once and keeps it for the tab instead of dead-ending on a
  Management Center-only message.

**Upgrade notes**

- Replace the plugin binary and restart CLIProxyAPI.
- **Minimum host is now CLIProxyAPI `v7.2.159`** (any `v8.x` works): the plugin
  builds against the v8 plugin SDK and reports `schema_version 6`, which hosts
  below `v7.2.159` reject. Schema 6 is also what makes the host stop
  HTML-escaping plugin management JSON, so the quota values arrive intact.

## v0.1.1 — Accept array function_call_output results

Responses `function_call_output.output` may be a string or an array of text
parts (`input_text`, `output_text`, or `text`). The array shape is flattened
into the string Chat Completions and Messages already send upstream. Non-text
parts are still rejected.

**Upgrade notes**

- Replace the plugin binary and restart CLIProxyAPI.

## v0.1.0 — CommandCode Go/GOAT/Pro/Max provider

First release of the plugin as a CommandCode Go/GOAT/Pro/Max provider, derived from
opencode-go-cliproxyapi v0.1.7.

**Adaptations**

- **Reasoning preservation**: upstream `reasoning`, `reasoning_details[].text`
  and `reasoning_content` are read together (mirrored values counted once) and
  emitted as a leading Claude `thinking` block, a leading Responses `reasoning`
  item with summary events, or a backfilled `reasoning_content` on the OpenAI
  passthrough — streaming and non-streaming.
- **Chat-completions-only routing**: every catalog model defaults to
  `{base-url}/chat/completions`, because CommandCode's `/messages` endpoint
  accepts Claude models only and `/responses` is not registered. The family
  prefix table was removed; `route-overrides` remain for other upstreams.
- **Effort passthrough**: models without thinking metadata (all CommandCode
  models) are no longer validated or clamped locally — `xhigh`/`max` reach the
  upstream verbatim, `auto`/`none` omit the field. A model that declares levels
  or bounds opts back into eager validation.
- **Quota page against the real account API**: cards show account email, plan,
  remaining plan credits, and the rolling 5-hour/weekly windows from
  `/alpha/billing/credits`, `/alpha/billing/subscriptions` and `/alpha/whoami`.
- Defaults renamed (`commandcode` prefix, `api.commandcode.ai/provider/v1`
  base URL, `commandcode-go-cliproxyapi` plugin id).

**Upgrade notes**

- The provider namespace changed, so client model ids must be updated
  (for example `commandcode/deepseek/deepseek-v4.1-flash`).
- Replace the plugin binary, restart CLIProxyAPI, and hard-refresh the
  Management Center.
