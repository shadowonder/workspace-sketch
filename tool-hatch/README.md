# ToolHatch

An independent local-first Go + Wails v3 desktop utility with a compact Mantine/React/Vite frontend. The String tab exposes **Avro JSON → Plain JSON**; the **Schema Registry** tab browses a topic’s value-schema versions through Go; the **Dynamic Proxy** tab runs a local programmable HTTP reverse proxy. Input and Namespace Prefix changes run automatically through Go; check **Manual apply** to run only with **Apply**. Output is read-only, with green Done or a red failure reason. Copy actions and System/Light/Dark themes remain; text is not persisted.

The converter recursively unwraps recognized Avro union labels using documented type/value-shape rules. It preserves ambiguous objects, precision-sensitive numbers, field order and schema-like definitions. Conversion is **schema-free and best-effort**, not guaranteed equivalent to full Avro deserialization; metadata warnings are displayed under the status. No schema input/resolution, schema normalization or Avro Parsing Canonical Form is implemented.

Examples: {"string":"hello"} becomes the JSON string "hello"; {"long":9007199254740993} becomes 9007199254740993. Unknown single-key business objects keep their keys. Set optional **Namespace Prefix** below Input to restrict named-type unwrapping (for example, com.example. matches com.example.Address). Empty uses the original automatic detection of qualified names and narrow PascalCase-like simple names. A nonempty prefix uses only literal matching, without heuristic fallback; primitives convert in either mode. Configured-prefix matching is exact, literal and case-sensitive, without trimming, regex or automatic separators; record/enum value-shape checks still apply. Nonmatching object keys remain, with recursive conversion of nested values; ambiguous/mismatched cases are preserved and reported. Actual map/record payload containers remain intact.

Each editor header copies its entire current string through the native clipboard. Output Copy is enabled only for a current successful result. Native selection/keyboard copying remain available; prefix text is not included in editor copies or persisted. Mantine persists only the theme preference.

## Schema Registry

Configuration uses two compact inline-label rows: URL/Fetch first, then a nearly full-width editable Topic input. Profile and SSL are thin divider-style sections, both initially collapsed; only their title/status is shown until expanded. Subject labels in the Versions sidebar and schema header are right aligned and preserve the end when space is limited; tooltips expose the full subject. Topic input scrolls horizontally without changing the request value.

Enter a required Schema Registry URL (`http://` or `https://`, optionally with a base path) and Topic Name. Press **Enter** or **Fetch** to retrieve `<topic>-value` versions, newest first; the latest is selected automatically. Select a sidebar version to load its formatted read-only Avro schema and use **Copy** for the whole schema. This tool makes only GET requests; it never registers, changes or deletes schemas. Schema references are displayed as returned, not resolved or normalized.

**SSL configuration** is collapsed initially. Leave Keystore Path, Truststore Path and Password all empty for ordinary HTTP/HTTPS with system trust. Any partial configuration lists the missing fields. Custom SSL requires HTTPS and all three fields; the indicator reports field completeness, not certificate validity. TLS is verified during each request.

Supported local files (read by Go, not the frontend):

- Keystore: `.jks` containing exactly one private-key entry and certificate chain; `.p12`/`.pfx` containing a private key and certificate chain; or combined `.pem` containing a certificate chain and unencrypted private key.
- Truststore: `.jks` trusted-certificate entries; Java-compatible trusted-certificate `.p12`/`.pfx`; or CA certificates in `.pem`/`.crt`. Configured trust replaces system roots for that connection.
- Native `.jks` files are decoded directly in Go; no conversion to P12 or PEM files is needed. Password is shared by both stores and JKS private keys. It is still required when enabling SSL with PEM files; unencrypted PEM itself does not use it. Encrypted PEM keys are unsupported—use PKCS#12/JKS instead. Multiple JKS private-key aliases and separate key passwords are not supported.

TLS 1.2+ retains chain/hostname validation; there is no insecure bypass. Requests time out after 20 seconds each, do not follow redirects, and accept at most 4 MiB per response; local files are regular files up to 16 MiB. Errors describe missing inputs/files, store/password problems, TLS, HTTP 401/403/404 and invalid responses without displaying raw server bodies or credentials. HTTP Basic/Bearer login is not implemented; the Password field unlocks SSL files, not HTTP authentication. Do not embed credentials in the URL.

