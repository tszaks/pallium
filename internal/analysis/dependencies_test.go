package analysis

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tszaks/pallium/internal/index"
)

func TestStructuralLinksIncludeGoDependencies(t *testing.T) {
	repo := indexRepo(t)
	store, err := index.OpenStore(repo)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	if _, err := index.New(store).Run(); err != nil {
		t.Fatalf("index run failed: %v", err)
	}

	links, err := StructuralLinks(store, "main.go", 10)
	if err != nil {
		t.Fatalf("StructuralLinks failed: %v", err)
	}

	found := false
	for _, link := range links {
		if link.Path == "helper.go" && link.Kind == "go-symbol" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected go dependency link to helper.go, got %#v", links)
	}
}

func TestStructuralLinksIncludeGoPackageImports(t *testing.T) {
	repo := indexRepo(t)
	store, err := index.OpenStore(repo)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	if _, err := index.New(store).Run(); err != nil {
		t.Fatalf("index run failed: %v", err)
	}

	links, err := StructuralLinks(store, "cli/app.go", 20)
	if err != nil {
		t.Fatalf("StructuralLinks failed: %v", err)
	}

	found := false
	for _, link := range links {
		if link.Path == "internalpkg/helper/helper.go" && link.Kind == "go-import" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected go import link to internalpkg/helper/helper.go, got %#v", links)
	}
}

func TestSuggestedTestCommandsForGoFile(t *testing.T) {
	repo := indexRepo(t)
	store, err := index.OpenStore(repo)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	if _, err := index.New(store).Run(); err != nil {
		t.Fatalf("index run failed: %v", err)
	}

	commands, err := SuggestedTestCommands(store, "main.go", 5)
	if err != nil {
		t.Fatalf("SuggestedTestCommands failed: %v", err)
	}

	if len(commands) < 2 || commands[0] != "go test ." || commands[1] != "go test ./..." {
		t.Fatalf("expected focused and broad go test commands, got %#v", commands)
	}
}

func TestStructuralLinksIncludeJSImports(t *testing.T) {
	repo := indexRepo(t)
	store, err := index.OpenStore(repo)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	if _, err := index.New(store).Run(); err != nil {
		t.Fatalf("index run failed: %v", err)
	}

	links, err := StructuralLinks(store, "web/app.ts", 10)
	if err != nil {
		t.Fatalf("StructuralLinks failed: %v", err)
	}

	found := false
	for _, link := range links {
		if link.Path == "web/session.ts" && link.Kind == "js-import" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected js import link to web/session.ts, got %#v", links)
	}
}

func TestStructuralLinksIncludeTSAliasImports(t *testing.T) {
	repo := indexRepo(t)
	store, err := index.OpenStore(repo)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	if _, err := index.New(store).Run(); err != nil {
		t.Fatalf("index run failed: %v", err)
	}

	links, err := StructuralLinks(store, "web/alias_app.ts", 10)
	if err != nil {
		t.Fatalf("StructuralLinks failed: %v", err)
	}

	found := false
	for _, link := range links {
		if link.Path == "web/session.ts" && link.Kind == "js-import" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected ts alias import link to web/session.ts, got %#v", links)
	}
}

func TestStructuralLinksIncludeNestedTSAliasImports(t *testing.T) {
	repo := indexRepo(t)
	store, err := index.OpenStore(repo)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	if _, err := index.New(store).Run(); err != nil {
		t.Fatalf("index run failed: %v", err)
	}

	links, err := StructuralLinks(store, "packages/app/src/feature.ts", 20)
	if err != nil {
		t.Fatalf("StructuralLinks failed: %v", err)
	}

	foundAppAlias := false
	foundSharedAlias := false
	for _, link := range links {
		if link.Path == "packages/app/src/util.ts" && link.Kind == "js-import" {
			foundAppAlias = true
		}
		if link.Path == "web/session.ts" && link.Kind == "js-import" {
			foundSharedAlias = true
		}
	}
	if !foundAppAlias || !foundSharedAlias {
		t.Fatalf("expected nested tsconfig aliases to resolve, got %#v", links)
	}
}

func TestStructuralLinksIncludePythonImports(t *testing.T) {
	repo := indexRepo(t)
	store, err := index.OpenStore(repo)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	if _, err := index.New(store).Run(); err != nil {
		t.Fatalf("index run failed: %v", err)
	}

	links, err := StructuralLinks(store, "pkg/app.py", 10)
	if err != nil {
		t.Fatalf("StructuralLinks failed: %v", err)
	}

	found := false
	for _, link := range links {
		if link.Path == "pkg/helper.py" && link.Kind == "py-import" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected python import link to pkg/helper.py, got %#v", links)
	}
}

func TestStructuralLinksIncludePythonSrcLayoutImports(t *testing.T) {
	repo := indexRepo(t)
	store, err := index.OpenStore(repo)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	if _, err := index.New(store).Run(); err != nil {
		t.Fatalf("index run failed: %v", err)
	}

	links, err := StructuralLinks(store, "src/pkgsrc/app.py", 10)
	if err != nil {
		t.Fatalf("StructuralLinks failed: %v", err)
	}

	found := false
	for _, link := range links {
		if link.Path == "src/pkgsrc/helper.py" && link.Kind == "py-import" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected python src-layout import link to src/pkgsrc/helper.py, got %#v", links)
	}
}

