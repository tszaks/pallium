package analysis

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/tszaks/pallium/internal/db"
)

type VerificationPlan struct {
	Fast []string `json:"fast"`
	Safe []string `json:"safe"`
	Full []string `json:"full"`
}

func SuggestedTestCommands(store *db.Store, targetPath string, limit int) ([]string, error) {
	plan, err := SuggestedVerificationPlan(store, targetPath)
	if err != nil {
		return nil, err
	}
	commands := make([]string, 0, len(plan.Fast)+len(plan.Safe)+len(plan.Full))
	commands = append(commands, plan.Fast...)
	commands = append(commands, plan.Safe...)
	commands = append(commands, plan.Full...)
	return uniqueStrings(commands, limit), nil
}

func SuggestedVerificationPlan(store *db.Store, targetPath string) (VerificationPlan, error) {
	normalized, err := normalizeRepoPath(store.RepoRoot, targetPath)
	if err != nil {
		return VerificationPlan{}, err
	}

	tests, err := SuggestedTests(store, normalized, 8)
	if err != nil {
		return VerificationPlan{}, err
	}

	return inferredVerificationPlan(store.RepoRoot, normalized, tests), nil
}

func inferredVerificationPlan(repoRoot, normalized string, tests []string) VerificationPlan {
	plan := VerificationPlan{}

	if strings.HasSuffix(normalized, ".go") || hasGoTests(tests) {
		packageDir := filepath.ToSlash(filepath.Dir(normalized))
		if packageDir == "." {
			plan.Fast = append(plan.Fast, "go test .")
			plan.Safe = append(plan.Safe, "go test .")
		} else {
			packageCmd := "go test ./" + packageDir
			plan.Fast = append(plan.Fast, packageCmd)
			plan.Safe = append(plan.Safe, packageCmd)
		}
		plan.Full = append(plan.Full, "go test ./...")
		return normalizeVerificationPlan(plan)
	}

	if strings.HasSuffix(normalized, ".go") {
		plan.Full = append(plan.Full, "go test ./...")
		return normalizeVerificationPlan(plan)
	}

	jsTest := firstMatchingPath(tests, func(path string) bool {
		return strings.HasSuffix(path, ".test.js") ||
			strings.HasSuffix(path, ".test.ts") ||
			strings.HasSuffix(path, ".test.tsx") ||
			strings.HasSuffix(path, ".spec.js") ||
			strings.HasSuffix(path, ".spec.ts") ||
			strings.HasSuffix(path, ".spec.tsx")
	})
	if jsTest != "" {
		packageManager := inferPackageManager(repoRoot)
		if packageManager != "" {
			scripts := packageScripts(repoRoot)
			switch {
			case hasScript(scripts, "test:unit"):
				plan.Fast = append(plan.Fast, targetedTestScriptCommand(packageManager, "test:unit", jsTest))
			case hasScript(scripts, "test"):
				plan.Fast = append(plan.Fast, targetedTestScriptCommand(packageManager, "test", jsTest))
			}
			if hasScript(scripts, "test:unit") {
				plan.Safe = append(plan.Safe, scriptCommand(packageManager, "test:unit"))
			}
			if hasScript(scripts, "test") {
				plan.Safe = append(plan.Safe, scriptCommand(packageManager, "test"))
			}
			plan.Full = append(plan.Full, inferJSTieredFullCheck(repoRoot, packageManager)...)
		}
		return normalizeVerificationPlan(plan)
	}
	if isJSImportFile(normalized) {
		packageManager := inferPackageManager(repoRoot)
		if packageManager != "" {
			scripts := packageScripts(repoRoot)
			if hasScript(scripts, "test:unit") {
				plan.Safe = append(plan.Safe, scriptCommand(packageManager, "test:unit"))
			}
			if hasScript(scripts, "test") {
				plan.Safe = append(plan.Safe, scriptCommand(packageManager, "test"))
			}
			plan.Full = append(plan.Full, inferJSTieredFullCheck(repoRoot, packageManager)...)
		}
		return normalizeVerificationPlan(plan)
	}

	pyTest := firstMatchingPath(tests, func(path string) bool {
		return strings.HasSuffix(path, "_test.py") || strings.HasPrefix(filepath.Base(path), "test_")
	})
	if pyTest != "" {
		plan.Fast = append(plan.Fast, "pytest "+pyTest)
		plan.Safe = append(plan.Safe, inferPySafeCommand(pyTest))
		plan.Safe = append(plan.Safe, inferPythonSafeChecks(repoRoot)...)
		plan.Full = append(plan.Full, inferPythonFullChecks(repoRoot)...)
		return normalizeVerificationPlan(plan)
	}
	if isPythonFile(normalized) {
		plan.Safe = append(plan.Safe, inferPythonSafeChecks(repoRoot)...)
		plan.Full = append(plan.Full, inferPythonFullChecks(repoRoot)...)
		return normalizeVerificationPlan(plan)
	}

	rubyTest := firstMatchingPath(tests, func(path string) bool {
		return strings.HasSuffix(path, "_spec.rb")
	})
	if rubyTest != "" {
		plan.Fast = append(plan.Fast, "bundle exec rspec "+rubyTest)
		plan.Safe = append(plan.Safe, inferRubySafeCommand(rubyTest))
		plan.Full = append(plan.Full, "bundle exec rspec")
		return normalizeVerificationPlan(plan)
	}
	if strings.HasSuffix(normalized, ".rb") {
		plan.Safe = append(plan.Safe, "bundle exec rspec")
		plan.Full = append(plan.Full, "bundle exec rspec")
		return normalizeVerificationPlan(plan)
	}

	return normalizeVerificationPlan(plan)
}

