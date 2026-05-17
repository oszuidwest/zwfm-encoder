# Smoke test evidence for #247 / PR #257

Screenshots captured against a locally-built encoder on macOS via Playwright
MCP. Server: `./encoder -config /tmp/encoder-smoke/config.json` on :8089;
browser session authenticated as `admin` / `encoder`.

The four manual smoke scenarios listed in the PR test plan are documented
here with one or more screenshots per scenario plus an asserted-state
inspection captured at each step.

## Scenario 1 - Graph save / clear / undo / re-clear / save

| Step | Screenshot | Asserted state |
|---|---|---|
| Save secret + reload, mask placeholder visible | `01a-after-reload-secret-saved-mask.png` | `graph_has_secret=true`, input enabled, mask `••••••••` |
| Clear (confirm dialog) | inline assert | `confirm()` text: `Remove the saved Microsoft Graph client secret on save?` |
| Pending clear | `01b-graph-pending-clear-input-disabled.png` | secret input `disabled`, placeholder `(will be cleared on save)`, Email Test button `disabled`, hint visible: `Save changes first to test with the cleared secret.` |
| Undo clear | `01c-graph-after-undo-input-enabled-mask-restored.png` | input enabled, mask `••••••••` restored, `clearSecret=false`, snapshot `null` |
| Re-clear + Save | `01d-graph-after-save-secret-cleared.png` | `graph_has_secret=false`, other Graph fields (tenant, client, from, recipients) preserved |

## Scenario 2 - Graph conflict 400 via DevTools fetch

| Step | Screenshot | Asserted state |
|---|---|---|
| `POST /api/settings` with `clear_graph_client_secret=true` + `graph_client_secret="x"` | `02-graph-conflict-400.png` | `HTTP 400`, body `{"errors":["clear_graph_client_secret: conflicts with non-empty graph_client_secret"]}` |

## Scenario 3 - WhatsApp disable / undo / save

| Step | Screenshot | Asserted state |
|---|---|---|
| Save complete WhatsApp + reload | `03a-whatsapp-after-reload-token-saved.png` | `whatsapp_has_token=true`, all 5 fields populated, Disable button visible |
| Click Disable WhatsApp (confirm dialog) | inline assert | `confirm()` text: `Disable WhatsApp notifications and clear all WhatsApp configuration (phone number, recipients, template, access token)?` |
| Pending disable | `03b-whatsapp-pending-disable-all-disabled.png` | all 5 inputs `disabled` with `(will be cleared on save)` placeholder; WhatsApp Test button `disabled` |
| Undo disable | `03c-whatsapp-after-undo-fields-restored.png` | `phoneNumberId`, `recipients`, `templateName`, `templateLanguage` restored from snapshot; `accessToken=""` (never pre-filled, by design); `clearToken=false` |
| Re-disable + Save | `03d-whatsapp-after-save-channel-disabled.png` | `whatsapp_has_token=false`, all 5 saved fields empty |

## Scenario 4 - Deprecated implicit-disable path (the breaking change)

| Step | Screenshot | Asserted state |
|---|---|---|
| With saved complete WhatsApp, `POST /api/settings` with all 5 WhatsApp fields `""` + `whatsapp_access_token=""` + **no** `clear_whatsapp_access_token` | `04-whatsapp-deprecated-implicit-disable-400.png` | `HTTP 400`, body `{"errors":["whatsapp_phone_number_id: is required when WhatsApp is configured","whatsapp_recipients: is required when WhatsApp is configured"]}` |

This proves the implicit-disable path is gone: the backend preserves the
saved token unconditionally on this request shape, the all-or-nothing
validator then rejects the visible-empty configuration.
