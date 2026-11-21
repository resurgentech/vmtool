package vmtool

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// G1: connect (connect.go) is the one caller of the libvirt binding's connect
// functions, so its test guard covers every libvirt connection the module
// opens. These tests only parse source files; nothing here connects.

// libvirtConnectFuncs are the binding's functions that open a connection.
var libvirtConnectFuncs = map[string]bool{
	"NewConnect":                true,
	"NewConnectReadOnly":        true,
	"NewConnectWithAuth":        true,
	"NewConnectWithAuthDefault": true,
}

// connectUses returns "file:line in func F" for each use of a connect function
// in src, other than inside the function named allowedFunc when file is
// allowedFile. A use is any selector X.NewConnect… (a call or a function
// value) or a bare identifier of that name (a dot import).
func connectUses(fset *token.FileSet, file string, src []byte, allowedFile, allowedFunc string) ([]string, error) {
	f, err := parser.ParseFile(fset, file, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var out []string
	check := func(n ast.Node, fn string) {
		ast.Inspect(n, func(n ast.Node) bool {
			var name string
			var pos token.Pos
			switch x := n.(type) {
			case *ast.SelectorExpr:
				name, pos = x.Sel.Name, x.Sel.Pos()
				if libvirtConnectFuncs[name] {
					if !(file == allowedFile && fn == allowedFunc) {
						out = append(out, fset.Position(pos).String()+" in func "+fn)
					}
				}
				// Do not visit Sel again as a bare identifier.
				ast.Inspect(x.X, func(m ast.Node) bool {
					if id, ok := m.(*ast.Ident); ok && libvirtConnectFuncs[id.Name] && !(file == allowedFile && fn == allowedFunc) {
						out = append(out, fset.Position(id.Pos()).String()+" in func "+fn)
					}
					return true
				})
				return false
			case *ast.Ident:
				if libvirtConnectFuncs[x.Name] && !(file == allowedFile && fn == allowedFunc) {
					out = append(out, fset.Position(x.Pos()).String()+" in func "+fn)
				}
			}
			return true
		})
	}
	for _, d := range f.Decls {
		fn := ""
		if fd, ok := d.(*ast.FuncDecl); ok {
			fn = fd.Name.Name
		}
		check(d, fn)
	}
	return out, nil
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the package directory")
		}
		dir = parent
	}
}

func TestOnlyConnectOpensLibvirt(t *testing.T) {
	root := moduleRoot(t)
	allowed := filepath.Join(root, "pkg", "vmtool", "connect.go")
	fset := token.NewFileSet()
	var bad []string
	files := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".claude", ".git", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files++
		uses, err := connectUses(fset, path, src, allowed, "connect")
		if err != nil {
			return err
		}
		bad = append(bad, uses...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 100 {
		t.Fatalf("walked only %d .go files under %s; the walk is broken", files, root)
	}
	for _, b := range bad {
		t.Errorf("libvirt connect outside pkg/vmtool/connect.go func connect: %s", b)
	}
}

// The checker itself finds each form of use, and allows the one function.
func TestConnectUsesFindsEveryForm(t *testing.T) {
	src := `package p
import (
	"libvirt.org/go/libvirt"
	lv "libvirt.org/go/libvirt"
)
func connect(uri string) (*libvirt.Connect, error) { return libvirt.NewConnect(uri) }
func a() { libvirt.NewConnect("x") }
func b() { lv.NewConnectReadOnly("x") }
func c() { f := libvirt.NewConnectWithAuth; _ = f }
func d() { libvirt.NewConnectWithAuthDefault("x", 0) }
var e = libvirt.NewConnect
`
	fset := token.NewFileSet()
	got, err := connectUses(fset, "/m/pkg/vmtool/connect.go", []byte(src), "/m/pkg/vmtool/connect.go", "connect")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("found %d uses, want 5 (a, b, c, d, e): %v", len(got), got)
	}
	for _, s := range got {
		if strings.Contains(s, "in func connect") {
			t.Errorf("the allowed function was flagged: %s", s)
		}
	}
	got, err = connectUses(fset, "/m/pkg/vmtool/other.go", []byte(src), "/m/pkg/vmtool/connect.go", "connect")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 6 {
		t.Fatalf("in another file found %d uses, want 6: %v", len(got), got)
	}
}
