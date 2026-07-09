package workflow

import (
	"fmt"

	"github.com/dop251/goja"
)

// LoopMetaConfig is what a loop script's `meta.loop` block configures.
// Captured ONCE at `loop start` (see cmd/loop.go) and stored on the Loop
// row — a loop does not silently re-read its script's meta on every tick,
// consistent with how regular workflow runs already handle script edits
// (script_hash/script_changed warns rather than silently adopting new
// content mid-run). Changing a loop's config requires an explicit
// `loop start` again, not just editing the file on disk.
type LoopMetaConfig struct {
	StagnationThreshold int
	CycleBudgetUSD      float64
	LifetimeBudgetUSD   float64
	StaleAfterMinutes   int
}

// ExtractLoopMeta evaluates a script's `export const meta = {...}` block
// (see extractMetaLiteral in runtime.go, the kernel-level half of this) and
// reports whether it declares meta.kind === "loop". A script with no meta
// at all, or meta.kind set to anything else, is NOT a loop script — ok is
// false and `loop start` must refuse it (protects against accidentally
// looping a one-shot workflow script forever).
//
// Evaluation happens in a throwaway goja VM with none of the deterministic
// guards or workflow primitives a real script execution gets — meta is a
// plain, side-effect-free JS object literal by convention, and this is
// ONLY extracting configuration from it, never running the script's actual
// body.
func ExtractLoopMeta(script string) (config LoopMetaConfig, ok bool, err error) {
	literal, found := extractMetaLiteral(script)
	if !found {
		return LoopMetaConfig{}, false, fmt.Errorf("loop scripts must declare export const meta = { kind: \"loop\", loop: {...} }; this script declares no meta block")
	}
	vm := goja.New()
	value, err := vm.RunString("(" + literal + ")")
	if err != nil {
		return LoopMetaConfig{}, false, fmt.Errorf("evaluating the script's meta block: %w", err)
	}
	raw, ok := value.Export().(map[string]any)
	if !ok {
		return LoopMetaConfig{}, false, fmt.Errorf("the script's meta block did not evaluate to an object")
	}
	kind, _ := raw["kind"].(string)
	if kind != "loop" {
		return LoopMetaConfig{}, false, fmt.Errorf("script's meta.kind is %q, not \"loop\" — pass a loop script to `loop start`", kind)
	}
	loopRaw, _ := raw["loop"].(map[string]any)
	cfg := LoopMetaConfig{
		StagnationThreshold: int(numberFromMeta(loopRaw["stagnationThreshold"])),
		CycleBudgetUSD:      numberFromMeta(loopRaw["cycleBudgetUsd"]),
		LifetimeBudgetUSD:   numberFromMeta(loopRaw["lifetimeBudgetUsd"]),
		StaleAfterMinutes:   int(numberFromMeta(loopRaw["staleAfterMinutes"])),
	}
	return cfg, true, nil
}

// numberFromMeta normalizes a goja-exported JS number: an integer-valued
// literal (e.g. `10`) exports as int64, anything with a fractional part
// (e.g. `0.5`) exports as float64. A missing/absent field is nil and
// returns 0, same as every other field this function defaults on absence.
func numberFromMeta(v any) float64 {
	switch n := v.(type) {
	case int64:
		return float64(n)
	case float64:
		return n
	default:
		return 0
	}
}
