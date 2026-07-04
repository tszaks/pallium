package workflow

import (
	"fmt"
	"strings"

	"github.com/dop251/goja"
)

type ValidationResult struct {
	Valid bool   `json:"valid"`
	Error string `json:"error,omitempty"`
}

func ValidateScript(script string) ValidationResult {
	body := strings.TrimSpace(stripMeta(script))
	if body == "" {
		return ValidationResult{Valid: false, Error: "workflow script is empty"}
	}
	_, err := goja.Compile("workflow.js", "(async function(){\n"+body+"\n})", false)
	if err != nil {
		return ValidationResult{Valid: false, Error: fmt.Sprintf("%v", err)}
	}
	return ValidationResult{Valid: true}
}