### Profile v1

Expand **Profile** to select/apply a connection, **Save** a named profile, **Save As** a new one, or **Delete** a named profile. Selecting a target does not apply it; **Not applied** blocks Fetch until Apply or selection back to the current profile. Applying a named profile replaces URL, both SSL paths and Password while keeping Topic. Unsaved named edits still require discard confirmation. Last applied/saved selection and active tab are restored.

**Default Profile** replaces the old temporary Manual mode. It is built in, cannot be deleted, and automatically saves connection changes without Save. Empty and incomplete URL/SSL drafts are retained; Fetch still enforces URL/topic and all-or-empty SSL validation. Inline credentials/query/fragment are not saved in Default URLs. Topic is never saved. The status indicator shows Saving, Saved or Attention; wait for Saved before quitting. Writes are serialized, keeping the latest snapshot when edits arrive during a save. Editing a named profile never autosaves into Default; deleting the active named profile restores the separately stored Default rather than copying named inputs into it.

Complete profiles, including the shared SSL password, live in `os.UserConfigDir()/ToolHatch/registry-profiles.json` (macOS `~/Library/Application Support/ToolHatch/`). This is ordinary local JSON, not encrypted storage: protect the file and its backups. ToolHatch never logs credentials or stores them in localStorage. Atomic tempfile/sync/rename writes use 0600 files and restrict the dedicated app directory to 0700 where supported. No OS credential manager is used for profiles; application code signing is separate.

Version3 stores Default and named passwords directly. Version1/2 connection settings and selected IDs migrate on the next successful write. Old Keychain passwords are neither read nor deleted: affected profiles show a re-entry warning until Password is explicitly entered and saved. Default nonsecret edits preserve its saved password or legacy re-entry marker. Corrupt data is not silently overwritten. A failed autosave keeps inputs and shows Attention; Default Save permits retry. If restoration failed, fix storage and Apply again before autosave is enabled, preventing blind overwrites.

Named saves retain strict URL/SSL completeness checks; Default stores drafts. Limits remain 100 named profiles plus Default, 1 MiB metadata, 120-byte names and 1024-byte passwords. New named profiles cannot use the reserved Default Profile name; any legacy named profile with that name is shown as `(named)`. Profile sync/export/import, multiple-process coordination, HTTP Basic/Bearer auth and quit-time save interception are outside scope. Drafts persist in memory across tab navigation; Topic and unsaved named edits disappear on quit. Only active-tab ID/theme live in localStorage.

## Dynamic Proxy

A local, programmable HTTP reverse proxy implemented in Go. Enter a **Local Port** (the proxy always listens on `127.0.0.1` only) and a **Target** of the form `http(s)://host[:port]`, then click **Start**. The status badge shows the actual Go server state (polled every second while the tab is visible); the proxy keeps running while you use other tabs and stops when ToolHatch quits. Configuration and script are validated before the port is opened; port conflicts, permission errors and script errors are reported without starting. Edits made while running take effect only after Stop/Start.

Requests are forwarded with Go's `httputil.ReverseProxy`: method, path, query string, headers and body are preserved (hop-by-hop headers are handled by Go, `Host` is set to the target, no `X-Forwarded-*` headers are added). Responses are passed through unchanged. Streaming responses, including `text/event-stream` (SSE), are flushed to the client as they arrive and never pass through JavaScript; Stop ends open streams promptly. Upstream TLS uses the system trust store with full verification.

**Path Mapping** (collapsed, optional): with Source Path `/api` and Target Path `/v2`, `/api/users?x=1` is forwarded as `/v2/users?x=1` and `/api` as `/v2`. Matching uses whole path segments, so `/apix` does not match and, like every non-matching path, is forwarded unchanged. Both fields are required together.

