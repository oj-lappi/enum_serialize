//Created because stringer [1] does not do enough.
//[1]: https://pkg.go.dev/golang.org/x/tools/cmd/stringer
package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"os"
	"regexp"
	"strings"
	"text/template"

	"golang.org/x/tools/go/packages"
)

var (
	flag_lookups = flag.String("lookup", "", "comma separated enum types to generate lookups for")
	flag_output  = flag.String("o", "", "output file")
	flag_package = flag.String("pkg", "", "package of output file")
)

func Usage() {
	fmt.Fprintf(os.Stderr, "Usage:\n")
	fmt.Fprintf(os.Stderr, "\tenum_serialize -lookup table1[enum1],table2[enum2] -o output -pkg pkg_name inputs\n")
	fmt.Fprintf(os.Stderr, "\n\n\tGenerates enum lookup tables $table for $enum in $output that can be used for serialization/deserialization or debugging.\n")
	fmt.Fprintf(os.Stderr, "\n\t-lookup table[enum],table[enum]...\n")
	fmt.Fprintf(os.Stderr, "\t\tgenerate a variable of type map[$enum]string with the name $table. To generate lookups for multiple enums, separate them with commas.\n")
	fmt.Fprintf(os.Stderr, "\n\t-o output\n")
	fmt.Fprintf(os.Stderr, "\t\tname of the generated output file\n")
	fmt.Fprintf(os.Stderr, "\n\t-pkg pkg_name\n")
	fmt.Fprintf(os.Stderr, "\t\tname of package in the generated output file\n")
	fmt.Fprintf(os.Stderr, "\n\tinputs\n")
	fmt.Fprintf(os.Stderr, "\t\tthe rest of the arguments are patterns or names of packages in which to look for the enums\n")
}

type Unit struct {
	pkg  *packages.Package
	file *ast.File
}

type Enum struct {
	Typ               string
	Imports           map[string]bool
	Lookup_table_name string
	Values            []EnumValue
}

type EnumValue struct {
	Name     string
	FullName string
	Value    interface{}
}

func main() {

	flag.Usage = Usage
	flag.Parse()

	if len(*flag_lookups) == 0 || len(*flag_output) == 0 || len(*flag_package) == 0 || flag.NArg() == 0 {
		flag.Usage()
		os.Exit(1)
	}

	enum_lookup_defs := strings.Split(*flag_lookups, ",")
	patterns := flag.Args()

	cfg := &packages.Config{Mode: packages.NeedSyntax | packages.NeedCompiledGoFiles | packages.NeedTypesInfo | packages.NeedTypes}
	pkgs, _ := packages.Load(cfg, patterns...)

	var units []Unit

	//Slurp all translation units from each listed package
	for _, pkg := range pkgs {
		for _, file := range pkg.Syntax {
			units = append(units, Unit{pkg, file})
		}
	}

	//Collect all.Values for each enum from the translation units
	enums := make(map[string]Enum)
	for _, enum_lookup_def := range enum_lookup_defs {
		enums[enum_lookup_def] = collectEnum(pkgs, units, enum_lookup_def)
	}

	//Generate
	generateFile(enums)
}

