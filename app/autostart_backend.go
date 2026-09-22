package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Auto-start registration identifiers. The login item name, registry value
// name and desktop-entry basename all derive from the app name "InferMesh".
const (
	autoStartLoginItemName = "InferMesh"
	autoStartDesktopFile   = "infermesh.desktop"

	// windowsRunKey is the per-user Run registry key where Windows stores
	// login commands (HKCU = current user only; no elevation required).
	windowsRunKey = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`
)

// desktopEntry is the XDG autostart entry body written on Linux; %s is the
// Exec line with the absolute path of the running app binary, quoted and
// escaped by escapeDesktopExecArg (always quoted — see that helper).
const desktopEntry = `[Desktop Entry]
Type=Application
Name=` + autoStartLoginItemName + `
Exec=%s
X-GNOME-Autostart-enabled=true
`

// darwin osascript -e scripts. Each is one argv element (exec, no shell), so
// only AppleScript string syntax applies — no shell quoting.
const (
	darwinListLoginItemsScript     = `tell application "System Events" to get the name of every login item`
	darwinMakeLoginItemScriptFmt   = `tell application "System Events" to make login item "%s" with properties {path: "%s", hidden: false}`
	darwinDeleteLoginItemScriptFmt = `tell application "System Events" to delete login item "%s"`
)

// newPlatformAutostart returns the registration backend for the current OS.
//
// Dispatch is runtime-based on purpose: no _darwin.go/_windows.go/_linux.go
// filename suffixes and no //go:build tags, so every implementation compiles
// on every development host and stays unit-testable anywhere. r is the
// command seam used by the osascript/reg.exe backends; the Linux backend is
// pure file I/O and ignores it.
func newPlatformAutostart(r cmdRunner, exePath string) autostarter {
	switch runtime.GOOS {
	case "darwin":
		return &darwinAutostart{r: r, exePath: exePath}
	case "windows":
		return &windowsAutostart{r: r, exePath: exePath}
	default:
		dir, err := defaultLinuxConfigDir()
		return newLinuxAutostart(exePath, dir, err)
	}
}

// runErr wraps a failed cmdRunner invocation with operation context, folding
// in the command output — that is where osascript/reg.exe put the real reason
// (the exec error itself is usually just "exit status 1").
func runErr(op string, err error, output string) error {
	if msg := strings.TrimSpace(output); msg != "" {
		return fmt.Errorf("autostart %s: %w: %s", op, err, msg)
	}
	return fmt.Errorf("autostart %s: %w", op, err)
}

// requireExePath guards registration against an unresolved executable path
// (fail closed): an empty path must never be written as a login command or
// Exec= value, and surfaces as a warning through SetAutoStart instead.
func requireExePath(p string) error {
	if strings.TrimSpace(p) == "" {
		return errors.New("app executable path is empty")
	}
	return nil
}

// ---- macOS: osascript login items -------------------------------------------

// darwinAutostart registers the login item through System Events via
// osascript.
type darwinAutostart struct {
	r       cmdRunner
	exePath string
}

// Enable is idempotent: it lists existing login items first and only issues
// the make when "InferMesh" is not already present (System Events errors on
// a duplicate make).
func (d *darwinAutostart) Enable() error {
	if err := requireExePath(d.exePath); err != nil {
		return fmt.Errorf("autostart enable: %w", err)
	}
	out, err := d.r.Run("osascript", "-e", darwinListLoginItemsScript)
	if err != nil {
		return runErr("enable", err, out)
	}
	if loginItemPresent(out, autoStartLoginItemName) {
		return nil // already registered
	}
	script := fmt.Sprintf(darwinMakeLoginItemScriptFmt, autoStartLoginItemName, d.exePath)
	out, err = d.r.Run("osascript", "-e", script)
	if err != nil {
		return runErr("enable", err, out)
	}
	return nil
}

// Disable is idempotent: deleting an item that is already gone makes
// osascript fail with AppleScript error -1728 (errAENoSuchObject); that is
// treated as success, mirroring regValueAbsent / os.ErrNotExist handling.
func (d *darwinAutostart) Disable() error {
	script := fmt.Sprintf(darwinDeleteLoginItemScriptFmt, autoStartLoginItemName)
	out, err := d.r.Run("osascript", "-e", script)
	if err != nil {
		if darwinItemAbsent(out, err) {
			return nil
		}
		return runErr("disable", err, out)
	}
	return nil
}

// IsEnabled lists every login item and looks for ours. The output is a
// comma-separated list; items containing spaces come back quoted.
func (d *darwinAutostart) IsEnabled() (bool, error) {
	out, err := d.r.Run("osascript", "-e", darwinListLoginItemsScript)
	if err != nil {
		return false, runErr("query", err, out)
	}
	return loginItemPresent(out, autoStartLoginItemName), nil
}

// loginItemPresent parses the comma-separated login-item list, trimming
// spaces and surrounding quotes from each item before comparison.
func loginItemPresent(list, name string) bool {
	for _, item := range strings.Split(list, ",") {
		item = strings.TrimSpace(item)
		item = strings.Trim(item, `"'`)
		if item == name {
			return true
		}
	}
	return false
}