**onRequest script** (optional; empty means plain forwarding). The script runs in an embedded [Goja](https://github.com/dop251/goja) JavaScript engine (ES5.1+ with most ES2015+ features; no Node.js APIs, modules, timers or `console`). Each request gets a fresh runtime; top-level code runs each time, so keep it to declarations. `function onRequest(req)` may be `async`.

| API | Behavior |
| --- | --- |
| `req.method`, `req.path` | Writable strings; the path is the escaped path after mapping and must start with `/` |
| `req.header(name)`, `req.headers()` | Case-insensitive lookup / snapshot object of all headers |
| `req.setHeader(name, value)`, `req.removeHeader(name)` | Modify outgoing headers |
| `req.query(name)`, `req.setQuery(name, value)`, `req.removeQuery(name)` | Query parameters; the original query string is kept byte-for-byte unless modified |
| `req.body` | Read/write UTF-8 string, max 10 MiB. Unread bodies stream through untouched; reading buffers the body and it is re-sent with a correct `Content-Length` (bytes preserved exactly if not assigned) |
| `http.request({ url, method, headers, body, timeoutMs })` | Synchronous Go-backed HTTP call; object `body` is sent as JSON. Returns `{ status, statusText, headers, body, header(name), json() }`. Redirects are returned, not followed |

```js
function onRequest(req) {
  const r = http.request({ method: 'POST', url: 'https://sso.example.internal/token', body: { client: 'cli' } });
  if (r.status !== 200) throw new Error('token request failed: ' + r.status);
  req.setHeader('Authorization', 'Bearer ' + r.json().access_token);
  const body = JSON.parse(req.body);
  body.model = 'my-model';
  req.body = JSON.stringify(body);
}
```

Limits: one `onRequest` call (including its HTTP calls) may take 20 s; each `http.request` defaults to 10 s, at most 8 calls per request, 4 MiB response bodies; scripts up to 256 KiB. A thrown error, timeout or invalid change answers the client with HTTP 500 (413 for an oversized body) and header `X-ToolHatch-Proxy-Error: script`; an unreachable target answers 502 with `X-ToolHatch-Proxy-Error: upstream`. Nothing is forwarded after a script failure. Bodies are not decompressed for scripts, and response bodies cannot be transformed.

**Activity** lists the last 100 requests (method, forwarded path without query string, status, duration, error) plus request/failure counters. Headers, query strings and bodies are never recorded or logged.

**Saved Configurations**: the current draft (port, target, mapping, script) autosaves and is restored on launch; **Save** stores a named configuration (same name replaces it), **Load** replaces the whole draft (confirming if the draft is not saved under any name), **Delete** removes it. Data lives in `os.UserConfigDir()/ToolHatch/proxy-configs.json` with the same private atomic writes as profiles (0600 file, 0700 directory). Scripts may contain secrets: this is plain local JSON, so protect the file and its backups.

## Prerequisites

- Go 1.26 or later and Wails CLI **v3.0.0**.
- Node 24.x recommended (`frontend/.nvmrc`); supported declaration: Node 22.12+ within 22.x, 24.x, or 26+. npm uses the committed lockfile.
- macOS: Xcode Command Line Tools; this scaffold targets macOS 12+. Windows: WebView2 Runtime. Linux: C compiler, pkg-config, GTK4 and WebKitGTK 6.0 development libraries.

Consult the official [installation requirements](https://v3.wails.io/getting-started/installation/) for your platform, then install the pinned CLI:

```sh
go install github.com/wailsapp/wails/v3/cmd/wails3@v3.0.0
# Ensure $(go env GOPATH)/bin is on PATH.
wails3 doctor
```

## Develop and build

Run from `tool-hatch/`, independently of `base-extension/`:

```sh
go mod download
wails3 dev
```

The development workflow generates bindings, installs frontend dependencies with `npm ci`, builds Go in development mode, starts Vite on loopback port 9245, and launches the native window. Frontend changes use Vite; Go changes rebuild the application. To change the port, use `wails3 dev -port 9246`.

```sh
wails3 build       # production frontend and thin host executable in bin/native/
wails3 task run    # launch the built application
# macOS only, optional: create a local .app without launching it
wails3 task mac:bundle
```

Host/development builds use `bin/native/ToolHatch`; Universal slices/output use `bin/universal/`. Packaging never leaves a fat executable in the native output, so `wails3 dev` can run immediately afterward. The legacy `bin/ToolHatch` file is no longer read or overwritten.

On macOS, the run task creates and signs `bin/ToolHatch.app` (configured local identity or ad-hoc fallback). No separate Task installation is needed; `wails3` runs the Taskfile.

## macOS distribution

On macOS with Xcode Command Line Tools, build both Apple Silicon and Intel binaries, combine them with `lipo`, create the complete bundle with its ToolHatch icon, sign it, and archive it:

```sh
wails3 task darwin:package:zip
python3 scripts/verify-macos-archive.py bin/ToolHatch-macos-universal.zip
```

The existing Taskfile follows the [Wails v3 Universal packaging pattern](https://v3.wails.io/guides/build/macos/), building architectures sequentially under `bin/universal/` while sharing generated frontend assets. The release bundle/ZIP names remain unchanged; `mac:bundle` normally bundles the native executable and Universal packaging explicitly supplies its own binary. The ZIP contains `ToolHatch.app`, including its executable, branded `.icns`, bundle metadata and signature. `ditto` preserves bundle structure and executable permissions. Verification extracts the actual archive and checks application identity, icon, both architectures, code signature and absence of workspace/development documents. Only this ZIP is distributed; the raw executable is not an application bundle.

### Optional local signing

By default, packaging uses offline ad-hoc signing. To use your existing self-managed macOS code-signing identity, set its exact name or SHA-1 fingerprint from `security find-identity -v -p codesigning`:

```sh
TOOLHATCH_SIGNING_IDENTITY="ToolHatch Local" wails3 task darwin:package:zip
# The same setting works with mac:bundle, task run and wails3 dev.
```

The identity must include an accessible private key in your normal Keychain search list and be valid for code signing. You manage its creation/import/access permissions in Keychain Access; the project does not create certificates, alter trust, store keys, or change your Keychain. Use a fingerprint if names are ambiguous. When the setting is empty, ad-hoc signing is used. When a configured identity is unavailable, packaging warns and falls back to ad-hoc. A selected identity that fails signing (for example, a locked key or denied access) fails the build so the problem is visible. Signing disables online timestamps; signature verification runs after signing and again on the extracted ZIP.

This option is only a convenience for local/private distribution with a stable signing identity. A self-managed identity is **not Apple Developer ID signing or Apple notarization**, and does not make public downloads trusted. No paid Apple account, notarization or CI secrets are required. The unchanged GitHub workflow has no configured identity and continues to sign ad-hoc.

GitHub-downloaded ZIPs/apps may carry quarantine metadata. Both self-managed and ad-hoc builds can be rejected by Gatekeeper because ToolHatch is not Developer ID signed/notarized. After verifying the source, users can follow Apple's normal [Open Anyway procedure](https://support.apple.com/en-us/102445); managed Macs may prohibit opening the app. A successful `codesign --verify` checks integrity, not Gatekeeper approval. Do not disable system protections or assume a locally trusted certificate transfers trust to another Mac. Older macOS and native Intel execution still require runtime checks; macOS 12 is a deployment target, not a verified compatibility claim.

### Icon assets

`build/appicon.svg` is the editable ToolHatch toolbox/terminal mark; `build/appicon.png` is the 1024px raster source for Wails; `build/darwin/icon.icns` is committed and copied to `Contents/Resources/icon.icns`, referenced by `CFBundleIconFile`. `frontend/public/toolhatch.svg` supplies the matching favicon. Ordinary builds use these checked-in assets and need no extra graphics tools.

After editing the SVG, refresh the PNG with your SVG exporter (ImageMagick was used here), copy the SVG to the frontend, then use the pinned Wails CLI to regenerate `.icns`:

```sh
magick -background none build/appicon.svg -depth 8 build/appicon.png
cp build/appicon.svg frontend/public/toolhatch.svg
wails3 generate icons -input build/appicon.png -macfilename build/darwin/icon.icns -windowsfilename /tmp/toolhatch-icon.ico
```

The temporary ICO is not distributed; no Windows packaging is introduced. Finder/Dock may cache an older icon for an already-open or previously installed copy; rebuild/relaunch the new bundle when checking it.

## Automatic GitHub releases

The repository workflow [toolhatch-release.yml](../.github/workflows/toolhatch-release.yml) runs on every push to `main`, including direct pushes and merged pull requests, with no path filters. One macOS job checks out the triggering commit (`github.sha`), installs pinned Go 1.26.4, Node 24.22.2, npm 10.9.2 and Wails 3.0.0, then runs the existing Universal packaging task. That task uses `npm ci`, generates bindings, builds both architectures and verifies the signed app ZIP. Go tests/vet and frontend tests must pass before publication.

The runner's GitHub CLI publishes `ToolHatch-macos-universal.zip` using the built-in `github.token` with `contents: write`. Tags use `toolhatch-<github.run_id>`, target the triggering commit, and stay unchanged on rerun. If that release already exists, publication is skipped without modifying its assets. GitHub generates release notes; these releases do not replace another project's repository-wide Latest label. No PR-event handling, custom publisher, manifest transfer or concurrency management is needed.

The workflow must be present on `main`, and repository/org policy must allow Actions and release/tag creation by its built-in token. A real hosted run is still needed to verify those permissions and publication. Interrupted publication may require inspecting/removing an incomplete release manually before retrying; there is no automatic draft recovery. No tag or release is created while implementing or locally verifying this workflow. Distribution remains ad-hoc signed and not notarized.

## Source layout

- `main.go`: embedded frontend, Wails service registration and one window.
- `stringservice.go`: processing dispatch, retained legacy operations and response metadata.
- `avrojson.go`: ordered schema-free union-wrapper conversion and warnings.
- `profileservice.go`: versioned complete local profiles, permission-restricted atomic writes and explicit storage errors.
- `registryservice.go`, `registrytls.go`: read-only Schema Registry HTTP/formatting and local TLS stores; `registryservice_test.go` includes HTTP and mutual-TLS fixtures.
- `proxyservice.go`, `proxyscript.go`: Dynamic Proxy lifecycle/forwarding/path mapping/activity/saved configurations and the Goja `onRequest` runtime with its `http.request` helper; `proxyservice_test.go` uses real loopback servers.
- `stringservice_test.go`, `avrojson_test.go`: legacy operation, conversion/boundary and response-contract checks.
- `frontend/`: React tool tabs, shared Editor/Copy, transient registry interaction, stable active-tab preference, generated binding imports, npm lockfile, Vite and focused UI tests.
- `Taskfile.yml`, `build/config.yml`: native build and development workflow.
- `build/darwin/Info.plist`: ToolHatch bundle identity, identifier `io.github.shadowonder.toolhatch` (bundle version currently 0.1.0; push releases use separate run identifiers).
- `build/appicon.svg`, `build/appicon.png`, `build/darwin/icon.icns`: editable source and packaged icon assets.
- `scripts/`: local signing selection and archive verification used by the existing packaging task.

`frontend/bindings/`, `frontend/dist/`, `frontend/node_modules/`, `.task/` and `bin/` are generated and ignored. Do not hand-edit bindings. Run `wails3 build` before standalone `go vet ./...` or `go build`; Go embeds the generated frontend. `npm run build` alone requires bindings to have been generated first.

## Verification

After `wails3 build` has finished generating bindings and frontend assets (do not run Go checks concurrently with it):

```sh
go test ./...
go vet ./...
cd frontend
npm test
```

Tests retain legacy operations and cover primitive/named wrappers, map/record containers, ambiguity/schema preservation, precision/order, malformed JSON, response warnings, literal namespace matching/empty-prefix/nested preservation, prefix automatic/manual behavior, request ordering/failures, clipboard, themes and StrictMode replay. Registry tests additionally cover versions/errors/path escaping and real local mutual TLS for PEM/PKCS#12/JKS, including untrusted CA/hostname rejection. UI tests cover registry ordering, copy, stale requests, SSL visibility, long topics, profile restoration/confirmation/CRUD/errors and active-tab persistence. Proxy Go tests cover path mapping, validation, unchanged forwarding, script request transformation with an outbound call, binary bodies, script errors/timeouts, upstream failure, start/stop/port conflict/restart, SSE streaming and stop during an open stream, and saved-configuration storage; proxy UI tests cover start/stop state, polling races, autosave and saved configurations. Go profile tests use temporary local files, verify complete password restoration, permissions, migration/re-entry, named/Default isolation and failure handling. Tests and application profile persistence never access a credential manager. UI tests mock the Wails boundary; native launch checks are separate. Windows/Linux and real GitHub publication remain unverified. Current native test evidence and limitations live in the local ignored workspace status/history.

The Go response is `{ result, metadata: { operation, success, message } }`. Preserve this contract; metadata is also reserved for future optional content information such as JSON detection and format details, which are not implemented yet. The optional warnings string array is implemented for best-effort conversion limits; required outcome fields remain unchanged. Input/operation/mode changes clear previous output and invalidate in-flight requests; only the latest applicable response can update the interface.

Official reference: [first application and build workflow](https://v3.wails.io/getting-started/your-first-app/).
