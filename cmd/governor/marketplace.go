package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// THE MARKETPLACE ROUTE (GOV-19): ONE TREE, TWO REGISTRATIONS.
//
// `governor install` is the only thing that writes the plugin. The marketplace entry in
// .claude-plugin/marketplace.json does not carry a second copy of it — a marketplace plugin's
// files come from git, and git does not hold the binary — it carries a COMMAND SOURCE that
// runs `governor plugin-path`, which prints the tree install already wrote. Claude Code links
// that tree into its cache, so a marketplace install and an install.sh install load the same
// files and run the same binary.
//
// Measured on 2.1.310 (hook-surface.md section 23), and each fact is why this is shaped as it
// is:
//
//  1. A marketplace plugin and a skills-dir plugin with the same manifest name do NOT both
//     load. The marketplace copy wins and the skills-dir copy is listed "Not loaded". So two
//     routes cannot double-register the hooks — but a FOREIGN copy, one that is not a link to
//     this tree, would silently take the hooks away from it. checkForeignCopies exists for that.
//  2. The command re-runs on EVERY session start, not only at install. plugin-path is on the
//     session-start path, which is why it stats two files and does nothing else: no daemon,
//     no network, no write.
//  3. Link mode makes ${CLAUDE_PLUGIN_ROOT} a cache directory whose top-level entries are
//     symlinks into the printed tree, so ${CLAUDE_PLUGIN_ROOT}/bin/governor is still the one
//     binary.
//  4. ${CLAUDE_PLUGIN_DATA} follows the route: data/<plugin>-<marketplace>. uninstall --purge
//     removes both names.
//  5. A command source cannot be accepted from inside a Claude Code session — -y and
//     --accept-command are both ignored there. A person accepts it in their own terminal.

// marketplaceName is the "name" in .claude-plugin/marketplace.json, and so the second half of
// both the plugin ID (claude-governor@claude-governor) and the data directory Claude Code
// creates for it. TestTheMarketplaceManifestPointsAtThisTree holds the file to it.
const marketplaceName = "claude-governor"

// pluginPathCommand is the command the marketplace entry runs. A bare name, resolved on PATH,
// because the binary arrives by brew or by install.sh's ~/.local/bin link, and an absolute
// path would be right on exactly one machine.
const pluginPathCommand = "governor plugin-path"

// runPluginPath prints the installed plugin tree, for a marketplace command source.
//
// It exits non-zero, printing nothing on stdout, when there is no tree to point at — a
// machine where `brew install` ran but `governor install` has not. Claude Code then reports
// the plugin as not loaded, which is the honest state: nothing is governed and no session is
// stalled, the same outcome as a daemon that is not running.
func runPluginPath(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintf(stderr, "governor plugin-path: takes no arguments\n")
		return 2
	}
	p, err := resolvePaths()
	if err != nil {
		fmt.Fprintf(stderr, "governor plugin-path: %v\n", err)
		return 1
	}
	if _, err := os.Stat(p.manifest); err != nil || !executableExists(p.binary) {
		fmt.Fprintf(stderr, "governor plugin-path: no plugin installed at %s — run: governor install\n",
			short(p.plugin))
		return 1
	}
	fmt.Fprintln(stdout, p.plugin)
	return 0
}

// pluginRegistration is one installed_plugins.json entry for a plugin named pluginName.
type pluginRegistration struct {
	id string // claude-governor@<marketplace>
	// producer is the directory a command source printed, when the entry came from one. For
	// our own marketplace entry it is the tree install wrote.
	producer string
}

// linksTo reports whether this registration is a link to tree rather than a copy from
// somewhere else.
func (r pluginRegistration) linksTo(tree string) bool {
	return r.producer != "" && samePath(r.producer, tree)
}

