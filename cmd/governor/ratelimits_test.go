package main

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sampleRateLimitsPayload is a real statusLine payload captured from Claude Code 2.1.331 on
// 2026-11-24 (docs/hook-surface.md §14.3), with the fields this command does not read trimmed.
// The rate_limits object is verbatim.
const sampleRateLimitsPayload = `{"session_id":"2ed4bf1c-470b-4e08-9b48-4f310663cb6e",` +
	`"cwd":"/tmp/work","model":{"id":"claude-haiku-5-5","display_name":"Haiku 5.5"},` +
	`"version":"2.1.331","exceeds_200k_tokens":false,` +
	`"rate_limits":{"five_hour":{"used_percentage":27,"resets_at":1795507200},` +
	`"seven_day":{"used_percentage":70,"resets_at":1795564800}}}`

// goldenNow is the captured_at the golden file carries: the moment the sample was captured.
var goldenNow = time.UnixMilli(1795496930024)

func ratesOf(t *testing.T, payload string) *rateLimits {
	t.Helper()
	var p statusLinePayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	return parseRateLimits(p)
}

func readExport(t *testing.T, dir string) (rateLimitsExport, []byte) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, rateLimitsFile))
	if err != nil {
		t.Fatalf("read export: %v", err)
	}
	var e rateLimitsExport
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatalf("export is not JSON: %v\n%s", err, raw)
	}
	return e, raw
}

// The format is a contract with another repository (headroom docs/design.md §5.1), so it is
// pinned byte for byte. If this fails because the format changed on purpose, change it in
// headroom's design first, then here.
func TestRateLimitsExportMatchesTheGoldenFile(t *testing.T) {
	dir := isolateState(t)
	r := ratesOf(t, sampleRateLimitsPayload)
	if r == nil {
		t.Fatal("no usage windows parsed from the captured payload")
	}
	writeRateLimits(&bytes.Buffer{}, *r, goldenNow)

	_, got := readExport(t, dir)
	want, err := os.ReadFile(filepath.Join("testdata", "rate_limits.golden.json"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("export does not match testdata/rate_limits.golden.json\n got: %s\nwant: %s", got, want)
	}
	if left := globAll(t, dir, rateLimitsFile+".*"); len(left) > 0 {
		t.Errorf("staging files left behind: %v", left)
	}
}

// Through the real command, against a live daemon: the export is written after the line,
// stamped with this machine's clock.
func TestStatuslineExportsRateLimits(t *testing.T) {
	dir := isolateState(t)
	endpoint, _, _ := statusDaemon(t, statusReply{V: protocolVersion, Budget: 4, InUse: 1, Slots: 1, Thermal: "nominal"})

	before := time.Now()
	if got, want := runStatuslineFor(t, endpoint, sampleRateLimitsPayload, 0), "1/4 slots - thermal ok"; got != want {
		t.Errorf("line = %q, want %q: the figures are exported, never rendered", got, want)
	}
	e, _ := readExport(t, dir)
	if e.V != 1 || e.Writer != "governor" || e.CCVersion != "2.1.331" {
		t.Errorf("export header = v %d writer %q cc_version %q", e.V, e.Writer, e.CCVersion)
	}
	if at := e.CapturedAt; at < float64(before.Unix())-1 || at > float64(time.Now().Unix())+1 {
		t.Errorf("captured_at = %v, want this machine's clock around %v", at, before.Unix())
	}
	if w := e.Windows["five_hour"]; w == nil || w.UsedPct != 27 || w.ResetsAt != 1795507200 {
		t.Errorf("five_hour = %+v", w)
	}
}

// The export comes from Claude Code, not from the daemon, so a daemon that never answers
// costs the line its numbers and the export nothing.
func TestStatuslineExportsRateLimitsWhenTheDaemonNeverAnswers(t *testing.T) {
	dir := isolateState(t)
	endpoint := listenerFunc(t, func(c net.Conn, _ []byte) { select {} })
	line := runStatuslineWithin(t, endpoint, sampleRateLimitsPayload, 0, statuslineBudget)
	if !strings.Contains(line, "daemon not running") {
		t.Errorf("line = %q, want the daemon reported down", line)
	}
	if e, _ := readExport(t, dir); e.Windows["seven_day"] == nil {
		t.Errorf("seven_day missing from the export: %+v", e.Windows)
	}
}

func TestStatuslineWritesNoExportWithoutRateLimits(t *testing.T) {
	for _, tc := range []struct{ name, payload string }{
		{"the field is absent", sampleStatusLinePayload},
		{"the field is null", `{"session_id":"s","rate_limits":null}`},
		{"the field is not an object", `{"session_id":"s","rate_limits":"soon"}`},
		{"the payload is not JSON", "not json at all"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := isolateState(t)
			endpoint, _, _ := statusDaemon(t, statusReply{V: protocolVersion, Budget: 4, InUse: 1, Slots: 1, Thermal: "nominal"})
			runStatuslineFor(t, endpoint, tc.payload, 0)
			if _, err := os.Stat(filepath.Join(dir, rateLimitsFile)); !os.IsNotExist(err) {
				t.Errorf("%s written for a payload without usage windows; err = %v", rateLimitsFile, err)
			}
			if left := globAll(t, dir, rateLimitsFile+"*"); len(left) > 0 {
				t.Errorf("left %v", left)
			}
		})
	}
}

