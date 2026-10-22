// Package e2e is the web workspace's end-to-end suite (ADR-0052 §10): Go
// tests driving a headless Chromium with chromedp against a real erp-web
// in front of the in-process API (internal/app/apitest) and PostgreSQL,
// with axe-core, vendored in testdata and pinned by its SHA-256, injected
// into every page at zero WCAG 2.2 A and AA violations. axe-core is test
// tooling that runs in the test browser; it is not part of the platform.
//
// The suite needs a Chromium-based browser: ERP_E2E_BROWSER names one,
// else the usual install locations are tried. Without one it is skipped,
// unless ERP_E2E_REQUIRE_BROWSER is set, as CI sets it, where it fails.
package e2e
