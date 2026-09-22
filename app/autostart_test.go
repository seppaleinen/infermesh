package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// ---- command-runner fake -----------------------------------------------------

type cmdResult struct {
	out string
	err error
}

// fakeCmdRunner records every invocation and returns scripted results keyed
// on the joined "name arg1 arg2..." command line. Unscripted calls succeed
// with empty output. It never touches a real shell or registry.
type fakeCmdRunner struct {
	calls   [][]string // calls[i] = [name, args...]
	results map[string]cmdResult
}

func newFakeCmdRunner() *fakeCmdRunner {
	return &fakeCmdRunner{results: map[string]cmdResult{}}
}

func (f *fakeCmdRunner) Run(name string, args ...string) (string, error) {
	full := append([]string{name}, args...)
	f.calls = append(f.calls, full)
	res, ok := f.results[strings.Join(full, " ")]
	if !ok {
		return "", nil
	}
	return res.out, res.err
}

// script registers the output/error for an exact command line (name + args).
func (f *fakeCmdRunner) script(out string, err error, cmd ...string) {
	f.results[strings.Join(cmd, " ")] = cmdResult{out: out, err: err}
}

// assertLastCmd checks the most recently recorded invocation against want.
func (f *fakeCmdRunner) assertLastCmd(t *testing.T, want ...string) {
	t.Helper()
	if len(f.calls) == 0 {
		t.Fatal("no command recorded")
	}
	got := f.calls[len(f.calls)-1]
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("command mismatch:\n got: %q\nwant: %q", got, want)
	}
}

// assertCmdAt checks the i-th recorded invocation against want.
func (f *fakeCmdRunner) assertCmdAt(t *testing.T, i int, want ...string) {
	t.Helper()
	if i >= len(f.calls) {
		t.Fatalf("no command at index %d (have %d)", i, len(f.calls))
	}
	if !reflect.DeepEqual(f.calls[i], want) {
		t.Fatalf("command %d mismatch:\n got: %q\nwant: %q", i, f.calls[i], want)
	}
}

// ---- autostarter test double -------------------------------------------------

// fakeAutostarter is the scripted autostarter behind AutoStartService tests.
type fakeAutostarter struct {
	enableErr  error
	disableErr error
	isErr      error
	enabled    bool

	enableCalls  int
	disableCalls int
	isCalls      int
}

func (f *fakeAutostarter) Enable() error {
	f.enableCalls++
	return f.enableErr
}

func (f *fakeAutostarter) Disable() error {
	f.disableCalls++
	return f.disableErr
}

func (f *fakeAutostarter) IsEnabled() (bool, error) {
	f.isCalls++
	return f.enabled, f.isErr
}

const testExePath = "/Applications/InferMesh.app/Contents/MacOS/infermesh"

// ---- macOS backend -----------------------------------------------------------

func TestDarwinAutostartEnable(t *testing.T) {
	r := newFakeCmdRunner()
	d := &darwinAutostart{r: r, exePath: testExePath}

	if err := d.Enable(); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	makeScript := fmt.Sprintf(darwinMakeLoginItemScriptFmt, autoStartLoginItemName, testExePath)
	r.assertCmdAt(t, 0, "osascript", "-e", darwinListLoginItemsScript)
	r.assertLastCmd(t, "osascript", "-e", makeScript)
	if len(r.calls) != 2 {
		t.Fatalf("expected list + make = 2 commands, got %d", len(r.calls))
	}
}

func TestDarwinAutostartEnableIdempotent(t *testing.T) {
	r := newFakeCmdRunner()
	r.script(`"InferMesh"`, nil, "osascript", "-e", darwinListLoginItemsScript)
	d := &darwinAutostart{r: r, exePath: testExePath}

	if err := d.Enable(); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if len(r.calls) != 1 {
		t.Fatalf("expected only the list call (no duplicate make), got %d", len(r.calls))
	}
	r.assertLastCmd(t, "osascript", "-e", darwinListLoginItemsScript)
}

func TestDarwinAutostartEnableEmptyPath(t *testing.T) {
	r := newFakeCmdRunner()
	d := &darwinAutostart{r: r, exePath: ""}

	err := d.Enable()
	if err == nil || !strings.Contains(err.Error(), "app executable path is empty") {
		t.Fatalf("Enable with empty path should fail closed, got: %v", err)
	}
	if len(r.calls) != 0 {
		t.Fatalf("no commands should run with an empty path, got %d", len(r.calls))
	}
}

