# macOS App Packaging & Gatekeeper Workaround

This document explains how the InferMesh desktop app is packaged for macOS and
what to do when Gatekeeper blocks the app.

## Current signing state

| Build target | Signature type | Hardened runtime | Entitlements |
|--------------|----------------|------------------|--------------|
| `make package-app` | Ad-hoc (`codesign --sign -`) | ✅ Enabled | ✅ Applied |
| `make package-app-dmg` | Ad-hoc (`codesign --sign -`) | ✅ Enabled | ✅ Applied |
| Developer ID (not configured) | Developer ID Application | ✅ Enabled | ✅ Applied |

**The shipped builds are ad-hoc signed with the hardened runtime enabled.**
This means:
- The app has a valid code signature (verified by `codesign --verify --deep --strict`)
- The hardened runtime is active, with these entitlements:
  - `com.apple.security.cs.allow-jit` — JavaScriptCore JIT in the Wails webview
  - `com.apple.security.cs.allow-unsigned-executable-memory` — Obj-C runtime bridge
  - `com.apple.security.cs.disable-library-validation` — Wails runtime dylib
- The signature is ad-hoc (no Apple Developer ID certificate)
- **Gatekeeper will block the app** when downloaded from the internet

## Why Gatekeeper blocks the app

Apple's Gatekeeper requires **notarization** for apps downloaded from the
internet. Notarization requires:
1. An Apple Developer Program membership ($99/year)
2. A "Developer ID Application" certificate
3. Submitting the app to Apple's notarization service

Without a paid Apple Developer account, we cannot notarize. The ad-hoc
signature is valid cryptographically, but Gatekeeper treats it as
"unverified by Apple" and shows the "Apple could not verify this app is free
of malware" dialog.

## Workaround: How to open the app

### Option 1 — right-click → Open (recommended, no admin)

1. In Finder, **right-click** (or Control-click) `InferMesh.app`.
2. Choose **Open** from the menu.
3. Click **Open** again in the dialog that appears.

This works for ad-hoc-signed apps with the hardened runtime enabled.

### Option 2 — System Settings → Privacy & Security → Open Anyway

After the first blocked launch:
1. Open **System Settings** → **Privacy & Security**.
2. Scroll to the bottom and click **Open Anyway** next to the blocked
   "InferMesh.app" entry.
3. Click **Open** in the confirmation dialog.

### Option 3 — Disable Gatekeeper temporarily (admin)

```bash
sudo spctl --master-disable
```

Open the app normally. Re-enable with:
```bash
sudo spctl --master-enable
```

Use this only if you trust the build and want to bypass Gatekeeper entirely.

## For a fully Gatekeeper-free build

To sign with a Developer ID and notarize so Gatekeeper never blocks the app,
you need an Apple Developer account. Once you have one:

1. Run `wails3 setup` to configure your signing identity and keychain profile
2. Build the package: `cd app && wails3 task package APP_NAME=InferMesh`
3. Sign and notarize:
   ```bash
   cd app && wails3 task darwin:sign:notarize
   ```

See `app/build/darwin/Taskfile.yml` for the `sign` and `sign:notarize` tasks,
and `app/build/darwin/entitlements.plist` for the hardened runtime entitlements.

## Files in this repo

| File | Purpose |
|------|---------|
| `app/build/darwin/entitlements.plist` | Hardened runtime entitlements |
| `app/build/darwin/README.md` | Included in the .dmg with user-facing workaround |
| `app/build/darwin/Taskfile.yml` | Wails v3 packaging tasks (codesign, verify, dmg) |
| `Makefile` | `package-app`, `package-app-dmg` targets |

## Verifying the signature locally

```bash
# Verify ad-hoc signature + hardened runtime + entitlements
codesign --verify --deep --strict bin/app/InferMesh.app

# Inspect signature details
codesign -d --verbose=4 bin/app/InferMesh.app

# Check entitlements
codesign -d --entitlements :- bin/app/InferMesh.app
```

Expected output for a valid build:
```
flags=0x10002(adhoc,runtime)   # ad-hoc + hardened runtime
Signature=adhoc
TeamIdentifier=not set
```