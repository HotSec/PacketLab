# Changelog

## v2.0 (2026-05-21)

### Features
- **HTTPS MITM Decryption** — Capture and view HTTPS traffic in plaintext. Self-signed CA certificate, one-click install guide.
- **API Map Tree View** — Visualize all captured API endpoints as a collapsible tree grouped by host, with method-color-coded leaves and status code badges.
- **Request Interception** — Manual approval mode with Allow/Drop/Modify actions. Pending requests appear in the main list with a yellow indicator.
- **Edit & Resend** — Modify method, URL, headers, and body of any captured request and resend through the proxy.
- **Batch Writing** — High-performance SQLite writes using WAL mode and batched transactions (50-entry buffer, 200ms flush).
- **i18n** — Chinese/English interface toggle with localStorage persistence.
- **Dark/Light Theme** — CSS variable-based theming with smooth transition.
- **Real-time WebSocket** — Live push of new captured requests to the frontend.
- **Search & Filter** — Filter by method chips, URL/status search, host-based filtering, error-only filter.
- **API Notes** — Add text notes to any API endpoint. Custom modal editor with save/delete.
- **Context Menu** — Right-click API map nodes for note editing, path copying, and host filtering.
- **Resizable Panels** — Drag the divider between request list and detail panel.
- **Stagger Animation** — Smooth cubic-bezier list entry animation with staggered delay.
- **Tab Indicator** — Sliding underline animation for detail tab switching.
- **Host Selector** — Searchable dropdown with incremental loading (20 hosts per page).

### Fixes
- LimitReader body truncation causing broken browser rendering.
- MITM non-HTTP protocol handling (WNS, telemetry, etc.) excluded from decryption.
- Batch write log noise reduced (only >1 entry logs).
- `detailContent` display:none→flex reflow issue on tab switching.
- `shouldMITM` keyword matching tightened (removed overly broad "events.data").
- API map tree nested children overlap due to `overflow:hidden` on collapsed nodes.

---

## v1.0 (2026-05-19)

### Initial Release
- HTTP/HTTPS proxy with request/response capture
- SQLite persistence with pagination
- Basic web UI with request list and detail view
- Method-based filtering
- CA certificate generation for MITM
- Configurable proxy and API ports