func TestDarwinAutostartEnableError(t *testing.T) {
	r := newFakeCmdRunner()
	makeScript := fmt.Sprintf(darwinMakeLoginItemScriptFmt, autoStartLoginItemName, testExePath)
	r.script("execution error: Not authorized", errors.New("exit status 1"), "osascript", "-e", makeScript)
	d := &darwinAutostart{r: r, exePath: testExePath}

	err := d.Enable()
	if err == nil {
		t.Fatal("Enable should fail when osascript errors")
	}
	if !strings.Contains(err.Error(), "autostart enable") {
		t.Errorf("error should carry autostart enable context, got: %v", err)
	}
	if !strings.Contains(err.Error(), "Not authorized") {
		t.Errorf("error should fold in command output, got: %v", err)
	}
}

func TestDarwinAutostartDisable(t *testing.T) {
	deleteScript := fmt.Sprintf(darwinDeleteLoginItemScriptFmt, autoStartLoginItemName)
	cases := []struct {
		name    string
		out     string
		err     error
		wantErr bool
	}{
		{"removes existing item", "", nil, false},
		{
			"idempotent when item absent (typographic apostrophe, -1728)",
			"execution error: \u201cInferMesh\u201d wasn\u2019t found. (-1728)",
			errors.New("exit status 1"),
			false,
		},
		{
			"idempotent when item absent (straight apostrophe)",
			`execution error: "InferMesh" wasn't found. (-1728)`,
			errors.New("exit status 1"),
			false,
		},
		{
			"other osascript errors surface",
			"execution error: Not authorized (-1743)",
			errors.New("exit status 1"),
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newFakeCmdRunner()
			r.script(tc.out, tc.err, "osascript", "-e", deleteScript)
			d := &darwinAutostart{r: r, exePath: testExePath}

			err := d.Disable()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Disable error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "autostart disable") {
				t.Errorf("error should carry autostart disable context, got: %v", err)
			}
			r.assertLastCmd(t, "osascript", "-e", deleteScript)
		})
	}
}

func TestDarwinAutostartIsEnabled(t *testing.T) {
	cases := []struct {
		name    string
		out     string
		err     error
		want    bool
		wantErr bool
	}{
		{"quoted list with InferMesh", `"Router Helper", "InferMesh"`, nil, true, false},
		{"quoted list without InferMesh", `"Router Helper", "Safari"`, nil, false, false},
		{"bare names", "Safari, InferMesh", nil, true, false},
		{"only InferMesh", "InferMesh", nil, true, false},
		{"similar name not matched", `"SuperInferMesh", "InferMeshHelper"`, nil, false, false},
		{"empty output", "", nil, false, false},
		{"probe error", "", errors.New("exit status 1"), false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newFakeCmdRunner()
			r.script(tc.out, tc.err, "osascript", "-e", darwinListLoginItemsScript)
			d := &darwinAutostart{r: r, exePath: testExePath}

			got, err := d.IsEnabled()
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("IsEnabled = %v, want %v", got, tc.want)
			}
			r.assertLastCmd(t, "osascript", "-e", darwinListLoginItemsScript)
		})
	}
}

// ---- Windows backend ---------------------------------------------------------

func TestWindowsAutostartEnable(t *testing.T) {
	r := newFakeCmdRunner()
	w := &windowsAutostart{r: r, exePath: testExePath}

	if err := w.Enable(); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	r.assertLastCmd(t, "reg.exe", "add",
		`HKCU\Software\Microsoft\Windows\CurrentVersion\Run`,
		"/v", "InferMesh", "/t", "REG_SZ", "/d", testExePath, "/f")
}

func TestWindowsAutostartEnableEmptyPath(t *testing.T) {
	r := newFakeCmdRunner()
	w := &windowsAutostart{r: r, exePath: ""}

	err := w.Enable()
	if err == nil || !strings.Contains(err.Error(), "app executable path is empty") {
		t.Fatalf("Enable with empty path should fail closed, got: %v", err)
	}
	if len(r.calls) != 0 {
		t.Fatalf("no commands should run with an empty path, got %d", len(r.calls))
	}
}

