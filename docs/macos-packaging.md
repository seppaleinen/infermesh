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

## Troubleshooting: app won't open / launch error -600

Opening the app can fail in two related ways:

1. A shell prints:

   ```
   _LSOpenURLsWithCompletionHandler() failed with error -600
   ```

2. Or — more common — double-clicking `InferMesh.app` in Finder just bounces the
   Dock icon and no window ever appears, with **no** error message.

Error `-600` is `procNotFound`: Launch Services could not find or start a valid
process for the bundle. **The bundle is usually fine** — a freshly built
`bin/app/InferMesh.app` verifies cleanly (`codesign --verify --deep --strict`,
`flags=adhoc,runtime`, arm64, no quarantine). Work through the causes below in
order.

### 1. Launching from a non-GUI (`Background`) session — most common

macOS suspends GUI apps launched through Launch Services from a process in a
`Background` launchd domain (an SSH session, CI, an agent harness, or some
terminal integrations). The process starts but is immediately stopped
(`SIGSTOP`, `ps` state `T`) and never creates its window — the Dock icon
bounces, then nothing. This affects **every** GUI app, not just InferMesh.

Diagnose:

```bash
launchctl managername              # "Background" => this shell cannot present GUI apps
ps -o pid,stat,command -p <pid>    # T = suspended
```

Confirm it is the environment and not InferMesh, using a built-in app:

```bash
open -n -a Calculator && sleep 1 && pgrep -x Calculator | xargs -I{} ps -o stat= -p {}
```

If Calculator also ends up in state `T`, the launching session is the cause.

> **`open -n <path>` is not a fix.** It exits 0 even when it leaves the app
> suspended, and that suspended process is exactly what causes problem 2.

### 2. A suspended instance holds the single-instance lock — why a later Finder double-click also fails

The app takes an exclusive `flock` at
`$TMPDIR/infermesh-desktop-<uid>.lock` for its whole lifetime
(`app/instance_lock.go`). A suspended instance left behind by an earlier `open`
keeps holding that lock, so the next launch hits `ErrAlreadyRunning`, logs to
stderr and exits with status 0 (`app/main.go:71-75`) — bouncing Dock icon, no
window, no error.

**Fix:** clear the zombie instance(s), then launch from Finder:

```bash
pkill -f 'InferMesh.app/Contents/MacOS/InferMesh'
lsof "$TMPDIR/infermesh-desktop-$(id -u).lock"    # should print nothing afterwards
```

Then open `InferMesh.app` from **Finder**. If a terminal is required, use one in
the GUI login session (Terminal.app on the desktop) — never SSH / CI / an agent
background shell.

When the whole GUI session is wedged, **reboot**: it clears the suspended
instances (releasing the lock) and resets Launch Services state. A reboot is
what resolved the reported incident.

### 3. Corrupted bundle

A signature that fails verification makes Launch Services refuse the bundle with
`-600`. A common case is a copy whose bundle root contains a nested
`InferMesh.app`; `codesign` reports `unsealed contents present in the bundle
root`.

Diagnose:

```bash
codesign --verify --deep --strict <path>/InferMesh.app
spctl --assess --type execute <path>/InferMesh.app
```

**Fix:** replace the copy with a freshly built, valid `.app`. A nested-bundle
copy cannot be repaired by re-copying itself — copy the **good** bundle.

### 4. Duplicate or stale Launch Services registrations

Several registrations of the same bundle id `io.infermesh.desktop` (ejected DMG
volumes, `~/Downloads`, `bin/app`) can make `open -b io.infermesh.desktop`
resolve to a stale path and return `-600`.

Inspect the registrations:

```bash
LSREG=/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister
$LSREG -dump | grep -B6 "identifier: *io.infermesh.desktop"
```

**Fix:** unregister the stale paths, then re-register the good one:

```bash
$LSREG -u <stale-path>/InferMesh.app
$LSREG -f <good-path>/InferMesh.app
```

As a heavier reset (affects **all** apps):

```bash
$LSREG -kill -r -domain local -domain system -domain user
```