func collectEnum(pkgs []*packages.Package, units []Unit, enum_lookup_def string) Enum {

	// Parse the lookup table generation expression,
	// which is in the form lookup_table_name[enum_type]
	re := regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9_]*)\[([a-zA-Z][a-zA-Z0-9_\.]*)\]`)
	def_parts := re.FindStringSubmatch(enum_lookup_def)
	if len(def_parts) != 3 {
		fmt.Fprintf(os.Stderr, "Error: \"%s\" is not of the form: table \"[\" enum \"]\"\n", enum_lookup_def)
		fmt.Fprintf(os.Stderr, "the regex %v found %s\n", re, def_parts)
		for i := 0; i < len(def_parts); i++ {
			fmt.Fprintf(os.Stderr, "\t%s\n", def_parts[i])
		}
		os.Exit(2)
	}

	//Parse the enum type
	enum_type_parts := strings.Split(def_parts[2], ".")

	if len(enum_type_parts) > 2 {
		fmt.Fprintf(os.Stderr, "Error: \"%s\" is not a valid type identifier. Too many \".\"'s\n", def_parts[2])
		os.Exit(3)
	}
	var type_package, type_name string
	if len(enum_type_parts) == 2 {
		type_package = enum_type_parts[0]
		type_name = enum_type_parts[1]
	} else {
		type_package = *flag_package
		type_name = enum_type_parts[0]
	}

	//Build the Enum struct bit by bit
	enum := Enum{Lookup_table_name: def_parts[1], Values: make([]EnumValue, 0, 20), Imports: make(map[string]bool)}

	for _, unit := range units {
		pkg, file := unit.pkg, unit.file

		//Look through all const declarations
		for _, decl_node := range file.Decls {
			decl, ok := decl_node.(*ast.GenDecl)
			if !ok || decl.Tok != token.CONST {
				continue
			}

			current_typename := ""
			current_package := file.Name.Name

			//Look through declaration statements in the const declaration
			for _, spec := range decl.Specs {
				value_spec := spec.(*ast.ValueSpec)

				//
				switch value_spec.Type.(type) {
				case *ast.Ident:
					current_typename = value_spec.Type.(*ast.Ident).Name
				case *ast.SelectorExpr:
					selector := value_spec.Type.(*ast.SelectorExpr)
					//The only case where a SelectorExpr (ident "." ident) appears in a type is if this is a QualifiedIdent
					//Therefore the selector.X is the name of the package where the type has been defined
					current_package = selector.X.(*ast.Ident).Name
					current_typename = selector.Sel.Name
					//TODO: what if package has been renamed? Do we need to canonicalize the package name
					//i.e. do the precise_type at this point already
				default:
				}

				if current_typename == type_name && current_package == type_package {
					//Extract all necessary info about the enum.Values
					for _, n := range value_spec.Names {
						name := n.Name
						if name == "_" {
							continue
						}

						//IN: enum value identifier and the containing package
						//OUT: enumValue{name, val} + pkg path & pkg-prefixed typename of the enum type
						type_str := pkg.TypesInfo.Defs[n].Type().String()
						precise_type := strings.SplitN(type_str, ".", 2)

						value := constant.Val(pkg.TypesInfo.Defs[n].(*types.Const).Val())
						value_pkg := pkg.TypesInfo.Defs[n].(*types.Const).Pkg()
						full_name := name
						if value_pkg.Name() != *flag_package {
							full_name = value_pkg.Name() + "." + name
							enum.Imports[value_pkg.Path()] = true
						}
						//TODO: what if the names are the same? We could add aliasing to the imports
						enum.Values = append(enum.Values, EnumValue{Name: name, FullName: full_name, Value: value})
						enum.Imports[precise_type[0]] = true
						package_path := strings.Split(precise_type[0], "/")
						enum.Typ = package_path[len(package_path)-1] + "." + precise_type[1]
					}
				}
			}
		}

	}
	return enum
}

func generateFile(enums map[string]Enum) {

	imports := make(map[string]string)
	for _, enum := range enums {
		for pkg, _ := range enum.Imports {
			imports[pkg] = pkg
		}
	}

	//Create generated file
	out_file, err := os.Create(*flag_output)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating file \"%s\":\n%v \n", err)
		os.Exit(4)
	}

	//The template takes an anonymous structs with two fields
	//We generate a lookup table per enum
	tmpl, _ := template.New("generated enum lookup file").Parse(`//Code generated by enum_serialize. DO NOT EDIT.

package {{ .Pkg }}
{{ if len .Imports | eq 1 }}
import {{ range .Imports -}} "{{ . }}" {{- end }}
{{- else }}
import (
	{{- range .Imports }}
	"{{ . }}"
	{{- end }}
)
{{- end }}

{{ range .Enums -}}
var {{ .Lookup_table_name }}  = map[{{ .Typ }}]string{
	{{- range .Values }}
	{{ .FullName }} : "{{ .Name }}",{{ end }}
}

{{ end }}`)

	err = tmpl.Execute(out_file, struct {
		Pkg     string
		Imports map[string]string
		Enums   map[string]Enum
	}{Pkg: *flag_package, Imports: imports, Enums: enums})

	if err != nil {
		fmt.Printf("Error writing to %s: %v\n", *flag_output, err)
		os.Exit(5)
	}
}