func TestWindowsAutostartEnableError(t *testing.T) {
	r := newFakeCmdRunner()
	add := []string{"reg.exe", "add", `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`,
		"/v", "InferMesh", "/t", "REG_SZ", "/d", testExePath, "/f"}
	r.script("ERROR: Access is denied.", errors.New("exit status 1"), add...)
	w := &windowsAutostart{r: r, exePath: testExePath}

	err := w.Enable()
	if err == nil {
		t.Fatal("Enable should fail when reg.exe errors")
	}
	if !strings.Contains(err.Error(), "autostart enable") {
		t.Errorf("error should carry autostart enable context, got: %v", err)
	}
}

func TestWindowsAutostartDisable(t *testing.T) {
	deleteCmd := []string{"reg.exe", "delete", `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`,
		"/v", "InferMesh", "/f"}
	cases := []struct {
		name    string
		out     string
		err     error
		wantErr bool
	}{
		{"removes existing value", "", nil, false},
		{
			"idempotent when value absent",
			"ERROR: The system was unable to find the specified registry key or value.",
			errors.New("exit status 1"),
			false,
		},
		{
			"failure with empty output still surfaces",
			"",
			errors.New("exit status 1"),
			true,
		},
		{
			"other registry errors surface",
			"ERROR: Access is denied.",
			errors.New("exit status 1"),
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newFakeCmdRunner()
			r.script(tc.out, tc.err, deleteCmd...)
			w := &windowsAutostart{r: r, exePath: testExePath}

			err := w.Disable()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Disable error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "autostart disable") {
				t.Errorf("error should carry autostart disable context, got: %v", err)
			}
			r.assertLastCmd(t, deleteCmd...)
		})
	}
}

func TestWindowsAutostartIsEnabled(t *testing.T) {
	query := []string{"reg.exe", "query", `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`, "/v", "InferMesh"}
	present := "HKEY_CURRENT_USER\\Software\\Microsoft\\Windows\\CurrentVersion\\Run\r\n" +
		"    InferMesh    REG_SZ    C:\\Users\\me\\bin\\infermesh-app.exe\r\n"
	absent := "ERROR: The system was unable to find the specified registry key or value."
	cases := []struct {
		name    string
		out     string
		err     error
		want    bool
		wantErr bool
	}{
		{"value present", present, nil, true, false},
		{"value present with spaced data path", "    InferMesh    REG_SZ    C:\\Program Files\\MyApp\\run.exe", nil, true, false},
		{"query fails because value absent", absent, errors.New("exit status 1"), false, false},
		{"name present but REG_SZ data empty", "    InferMesh    REG_SZ", nil, false, false},
		{"unrelated Run entries only", "    OtherApp    REG_SZ    C:\\other.exe", nil, false, false},
		{
			"name in header but REG_SZ belongs to another value",
			"HKEY_CURRENT_USER\\Software\\Microsoft\\Windows\\CurrentVersion\\Run\\InferMesh\r\n" +
				"    OtherApp    REG_SZ    C:\\other.exe\r\n",
			nil, false, false,
		},
		{"unexpected query failure", "ERROR: Access is denied.", errors.New("exit status 1"), false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newFakeCmdRunner()
			r.script(tc.out, tc.err, query...)
			w := &windowsAutostart{r: r, exePath: testExePath}

			got, err := w.IsEnabled()
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("IsEnabled = %v, want %v", got, tc.want)
			}
			r.assertLastCmd(t, query...)
		})
	}
}

// ---- Linux backend -----------------------------------------------------------

func linuxTestBackend(t *testing.T) (*linuxAutostart, string) {
	t.Helper()
	dir := t.TempDir()
	return newLinuxAutostart(testExePath, dir, nil), dir
}

func TestLinuxAutostartEnable(t *testing.T) {
	l, dir := linuxTestBackend(t)
	desktop := filepath.Join(dir, "autostart", "infermesh.desktop")

	if err := l.Enable(); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	data, err := os.ReadFile(desktop)
	if err != nil {
		t.Fatalf("desktop entry not written: %v", err)
	}
	content := string(data)
	for _, want := range []string{
		"[Desktop Entry]",
		"Type=Application",
		"Name=InferMesh",
		"Exec=" + escapeDesktopExecArg(testExePath),
		"X-GNOME-Autostart-enabled=true",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("desktop entry missing %q, got:\n%s", want, content)
		}
	}
	info, err := os.Stat(desktop)
	if err != nil {
		t.Fatalf("stat desktop entry: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("desktop entry mode = %04o, want 0644", info.Mode().Perm())
	}
	enabled, err := l.IsEnabled()
	if err != nil {
		t.Fatalf("IsEnabled after enable: %v", err)
	}
	if !enabled {
		t.Error("IsEnabled should be true after Enable")
	}
}

