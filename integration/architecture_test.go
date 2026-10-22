package integration

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Check compiler-selected production files, not test fixtures or generated
// messages. This protects service ownership even when no runtime is configured.
func TestProductionServiceOwnershipBoundaries(t *testing.T) {
	command := exec.Command("go", "list", "-json", "./services/...", "./cmd/...", "./pkg/...", "./adapters/...", "./workflows")
	command.Dir = ".."
	output, err := command.Output()
	if err != nil {
		t.Fatalf("list production packages: %v", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(output)))
	ledgerWrite := regexp.MustCompile(`(?i)\b(?:INSERT\s+INTO|UPDATE|DELETE\s+FROM)\s+ledger_`)
	const module = "github.com/osai/osai/"
	commandOwners := map[string]string{"ledger-server": "ledger", "ledger-sandbox": "ledger", "reconciliation": "reconciliation", "flutterwave-reconcile": "reconciliation", "temporal-worker": "settlement"}
	var packageCount int
	for {
		var pkg struct {
			ImportPath, Dir  string
			GoFiles, Imports []string
		}
		if err := decoder.Decode(&pkg); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		packageCount++
		relative := strings.TrimPrefix(pkg.ImportPath, module)
		owner := ""
		parts := strings.Split(relative, "/")
		if len(parts) > 1 && parts[0] == "services" {
			owner = parts[1]
		}
		if len(parts) > 1 && parts[0] == "cmd" {
			owner = commandOwners[parts[1]]
		}
		if parts[0] == "workflows" {
			owner = "settlement"
		}
		for _, dependency := range pkg.Imports {
			if strings.HasPrefix(dependency, module+"services/") && (owner == "" || !strings.HasPrefix(dependency+"/", module+"services/"+owner+"/")) {
				t.Errorf("%s imports another service implementation: %s", relative, dependency)
			}
		}
		for _, name := range pkg.GoFiles {
			file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(pkg.Dir, name), nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			if owner == "ledger" {
				continue
			}
			ast.Inspect(file, func(node ast.Node) bool {
				literal, ok := node.(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					return true
				}
				value, err := strconv.Unquote(literal.Value)
				if err == nil && ledgerWrite.MatchString(value) {
					t.Errorf("%s/%s writes Ledger tables outside the Ledger owner", relative, name)
				}
				return true
			})
		}
	}
	if packageCount == 0 {
		t.Fatal("ownership audit inspected no production packages")
	}
	t.Logf("inspected %d production packages: service implementation imports and literal Ledger writes", packageCount)
}
