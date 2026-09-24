# InferMesh for macOS — How to open this app

This `.dmg` contains **InferMesh.app**, a desktop manager for your InferMesh
inference pool (router + worker + system tray).

## If macOS says "Apple could not verify InferMesh.app is free of malware"

This is **Gatekeeper**, macOS's default malware check. InferMesh is ad-hoc
signed with the hardened runtime enabled — it is not signed with an Apple
**Developer ID** certificate, and it is **not notarized**, because that
requires a paid Apple Developer account ($99/yr). The app itself is safe; it
is the signature that Gatekeeper cannot verify.

### Option 1 — right-click → Open (recommended, no admin)

1. In Finder, **right-click** (or Control-click) `InferMesh.app`.
2. Choose **Open** from the menu.
3. Click **Open** again in the dialog that appears.

This works for ad-hoc-signed apps with the hardened runtime enabled, which is
exactly what this build produces.

### Option 2 — System Settings → Privacy & Security → Open Anyway

After the first blocked launch:

1. Open **System Settings** → **Privacy & Security**.
2. Scroll to the bottom and click **Open Anyway** next to the blocked
   "InferMesh.app" entry.
3. Click **Open** in the confirmation dialog.

### Option 3 — disable Gatekeeper for this session (admin)

```bash
sudo spctl --master-disable
```

Then open the app normally. Re-enable with:

```bash
sudo spctl --master-enable
```

Use this only if you trust the build and want to bypass Gatekeeper entirely.

## For a fully Gatekeeper-free build

To sign with a Developer ID and notarize so Gatekeeper never blocks the app,
you need an Apple Developer account. Once you have one, run:

```bash
cd app && wails3 task darwin:sign:notarize
```

See `app/build/darwin/Taskfile.yml` (`task darwin:sign:notarize`) and
`app/build/darwin/entitlements.plist` for the signing configuration.

## Signing details

This build is ad-hoc signed (`codesign --sign -`) with the **hardened
runtime** enabled and three entitlements applied (see
`app/build/darwin/entitlements.plist`):

- `com.apple.security.cs.allow-jit` — JavaScriptCore JIT in the Wails webview
- `com.apple.security.cs.allow-unsigned-executable-memory` — Obj-C runtime bridge
- `com.apple.security.cs.disable-library-validation` — Wails runtime dylib

These are the minimum entitlements for the Wails v3 webview to run correctly
under the hardened runtime. To ship without Gatekeeper blocking at all, sign
with a Developer ID certificate and notarize:

```bash
cd app && wails3 task darwin:sign:notarize
```

## Version

InferMesh 0.1.0 — ad-hoc signed, hardened runtime enabled.