// Windows are independently optional, and an absent one is absent, never 0% used.
func TestRateLimitsMissingWindowsStayMissing(t *testing.T) {
	for _, tc := range []struct {
		name, rateLimits string
		want             map[string]*rateWindow
	}{
		{"seven_day only, as Ultra sends it", `{"seven_day":{"used_percentage":41.5,"resets_at":1795830000}}`,
			map[string]*rateWindow{"seven_day": {41.5, 1795830000}}},
		{"a null window is carried as null", `{"seven_day":{"used_percentage":41,"resets_at":1795830000},"spend_limit":null}`,
			map[string]*rateWindow{"seven_day": {41, 1795830000}, "spend_limit": nil}},
		{"a window missing a field is dropped", `{"five_hour":{"used_percentage":12},"seven_day":{"used_percentage":41,"resets_at":1795830000}}`,
			map[string]*rateWindow{"seven_day": {41, 1795830000}}},
		{"a window of the wrong shape is dropped", `{"five_hour":"high","seven_day":{"used_percentage":"41","resets_at":1}}`,
			map[string]*rateWindow{}},
		{"a window added later is carried through", `{"opus_week":{"used_percentage":3,"resets_at":1795830000}}`,
			map[string]*rateWindow{"opus_week": {3, 1795830000}}},
		{"every window reset", `{}`, map[string]*rateWindow{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := isolateState(t)
			r := ratesOf(t, `{"session_id":"s","version":"2.1.331","rate_limits":`+tc.rateLimits+`}`)
			if r == nil {
				t.Fatal("rate_limits was an object, so there is something to export")
			}
			writeRateLimits(&bytes.Buffer{}, *r, goldenNow)
			e, raw := readExport(t, dir)
			if len(e.Windows) != len(tc.want) {
				t.Fatalf("windows = %s, want %d of them", raw, len(tc.want))
			}
			for name, w := range tc.want {
				got, ok := e.Windows[name]
				if !ok || (got == nil) != (w == nil) || (w != nil && *got != *w) {
					t.Errorf("%s = %+v, want %+v (file: %s)", name, got, w, raw)
				}
			}
			if strings.Contains(string(raw), `"five_hour"`) && tc.want["five_hour"] == nil {
				t.Errorf("five_hour written though the payload did not carry a readable one: %s", raw)
			}
		})
	}
}

// A rate_limits or version of a shape this binary does not expect costs the export only,
// never the session's own slot count.
func TestAnOddRateLimitsShapeDoesNotCostTheSessionID(t *testing.T) {
	isolateState(t)
	endpoint, _, _ := statusDaemon(t, statusReply{
		V: protocolVersion, Budget: 5, InUse: 3, Slots: 3, Thermal: "nominal",
		Sessions: []sessionStat{{Session: sampleSessionID, Confirmed: 2}},
	})
	payload := `{"session_id":"` + sampleSessionID + `","version":5,"rate_limits":[1,2,3]}`
	if got, want := runStatuslineFor(t, endpoint, payload, 0), "3/5 slots (2 yours) - thermal ok"; got != want {
		t.Errorf("line = %q, want %q", got, want)
	}
}

// The 30 s rule: twenty sessions on a 2 s refresh must not rewrite an unchanged file.
func TestRateLimitsWriteRule(t *testing.T) {
	setup := func(t *testing.T, age time.Duration) (string, time.Time) {
		dir := isolateState(t)
		writeRateLimits(&bytes.Buffer{}, *ratesOf(t, sampleRateLimitsPayload), goldenNow)
		stamp := goldenNow.Add(-age).Truncate(time.Second)
		path := filepath.Join(dir, rateLimitsFile)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		return path, stamp
	}
	mtime := func(t *testing.T, path string) time.Time {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return info.ModTime()
	}

	t.Run("unchanged and fresh leaves the file alone", func(t *testing.T) {
		path, stamp := setup(t, 10*time.Second)
		writeRateLimits(&bytes.Buffer{}, *ratesOf(t, sampleRateLimitsPayload), goldenNow)
		if got := mtime(t, path); !got.Equal(stamp) {
			t.Errorf("mtime moved from %v to %v for an unchanged export under 30 s old", stamp, got)
		}
	})
	t.Run("unchanged but 30 s old is rewritten", func(t *testing.T) {
		path, stamp := setup(t, 31*time.Second)
		writeRateLimits(&bytes.Buffer{}, *ratesOf(t, sampleRateLimitsPayload), goldenNow)
		if got := mtime(t, path); got.Equal(stamp) {
			t.Error("an unchanged export 31 s old was not refreshed; captured_at would go stale")
		}
	})
	t.Run("a changed value is rewritten at once", func(t *testing.T) {
		path, _ := setup(t, 2*time.Second)
		changed := strings.Replace(sampleRateLimitsPayload, `"used_percentage":27`, `"used_percentage":28`, 1)
		writeRateLimits(&bytes.Buffer{}, *ratesOf(t, changed), goldenNow)
		e, _ := readExport(t, filepath.Dir(path))
		if w := e.Windows["five_hour"]; w == nil || w.UsedPct != 28 {
			t.Errorf("five_hour = %+v, want 28%% used", w)
		}
	})
	t.Run("a window that disappears is rewritten at once", func(t *testing.T) {
		path, _ := setup(t, 2*time.Second)
		r := ratesOf(t, sampleRateLimitsPayload)
		delete(r.Windows, "five_hour")
		writeRateLimits(&bytes.Buffer{}, *r, goldenNow)
		if e, _ := readExport(t, filepath.Dir(path)); e.Windows["five_hour"] != nil || len(e.Windows) != 1 {
			t.Errorf("windows = %+v, want seven_day alone", e.Windows)
		}
	})
}

func globAll(t *testing.T, dir, pattern string) []string {
	t.Helper()
	found, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	return found
}
