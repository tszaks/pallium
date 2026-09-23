package codeindex

import "testing"

func TestScannerMasksCommentsAndFindsMethodsAndMultilineImports(t *testing.T) {
	text := []byte("/*\nexport function ImaginaryAdmin() {}\n*/\nimport {\n realThing\n} from './dependency';\nexport class Service {\n call() {\n  return realThing();\n }\n}\nconst quoted = `\nexport function Fake() {}\n`;\n")
	p, ok := Parse("service.ts", text, NewResolver(t.TempDir(), []string{"service.ts", "dependency.ts"}))
	if !ok {
		t.Fatal("parse")
	}
	method := false
	for _, s := range p.Symbols {
		if s.Name == "ImaginaryAdmin" || s.Name == "Fake" {
			t.Fatal("invented symbol")
		}
		if s.Name == "call" {
			method = s.Receiver == "Service" && s.EndLine > s.StartLine
		}
	}
	if !method {
		t.Fatalf("method missing: %+v", p.Symbols)
	}
	if len(p.Imports) != 1 || p.Imports[0].RawSpec != "./dependency" {
		t.Fatalf("imports: %+v", p.Imports)
	}
}
