package workflow

import "testing"

func TestExtractLoopMetaParsesLoopConfig(t *testing.T) {
	script := `export const meta = {
  name: "review-until-clean",
  description: "review loop",
  kind: "loop",
  loop: {
    stagnationThreshold: 5,
    cycleBudgetUsd: 0.50,
    lifetimeBudgetUsd: 10,
    staleAfterMinutes: 20,
  },
};
phase("observe");
return { state: "success" };`

	cfg, ok, err := ExtractLoopMeta(script)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected meta.kind==\"loop\" to be recognized")
	}
	if cfg.StagnationThreshold != 5 {
		t.Fatalf("expected stagnationThreshold=5, got %d", cfg.StagnationThreshold)
	}
	if cfg.CycleBudgetUSD != 0.50 {
		t.Fatalf("expected cycleBudgetUsd=0.50, got %v", cfg.CycleBudgetUSD)
	}
	if cfg.LifetimeBudgetUSD != 10 {
		t.Fatalf("expected lifetimeBudgetUsd=10, got %v", cfg.LifetimeBudgetUSD)
	}
	if cfg.StaleAfterMinutes != 20 {
		t.Fatalf("expected staleAfterMinutes=20, got %d", cfg.StaleAfterMinutes)
	}
}

func TestExtractLoopMetaRejectsNonLoopKind(t *testing.T) {
	script := `export const meta = { name: "x", description: "y", phases: ["p"] };
phase("p");`
	_, ok, err := ExtractLoopMeta(script)
	if ok {
		t.Fatal("expected a regular (non-loop) script to be rejected")
	}
	if err == nil {
		t.Fatal("expected an explanatory error")
	}
}

func TestExtractLoopMetaRejectsMissingMeta(t *testing.T) {
	script := `phase("p");
return {};`
	_, ok, err := ExtractLoopMeta(script)
	if ok || err == nil {
		t.Fatal("expected a script with no meta block at all to be rejected with an error")
	}
}

func TestExtractLoopMetaDefaultsMissingLoopBlock(t *testing.T) {
	script := `export const meta = { name: "x", description: "y", kind: "loop" };
return { state: "no_op" };`
	cfg, ok, err := ExtractLoopMeta(script)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected meta.kind==\"loop\" to be recognized even with no loop{} block")
	}
	if cfg.StagnationThreshold != 0 || cfg.CycleBudgetUSD != 0 || cfg.LifetimeBudgetUSD != 0 {
		t.Fatalf("expected zero-value config when loop{} is absent, got %+v", cfg)
	}
}