func TestRiskInfersContextForNewFile(t *testing.T) {
	repo := indexRepo(t)
	store, err := index.OpenStore(repo)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	if _, err := index.New(store).Run(); err != nil {
		t.Fatalf("index run failed: %v", err)
	}

	if err := os.WriteFile(filepath.Join(repo, "feature.go"), []byte("package main\n\nfunc feature() string { return helper() }\n"), 0o644); err != nil {
		t.Fatalf("write feature.go failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "feature_test.go"), []byte("package main\n\nimport \"testing\"\n\nfunc TestFeature(t *testing.T) {}\n"), 0o644); err != nil {
		t.Fatalf("write feature_test.go failed: %v", err)
	}

	report, err := Risk(store, "feature.go")
	if err != nil {
		t.Fatalf("Risk failed for new file: %v", err)
	}

	if report.Level == "unknown" {
		t.Fatalf("expected inferred risk level for new file, got %#v", report)
	}
	if len(report.Reasons) == 0 {
		t.Fatalf("expected inferred reasons for new file, got %#v", report)
	}

	commands, err := SuggestedTestCommands(store, "feature.go", 5)
	if err != nil {
		t.Fatalf("SuggestedTestCommands failed for new file: %v", err)
	}
	if len(commands) < 2 || commands[0] != "go test ." {
		t.Fatalf("expected focused go commands for new file, got %#v", commands)
	}
}

func TestSuggestedVerificationPlanForPythonFile(t *testing.T) {
	repo := indexRepo(t)
	store, err := index.OpenStore(repo)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	if _, err := index.New(store).Run(); err != nil {
		t.Fatalf("index run failed: %v", err)
	}

	plan, err := SuggestedVerificationPlan(store, "pkg/app.py")
	if err != nil {
		t.Fatalf("SuggestedVerificationPlan failed: %v", err)
	}

	if len(plan.Fast) == 0 || plan.Fast[0] != "pytest pkg/test_app.py" {
		t.Fatalf("expected focused python verification, got %#v", plan)
	}
	for _, expected := range []string{"pytest", "ruff check .", "mypy ."} {
		found := false
		for _, command := range plan.Full {
			if command == expected {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected full python verification to include %q, got %#v", expected, plan)
		}
	}
	for _, expected := range []string{"pytest pkg", "pytest", "ruff check ."} {
		found := false
		for _, command := range plan.Safe {
			if command == expected {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected safe python verification to include %q, got %#v", expected, plan)
		}
	}
}

func TestSuggestedVerificationPlanUsesRepoScriptsForJSTS(t *testing.T) {
	repo := indexRepo(t)
	store, err := index.OpenStore(repo)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	if _, err := index.New(store).Run(); err != nil {
		t.Fatalf("index run failed: %v", err)
	}

	plan, err := SuggestedVerificationPlan(store, "web/app.ts")
	if err != nil {
		t.Fatalf("SuggestedVerificationPlan failed: %v", err)
	}

	if len(plan.Fast) == 0 || plan.Fast[0] != "npm run test:unit -- web/session.test.ts" {
		t.Fatalf("expected repo-specific fast JS verification, got %#v", plan)
	}
	if len(plan.Safe) == 0 || plan.Safe[0] != "npm run test:unit" {
		t.Fatalf("expected repo-specific safe JS verification, got %#v", plan)
	}
	foundTypecheck := false
	for _, command := range plan.Full {
		if command == "npm run typecheck" {
			foundTypecheck = true
		}
	}
	if !foundTypecheck {
		t.Fatalf("expected full JS verification to include typecheck, got %#v", plan)
	}
}

func TestSuggestedVerificationPlanUsesMakeTargetsForJSTS(t *testing.T) {
	repo := indexRepo(t)
	store, err := index.OpenStore(repo)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	if err := os.WriteFile(filepath.Join(repo, "Makefile"), []byte("test:\n\t@echo test\nlint:\n\t@echo lint\nbuild:\n\t@echo build\n"), 0o644); err != nil {
		t.Fatalf("write Makefile failed: %v", err)
	}

	if _, err := index.New(store).Run(); err != nil {
		t.Fatalf("index run failed: %v", err)
	}

	plan, err := SuggestedVerificationPlan(store, "web/app.ts")
	if err != nil {
		t.Fatalf("SuggestedVerificationPlan failed: %v", err)
	}

	for _, expected := range []string{"make test", "make lint", "make build"} {
		found := false
		for _, command := range plan.Full {
			if command == expected {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected JS full verification to include %q, got %#v", expected, plan)
		}
	}
}

func TestSuggestedVerificationPlanUsesPythonRepoConfigs(t *testing.T) {
	repo := indexRepo(t)
	store, err := index.OpenStore(repo)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	if err := os.WriteFile(filepath.Join(repo, "tox.ini"), []byte("[tox]\nenvlist = py\n"), 0o644); err != nil {
		t.Fatalf("write tox.ini failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "pytest.ini"), []byte("[pytest]\naddopts = -q\n"), 0o644); err != nil {
		t.Fatalf("write pytest.ini failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "Makefile"), []byte("test:\n\t@pytest\ncheck:\n\t@echo check\n"), 0o644); err != nil {
		t.Fatalf("write Makefile failed: %v", err)
	}

	if _, err := index.New(store).Run(); err != nil {
		t.Fatalf("index run failed: %v", err)
	}

	plan, err := SuggestedVerificationPlan(store, "pkg/app.py")
	if err != nil {
		t.Fatalf("SuggestedVerificationPlan failed: %v", err)
	}

	for _, expected := range []string{"tox", "make test", "make check"} {
		found := false
		for _, command := range plan.Full {
			if command == expected {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected python verification to include %q, got %#v", expected, plan)
		}
	}
}