func TestLinuxAutostartEnableSpacedPath(t *testing.T) {
	dir := t.TempDir()
	spaced := "/home/John Smith/.local/bin/infermesh-app"
	l := newLinuxAutostart(spaced, dir, nil)

	if err := l.Enable(); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "autostart", "infermesh.desktop"))
	if err != nil {
		t.Fatalf("read desktop entry: %v", err)
	}
	// The Exec line must quote the whole path AND backslash-escape the space,
	// or the autostart daemon splits the argument at whitespace.
	wantLine := `Exec="/home/John\ Smith/.local/bin/infermesh-app"` + "\n"
	if !strings.Contains(string(data), wantLine) {
		t.Errorf("Exec line missing %q, got:\n%s", wantLine, data)
	}
}

func TestLinuxAutostartEnableFailsClosed(t *testing.T) {
	t.Run("empty exe path", func(t *testing.T) {
		dir := t.TempDir()
		l := newLinuxAutostart("", dir, nil)

		err := l.Enable()
		if err == nil || !strings.Contains(err.Error(), "autostart enable") ||
			!strings.Contains(err.Error(), "app executable path is empty") {
			t.Fatalf("Enable with empty path should fail closed, got: %v", err)
		}
		if _, statErr := os.Stat(filepath.Join(dir, "autostart", "infermesh.desktop")); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatal("desktop entry must not be written with an empty exe path")
		}
	})

	t.Run("unresolvable config dir", func(t *testing.T) {
		dir := t.TempDir()
		initErr := errors.New("no home directory")
		l := newLinuxAutostart(testExePath, dir, initErr)

		if err := l.Enable(); err == nil || !strings.Contains(err.Error(), initErr.Error()) {
			t.Errorf("Enable should fail closed on init error, got: %v", err)
		}
		if err := l.Disable(); err == nil || !strings.Contains(err.Error(), initErr.Error()) {
			t.Errorf("Disable should fail closed on init error, got: %v", err)
		}
		if _, err := l.IsEnabled(); err == nil || !strings.Contains(err.Error(), initErr.Error()) {
			t.Errorf("IsEnabled should fail closed on init error, got: %v", err)
		}
	})
}

func TestLinuxAutostartDisable(t *testing.T) {
	t.Run("removes existing entry", func(t *testing.T) {
		l, dir := linuxTestBackend(t)
		desktop := filepath.Join(dir, "autostart", "infermesh.desktop")
		if err := l.Enable(); err != nil {
			t.Fatalf("Enable: %v", err)
		}
		if err := l.Disable(); err != nil {
			t.Fatalf("Disable: %v", err)
		}
		if _, err := os.Stat(desktop); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("desktop entry should be gone, stat err = %v", err)
		}
	})

	t.Run("missing file is success", func(t *testing.T) {
		l, _ := linuxTestBackend(t)
		if err := l.Disable(); err != nil {
			t.Fatalf("Disable on missing file should be nil, got: %v", err)
		}
	})
}