func firstMatchingPath(paths []string, predicate func(string) bool) string {
	for _, path := range paths {
		if predicate(path) {
			return path
		}
	}
	return ""
}

func inferPackageManager(repoRoot string) string {
	switch {
	case fileExists(filepath.Join(repoRoot, "pnpm-lock.yaml")):
		return "pnpm"
	case fileExists(filepath.Join(repoRoot, "yarn.lock")):
		return "yarn"
	case fileExists(filepath.Join(repoRoot, "bun.lock")), fileExists(filepath.Join(repoRoot, "bun.lockb")):
		return "bun"
	case fileExists(filepath.Join(repoRoot, "package-lock.json")), fileExists(filepath.Join(repoRoot, "package.json")):
		return "npm"
	default:
		return ""
	}
}

func inferJSTieredFullCheck(repoRoot, packageManager string) []string {
	scripts := packageScripts(repoRoot)
	commands := make([]string, 0, 4)
	if hasScript(scripts, "test") {
		commands = append(commands, scriptCommand(packageManager, "test"))
	}
	for _, script := range []string{"lint", "typecheck", "check", "build"} {
		if hasScript(scripts, script) {
			commands = append(commands, scriptCommand(packageManager, script))
		}
	}
	commands = append(commands, inferMakeTargets(repoRoot, "test", "lint", "typecheck", "check", "build")...)
	return commands
}

func inferPySafeCommand(testPath string) string {
	dir := filepath.ToSlash(filepath.Dir(testPath))
	if dir == "." {
		return "pytest"
	}
	return "pytest " + dir
}

func inferRubySafeCommand(testPath string) string {
	dir := filepath.ToSlash(filepath.Dir(testPath))
	if dir == "." {
		return "bundle exec rspec"
	}
	return "bundle exec rspec " + dir
}

func normalizeVerificationPlan(plan VerificationPlan) VerificationPlan {
	plan.Fast = uniqueStrings(plan.Fast, 0)
	plan.Safe = uniqueStrings(append(plan.Safe, plan.Fast...), 0)
	plan.Full = uniqueStrings(append(plan.Full, plan.Safe...), 0)
	return plan
}

func hasPackageScript(repoRoot, script string) bool {
	return hasScript(packageScripts(repoRoot), script)
}

func packageScripts(repoRoot string) map[string]string {
	content, err := os.ReadFile(filepath.Join(repoRoot, "package.json"))
	if err != nil {
		return nil
	}
	var parsed struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(content, &parsed); err != nil {
		return nil
	}
	return parsed.Scripts
}

func hasScript(scripts map[string]string, name string) bool {
	if len(scripts) == 0 {
		return false
	}
	_, ok := scripts[name]
	return ok
}

func scriptCommand(packageManager, script string) string {
	switch packageManager {
	case "npm":
		if script == "test" {
			return "npm test"
		}
		return "npm run " + script
	case "yarn":
		return "yarn " + script
	case "pnpm":
		return "pnpm " + script
	case "bun":
		return "bun run " + script
	default:
		return packageManager + " " + script
	}
}

func targetedTestScriptCommand(packageManager, script, testPath string) string {
	base := scriptCommand(packageManager, script)
	if testPath == "" {
		return base
	}
	return base + " -- " + testPath
}

func inferPythonSafeChecks(repoRoot string) []string {
	commands := []string{"pytest"}
	if text := readPythonProjectText(repoRoot); text != "" {
		if strings.Contains(text, "[tool.ruff") {
			commands = append(commands, "ruff check .")
		}
		if strings.Contains(text, "[testenv") || strings.Contains(text, "[tox]") {
			commands = append(commands, "tox")
		}
	}
	commands = append(commands, inferMakeTargets(repoRoot, "test", "lint")...)
	return uniqueStrings(commands, 0)
}

func inferPythonFullChecks(repoRoot string) []string {
	commands := inferPythonSafeChecks(repoRoot)
	if text := readPythonProjectText(repoRoot); text != "" {
		if strings.Contains(text, "[tool.mypy") {
			commands = append(commands, "mypy .")
		}
		if strings.Contains(text, "[tool.pyright") || fileExists(filepath.Join(repoRoot, "pyrightconfig.json")) {
			commands = append(commands, "pyright")
		}
	}
	commands = append(commands, inferMakeTargets(repoRoot, "check", "build")...)
	return uniqueStrings(commands, 0)
}

func inferMakeTargets(repoRoot string, targets ...string) []string {
	content, err := os.ReadFile(filepath.Join(repoRoot, "Makefile"))
	if err != nil {
		return nil
	}
	text := string(content)
	commands := make([]string, 0, len(targets))
	for _, target := range targets {
		if strings.Contains(text, target+":") {
			commands = append(commands, "make "+target)
		}
	}
	return uniqueStrings(commands, 0)
}

func readPythonProjectText(repoRoot string) string {
	var out strings.Builder
	for _, name := range []string{"pyproject.toml", "pytest.ini", "tox.ini", "setup.cfg"} {
		content, err := os.ReadFile(filepath.Join(repoRoot, name))
		if err != nil {
			continue
		}
		if out.Len() > 0 {
			out.WriteString("\n")
		}
		out.Write(content)
	}
	if fileExists(filepath.Join(repoRoot, "noxfile.py")) {
		out.WriteString("\n[nox]\n")
	}
	return out.String()
}