// samePath compares two paths after resolving symlinks, falling back to a lexical compare for
// a path that does not exist. macOS's /tmp is /private/tmp, and Claude Code records whichever
// spelling the command printed.
func samePath(a, b string) bool {
	if ra, err := filepath.EvalSymlinks(a); err == nil {
		a = ra
	}
	if rb, err := filepath.EvalSymlinks(b); err == nil {
		b = rb
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// installedPluginsPath is Claude Code's record of marketplace installs (hook-surface.md
// section 15.1).
func installedPluginsPath(home string) string {
	return filepath.Join(home, ".claude", "plugins", "installed_plugins.json")
}

// pluginRegistrations reads every marketplace install of a plugin named pluginName.
//
// A missing file is no registrations, not an error: most machines never add a marketplace.
// The file is Claude Code's and its schema is not ours, so only the two fields this needs are
// read and anything else in it is ignored.
func pluginRegistrations(home string) ([]pluginRegistration, error) {
	raw, err := os.ReadFile(installedPluginsPath(home))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var doc struct {
		Plugins map[string][]struct {
			SourceProducerPath string `json:"sourceProducerPath"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s: %v", short(installedPluginsPath(home)), err)
	}
	var regs []pluginRegistration
	for id, entries := range doc.Plugins {
		if !strings.HasPrefix(id, pluginName+"@") {
			continue
		}
		r := pluginRegistration{id: id}
		for _, e := range entries {
			if e.SourceProducerPath != "" {
				r.producer = e.SourceProducerPath
			}
		}
		regs = append(regs, r)
	}
	sort.Slice(regs, func(i, j int) bool { return regs[i].id < regs[j].id })
	return regs, nil
}

// foreignCopies describes every marketplace install of this plugin that is not a link to
// tree, with the command that removes them, or "" when there are none.
//
// Our own entry linking to tree is not a problem: it is the same files, and the skills-dir
// copy it shadows is the one it points at. An unreadable installed_plugins.json is reported
// as "" rather than a failure — it is Claude Code's file, and doctor failing the hooks check
// over a file it does not own and cannot fix would be the wrong instrument.
func foreignCopies(tree string) (problem, fix string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", ""
	}
	regs, err := pluginRegistrations(home)
	if err != nil {
		return "", ""
	}
	var ids, fixes []string
	for _, r := range regs {
		if r.linksTo(tree) {
			continue
		}
		ids = append(ids, r.id)
		fixes = append(fixes, "claude plugin uninstall "+r.id)
	}
	if len(ids) == 0 {
		return "", ""
	}
	return fmt.Sprintf("%s is installed from a marketplace and takes precedence over %s, so its hooks run instead of these",
		strings.Join(ids, ", "), short(tree)), strings.Join(fixes, " && ")
}

// marketplaceDataDir is ${CLAUDE_PLUGIN_DATA} when the plugin loaded through our marketplace.
func marketplaceDataDir(home string) string {
	return filepath.Join(home, ".claude", "plugins", "data", pluginName+"-"+marketplaceName)
}

// THE RENAME (GOV-19). The plugin was called claude-governor until `claude plugin validate`
// refused the name: Claude Code reserves the "claude-" prefix so that a third party's plugin
// cannot pass as Anthropic's own. It is build-governor now, and that changes the one thing a
// rename must not get wrong.
//
// TWO NAMES MEANS TWO PLUGINS. The precedence rule that stops a marketplace copy and a
// skills-dir copy from both loading is keyed on the manifest NAME (hook-surface.md section 23).
// A machine with ~/.claude/skills/claude-governor/ from an earlier install and
// ~/.claude/skills/build-governor/ from this one has two differently named plugins, and both
// load: every hook registered twice, every spawn taking two leases, the standing advice
// injected twice. So install removes the old tree, uninstall removes it too, and doctor fails
// while both are present.

// legacyPluginName is what the plugin was called before the rename.
const legacyPluginName = "claude-governor"

// legacyPluginTree is where an install from before the rename put the plugin.
func legacyPluginTree(home string) string {
	return filepath.Join(home, ".claude", "skills", legacyPluginName)
}

// legacyDataDir is ${CLAUDE_PLUGIN_DATA} as it was named before the rename. Its config and
// onboarding marker are not carried across — install recalibrates, and the notice shows once
// more under the new name — but uninstall --purge still has to remove it.
func legacyDataDir(home string) string {
	return filepath.Join(home, ".claude", "plugins", "data", legacyPluginName+"-skills-dir")
}

// isOurLegacyTree reports whether dir is a plugin tree an earlier governor install wrote.
//
// Both halves are required before anything is removed: the manifest naming the old plugin AND
// the binary install puts beside it. A directory of that name holding anything else is
// somebody's own, and install does not delete what it did not write.
func isOurLegacyTree(dir string) bool {
	raw, err := os.ReadFile(filepath.Join(dir, ".claude-plugin", "plugin.json"))
	if err != nil {
		return false
	}
	var m struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &m) != nil || m.Name != legacyPluginName {
		return false
	}
	return executableExists(filepath.Join(dir, "bin", installedBinaryName))
}

// removeLegacyTree removes the pre-rename plugin tree when it is ours, recording the step. With
// plan set it only records what it would do.
func (in *installer) removeLegacyTree(plan bool) {
	dir := legacyPluginTree(in.p.home)
	if !isOurLegacyTree(dir) {
		return
	}
	if plan {
		in.steps = append(in.steps, step{path: dir, action: "remove",
			comment: "the plugin's name before the rename; left in place it would load beside this one"})
		return
	}
	in.removeTree(dir)
}