// darwinItemAbsent reports whether an osascript failure is the "no such
// login item" case. Real osascript emits a typographic apostrophe with error
// -1728, so both quote forms and the numeric code are matched.
func darwinItemAbsent(output string, err error) bool {
	low := normalizeAppleScriptText(strings.ToLower(output + "\n" + err.Error()))
	return strings.Contains(low, "wasn't found") ||
		strings.Contains(low, "was not found") ||
		strings.Contains(low, "cannot find") ||
		strings.Contains(low, "not found") ||
		strings.Contains(low, "-1728")
}

// normalizeAppleScriptText flattens AppleScript's typographic apostrophes
// (U+2019/U+2018) to ASCII so error matching is locale-independent.
func normalizeAppleScriptText(s string) string {
	return strings.NewReplacer("\u2019", "'", "\u2018", "'").Replace(s)
}

// ---- Windows: HKCU Run key via reg.exe --------------------------------------

// windowsAutostart registers the login command under the per-user Run key
// using reg.exe through the cmdRunner seam — stdlib exec only, no
// golang.org/x/sys registry bindings.
type windowsAutostart struct {
	r       cmdRunner
	exePath string
}

func (w *windowsAutostart) Enable() error {
	if err := requireExePath(w.exePath); err != nil {
		return fmt.Errorf("autostart enable: %w", err)
	}
	out, err := w.r.Run("reg.exe", "add", windowsRunKey,
		"/v", autoStartLoginItemName, "/t", "REG_SZ", "/d", w.exePath, "/f")
	if err != nil {
		return runErr("enable", err, out)
	}
	return nil
}

// Disable removes the Run value. It is idempotent: reg.exe exits non-zero
// when the value is already absent, which we treat as success.
func (w *windowsAutostart) Disable() error {
	out, err := w.r.Run("reg.exe", "delete", windowsRunKey,
		"/v", autoStartLoginItemName, "/f")
	if err == nil {
		return nil
	}
	if regValueAbsent(out) {
		return nil // already unregistered
	}
	return runErr("disable", err, out)
}

// IsEnabled queries the Run value. Enabled requires a successful query whose
// REG_SZ record is for OUR value name and carries non-empty data.
func (w *windowsAutostart) IsEnabled() (bool, error) {
	out, err := w.r.Run("reg.exe", "query", windowsRunKey, "/v", autoStartLoginItemName)
	if err != nil {
		if regValueAbsent(out) {
			return false, nil // missing value is the normal "disabled" state
		}
		return false, runErr("query", err, out)
	}
	data, ok := regSzDataForValue(out, autoStartLoginItemName)
	return ok && strings.TrimSpace(data) != "", nil
}

// regValueAbsent reports whether reg.exe output indicates the value/key was
// not found — the normal result of deleting or querying an absent value.
func regValueAbsent(output string) bool {
	low := strings.ToLower(output)
	return strings.Contains(low, "unable to find") ||
		strings.Contains(low, "cannot find") ||
		strings.Contains(low, "not found")
}

// regSzDataForValue parses a `reg.exe query` result and returns the data of
// the REG_SZ record whose value name exactly matches valueName. A record is
// a whitespace-separated "<name> <type> <data...>" line; the name must match
// exactly (not merely appear anywhere in the output) so lookalike values are
// not misread as ours.
func regSzDataForValue(output, valueName string) (string, bool) {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[1] != "REG_SZ" {
			continue
		}
		if strings.Trim(fields[0], `"`) != valueName {
			continue
		}
		return strings.Join(fields[2:], " "), true
	}
	return "", false
}

