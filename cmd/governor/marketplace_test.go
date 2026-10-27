package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPluginPathPointsAtTheInstalledTree is the marketplace route's whole contract: before
// install there is nothing to point at and it says so by exiting non-zero with an empty
// stdout, and after install it prints exactly the tree install wrote.
//
// The empty stdout on failure matters as much as the exit code. Claude Code reads one path off
// stdout; a diagnostic printed there would be taken for a directory.
func TestPluginPathPointsAtTheInstalledTree(t *testing.T) {
	p := stagedHome(t)

	code, stdout, stderr := governor(t, "plugin-path")
	if code == 0 || stdout != "" {
		t.Fatalf("before install: exit %d, stdout %q; want non-zero and nothing on stdout", code, stdout)
	}
	if !strings.Contains(stderr, "governor install") {
		t.Errorf("before install, stderr does not name the fix: %q", stderr)
	}

	installOK(t)
	code, stdout, stderr = governor(t, "plugin-path")
	if code != 0 {
		t.Fatalf("after install: exit %d, stderr %q", code, stderr)
	}
	if stdout != p.plugin+"\n" {
		t.Errorf("after install: stdout %q, want %q", stdout, p.plugin+"\n")
	}
}

// TestTheMarketplaceManifestPointsAtThisTree holds .claude-plugin/marketplace.json to the code
// that answers it. Each assertion is a way the marketplace route breaks silently: a plugin name
// that is not the manifest name loads a second plugin beside the skills-dir one instead of
// taking precedence over it; a copy source carries no binary; a command this binary does not
// have makes every session start fail to load the plugin.
func TestTheMarketplaceManifestPointsAtThisTree(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", ".claude-plugin", "marketplace.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Name    string `json:"name"`
		Plugins []struct {
			Name   string `json:"name"`
			Source struct {
				Source  string `json:"source"`
				Command string `json:"command"`
				Mode    string `json:"mode"`
			} `json:"source"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.Name != marketplaceName {
		t.Errorf("marketplace is named %q; the data directory uninstall --purge removes assumes %q", m.Name, marketplaceName)
	}
	if len(m.Plugins) != 1 {
		t.Fatalf("want exactly one plugin entry, have %d", len(m.Plugins))
	}
	e := m.Plugins[0]
	if e.Name != pluginName {
		t.Errorf("entry is named %q, the manifest install writes is %q", e.Name, pluginName)
	}
	if strings.HasPrefix(e.Name, "claude-") {
		t.Errorf("plugin name %q carries the prefix Claude Code reserves for Anthropic's own plugins", e.Name)
	}
	if e.Source.Source != "command" || e.Source.Mode != "link" || e.Source.Command != pluginPathCommand {
		t.Errorf("source is %+v; want a command source running %q in link mode", e.Source, pluginPathCommand)
	}
	sub := strings.TrimPrefix(pluginPathCommand, installedBinaryName+" ")
	for _, c := range commands {
		if c.name == sub {
			return
		}
	}
	t.Errorf("the entry runs %q, and %q is not a subcommand of this binary", pluginPathCommand, sub)
}

// stageLegacyTree writes a plugin tree under the old name the way a pre-rename install did.
func stageLegacyTree(t *testing.T, home, manifestName string, withBinary bool) string {
	t.Helper()
	dir := legacyPluginTree(home)
	writeFileT(t, filepath.Join(dir, ".claude-plugin", "plugin.json"), `{"name": "`+manifestName+`"}`)
	writeFileT(t, filepath.Join(dir, "hooks", "hooks.json"), `{"hooks": {}}`)
	if withBinary {
		bin := filepath.Join(dir, "bin", installedBinaryName)
		writeFileT(t, bin, "#!/bin/sh\n")
		if err := os.Chmod(bin, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestInstallRemovesThePluginTreeFromBeforeTheRename is the migration. Left in place, the old
// tree loads beside the new one — the names differ, so no precedence rule picks one — and every
// hook runs twice.
func TestInstallRemovesThePluginTreeFromBeforeTheRename(t *testing.T) {
	p := stagedHome(t)
	legacy := stageLegacyTree(t, p.home, legacyPluginName, true)

	if out := installOK(t, "--dry-run"); !strings.Contains(out, short(legacy)) {
		t.Errorf("--dry-run does not say it would remove %s:\n%s", legacy, out)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("--dry-run removed the old tree: %v", err)
	}

	out := installOK(t)
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("install left the pre-rename tree at %s", legacy)
	}
	if !strings.Contains(out, short(legacy)) {
		t.Errorf("install removed %s without reporting it:\n%s", legacy, out)
	}
}

// TestInstallLeavesASameNamedTreeItDidNotWrite: the old name is a directory anyone could have,
// and install deletes only what it can show it wrote.
func TestInstallLeavesASameNamedTreeItDidNotWrite(t *testing.T) {
	for _, tc := range []struct {
		name, manifest string
		binary         bool
	}{
		{"the old name but no governor binary", legacyPluginName, false},
		{"a governor binary but another plugin's manifest", "someone-elses-plugin", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := stagedHome(t)
			dir := stageLegacyTree(t, p.home, tc.manifest, tc.binary)
			installOK(t)
			if _, err := os.Stat(dir); err != nil {
				t.Errorf("install removed %s, which it did not write: %v", dir, err)
			}
		})
	}
}

// skillsRoot stages the tree install writes, correctly wired, and returns it.
func (w *world) skillsRoot() string {
	w.t.Helper()
	root := filepath.Join(w.home, ".claude", "skills", pluginName)
	copyOfSelf(w.t, mkdir(w.t, filepath.Join(root, "bin")), installedBinaryName)
	w.writeHooks(filepath.Join(root, "hooks", "hooks.json"), "${CLAUDE_PLUGIN_ROOT}/bin/governor", goodHooks())
	return root
}

// TestDoctorFailsWhileTheGateIsRegisteredTwice: every file correct on its own, and together a
// budget that fills at half its size.
func TestDoctorFailsWhileTheGateIsRegisteredTwice(t *testing.T) {
	w := newWorld(t)
	w.skillsRoot()
	legacy := legacyPluginTree(w.home)
	copyOfSelf(t, mkdir(t, filepath.Join(legacy, "bin")), installedBinaryName)
	w.writeHooks(filepath.Join(legacy, "hooks", "hooks.json"), "${CLAUDE_PLUGIN_ROOT}/bin/governor", goodHooks())

	_, found, out := doctor(t)
	if got := want(t, found, "hooks", "FAIL", out); !strings.Contains(got.detail, "registered in 2 places") {
		t.Errorf("the hooks finding does not say the gate is registered twice: %q", got.detail)
	}
}

// writeInstalledPlugins stages Claude Code's record of marketplace installs.
func (w *world) writeInstalledPlugins(entries map[string]string) {
	w.t.Helper()
	plugins := map[string]any{}
	for id, producer := range entries {
		e := map[string]any{"scope": "user", "installPath": "/somewhere"}
		if producer != "" {
			e["sourceProducerPath"] = producer
		}
		plugins[id] = []any{e}
	}
	body, err := json.Marshal(map[string]any{"version": 2, "plugins": plugins})
	if err != nil {
		w.t.Fatal(err)
	}
	writeFileT(w.t, installedPluginsPath(w.home), string(body))
}

// TestDoctorTellsOurMarketplaceEntryFromAForeignCopy: our own entry is a link to the tree doctor
// just checked and is fine; any other copy of the plugin takes the hooks away from it.
func TestDoctorTellsOurMarketplaceEntryFromAForeignCopy(t *testing.T) {
	t.Run("our entry, linked to this tree", func(t *testing.T) {
		w := newWorld(t)
		root := w.skillsRoot()
		w.writeInstalledPlugins(map[string]string{pluginName + "@" + marketplaceName: root})
		_, found, out := doctor(t)
		want(t, found, "hooks", "ok", out)
	})
	t.Run("a copy from somewhere else", func(t *testing.T) {
		w := newWorld(t)
		w.skillsRoot()
		foreign := pluginName + "@elsewhere"
		w.writeInstalledPlugins(map[string]string{foreign: ""})
		_, found, out := doctor(t)
		got := want(t, found, "hooks", "FAIL", out)
		if !strings.Contains(got.detail, foreign) || !strings.Contains(got.fix, "claude plugin uninstall "+foreign) {
			t.Errorf("the finding does not name %s and how to remove it: %+v", foreign, got)
		}
	})
}

// TestUninstallPurgeRemovesEveryDataDirectoryName: ${CLAUDE_PLUGIN_DATA} is named after the
// route the plugin loaded by and after the plugin's name, so a machine can hold three of them.
func TestUninstallPurgeRemovesEveryDataDirectoryName(t *testing.T) {
	p := stagedHome(t)
	installOK(t)
	dirs := []string{p.data, marketplaceDataDir(p.home), legacyDataDir(p.home)}
	for _, d := range dirs {
		writeFileT(t, filepath.Join(d, "config.json"), "{}")
	}
	uninstallOK(t, "--purge")
	for _, d := range dirs {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Errorf("uninstall --purge left %s", d)
		}
	}
}

// TestUninstallNamesTheMarketplaceRegistration: that registration is Claude Code's and is not
// removed, so the report has to say it is still there rather than imply a clean machine.
func TestUninstallNamesTheMarketplaceRegistration(t *testing.T) {
	p := stagedHome(t)
	installOK(t)
	id := pluginName + "@" + marketplaceName
	writeFileT(t, installedPluginsPath(p.home),
		`{"version": 2, "plugins": {"`+id+`": [{"sourceProducerPath": "`+p.plugin+`"}]}}`)
	if out := uninstallOK(t); !strings.Contains(out, "claude plugin uninstall "+id) {
		t.Errorf("uninstall does not name the marketplace registration it left:\n%s", out)
	}
}