func TestLinuxAutostartIsEnabled(t *testing.T) {
	t.Run("false when missing", func(t *testing.T) {
		l, _ := linuxTestBackend(t)
		got, err := l.IsEnabled()
		if err != nil {
			t.Fatalf("IsEnabled: %v", err)
		}
		if got {
			t.Error("expected false for missing entry")
		}
	})

	t.Run("true when regular file", func(t *testing.T) {
		l, _ := linuxTestBackend(t)
		if err := l.Enable(); err != nil {
			t.Fatalf("Enable: %v", err)
		}
		got, err := l.IsEnabled()
		if err != nil {
			t.Fatalf("IsEnabled: %v", err)
		}
		if !got {
			t.Error("expected true for existing entry")
		}
	})

	t.Run("false when path is a directory", func(t *testing.T) {
		l, _ := linuxTestBackend(t)
		if err := os.MkdirAll(l.desktopPath(), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		got, err := l.IsEnabled()
		if err != nil {
			t.Fatalf("IsEnabled: %v", err)
		}
		if got {
			t.Error("expected false when the path is a directory")
		}
	})
}

func TestEscapeDesktopExecArg(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"unspaced path is always quoted", "/usr/bin/infermesh-app", `"/usr/bin/infermesh-app"`},
		{"spaced path escapes the space", "/home/John Smith/.local/bin/infermesh-app", `"/home/John\ Smith/.local/bin/infermesh-app"`},
		{"double quote escaped", `/opt/my"app/infermesh`, `"/opt/my\"app/infermesh"`},
		{"backslash escaped", `C:\apps\infermesh\app.exe`, `"C:\\apps\\infermesh\\app.exe"`},
		{"single quote escaped", "/opt/it's/app/infermesh", `"/opt/it\'s/app/infermesh"`},
		{
			"full reserved set",
			"/tmp/a$b;c(d)e?f#g~h|i&j<k>l" + "`" + "'m",
			`"/tmp/a\$b\;c\(d\)e\?f\#g\~h\|i\&j\<k\>l\` + "`" + `\'m"`,
		},
		{"empty argument", "", `""`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := escapeDesktopExecArg(tc.in); got != tc.want {
				t.Errorf("escapeDesktopExecArg(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestDefaultLinuxConfigDir(t *testing.T) {
	t.Run("honors XDG_CONFIG_HOME", func(t *testing.T) {
		want := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", want)
		got, err := defaultLinuxConfigDir()
		if err != nil {
			t.Fatalf("defaultLinuxConfigDir: %v", err)
		}
		if got != want {
			t.Errorf("defaultLinuxConfigDir = %q, want %q", got, want)
		}
	})

	t.Run("falls back to ~/.config", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", "")
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skipf("no home dir: %v", err)
		}
		want := filepath.Join(home, ".config")
		got, err := defaultLinuxConfigDir()
		if err != nil {
			t.Fatalf("defaultLinuxConfigDir: %v", err)
		}
		if got != want {
			t.Errorf("defaultLinuxConfigDir = %q, want %q", got, want)
		}
	})
}

// ---- runtime dispatch --------------------------------------------------------

func TestNewPlatformAutostartDispatch(t *testing.T) {
	r := newFakeCmdRunner()
	got := newPlatformAutostart(r, testExePath)
	if got == nil {
		t.Fatal("newPlatformAutostart returned nil")
	}
	switch runtime.GOOS {
	case "darwin":
		if _, ok := got.(*darwinAutostart); !ok {
			t.Fatalf("GOOS=darwin: got %T, want *darwinAutostart", got)
		}
	case "windows":
		if _, ok := got.(*windowsAutostart); !ok {
			t.Fatalf("GOOS=windows: got %T, want *windowsAutostart", got)
		}
	default:
		if _, ok := got.(*linuxAutostart); !ok {
			t.Fatalf("GOOS=%s: got %T, want *linuxAutostart", runtime.GOOS, got)
		}
	}
}

// ---- service layer -----------------------------------------------------------

func TestAutoStartServiceSetAutoStart(t *testing.T) {
	cases := []struct {
		name       string
		backend    autostarter // nil = unwired host
		enabled    bool
		enableErr  error
		disableErr error
		wantWarn   string // substring; empty means expect empty warning
	}{
		{"enable success", &fakeAutostarter{}, true, nil, nil, ""},
		{
			"enable OS failure becomes warning",
			&fakeAutostarter{enableErr: errors.New("osascript: not authorized")},
			true, nil, nil,
			"Auto-start could not be enabled: osascript: not authorized",
		},
		{"disable success", &fakeAutostarter{}, false, nil, nil, ""},
		{
			"disable OS failure becomes warning",
			&fakeAutostarter{disableErr: errors.New("registry denied")},
			false, nil, nil,
			"Auto-start could not be disabled: registry denied",
		},
		{"nil backend warns instead of panicking", nil, true, nil, nil,
			"auto-start is not configured on this host"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := NewAutoStartService(tc.backend)
			warn, err := svc.SetAutoStart(tc.enabled)
			if err != nil {
				t.Fatalf("SetAutoStart must not fail on OS/backend issues, got: %v", err)
			}
			if tc.wantWarn == "" {
				if warn != "" {
					t.Errorf("expected empty warning, got %q", warn)
				}
			} else if !strings.Contains(warn, tc.wantWarn) {
				t.Errorf("warning = %q, want it to contain %q", warn, tc.wantWarn)
			}
			if fa, ok := tc.backend.(*fakeAutostarter); ok {
				if tc.enabled && fa.enableCalls != 1 {
					t.Errorf("Enable calls = %d, want 1", fa.enableCalls)
				}
				if !tc.enabled && fa.disableCalls != 1 {
					t.Errorf("Disable calls = %d, want 1", fa.disableCalls)
				}
			}
		})
	}
}

func TestAutoStartServiceIsAutoStartEnabled(t *testing.T) {
	cases := []struct {
		name           string
		backend        autostarter
		enabled        bool
		isErr          error
		want           bool
		wantErrContain string // empty = expect nil error
	}{
		{"reports true", &fakeAutostarter{enabled: true}, true, nil, true, ""},
		{"reports false", &fakeAutostarter{enabled: false}, false, nil, false, ""},
		{"probe error surfaces", &fakeAutostarter{isErr: errors.New("probe failed")}, false,
			errors.New("probe failed"), false, "probe failed"},
		{"nil backend reports not wired", nil, false, nil, false, "not wired"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := NewAutoStartService(tc.backend)
			got, err := svc.IsAutoStartEnabled()
			if tc.wantErrContain == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("expected an error")
				}
				if !strings.Contains(err.Error(), tc.wantErrContain) {
					t.Errorf("error = %v, want it to contain %q", err, tc.wantErrContain)
				}
			}
			if got != tc.want {
				t.Errorf("IsAutoStartEnabled = %v, want %v", got, tc.want)
			}
		})
	}
}

// ---- config: AutoStartOnLogin tri-state --------------------------------------

func TestMergeDefaultsAutoStartOnLogin(t *testing.T) {
	t.Run("nil merges to default false", func(t *testing.T) {
		got := mergeDefaults(Settings{})
		if got.AutoStartOnLogin == nil {
			t.Fatal("AutoStartOnLogin is nil after mergeDefaults")
		}
		if *got.AutoStartOnLogin {
			t.Error("default should be false (opt-in)")
		}
	})
	t.Run("explicit true preserved", func(t *testing.T) {
		got := mergeDefaults(Settings{AutoStartOnLogin: boolPtr(true)})
		if got.AutoStartOnLogin == nil || !*got.AutoStartOnLogin {
			t.Errorf("explicit true was clobbered: %v", got.AutoStartOnLogin)
		}
	})
	t.Run("explicit false preserved", func(t *testing.T) {
		got := mergeDefaults(Settings{AutoStartOnLogin: boolPtr(false)})
		if got.AutoStartOnLogin == nil {
			t.Fatal("explicit false became nil")
		}
		if *got.AutoStartOnLogin {
			t.Error("explicit false was flipped")
		}
	})
}

func TestAutoStartOnLoginYAMLRoundTrip(t *testing.T) {
	for _, val := range []bool{true, false} {
		name := "false"
		if val {
			name = "true"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.yml")
			s := DefaultSettings()
			s.AutoStartOnLogin = boolPtr(val)
			if err := SaveTo(s, path); err != nil {
				t.Fatalf("SaveTo: %v", err)
			}
			loaded, err := LoadFrom(path)
			if err != nil {
				t.Fatalf("LoadFrom: %v", err)
			}
			merged := mergeDefaults(loaded)
			if merged.AutoStartOnLogin == nil {
				t.Fatal("AutoStartOnLogin nil after round-trip")
			}
			if *merged.AutoStartOnLogin != val {
				t.Errorf("round-trip got %v, want %v", *merged.AutoStartOnLogin, val)
			}
		})
	}

	t.Run("old file without key merges to false", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "settings.yml")
		if err := os.WriteFile(path, []byte("router_addr: \":9000\"\n"), 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		loaded, err := LoadFrom(path)
		if err != nil {
			t.Fatalf("LoadFrom: %v", err)
		}
		if loaded.AutoStartOnLogin != nil {
			t.Fatalf("expected nil before merge, got %v", *loaded.AutoStartOnLogin)
		}
		merged := mergeDefaults(loaded)
		if merged.AutoStartOnLogin == nil || *merged.AutoStartOnLogin {
			t.Errorf("expected merged default false, got %v", merged.AutoStartOnLogin)
		}
	})
}