// ---- Linux: XDG autostart .desktop file -------------------------------------

// linuxAutostart writes an XDG autostart entry. Pure file I/O — no commands
// are ever executed on Linux, so the cmdRunner seam is not needed here.
// initErr, when set (unresolvable config dir), makes every operation fail
// closed instead of writing into an unexpected location.
type linuxAutostart struct {
	exePath   string
	configDir string // base config dir; overridable so tests can use t.TempDir()
	initErr   error  // non-nil when configDir could not be resolved
}

// newLinuxAutostart builds the file-based backend rooted at configDir.
// newPlatformAutostart passes defaultLinuxConfigDir() and any resolution
// error; tests pass a temp dir and nil.
func newLinuxAutostart(exePath, configDir string, initErr error) *linuxAutostart {
	return &linuxAutostart{exePath: exePath, configDir: configDir, initErr: initErr}
}

// defaultLinuxConfigDir resolves the XDG config base directory:
// $XDG_CONFIG_HOME when set, otherwise ~/.config. A failed home-directory
// lookup is an error — silently falling back to a relative ".config" would
// write the autostart entry into the process's CWD.
func defaultLinuxConfigDir() (string, error) {
	if x := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); x != "" {
		return x, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".config"), nil
}

// desktopPath is <configDir>/autostart/infermesh.desktop.
func (l *linuxAutostart) desktopPath() string {
	return filepath.Join(l.configDir, "autostart", autoStartDesktopFile)
}

// Enable atomically writes the .desktop entry (temp file + rename, like
// SaveTo) so a crash never leaves a truncated file behind.
func (l *linuxAutostart) Enable() error {
	if l.initErr != nil {
		return fmt.Errorf("autostart enable: %w", l.initErr)
	}
	if err := requireExePath(l.exePath); err != nil {
		return fmt.Errorf("autostart enable: %w", err)
	}
	path := l.desktopPath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("autostart enable: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "autostart-*.desktop")
	if err != nil {
		return fmt.Errorf("autostart enable: %w", err)
	}
	tmpName := tmp.Name()
	// Clean up the temp file on every early-return path; renamed-away paths
	// clear tmpName so the deferred remove becomes a no-op.
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := fmt.Fprintf(tmp, desktopEntry, escapeDesktopExecArg(l.exePath)); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("autostart enable: %w", err)
	}
	// CreateTemp creates 0600 files; the entry must be world-readable for
	// the session autostart daemon.
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("autostart enable: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("autostart enable: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("autostart enable: %w", err)
	}
	tmpName = ""
	return nil
}

// Disable removes the entry; an already-missing file is success.
func (l *linuxAutostart) Disable() error {
	if l.initErr != nil {
		return fmt.Errorf("autostart disable: %w", l.initErr)
	}
	if err := os.Remove(l.desktopPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("autostart disable: %w", err)
	}
	return nil
}

// IsEnabled reports whether the entry exists and is a regular file.
func (l *linuxAutostart) IsEnabled() (bool, error) {
	if l.initErr != nil {
		return false, fmt.Errorf("autostart query: %w", l.initErr)
	}
	info, err := os.Stat(l.desktopPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("autostart query: %w", err)
	}
	return !info.IsDir(), nil
}

// desktopExecReserved is the freedesktop Desktop Entry Spec's reserved
// character set for Exec arguments: space, tab, newline, ", ', \, >, <, ~,
// |, &, ;, $, *, ?, #, (, ) and backtick. Raw string (backquotes) holds
// everything except the backtick itself, which is appended.
const desktopExecReserved = ` "'\><~|&;$*?#()` + "`"

// escapeDesktopExecArg renders a single Exec argument per the freedesktop
// spec: the whole argument is wrapped in double quotes (so it survives word
// splitting even when empty-ish or leading/trailing space) and every
// reserved character inside is backslash-escaped. This is valid inside or
// outside quotes and is understood by GLib's desktop-entry Exec tokenizer,
// so both "always quoted" and "escaped reserved chars" hold for all inputs.
func escapeDesktopExecArg(arg string) string {
	var b strings.Builder
	b.Grow(len(arg) + 2)
	b.WriteByte('"')
	for _, r := range arg {
		if strings.ContainsRune(desktopExecReserved, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}
