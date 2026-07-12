package cmd

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/tszaks/pallium/internal/output"
)

var buildVersion = "dev"

type VersionReport struct {
	Module      string `json:"module"`
	Version     string `json:"version"`
	GoVersion   string `json:"go_version"`
	VCSRevision string `json:"vcs_revision,omitempty"`
	VCSModified string `json:"vcs_modified,omitempty"`
	Executable  string `json:"executable,omitempty"`
}

func runVersion(out io.Writer, jsonOutput bool) error {
	report := VersionReport{
		Module:    "github.com/tszaks/pallium",
		Version:   buildVersion,
		GoVersion: runtime.Version(),
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if info.Main.Path != "" {
			report.Module = info.Main.Path
		}
		if report.Version == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
			report.Version = info.Main.Version
		}
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				report.VCSRevision = setting.Value
			case "vcs.modified":
				report.VCSModified = setting.Value
			}
		}
	}
	report.Executable, _ = os.Executable()

	return output.Write(out, report, jsonOutput, func() string {
		lines := []string{
			"pallium " + report.Version,
			"module: " + report.Module,
			"go: " + report.GoVersion,
		}
		if report.VCSRevision != "" {
			lines = append(lines, "vcs revision: "+report.VCSRevision)
		}
		if report.VCSModified != "" {
			lines = append(lines, "vcs modified: "+report.VCSModified)
		}
		if report.Executable != "" {
			lines = append(lines, "executable: "+report.Executable)
		}
		if hint := adoptionHintLine(); hint != "" {
			lines = append(lines, hint)
		}
		return strings.Join(lines, "\n")
	})
}

// adoptionHintLine is the one-line nudge `version` prints when the current
// working directory has no Pallium adoption block yet — the second half of
// the "installer nobody runs" fix alongside maybeOfferAdoptionInstall
// (agents.go). `version` is the command most likely to run early in any
// session (an agent probing what's on the machine), so it is the cheapest
// place to plant a hint even for a repo where nobody has run `start` yet.
func adoptionHintLine() string {
	if adoptionHintSuppressed() {
		return ""
	}
	cwd, err := os.Getwd()
	if err != nil || hasAgentsBlock(cwd) {
		return ""
	}
	return "hint: no Pallium adoption block found in this repo — run `pallium agents install` to add one, or `pallium start \"<task>\"` will offer"
}

func shortVersion(report VersionReport) string {
	if report.Version != "" {
		return report.Version
	}
	if report.VCSRevision != "" {
		return report.VCSRevision
	}
	return fmt.Sprintf("%s %s", report.Module, report.GoVersion)
}
