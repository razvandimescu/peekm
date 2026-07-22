# Vendored third-party libraries

These power the **DOCX export** in the shared view (`shared-view.html`). They are
committed here and embedded into the peekm binary (`//go:embed theme/*`), then
served same-origin under `/s/{token}/_vendor/…` and lazy-loaded only when a viewer
clicks export. Self-hosting keeps the feature working on every access path
(relay, LAN, tailscale, localhost) with no third-party CDN and no Subresource
Integrity to drift.

| File | Package | Source |
|------|---------|--------|
| `html-docx.min.js` | `html-docx-js@0.3.1` | `https://cdn.jsdelivr.net/npm/html-docx-js@0.3.1/dist/html-docx.min.js` |
| `html-to-image.min.js` | `html-to-image@1.11.13` | `https://cdn.jsdelivr.net/npm/html-to-image@1.11.13/dist/html-to-image.min.js` |

> Note: `html-docx-js@0.3.1` does not ship a minified file in its npm tarball;
> jsdelivr synthesizes it on the fly (which is why its CDN SHA-384 drifted and
> broke our old SRI — the original reason for vendoring). The copy here is frozen.

## Re-verify

```sh
curl -fsSL "https://cdn.jsdelivr.net/npm/html-to-image@1.11.13/dist/html-to-image.min.js" \
  | openssl dgst -sha384 -binary | openssl base64 -A
# sha384-wNc5eWDV2JUK6HVVRPhI8Bfc08Br5nd66LmdO7jDYWK0fd5uNvfqKKS9QXEOLwfO
```

(`html-docx.min.js` matches `sha384-2n96D42bsIL8DZhdHuMXFjXl/9kWZ9dm7auvMAdBFL7Em+Bnkmj+C+QLTSs/+weU`
against the current jsdelivr build, but that value is not stable upstream — trust
the committed bytes, not the CDN.)
